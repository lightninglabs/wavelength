package oor

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/tlv"
)

// PackageVersion identifies the wire/storage encoding version for OOR packages.
type PackageVersion uint32

const (
	// PackageVersionV1 is the first versioned encoding of OOR
	// submit/finalize packages.
	PackageVersionV1 PackageVersion = 1
)

const (
	submitPackageVersionRecordType     tlv.Type = 1
	submitPackageArkPSBTRecordType     tlv.Type = 2
	submitPackageCheckpointsRecordType tlv.Type = 3
)

// SubmitPackage is the v0 OOR submit payload: an Ark tx PSBT plus checkpoint
// PSBTs.
//
// This is the shape we expect to send over RPC later. Today it is also the
// natural unit to persist durably and to hash/deduplicate.
type SubmitPackage struct {
	// ArkPSBT is the canonical Ark transaction PSBT.
	//
	// The v0 session identifier is derived from its unsigned txid.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs are the checkpoint transactions that the Ark tx
	// spends.
	CheckpointPSBTs []*psbt.Packet
}

// SessionID derives the stable v0 session identifier from the Ark txid.
func (p *SubmitPackage) SessionID() (chainhash.Hash, error) {
	if p == nil || p.ArkPSBT == nil || p.ArkPSBT.UnsignedTx == nil {
		return chainhash.Hash{}, fmt.Errorf("ark psbt must be provided")
	}

	return p.ArkPSBT.UnsignedTx.TxHash(), nil
}

// Validate applies the shared structural submit validation rules.
func (p *SubmitPackage) Validate() (*ValidatedSubmitPackage, error) {
	if p == nil {
		return nil, fmt.Errorf("submit package must be provided")
	}

	return ValidateSubmitPackage(p.ArkPSBT, p.CheckpointPSBTs)
}

// FinalizePackage is the v0 OOR finalize payload: the Ark tx PSBT plus
// finalized checkpoint PSBTs.
type FinalizePackage struct {
	// ArkPSBT is the canonical Ark transaction PSBT.
	ArkPSBT *psbt.Packet

	// FinalCheckpointPSBTs are the finalized checkpoint PSBTs.
	// They match the Ark tx inputs and carry the canonical
	// checkpoint metadata.
	FinalCheckpointPSBTs []*psbt.Packet
}

// Validate applies the shared structural finalize validation rules.
func (p *FinalizePackage) Validate() error {
	if p == nil {
		return fmt.Errorf("finalize package must be provided")
	}

	return ValidateFinalizePackage(p.ArkPSBT, p.FinalCheckpointPSBTs)
}

// MarshalSubmitPackage encodes a submit package as versioned TLV with binary
// PSBT payloads.
func MarshalSubmitPackage(pkg *SubmitPackage) ([]byte, error) {
	if pkg == nil {
		return nil, fmt.Errorf("submit package must be provided")
	}

	arkRaw, err := psbtutil.Serialize(pkg.ArkPSBT)
	if err != nil {
		return nil, err
	}

	checkpoints := make([][]byte, 0, len(pkg.CheckpointPSBTs))
	for _, cp := range pkg.CheckpointPSBTs {
		cpRaw, err := psbtutil.Serialize(cp)
		if err != nil {
			return nil, err
		}

		checkpoints = append(checkpoints, cpRaw)
	}

	checkpointBlob, err := encodeBlobList(checkpoints)
	if err != nil {
		return nil, err
	}

	version := uint64(PackageVersionV1)
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			submitPackageVersionRecordType, &version,
		),
		tlv.MakePrimitiveRecord(
			submitPackageArkPSBTRecordType, &arkRaw,
		),
		tlv.MakePrimitiveRecord(
			submitPackageCheckpointsRecordType, &checkpointBlob,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var b bytes.Buffer
	if err := stream.Encode(&b); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

// UnmarshalSubmitPackage decodes a submit package previously encoded by
// MarshalSubmitPackage.
func UnmarshalSubmitPackage(b []byte) (*SubmitPackage, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("submit package bytes must be provided")
	}

	var (
		version        uint64
		arkRaw         []byte
		checkpointBlob []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			submitPackageVersionRecordType, &version,
		),
		tlv.MakePrimitiveRecord(
			submitPackageArkPSBTRecordType, &arkRaw,
		),
		tlv.MakePrimitiveRecord(
			submitPackageCheckpointsRecordType, &checkpointBlob,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	// Validate the record framing against the bytes physically present
	// before decoding. The lnd tlv non-P2P decode path sizes allocations
	// from the declared record length without bounding it, so an
	// attacker-controlled length on this RPC/durable path could otherwise
	// panic or OOM.
	reader, err := safeTLVReader(b)
	if err != nil {
		return nil, err
	}
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return nil, err
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("trailing package bytes")
	}

	if version != uint64(PackageVersionV1) {
		return nil, fmt.Errorf("unknown package version: %d", version)
	}

	ark, err := psbtutil.Parse(arkRaw)
	if err != nil {
		return nil, err
	}

	checkpointRaws, err := decodeBlobList(checkpointBlob)
	if err != nil {
		return nil, err
	}

	checkpoints := make([]*psbt.Packet, 0, len(checkpointRaws))
	for _, cpRaw := range checkpointRaws {
		cp, err := psbtutil.Parse(cpRaw)
		if err != nil {
			return nil, err
		}

		checkpoints = append(checkpoints, cp)
	}

	return &SubmitPackage{
		ArkPSBT:         ark,
		CheckpointPSBTs: checkpoints,
	}, nil
}

func encodeBlobList(blobs [][]byte) ([]byte, error) {
	var (
		buf     bytes.Buffer
		scratch [8]byte
	)

	if err := tlv.WriteVarInt(
		&buf, uint64(len(blobs)), &scratch,
	); err != nil {
		return nil, err
	}

	for i := range blobs {
		if err := tlv.WriteVarInt(
			&buf,
			uint64(
				len(blobs[i]),
			),
			&scratch,
		); err != nil {
			return nil, err
		}

		if _, err := buf.Write(blobs[i]); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

func decodeBlobList(raw []byte) ([][]byte, error) {
	var scratch [8]byte

	reader := bytes.NewReader(raw)
	count, err := tlv.ReadVarInt(reader, &scratch)
	if err != nil {
		return nil, err
	}

	// Bound the declared count against the bytes physically present
	// before allocating. Each blob is encoded as at least a one-byte
	// length prefix, so a count larger than the remaining bytes is
	// definitionally a lie and would otherwise drive an unbounded (or
	// panicking) make() from attacker-controlled input.
	if count > uint64(reader.Len()) {
		return nil, fmt.Errorf("blob count %d exceeds %d remaining "+
			"bytes", count, reader.Len())
	}

	blobs := make([][]byte, 0, count)
	for i := uint64(0); i < count; i++ {
		size, err := tlv.ReadVarInt(reader, &scratch)
		if err != nil {
			return nil, err
		}

		// Likewise bound each blob length against the bytes still
		// available so a huge declared size cannot allocate gigabytes
		// before io.ReadFull discovers the truncation.
		if size > uint64(reader.Len()) {
			return nil, fmt.Errorf("blob %d size %d exceeds %d "+
				"remaining bytes", i, size, reader.Len())
		}

		blob := make([]byte, size)
		if _, err := io.ReadFull(reader, blob); err != nil {
			return nil, err
		}

		blobs = append(blobs, blob)
	}

	if reader.Len() != 0 {
		return nil, fmt.Errorf("trailing package bytes")
	}

	return blobs, nil
}

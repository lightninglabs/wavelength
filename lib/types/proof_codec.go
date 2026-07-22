// TxProof TLV serialization and deserialization.
//
// This is the canonical location for the TxProof TLV codec. Both the
// wallet (serialization for wire transport) and the db package
// (persistence) import from here.

package types

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightningnetwork/lnd/tlv"
)

// TLV type aliases for TxProof serialization. These MUST match the
// assignments in db/proof_codec.go.
type (
	tlvTxProofMsgTx         = tlv.TlvType0
	tlvTxProofBlockHeader   = tlv.TlvType1
	tlvTxProofBlockHeight   = tlv.TlvType2
	tlvTxProofMerkleProof   = tlv.TlvType3
	tlvTxProofClaimedOutput = tlv.TlvType4
	tlvTxProofInternalKey   = tlv.TlvType5
	tlvTxProofMerkleRoot    = tlv.TlvType6
)

type txProofOutpointRecord struct {
	wire.OutPoint
}

func (o *txProofOutpointRecord) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		0, o, 36, txProofOutpointEncoder, txProofOutpointDecoder,
	)
}

func txProofOutpointEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	if o, ok := val.(*txProofOutpointRecord); ok {
		if _, err := w.Write(o.Hash[:]); err != nil {
			return err
		}

		var buf [4]byte
		binary.LittleEndian.PutUint32(buf[:], o.Index)
		_, err := w.Write(buf[:])

		return err
	}

	return tlv.NewTypeForEncodingErr(val, "txProofOutpointRecord")
}

func txProofOutpointDecoder(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if l != 36 {
		return fmt.Errorf("invalid outpoint length: %d", l)
	}

	if o, ok := val.(*txProofOutpointRecord); ok {
		if _, err := io.ReadFull(r, o.Hash[:]); err != nil {
			return err
		}

		var buf [4]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return err
		}

		o.Index = binary.LittleEndian.Uint32(buf[:])

		return nil
	}

	return tlv.NewTypeForDecodingErr(
		val, "txProofOutpointRecord", l, 36,
	)
}

type txProofMsgTxRecord struct {
	Tx *wire.MsgTx
}

func (t *txProofMsgTxRecord) Record() tlv.Record {
	recordSize := func() uint64 {
		if t.Tx == nil {
			return 0
		}

		return uint64(t.Tx.SerializeSize())
	}

	return tlv.MakeDynamicRecord(
		0, t, recordSize, txProofMsgTxEncoder, txProofMsgTxDecoder,
	)
}

func txProofMsgTxEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	if t, ok := val.(*txProofMsgTxRecord); ok {
		if t.Tx == nil {
			return nil
		}

		return t.Tx.Serialize(w)
	}

	return tlv.NewTypeForEncodingErr(val, "txProofMsgTxRecord")
}

func txProofMsgTxDecoder(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if t, ok := val.(*txProofMsgTxRecord); ok {
		t.Tx = &wire.MsgTx{}

		return t.Tx.Deserialize(r)
	}

	return tlv.NewTypeForDecodingErr(
		val, "txProofMsgTxRecord", l, l,
	)
}

type txProofBlockHeaderRecord struct {
	Header wire.BlockHeader
}

func (t *txProofBlockHeaderRecord) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		0, t, 80, txProofBlockHeaderEncoder, txProofBlockHeaderDecoder,
	)
}

func txProofBlockHeaderEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	if t, ok := val.(*txProofBlockHeaderRecord); ok {
		return t.Header.Serialize(w)
	}

	return tlv.NewTypeForEncodingErr(
		val, "txProofBlockHeaderRecord",
	)
}

func txProofBlockHeaderDecoder(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if l != 80 {
		return fmt.Errorf("invalid block header length: %d", l)
	}

	if t, ok := val.(*txProofBlockHeaderRecord); ok {
		return t.Header.Deserialize(r)
	}

	return tlv.NewTypeForDecodingErr(
		val, "txProofBlockHeaderRecord", l, 80,
	)
}

type txProofMerkleProofRecord struct {
	Proof proof.TxMerkleProof
}

func (t *txProofMerkleProofRecord) Record() tlv.Record {
	recordSize := func() uint64 {
		var buf bytes.Buffer
		_ = t.Proof.Encode(&buf)

		return uint64(buf.Len())
	}

	return tlv.MakeDynamicRecord(
		0, t, recordSize, txProofMerkleProofEncoder,
		txProofMerkleProofDecoder,
	)
}

func txProofMerkleProofEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	if t, ok := val.(*txProofMerkleProofRecord); ok {
		return t.Proof.Encode(w)
	}

	return tlv.NewTypeForEncodingErr(
		val, "txProofMerkleProofRecord",
	)
}

func txProofMerkleProofDecoder(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if t, ok := val.(*txProofMerkleProofRecord); ok {
		return t.Proof.Decode(r)
	}

	return tlv.NewTypeForDecodingErr(
		val, "txProofMerkleProofRecord", l, l,
	)
}

type tlvTxProof struct {
	MsgTx       tlv.RecordT[tlvTxProofMsgTx, txProofMsgTxRecord]
	BlockHeader tlv.RecordT[tlvTxProofBlockHeader, txProofBlockHeaderRecord]
	BlockHeight tlv.RecordT[tlvTxProofBlockHeight, uint32]
	MerkleProof tlv.RecordT[tlvTxProofMerkleProof, txProofMerkleProofRecord]

	//nolint:ll
	ClaimedOutput tlv.RecordT[tlvTxProofClaimedOutput, txProofOutpointRecord]
	InternalKey   tlv.RecordT[tlvTxProofInternalKey, *btcec.PublicKey]
	MerkleRoot    tlv.RecordT[tlvTxProofMerkleRoot, []byte]
}

// SerializeTxProof serializes a proof.TxProof to TLV-encoded bytes.
func SerializeTxProof(p *proof.TxProof) ([]byte, error) {
	if p == nil {
		return nil, nil
	}

	t := &tlvTxProof{
		MsgTx: tlv.NewRecordT[tlvTxProofMsgTx](
			txProofMsgTxRecord{
				Tx: &p.MsgTx,
			},
		),
		BlockHeader: tlv.NewRecordT[tlvTxProofBlockHeader](
			txProofBlockHeaderRecord{
				Header: p.BlockHeader,
			},
		),
		BlockHeight: tlv.NewPrimitiveRecord[tlvTxProofBlockHeight](
			p.BlockHeight,
		),
		MerkleProof: tlv.NewRecordT[tlvTxProofMerkleProof](
			txProofMerkleProofRecord{
				Proof: p.MerkleProof,
			},
		),
		ClaimedOutput: tlv.NewRecordT[tlvTxProofClaimedOutput](
			txProofOutpointRecord{
				OutPoint: p.ClaimedOutPoint,
			},
		),
		InternalKey: tlv.NewPrimitiveRecord[tlvTxProofInternalKey](
			&p.InternalKey,
		),
		MerkleRoot: tlv.NewPrimitiveRecord[tlvTxProofMerkleRoot](
			p.MerkleRoot,
		),
	}

	records := []tlv.Record{
		t.MsgTx.Record(),
		t.BlockHeader.Record(),
		t.BlockHeight.Record(),
		t.MerkleProof.Record(),
		t.ClaimedOutput.Record(),
		t.InternalKey.Record(),
		t.MerkleRoot.Record(),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, fmt.Errorf("create TLV stream: %w", err)
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, fmt.Errorf("encode TxProof: %w", err)
	}

	return buf.Bytes(), nil
}

// DeserializeTxProof deserializes a proof.TxProof from TLV-encoded bytes.
func DeserializeTxProof(data []byte) (*proof.TxProof, error) {
	if len(data) == 0 {
		return nil, nil
	}

	t := &tlvTxProof{}

	records := []tlv.Record{
		t.MsgTx.Record(),
		t.BlockHeader.Record(),
		t.BlockHeight.Record(),
		t.MerkleProof.Record(),
		t.ClaimedOutput.Record(),
		t.InternalKey.Record(),
		t.MerkleRoot.Record(),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, fmt.Errorf("create TLV stream: %w", err)
	}

	// Pre-validate the framing so a record declaring a length larger than
	// the bytes present cannot drive an unbounded make() inside the tlv
	// decoder (notably the variable-length MerkleRoot record and the
	// unknown-record discard buffer). TxProof bytes arrive as an untrusted
	// boarding SPV proof and also persist durably.
	reader, err := safeTypesTLVBytes(data)
	if err != nil {
		return nil, fmt.Errorf("decode TxProof: %w", err)
	}

	if err := stream.Decode(reader); err != nil {
		return nil, fmt.Errorf("decode TxProof: %w", err)
	}

	// Reconstruct the TxProof from decoded TLV fields.
	p := &proof.TxProof{
		BlockHeader:     t.BlockHeader.Val.Header,
		BlockHeight:     t.BlockHeight.Val,
		MerkleProof:     t.MerkleProof.Val.Proof,
		ClaimedOutPoint: t.ClaimedOutput.Val.OutPoint,
		MerkleRoot:      t.MerkleRoot.Val,
	}

	// Copy the MsgTx if present.
	if t.MsgTx.Val.Tx != nil {
		p.MsgTx = *t.MsgTx.Val.Tx
	}

	// Copy the InternalKey if present.
	if t.InternalKey.Val != nil {
		p.InternalKey = *t.InternalKey.Val
	}

	return p, nil
}

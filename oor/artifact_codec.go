package oor

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	clientdb "github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	incomingMetadataMatchOutputIndexRecordType    tlv.Type = 1
	incomingMetadataMatchRoundIDRecordType        tlv.Type = 3
	incomingMetadataMatchCommitmentTxIDRecordType tlv.Type = 5
	incomingMetadataMatchBatchExpiryRecordType    tlv.Type = 7
	incomingMetadataMatchChainDepthRecordType     tlv.Type = 11
	incomingMetadataMatchCreatedHeightRecordType  tlv.Type = 13
	incomingMetadataMatchAncestryPathsRecordType  tlv.Type = 17
	incomingMetadataMatchOperatorKeyRecordType    tlv.Type = 19
)

// Per-Ancestry TLV record types used inside the ancestry-paths blob list.
// Tags are scoped to a single ancestry record so they do not conflict with
// the outer IncomingMetadataMatch record-type space.
const (
	ancestryPathTreePathRecordType       tlv.Type = 1
	ancestryPathCommitmentTxIDRecordType tlv.Type = 3
	ancestryPathInputIndicesRecordType   tlv.Type = 5
	ancestryPathTreeDepthRecordType      tlv.Type = 7
)

// encodeAncestryList encodes []vtxo.Ancestry as a length-prefixed blob
// list. Each blob is a TLV stream encoding one Ancestry's fields. The
// outer length prefix lets replay tolerate additive
// changes to the per-ancestry record set without breaking older blobs.
func encodeAncestryList(ancestry []vtxo.Ancestry) ([]byte, error) {
	blobs := make([][]byte, 0, len(ancestry))
	for i := range ancestry {
		raw, err := encodeAncestryEntry(ancestry[i])
		if err != nil {
			return nil, err
		}
		blobs = append(blobs, raw)
	}

	return encodeLengthPrefixedBlobList(blobs)
}

// decodeAncestryListWithLimits decodes ancestry entries using the supplied
// receive limits for the outer blob list.
func decodeAncestryListWithLimits(raw []byte,
	limits ReceiveLimits) ([]vtxo.Ancestry, error) {

	if len(raw) == 0 {
		return nil, nil
	}

	blobs, err := decodeLengthPrefixedBlobListWithLimits(raw, limits)
	if err != nil {
		return nil, err
	}

	ancestry := make([]vtxo.Ancestry, 0, len(blobs))
	for i := range blobs {
		entry, err := decodeAncestryEntry(blobs[i])
		if err != nil {
			return nil, err
		}
		ancestry = append(ancestry, entry)
	}

	return ancestry, nil
}

// encodeAncestryEntry encodes one vtxo.Ancestry into a TLV blob.
func encodeAncestryEntry(a vtxo.Ancestry) ([]byte, error) {
	var treePath []byte
	if a.TreePath != nil {
		var err error
		treePath, err = clientdb.SerializeTree(a.TreePath)
		if err != nil {
			return nil, err
		}
	}

	commitmentTxID := a.CommitmentTxID[:]

	// Serialize input_indices as a length-prefixed list of uint32.
	indices := encodeUint32List(a.InputIndices)
	treeDepth := a.TreeDepth

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			ancestryPathTreePathRecordType, &treePath,
		),
		tlv.MakePrimitiveRecord(
			ancestryPathCommitmentTxIDRecordType, &commitmentTxID,
		),
		tlv.MakePrimitiveRecord(
			ancestryPathInputIndicesRecordType, &indices,
		),
		tlv.MakePrimitiveRecord(
			ancestryPathTreeDepthRecordType, &treeDepth,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// decodeAncestryEntry is the inverse of encodeAncestryEntry.
func decodeAncestryEntry(raw []byte) (vtxo.Ancestry, error) {
	var (
		treePath       []byte
		commitmentTxID []byte
		indicesRaw     []byte
		treeDepth      uint32
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			ancestryPathTreePathRecordType, &treePath,
		),
		tlv.MakePrimitiveRecord(
			ancestryPathCommitmentTxIDRecordType, &commitmentTxID,
		),
		tlv.MakePrimitiveRecord(
			ancestryPathInputIndicesRecordType, &indicesRaw,
		),
		tlv.MakePrimitiveRecord(
			ancestryPathTreeDepthRecordType, &treeDepth,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return vtxo.Ancestry{}, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return vtxo.Ancestry{}, err
	}

	if len(commitmentTxID) != chainhash.HashSize {
		return vtxo.Ancestry{}, fmt.Errorf("ancestry path commitment "+
			"txid must be %d bytes, got %d", chainhash.HashSize,
			len(commitmentTxID))
	}

	var decodedCommitmentTxID chainhash.Hash
	copy(decodedCommitmentTxID[:], commitmentTxID)

	indices, err := decodeUint32List(indicesRaw)
	if err != nil {
		return vtxo.Ancestry{}, err
	}

	var decodedTreePath *tree.Tree
	if len(treePath) > 0 {
		decodedTreePath, err = clientdb.DeserializeTree(treePath)
		if err != nil {
			return vtxo.Ancestry{}, err
		}
	}

	return vtxo.Ancestry{
		TreePath:       decodedTreePath,
		CommitmentTxID: decodedCommitmentTxID,
		InputIndices:   indices,
		TreeDepth:      treeDepth,
	}, nil
}

// encodeUint32List packs a slice of uint32 as len-prefixed big-endian bytes.
// Empty/nil slices encode to a single zero byte (count=0).
func encodeUint32List(values []uint32) []byte {
	buf := make([]byte, 0, 4+len(values)*4)

	count := uint32(len(values))
	buf = append(
		buf, byte(count>>24), byte(count>>16), byte(count>>8),
		byte(count),
	)
	for _, v := range values {
		buf = append(
			buf, byte(v>>24), byte(v>>16), byte(v>>8), byte(v),
		)
	}

	return buf
}

// decodeUint32List inverts encodeUint32List, returning a fresh slice.
func decodeUint32List(raw []byte) ([]uint32, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	if len(raw) < 4 {
		return nil, fmt.Errorf("uint32 list missing length prefix "+
			"(got %d bytes)", len(raw))
	}

	count := uint32(raw[0])<<24 | uint32(raw[1])<<16 |
		uint32(raw[2])<<8 | uint32(raw[3])

	// Compute the implied size in uint64 so a malicious count cannot
	// wrap int(count)*4 on 32-bit platforms and slip past the bounds
	// check. The TLV blob is sourced from persisted state so a
	// crafted or corrupt record could otherwise crash the actor on
	// the make() below.
	implied := 4 + uint64(count)*4
	if uint64(len(raw)) != implied {
		return nil, fmt.Errorf("uint32 list length mismatch: count %d "+
			"implies %d bytes, got %d", count, implied, len(raw))
	}

	out := make([]uint32, count)
	for i := range out {
		off := 4 + i*4
		out[i] = uint32(raw[off])<<24 | uint32(raw[off+1])<<16 |
			uint32(raw[off+2])<<8 | uint32(raw[off+3])
	}

	return out, nil
}

func encodeIncomingMetadataMatch(match IncomingMetadataMatch) ([]byte, error) {
	outputIndex := match.OutputIndex
	roundID := []byte(match.Metadata.RoundID)
	commitmentTxID := match.Metadata.CommitmentTxID[:]
	batchExpiry := uint32(match.Metadata.BatchExpiry)
	chainDepth := uint32(match.Metadata.ChainDepth)
	createdHeight := uint32(match.Metadata.CreatedHeight)
	operatorKey := encodeOptionalPubKey(match.Metadata.OperatorKey)

	ancestryBytes, err := encodeAncestryList(match.Metadata.Ancestry)
	if err != nil {
		return nil, err
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchOutputIndexRecordType,
			&outputIndex,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchRoundIDRecordType, &roundID,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchCommitmentTxIDRecordType,
			&commitmentTxID,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchBatchExpiryRecordType,
			&batchExpiry,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchChainDepthRecordType, &chainDepth,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchCreatedHeightRecordType,
			&createdHeight,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchAncestryPathsRecordType,
			&ancestryBytes,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchOperatorKeyRecordType,
			&operatorKey,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// decodeIncomingMetadataMatchWithLimits decodes one incoming metadata match
// and applies receive limits to its ancestry list.
func decodeIncomingMetadataMatchWithLimits(raw []byte,
	limits ReceiveLimits) (IncomingMetadataMatch, error) {

	var (
		outputIndex    uint32
		roundID        []byte
		commitmentTxID []byte
		batchExpiry    uint32
		chainDepth     uint32
		createdHeight  uint32
		ancestryBytes  []byte
		operatorKey    []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchOutputIndexRecordType,
			&outputIndex,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchRoundIDRecordType, &roundID,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchCommitmentTxIDRecordType,
			&commitmentTxID,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchBatchExpiryRecordType,
			&batchExpiry,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchChainDepthRecordType, &chainDepth,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchCreatedHeightRecordType,
			&createdHeight,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchAncestryPathsRecordType,
			&ancestryBytes,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchOperatorKeyRecordType,
			&operatorKey,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return IncomingMetadataMatch{}, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return IncomingMetadataMatch{}, err
	}

	if len(commitmentTxID) != chainhash.HashSize {
		return IncomingMetadataMatch{}, fmt.Errorf("incoming " +
			"metadata commitment txid must be provided")
	}

	decodedBatchExpiry, err := uint32ToInt32(
		batchExpiry, "incoming batch expiry",
	)
	if err != nil {
		return IncomingMetadataMatch{}, err
	}

	decodedChainDepth, err := uint32ToInt32(
		chainDepth, "incoming chain depth",
	)
	if err != nil {
		return IncomingMetadataMatch{}, err
	}

	decodedCreatedHeight, err := uint32ToInt32(
		createdHeight, "incoming created height",
	)
	if err != nil {
		return IncomingMetadataMatch{}, err
	}

	ancestry, err := decodeAncestryListWithLimits(ancestryBytes, limits)
	if err != nil {
		return IncomingMetadataMatch{}, err
	}

	decodedOperatorKey, err := decodeOptionalPubKey(
		operatorKey, "incoming operator key",
	)
	if err != nil {
		return IncomingMetadataMatch{}, err
	}

	var decodedCommitmentTxID chainhash.Hash
	copy(decodedCommitmentTxID[:], commitmentTxID)

	return IncomingMetadataMatch{
		OutputIndex: outputIndex,
		Metadata: IncomingVTXOMetadata{
			RoundID:        string(roundID),
			CommitmentTxID: decodedCommitmentTxID,
			BatchExpiry:    decodedBatchExpiry,
			ChainDepth:     int(decodedChainDepth),
			CreatedHeight:  decodedCreatedHeight,
			OperatorKey:    decodedOperatorKey,
			Ancestry:       ancestry,
		},
	}, nil
}

// encodeOptionalPubKey returns the compressed encoding of pubKey, or nil when
// no key is present.
func encodeOptionalPubKey(pubKey *btcec.PublicKey) []byte {
	if pubKey == nil {
		return nil
	}

	return pubKey.SerializeCompressed()
}

// decodeOptionalPubKey parses a compressed public key when raw is non-empty.
func decodeOptionalPubKey(raw []byte, name string) (*btcec.PublicKey, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	pubKey, err := btcec.ParsePubKey(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}

	return pubKey, nil
}

func validateSubmitAcceptedIdentity(sessionID SessionID,
	event *SubmitAcceptedEvent) error {

	if event == nil {
		return fmt.Errorf("submit accepted event must be provided")
	}

	if event.SessionID != sessionID {
		return fmt.Errorf("submit accepted event session id mismatch")
	}

	if event.ArkPSBT == nil || event.ArkPSBT.UnsignedTx == nil {
		return fmt.Errorf("submit accepted event ark psbt must be " +
			"provided")
	}

	arkSessionID, err := sessionIDFromArk(event.ArkPSBT)
	if err != nil {
		return err
	}

	if arkSessionID != sessionID {
		return fmt.Errorf("submit accepted event ark txid mismatch")
	}

	return nil
}

func outPointBytes(out wire.OutPoint) []byte {
	raw := make([]byte, chainhash.HashSize+4)
	copy(raw[:chainhash.HashSize], out.Hash[:])
	binary.LittleEndian.PutUint32(raw[chainhash.HashSize:], out.Index)

	return raw
}

func encodeLengthPrefixedBlobList(blobs [][]byte) ([]byte, error) {
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
		element := blobs[i]

		if err := tlv.WriteVarInt(
			&buf,
			uint64(
				len(element),
			),
			&scratch,
		); err != nil {
			return nil, err
		}

		if _, err := buf.Write(element); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// decodeLengthPrefixedBlobListWithLimits decodes a blob list after enforcing
// the configured item-count cap before allocating the output slice.
func decodeLengthPrefixedBlobListWithLimits(raw []byte,
	limits ReceiveLimits) ([][]byte, error) {

	var scratch [8]byte

	reader := bytes.NewReader(raw)
	count, err := tlv.ReadVarInt(reader, &scratch)
	if err != nil {
		return nil, err
	}

	limits = normalizeReceiveLimits(limits)
	if count > uint64(limits.MaxMailboxItems) {
		return nil, fmt.Errorf("max mailbox items exceeded: blob list "+
			"count %d exceeds limit %d", count,
			limits.MaxMailboxItems)
	}

	blobs := make([][]byte, 0, count)
	for i := uint64(0); i < count; i++ {
		elementLen, err := tlv.ReadVarInt(reader, &scratch)
		if err != nil {
			return nil, err
		}

		element := make([]byte, elementLen)
		if _, err := io.ReadFull(reader, element); err != nil {
			return nil, err
		}

		blobs = append(blobs, element)
	}

	if reader.Len() != 0 {
		return nil, fmt.Errorf("trailing payload bytes")
	}

	return blobs, nil
}

func sessionIDBytes(sessionID SessionID) []byte {
	h := [32]byte(sessionID)
	raw := make([]byte, len(h))
	copy(raw, h[:])

	return raw
}

func uint32ToInt32(value uint32, field string) (int32, error) {
	if value > math.MaxInt32 {
		return 0, fmt.Errorf("%s overflows int32: %d", field, value)
	}

	return int32(value), nil
}

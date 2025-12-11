package db

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightningnetwork/lnd/tlv"
)

// TLV type aliases for TxProof serialization.
type (
	tlvTxProofMsgTx         = tlv.TlvType0
	tlvTxProofBlockHeader   = tlv.TlvType1
	tlvTxProofBlockHeight   = tlv.TlvType2
	tlvTxProofMerkleProof   = tlv.TlvType3
	tlvTxProofClaimedOutput = tlv.TlvType4
	tlvTxProofInternalKey   = tlv.TlvType5
	tlvTxProofMerkleRoot    = tlv.TlvType6
)

// txProofMsgTxRecord is a TLV record for wire.MsgTx.
type txProofMsgTxRecord struct {
	Tx *wire.MsgTx
}

// Record returns the TLV record for encoding/decoding.
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

	return tlv.NewTypeForDecodingErr(val, "txProofMsgTxRecord", l, l)
}

// txProofBlockHeaderRecord is a TLV record for wire.BlockHeader.
type txProofBlockHeaderRecord struct {
	Header wire.BlockHeader
}

// Record returns the TLV record for encoding/decoding.
func (t *txProofBlockHeaderRecord) Record() tlv.Record {
	// Block header is always 80 bytes.
	return tlv.MakeStaticRecord(
		0, t, 80, txProofBlockHeaderEncoder, txProofBlockHeaderDecoder,
	)
}

func txProofBlockHeaderEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	if t, ok := val.(*txProofBlockHeaderRecord); ok {
		return t.Header.Serialize(w)
	}

	return tlv.NewTypeForEncodingErr(val, "txProofBlockHeaderRecord")
}

func txProofBlockHeaderDecoder(
	r io.Reader, val interface{}, _ *[8]byte, l uint64) error {

	if l != 80 {
		return fmt.Errorf("invalid block header length: %d", l)
	}

	if t, ok := val.(*txProofBlockHeaderRecord); ok {
		return t.Header.Deserialize(r)
	}

	return tlv.NewTypeForDecodingErr(val, "txProofBlockHeaderRecord", l, 80)
}

// txProofMerkleProofRecord is a TLV record for proof.TxMerkleProof.
// Uses the TxMerkleProof's built-in Encode/Decode methods.
type txProofMerkleProofRecord struct {
	Proof proof.TxMerkleProof
}

// Record returns the TLV record for encoding/decoding.
func (t *txProofMerkleProofRecord) Record() tlv.Record {
	recordSize := func() uint64 {
		// Estimate size: we need to encode to know exact size.
		var buf bytes.Buffer
		_ = t.Proof.Encode(&buf)

		return uint64(buf.Len())
	}

	return tlv.MakeDynamicRecord(
		0, t, recordSize,
		txProofMerkleProofEncoder, txProofMerkleProofDecoder,
	)
}

func txProofMerkleProofEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	if t, ok := val.(*txProofMerkleProofRecord); ok {
		return t.Proof.Encode(w)
	}

	return tlv.NewTypeForEncodingErr(val, "txProofMerkleProofRecord")
}

func txProofMerkleProofDecoder(
	r io.Reader, val interface{}, _ *[8]byte, l uint64) error {

	if t, ok := val.(*txProofMerkleProofRecord); ok {
		return t.Proof.Decode(r)
	}

	return tlv.NewTypeForDecodingErr(val, "txProofMerkleProofRecord", l, l)
}

// tlvTxProof is the TLV-encoded representation of proof.TxProof.
type tlvTxProof struct {
	MsgTx       tlv.RecordT[tlvTxProofMsgTx, txProofMsgTxRecord]
	BlockHeader tlv.RecordT[tlvTxProofBlockHeader, txProofBlockHeaderRecord]
	BlockHeight tlv.RecordT[tlvTxProofBlockHeight, uint32]
	MerkleProof tlv.RecordT[tlvTxProofMerkleProof, txProofMerkleProofRecord]

	ClaimedOutput tlv.RecordT[tlvTxProofClaimedOutput, outpointRecord]
	InternalKey   tlv.RecordT[tlvTxProofInternalKey, *btcec.PublicKey]
	MerkleRoot    tlv.RecordT[tlvTxProofMerkleRoot, []byte]
}

// SerializeTxProof serializes a proof.TxProof to bytes using TLV encoding.
func SerializeTxProof(p *proof.TxProof) ([]byte, error) {
	if p == nil {
		return nil, nil
	}

	t := &tlvTxProof{
		MsgTx: tlv.NewRecordT[tlvTxProofMsgTx](
			txProofMsgTxRecord{Tx: &p.MsgTx},
		),
		BlockHeader: tlv.NewRecordT[tlvTxProofBlockHeader](
			txProofBlockHeaderRecord{Header: p.BlockHeader},
		),
		BlockHeight: tlv.NewPrimitiveRecord[tlvTxProofBlockHeight](
			p.BlockHeight,
		),
		MerkleProof: tlv.NewRecordT[tlvTxProofMerkleProof](
			txProofMerkleProofRecord{Proof: p.MerkleProof},
		),
		ClaimedOutput: tlv.NewRecordT[tlvTxProofClaimedOutput](
			outpointRecord{OutPoint: p.ClaimedOutPoint},
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

	reader := bytes.NewReader(data)
	if err := stream.Decode(reader); err != nil {
		return nil, fmt.Errorf("decode TxProof: %w", err)
	}

	// Reconstruct the TxProof.
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

package db

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightningnetwork/lnd/tlv"
)

// TLV type aliases for Tree serialization.
type (
	tlvTreeBatchOutpoint = tlv.TlvType0
	tlvTreeBatchOutput   = tlv.TlvType1
	tlvTreeSweepRoot     = tlv.TlvType2
	tlvTreeRootNode      = tlv.TlvType3
)

// Tree-decode safety bounds. The wire layer feeds DeserializeTree from
// the durable mailbox, persisted rows, and operator-supplied indexer
// responses, all of which must be treated as untrusted. Without these
// caps, a varint-driven numChildren or a deeply nested chain could
// trigger a make() OOM or stack overflow that crashes the actor on
// every replay.
const (
	// MaxTreeDeserializeDepth bounds the recursion depth allowed when
	// deserializing a tree.Tree blob. Production trees are radix-2
	// (binary) and refresh policy keeps practical depth well under
	// this; 32 is a generous safety margin that still fails fast on
	// adversarial linear-depth payloads.
	MaxTreeDeserializeDepth = 32

	// MaxTreeChildrenPerNode bounds the per-node child count read off
	// the wire before allocating the children map. The configured
	// radix is 2; this cap is a defense-in-depth ceiling that still
	// rejects a malicious uint64-shaped numChildren before it reaches
	// make().
	MaxTreeChildrenPerNode = 64
)

// TLV type aliases for Node serialization.
type (
	tlvNodeInput     = tlv.TlvType0
	tlvNodeOutputs   = tlv.TlvType1
	tlvNodeCoSigners = tlv.TlvType2
	tlvNodeSignature = tlv.TlvType3
	tlvNodeFinalKey  = tlv.TlvType4
	tlvNodeChildren  = tlv.TlvType5
)

// outpointRecord is a TLV record for wire.OutPoint. It implements
// RecordProducer for use with tlv.RecordT.
type outpointRecord struct {
	wire.OutPoint
}

// Record returns the TLV record for encoding/decoding.
func (o *outpointRecord) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		0, o, 36, outpointEncoder, outpointDecoder,
	)
}

func outpointEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	if o, ok := val.(*outpointRecord); ok {
		if _, err := w.Write(o.Hash[:]); err != nil {
			return err
		}

		var buf [4]byte
		binary.LittleEndian.PutUint32(buf[:], o.Index)

		_, err := w.Write(buf[:])

		return err
	}

	return tlv.NewTypeForEncodingErr(val, "outpointRecord")
}

func outpointDecoder(r io.Reader, val interface{}, _ *[8]byte, l uint64) error {
	if l != 36 {
		return fmt.Errorf("invalid outpoint length: %d", l)
	}

	if o, ok := val.(*outpointRecord); ok {
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

	return tlv.NewTypeForDecodingErr(val, "outpointRecord", l, 36)
}

// txOutRecord is a TLV record for wire.TxOut. It implements RecordProducer
// for use with tlv.RecordT.
type txOutRecord struct {
	wire.TxOut
}

// Record returns the TLV record for encoding/decoding.
func (t *txOutRecord) Record() tlv.Record {
	recordSize := func() uint64 {

		// 8 bytes value + varint length + pkscript bytes.
		return 8 + tlv.VarIntSize(uint64(len(t.PkScript))) +
			uint64(len(t.PkScript))
	}

	return tlv.MakeDynamicRecord(
		0, t, recordSize, txOutEncoder, txOutDecoder,
	)
}

func txOutEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if t, ok := val.(*txOutRecord); ok {
		binary.LittleEndian.PutUint64(buf[:], uint64(t.Value))
		if _, err := w.Write(buf[:]); err != nil {
			return err
		}

		if err := tlv.WriteVarInt(
			w, uint64(len(t.PkScript)), buf,
		); err != nil {
			return err
		}

		_, err := w.Write(t.PkScript)

		return err
	}

	return tlv.NewTypeForEncodingErr(val, "txOutRecord")
}

func txOutDecoder(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if t, ok := val.(*txOutRecord); ok {
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return err
		}

		t.Value = int64(binary.LittleEndian.Uint64(buf[:]))

		scriptLen, err := tlv.ReadVarInt(r, buf)
		if err != nil {
			return err
		}

		t.PkScript = make([]byte, scriptLen)
		_, err = io.ReadFull(r, t.PkScript)

		return err
	}

	return tlv.NewTypeForDecodingErr(val, "txOutRecord", l, l)
}

// txOutsRecord is a TLV record for a slice of wire.TxOut.
// It implements RecordProducer for use with tlv.RecordT.
type txOutsRecord struct {
	Outputs []*wire.TxOut
}

// Record returns the TLV record for encoding/decoding.
func (t *txOutsRecord) Record() tlv.Record {
	recordSize := func() uint64 {
		size := tlv.VarIntSize(uint64(len(t.Outputs)))
		for _, out := range t.Outputs {
			size += 8 + tlv.VarIntSize(uint64(len(out.PkScript))) +
				uint64(len(out.PkScript))
		}

		return size
	}

	return tlv.MakeDynamicRecord(
		0, t, recordSize, txOutsEncoder, txOutsDecoder,
	)
}

func txOutsEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if t, ok := val.(*txOutsRecord); ok {
		if err := tlv.WriteVarInt(
			w, uint64(len(t.Outputs)), buf,
		); err != nil {
			return err
		}

		for _, out := range t.Outputs {
			binary.LittleEndian.PutUint64(buf[:], uint64(out.Value))
			if _, err := w.Write(buf[:]); err != nil {
				return err
			}

			if err := tlv.WriteVarInt(
				w,
				uint64(
					len(out.PkScript),
				),
				buf,
			); err != nil {
				return err
			}

			if _, err := w.Write(out.PkScript); err != nil {
				return err
			}
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "txOutsRecord")
}

func txOutsDecoder(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if t, ok := val.(*txOutsRecord); ok {
		numOutputs, err := tlv.ReadVarInt(r, buf)
		if err != nil {
			return err
		}

		t.Outputs = make([]*wire.TxOut, numOutputs)
		for i := uint64(0); i < numOutputs; i++ {
			if _, err := io.ReadFull(r, buf[:]); err != nil {
				return err
			}

			value := int64(binary.LittleEndian.Uint64(buf[:]))

			scriptLen, err := tlv.ReadVarInt(r, buf)
			if err != nil {
				return err
			}

			pkScript := make([]byte, scriptLen)
			if _, err := io.ReadFull(r, pkScript); err != nil {
				return err
			}

			t.Outputs[i] = &wire.TxOut{
				Value:    value,
				PkScript: pkScript,
			}
		}

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "txOutsRecord", l, l)
}

// pubKeysRecord is a TLV record for a slice of btcec.PublicKey.
// It implements RecordProducer for use with tlv.RecordT.
type pubKeysRecord struct {
	Keys []*btcec.PublicKey
}

// Record returns the TLV record for encoding/decoding.
func (p *pubKeysRecord) Record() tlv.Record {
	recordSize := func() uint64 {
		return tlv.VarIntSize(uint64(len(p.Keys))) +
			uint64(len(p.Keys)*33)
	}

	return tlv.MakeDynamicRecord(
		0, p, recordSize, pubKeysEncoder, pubKeysDecoder,
	)
}

func pubKeysEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if p, ok := val.(*pubKeysRecord); ok {
		if err := tlv.WriteVarInt(
			w, uint64(len(p.Keys)), buf,
		); err != nil {
			return err
		}

		for _, key := range p.Keys {
			if _, err := w.Write(
				key.SerializeCompressed(),
			); err != nil {
				return err
			}
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "pubKeysRecord")
}

func pubKeysDecoder(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	if p, ok := val.(*pubKeysRecord); ok {
		numKeys, err := tlv.ReadVarInt(r, buf)
		if err != nil {
			return err
		}

		p.Keys = make([]*btcec.PublicKey, numKeys)
		for i := uint64(0); i < numKeys; i++ {
			var keyBuf [33]byte
			if _, err := io.ReadFull(r, keyBuf[:]); err != nil {
				return err
			}

			key, err := btcec.ParsePubKey(keyBuf[:])
			if err != nil {
				return err
			}

			p.Keys[i] = key
		}

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "pubKeysRecord", l, l)
}

// sigRecord is a TLV record for a schnorr.Signature.
// It implements RecordProducer for use with tlv.RecordT.
type sigRecord struct {
	Sig *schnorr.Signature
}

// Record returns the TLV record for encoding/decoding.
func (s *sigRecord) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		0, s, 64, sigEncoder, sigDecoder,
	)
}

func sigEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	if s, ok := val.(*sigRecord); ok {
		if s.Sig == nil {
			// Write zeros for nil signature.
			var zeroBuf [64]byte
			_, err := w.Write(zeroBuf[:])

			return err
		}

		_, err := w.Write(s.Sig.Serialize())

		return err
	}

	return tlv.NewTypeForEncodingErr(val, "sigRecord")
}

func sigDecoder(r io.Reader, val interface{}, _ *[8]byte, l uint64) error {
	if l != 64 {
		return fmt.Errorf("invalid signature length: %d", l)
	}

	if s, ok := val.(*sigRecord); ok {
		var buf [64]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return err
		}

		// Check for zero signature (means nil).
		var zeroBuf [64]byte
		if bytes.Equal(buf[:], zeroBuf[:]) {
			s.Sig = nil

			return nil
		}

		sig, err := schnorr.ParseSignature(buf[:])
		if err != nil {
			return err
		}

		s.Sig = sig

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "sigRecord", l, 64)
}

// pubKeyRecord is a TLV record for a single btcec.PublicKey.
// It implements RecordProducer for use with tlv.RecordT.
type pubKeyRecord struct {
	Key *btcec.PublicKey
}

// Record returns the TLV record for encoding/decoding.
func (p *pubKeyRecord) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		0, p, 33, pubKeyEncoder, pubKeyDecoder,
	)
}

func pubKeyEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	if p, ok := val.(*pubKeyRecord); ok {
		if p.Key == nil {
			// Write zeros for nil key.
			var zeroBuf [33]byte
			_, err := w.Write(zeroBuf[:])

			return err
		}

		_, err := w.Write(p.Key.SerializeCompressed())

		return err
	}

	return tlv.NewTypeForEncodingErr(val, "pubKeyRecord")
}

func pubKeyDecoder(r io.Reader, val interface{}, _ *[8]byte, l uint64) error {
	if l != 33 {
		return fmt.Errorf("invalid pubkey length: %d", l)
	}

	if p, ok := val.(*pubKeyRecord); ok {
		var buf [33]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return err
		}

		// Check for zero pubkey (means nil).
		var zeroBuf [33]byte
		if bytes.Equal(buf[:], zeroBuf[:]) {
			p.Key = nil

			return nil
		}

		key, err := btcec.ParsePubKey(buf[:])
		if err != nil {
			return err
		}

		p.Key = key

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "pubKeyRecord", l, 33)
}

// childrenDataRecord is a TLV record for serialized children map data.
// It implements RecordProducer for use with tlv.RecordT.
type childrenDataRecord struct {
	Data []byte
}

// Record returns the TLV record for encoding/decoding.
func (c *childrenDataRecord) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, &c.Data, tlv.SizeVarBytes(&c.Data), tlv.EVarBytes,
		tlv.DVarBytes,
	)
}

// tlvTree is the TLV-serializable wrapper for tree.Tree using RecordT.
type tlvTree struct {
	// BatchOutpoint is the outpoint in the commitment transaction that the
	// root transaction spends.
	BatchOutpoint tlv.RecordT[tlvTreeBatchOutpoint, outpointRecord]

	// BatchOutput is the actual output at BatchOutpoint.
	BatchOutput tlv.RecordT[tlvTreeBatchOutput, txOutRecord]

	// SweepRoot is the tapscript root hash used for tweaking branch
	// outputs. Uses tlv.Blob (primitive []byte) directly.
	SweepRoot tlv.RecordT[tlvTreeSweepRoot, tlv.Blob]

	// RootNodeData is the serialized root node. Uses tlv.Blob (primitive
	// []byte) directly.
	RootNodeData tlv.RecordT[tlvTreeRootNode, tlv.Blob]
}

// newTlvTree creates a new tlvTree with initialized RecordT fields.
func newTlvTree() *tlvTree {
	return &tlvTree{
		BatchOutpoint: tlv.NewRecordT[tlvTreeBatchOutpoint](
			outpointRecord{},
		),
		BatchOutput: tlv.NewRecordT[tlvTreeBatchOutput](txOutRecord{}),
		SweepRoot: tlv.NewPrimitiveRecord[tlvTreeSweepRoot, tlv.Blob](
			nil,
		),
		RootNodeData: tlv.NewPrimitiveRecord[tlvTreeRootNode, tlv.Blob](
			nil,
		),
	}
}

// EncodeRecords returns the TLV records for encoding.
func (t *tlvTree) EncodeRecords() []tlv.Record {
	return []tlv.Record{
		t.BatchOutpoint.Record(),
		t.BatchOutput.Record(),
		t.SweepRoot.Record(),
		t.RootNodeData.Record(),
	}
}

// DecodeRecords returns the TLV records for decoding.
func (t *tlvTree) DecodeRecords() []tlv.Record {
	return t.EncodeRecords()
}

// Encode serializes the tlvTree to a writer.
func (t *tlvTree) Encode(w io.Writer) error {
	stream, err := tlv.NewStream(t.EncodeRecords()...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes the tlvTree from a reader.
func (t *tlvTree) Decode(r io.Reader) error {
	stream, err := tlv.NewStream(t.DecodeRecords()...)
	if err != nil {
		return err
	}

	return stream.Decode(r)
}

// tlvNode is the TLV-serializable wrapper for tree.Node using RecordT.
type tlvNode struct {
	// Input is the single outpoint this transaction spends.
	Input tlv.RecordT[tlvNodeInput, outpointRecord]

	// Outputs are all the outputs created by this transaction.
	Outputs tlv.RecordT[tlvNodeOutputs, txOutsRecord]

	// CoSigners is the set of public keys for MuSig2 signing.
	CoSigners tlv.RecordT[tlvNodeCoSigners, pubKeysRecord]

	// Signature is the final aggregated MuSig2 signature (optional).
	Signature tlv.RecordT[tlvNodeSignature, sigRecord]

	// FinalKey is the final aggregated public key (optional).
	FinalKey tlv.RecordT[tlvNodeFinalKey, pubKeyRecord]

	// Children is the serialized children data.
	Children tlv.RecordT[tlvNodeChildren, childrenDataRecord]
}

// newTlvNode creates a new tlvNode with initialized RecordT fields.
func newTlvNode() *tlvNode {
	return &tlvNode{
		Input: tlv.NewRecordT[tlvNodeInput](outpointRecord{}),
		Outputs: tlv.NewRecordT[tlvNodeOutputs](txOutsRecord{
			Outputs: make([]*wire.TxOut, 0),
		}),
		CoSigners: tlv.NewRecordT[tlvNodeCoSigners](pubKeysRecord{
			Keys: make([]*btcec.PublicKey, 0),
		}),
		Signature: tlv.NewRecordT[tlvNodeSignature](sigRecord{}),
		FinalKey:  tlv.NewRecordT[tlvNodeFinalKey](pubKeyRecord{}),
		Children: tlv.NewRecordT[tlvNodeChildren](
			childrenDataRecord{},
		),
	}
}

// EncodeRecords returns the TLV records for encoding.
func (n *tlvNode) EncodeRecords() []tlv.Record {
	records := []tlv.Record{
		n.Input.Record(),
		n.Outputs.Record(),
		n.CoSigners.Record(),
	}

	if n.Signature.Val.Sig != nil {
		records = append(records, n.Signature.Record())
	}

	if n.FinalKey.Val.Key != nil {
		records = append(records, n.FinalKey.Record())
	}

	records = append(records, n.Children.Record())

	return records
}

// DecodeRecords returns the TLV records for decoding.
func (n *tlvNode) DecodeRecords() []tlv.Record {

	// For decoding, we include all possible records.
	return []tlv.Record{
		n.Input.Record(),
		n.Outputs.Record(),
		n.CoSigners.Record(),
		n.Signature.Record(),
		n.FinalKey.Record(),
		n.Children.Record(),
	}
}

// Encode serializes the tlvNode to a writer.
func (n *tlvNode) Encode(w io.Writer) error {
	stream, err := tlv.NewStream(n.EncodeRecords()...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes the tlvNode from a reader.
func (n *tlvNode) Decode(r io.Reader) error {
	stream, err := tlv.NewStream(n.DecodeRecords()...)
	if err != nil {
		return err
	}

	_, err = stream.DecodeWithParsedTypes(r)
	if err != nil {
		return err
	}

	return nil
}

// SerializeTree serializes a tree.Tree to bytes using TLV encoding.
func SerializeTree(t *tree.Tree) ([]byte, error) {
	if t == nil {
		return nil, fmt.Errorf("cannot serialize nil tree")
	}

	tlvT := newTlvTree()

	// Set BatchOutpoint.
	tlvT.BatchOutpoint.Val.OutPoint = t.BatchOutpoint

	// Set BatchOutput.
	if t.BatchOutput != nil {
		tlvT.BatchOutput.Val.TxOut = *t.BatchOutput
	}

	// Set SweepRoot.
	tlvT.SweepRoot.Val = t.SweepTapscriptRoot

	// Serialize RootNode recursively.
	if t.Root != nil {
		rootData, err := serializeNode(t.Root)
		if err != nil {
			return nil, fmt.Errorf("serialize root node: %w", err)
		}

		tlvT.RootNodeData.Val = rootData
	}

	var buf bytes.Buffer
	if err := tlvT.Encode(&buf); err != nil {
		return nil, fmt.Errorf("encode tree: %w", err)
	}

	return buf.Bytes(), nil
}

// DeserializeTree deserializes a tree.Tree from bytes using TLV encoding.
func DeserializeTree(data []byte) (*tree.Tree, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("cannot deserialize empty data")
	}

	tlvT := newTlvTree()
	if err := tlvT.Decode(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("decode tree: %w", err)
	}

	t := &tree.Tree{
		BatchOutpoint:      tlvT.BatchOutpoint.Val.OutPoint,
		SweepTapscriptRoot: tlvT.SweepRoot.Val,
	}

	// Set BatchOutput if it has a non-zero value or non-empty script.
	if tlvT.BatchOutput.Val.Value != 0 ||
		len(tlvT.BatchOutput.Val.PkScript) > 0 {

		t.BatchOutput = &wire.TxOut{
			Value:    tlvT.BatchOutput.Val.Value,
			PkScript: tlvT.BatchOutput.Val.PkScript,
		}
	}

	// Deserialize root node. Depth starts at 1 so a root with no
	// children still counts as depth 1, matching the convention used
	// by tree.Node.Depth.
	if len(tlvT.RootNodeData.Val) > 0 {
		rootNode, err := deserializeNode(tlvT.RootNodeData.Val, 1)
		if err != nil {
			return nil, fmt.Errorf("deserialize root node: %w", err)
		}

		t.Root = rootNode
	}

	return t, nil
}

// serializeNode serializes a tree.Node to bytes using TLV encoding.
func serializeNode(n *tree.Node) ([]byte, error) {
	if n == nil {
		return nil, fmt.Errorf("cannot serialize nil node")
	}

	tlvN := newTlvNode()

	// Set Input.
	tlvN.Input.Val.OutPoint = n.Input

	// Set Outputs.
	tlvN.Outputs.Val.Outputs = n.Outputs

	// Set CoSigners.
	tlvN.CoSigners.Val.Keys = n.CoSigners

	// Set optional Signature if present.
	if n.Signature != nil {
		tlvN.Signature.Val.Sig = n.Signature
	}

	// Set optional FinalKey if present.
	if n.FinalKey != nil {
		tlvN.FinalKey.Val.Key = n.FinalKey
	}

	// Serialize children recursively.
	childrenData, err := serializeChildren(n.Children)
	if err != nil {
		return nil, fmt.Errorf("serialize children: %w", err)
	}

	tlvN.Children.Val.Data = childrenData

	var buf bytes.Buffer
	if err := tlvN.Encode(&buf); err != nil {
		return nil, fmt.Errorf("encode node: %w", err)
	}

	return buf.Bytes(), nil
}

// deserializeNode deserializes a tree.Node from bytes using TLV
// encoding. depth is the current recursion depth (1 for the root) and
// is enforced against MaxTreeDeserializeDepth to prevent untrusted
// blobs from triggering goroutine stack overflow.
func deserializeNode(data []byte, depth int) (*tree.Node, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("cannot deserialize empty node data")
	}

	if depth > MaxTreeDeserializeDepth {
		return nil, fmt.Errorf("tree depth exceeds max %d",
			MaxTreeDeserializeDepth)
	}

	tlvN := newTlvNode()
	if err := tlvN.Decode(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("decode node: %w", err)
	}

	n := &tree.Node{
		Input:     tlvN.Input.Val.OutPoint,
		Outputs:   tlvN.Outputs.Val.Outputs,
		CoSigners: tlvN.CoSigners.Val.Keys,
		Children:  make(map[uint32]*tree.Node),
	}

	// Set optional Signature if present in the decoded TLV.
	if tlvN.Signature.Val.Sig != nil {
		n.Signature = tlvN.Signature.Val.Sig
	}

	// Set optional FinalKey if present in the decoded TLV.
	if tlvN.FinalKey.Val.Key != nil {
		n.FinalKey = tlvN.FinalKey.Val.Key
	}

	// Deserialize children with depth+1 so a deep chain is rejected
	// well before the goroutine stack grows.
	children, err := deserializeChildren(tlvN.Children.Val.Data, depth+1)
	if err != nil {
		return nil, fmt.Errorf("deserialize children: %w", err)
	}

	n.Children = children

	return n, nil
}

// serializeChildren serializes a map of children nodes.
func serializeChildren(children map[uint32]*tree.Node) ([]byte, error) {
	var buf bytes.Buffer
	var scratch [8]byte

	// Write number of children.
	if err := tlv.WriteVarInt(
		&buf, uint64(len(children)), &scratch,
	); err != nil {
		return nil, err
	}

	// Sort indices for deterministic encoding.
	indices := make([]uint32, 0, len(children))
	for idx := range children {
		indices = append(indices, idx)
	}
	sort.Slice(indices, func(i, j int) bool {
		return indices[i] < indices[j]
	})

	// Write each child.
	for _, idx := range indices {
		child := children[idx]

		// Write the output index.
		binary.LittleEndian.PutUint32(scratch[:4], idx)
		if _, err := buf.Write(scratch[:4]); err != nil {
			return nil, err
		}

		// Serialize the child node.
		childData, err := serializeNode(child)
		if err != nil {
			return nil, fmt.Errorf("serialize child at index "+
				"%d: %w", idx, err)
		}

		// Write length-prefixed node data.
		if err := tlv.WriteVarInt(
			&buf,
			uint64(
				len(childData),
			),
			&scratch,
		); err != nil {
			return nil, err
		}

		if _, err := buf.Write(childData); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// deserializeChildren deserializes a map of children nodes. depth is
// the recursion depth at which these children sit (i.e. the parent's
// depth+1) and is forwarded to deserializeNode so a deep linear chain
// is rejected before the goroutine stack grows.
func deserializeChildren(data []byte,
	depth int) (map[uint32]*tree.Node, error) {

	if len(data) == 0 {
		return make(map[uint32]*tree.Node), nil
	}

	r := bytes.NewReader(data)
	var scratch [8]byte

	numChildren, err := tlv.ReadVarInt(r, &scratch)
	if err != nil {
		return nil, err
	}

	// Reject pathological numChildren values before the make() call.
	// A varint can carry up to uint64; without this cap a malicious
	// blob could trip "makemap: size out of range" or trigger an
	// OOM-killed process at replay time.
	if numChildren > MaxTreeChildrenPerNode {
		return nil, fmt.Errorf("num children %d exceeds max %d",
			numChildren, MaxTreeChildrenPerNode)
	}

	// nodeData length is also varint-driven below; bound it against
	// the bytes actually available in the reader so make([]byte,
	// nodeLen) cannot OOM on a crafted nodeLen.
	maxNodeLen := uint64(r.Len())

	children := make(map[uint32]*tree.Node, numChildren)

	for i := uint64(0); i < numChildren; i++ {
		// Read the output index.
		if _, err := io.ReadFull(r, scratch[:4]); err != nil {
			return nil, err
		}

		idx := binary.LittleEndian.Uint32(scratch[:4])

		// Read length-prefixed node data.
		nodeLen, err := tlv.ReadVarInt(r, &scratch)
		if err != nil {
			return nil, err
		}

		if nodeLen > maxNodeLen {
			return nil, fmt.Errorf("child node length %d exceeds "+
				"available bytes %d", nodeLen, maxNodeLen)
		}

		nodeData := make([]byte, nodeLen)
		if _, err := io.ReadFull(r, nodeData); err != nil {
			return nil, err
		}

		// Deserialize the child node.
		child, err := deserializeNode(nodeData, depth)
		if err != nil {
			return nil, fmt.Errorf("deserialize child at index "+
				"%d: %w", idx, err)
		}

		children[idx] = child
	}

	return children, nil
}

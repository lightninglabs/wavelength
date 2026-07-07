package recovery

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// ProofCodecVersion is the on-disk version byte for the Proof codec. Bumping
// this value lets us migrate the serialized form without silently
// re-interpreting older blobs.
const ProofCodecVersion uint8 = 1

const (
	// proofVersionRecordType carries the codec version byte.
	proofVersionRecordType tlv.Type = 1

	// proofTargetOutpointRecordType carries the 36-byte target outpoint
	// (32-byte txid little-endian || 4-byte index big-endian).
	proofTargetOutpointRecordType tlv.Type = 3

	// proofCSVDelayRecordType carries the csv delay in raw blocks.
	proofCSVDelayRecordType tlv.Type = 5

	// proofNodesRecordType carries the length-prefixed list of nested
	// per-Node TLV sub-streams.
	proofNodesRecordType tlv.Type = 7
)

const (
	// nodeKindRecordType carries the 1-byte NodeKind.
	nodeKindRecordType tlv.Type = 1

	// nodeTxRecordType carries the serialized wire.MsgTx bytes. The
	// serialization length is variable, so the record is dynamic.
	nodeTxRecordType tlv.Type = 3
)

// Record returns a TLV record that encodes this Node as a nested sub-stream
// containing a NodeKind record and a wire.MsgTx record. Putting each Node in
// its own TLV stream (rather than a hand-packed binary frame) means we can
// add new per-Node fields later (signatures, metadata, version tags) by
// appending new odd-typed TLV records; older decoders will skip unknown
// records per the TLV spec's odd-is-optional rule.
func (n *Node) Record() tlv.Record {
	sizeFn := func() uint64 {
		// The record is dynamic because the inner MsgTx varies in
		// length. We precompute the exact nested-stream size so the
		// outer TLV emits the correct length prefix.
		raw, err := encodeNodeStream(n)
		if err != nil {
			return 0
		}

		return uint64(len(raw))
	}

	return tlv.MakeDynamicRecord(
		0, n, sizeFn, nodeEncoder, nodeDecoder,
	)
}

// nodeEncoder writes a Node as a nested TLV sub-stream.
func nodeEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	node, ok := val.(*Node)
	if !ok {
		return tlv.NewTypeForEncodingErr(val, "*recovery.Node")
	}

	raw, err := encodeNodeStream(node)
	if err != nil {
		return err
	}

	_, err = w.Write(raw)

	return err
}

// nodeDecoder reads a Node from a nested TLV sub-stream.
func nodeDecoder(r io.Reader, val interface{}, _ *[8]byte, l uint64) error {
	node, ok := val.(*Node)
	if !ok {
		return tlv.NewTypeForDecodingErr(
			val, "*recovery.Node", l, l,
		)
	}

	buf := make([]byte, l)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}

	decoded, err := decodeNodeStream(buf)
	if err != nil {
		return err
	}

	*node = *decoded

	return nil
}

// encodeNodeStream emits a Node as a standalone TLV stream: NodeKind followed
// by the serialized MsgTx. The caller is responsible for placing the
// resulting bytes inside an outer record (either the outer proof stream via
// nodeEncoder, or a length-prefixed list for the proof nodes record).
func encodeNodeStream(n *Node) ([]byte, error) {
	if n == nil || n.Tx == nil {
		return nil, fmt.Errorf("node missing tx")
	}

	kind := uint8(n.Kind)

	// The MsgTx serializer writes directly to an io.Writer. We capture
	// the bytes here so we can pass them as a primitive TLV payload
	// instead of wrapping the serializer in a dynamic Record.
	var txBuf bytes.Buffer
	if err := n.Tx.Serialize(&txBuf); err != nil {
		return nil, fmt.Errorf("serialize tx: %w", err)
	}
	txBytes := txBuf.Bytes()

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(nodeKindRecordType, &kind),
		tlv.MakePrimitiveRecord(nodeTxRecordType, &txBytes),
	)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	if err := stream.Encode(&out); err != nil {
		return nil, err
	}

	return out.Bytes(), nil
}

// decodeNodeStream reverses encodeNodeStream. It re-validates NodeKind to
// reject unknown kinds, preserving the invariant that a decoded Node is
// always a well-formed value.
func decodeNodeStream(raw []byte) (*Node, error) {
	var (
		kind    uint8
		txBytes []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(nodeKindRecordType, &kind),
		tlv.MakePrimitiveRecord(nodeTxRecordType, &txBytes),
	)
	if err != nil {
		return nil, err
	}

	parsed, err := stream.DecodeWithParsedTypes(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode node: %w", err)
	}

	if _, ok := parsed[nodeKindRecordType]; !ok {
		return nil, fmt.Errorf("node missing kind record")
	}
	if _, ok := parsed[nodeTxRecordType]; !ok {
		return nil, fmt.Errorf("node missing tx record")
	}

	nodeKind := NodeKind(kind)
	if nodeKind < NodeKindTree || nodeKind > NodeKindArk {
		return nil, fmt.Errorf("invalid node kind %d", kind)
	}

	tx := &wire.MsgTx{}
	if err := tx.Deserialize(bytes.NewReader(txBytes)); err != nil {
		return nil, fmt.Errorf("deserialize tx: %w", err)
	}

	return &Node{Kind: nodeKind, Tx: tx}, nil
}

// EncodeProof serializes a Proof into a deterministic TLV byte slice. The
// node list is emitted in ascending txid byte order to make the encoding
// reproducible and easy to diff across runs.
func EncodeProof(proof *Proof) ([]byte, error) {
	if proof == nil {
		return nil, fmt.Errorf("proof cannot be nil")
	}

	version := ProofCodecVersion
	outpoint := encodeOutpoint(proof.targetOutpoint)
	csvDelay := proof.csvDelay
	nodesRaw, err := encodeProofNodes(proof.nodes)
	if err != nil {
		return nil, fmt.Errorf("encode proof nodes: %w", err)
	}

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(proofVersionRecordType, &version),
		tlv.MakePrimitiveRecord(
			proofTargetOutpointRecordType, &outpoint,
		),
		tlv.MakePrimitiveRecord(proofCSVDelayRecordType, &csvDelay),
		tlv.MakePrimitiveRecord(proofNodesRecordType, &nodesRaw),
	)
	if err != nil {
		return nil, fmt.Errorf("create proof stream: %w", err)
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, fmt.Errorf("encode proof: %w", err)
	}

	return buf.Bytes(), nil
}

// DecodeProof reverses EncodeProof and runs the bytes back through NewProof
// so the validation invariants (cycle check, reachability, MaxCSVDelay,
// MaxProofNodes) all hold on the decoded result.
func DecodeProof(raw []byte) (*Proof, error) {
	var (
		version     uint8
		outpointRaw []byte
		csvDelay    uint32
		nodesRaw    []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(proofVersionRecordType, &version),
		tlv.MakePrimitiveRecord(
			proofTargetOutpointRecordType, &outpointRaw,
		),
		tlv.MakePrimitiveRecord(proofCSVDelayRecordType, &csvDelay),
		tlv.MakePrimitiveRecord(proofNodesRecordType, &nodesRaw),
	)
	if err != nil {
		return nil, fmt.Errorf("create proof stream: %w", err)
	}

	_, err = stream.DecodeWithParsedTypes(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode proof: %w", err)
	}

	if version != ProofCodecVersion {
		return nil, fmt.Errorf("unsupported proof codec version %d "+
			"(expected %d)", version, ProofCodecVersion)
	}

	outpoint, err := decodeOutpoint(outpointRaw)
	if err != nil {
		return nil, fmt.Errorf("decode target outpoint: %w", err)
	}

	nodes, err := decodeProofNodes(nodesRaw)
	if err != nil {
		return nil, fmt.Errorf("decode proof nodes: %w", err)
	}

	// Rebuild through NewProof so every structural invariant (cycle-
	// freedom, reachability, caps) is re-enforced on the decoded bytes.
	return NewProof(outpoint, csvDelay, nodes...)
}

// encodeOutpoint writes a 36-byte outpoint: 32-byte hash followed by a 4-byte
// big-endian index.
func encodeOutpoint(op wire.OutPoint) []byte {
	out := make([]byte, chainhash.HashSize+4)
	copy(out, op.Hash[:])
	binary.BigEndian.PutUint32(out[chainhash.HashSize:], op.Index)

	return out
}

// decodeOutpoint reverses encodeOutpoint.
func decodeOutpoint(raw []byte) (wire.OutPoint, error) {
	if len(raw) != chainhash.HashSize+4 {
		return wire.OutPoint{}, fmt.Errorf("outpoint length %d invalid",
			len(raw))
	}

	var op wire.OutPoint
	copy(op.Hash[:], raw[:chainhash.HashSize])
	op.Index = binary.BigEndian.Uint32(raw[chainhash.HashSize:])

	return op, nil
}

// encodeProofNodes emits each Node as a length-prefixed nested TLV
// sub-stream. Nodes are emitted in ascending txid byte order to make the
// encoding deterministic. The wrapper format is:
//
//	4-byte big-endian count
//	   for each node:
//	     4-byte big-endian sub-stream length
//	     sub-stream bytes (encodeNodeStream output)
//
// We use a length-prefix rather than relying on TLV concatenation because
// the outer proof record expects a single opaque byte payload; nesting a
// TLV stream inside it means decoders can evolve the per-Node layout
// independently of the outer layout.
func encodeProofNodes(nodes map[chainhash.Hash]*Node) ([]byte, error) {
	keys := sortedHashKeys(nodes)

	var buf bytes.Buffer
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(keys)))
	if _, err := buf.Write(lenBuf[:]); err != nil {
		return nil, err
	}

	for _, key := range keys {
		node := nodes[key]
		raw, err := encodeNodeStream(node)
		if err != nil {
			return nil, fmt.Errorf("encode node %s: %w", key, err)
		}

		var nodeLen [4]byte
		binary.BigEndian.PutUint32(nodeLen[:], uint32(len(raw)))
		if _, err := buf.Write(nodeLen[:]); err != nil {
			return nil, err
		}
		if _, err := buf.Write(raw); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// decodeProofNodes reverses encodeProofNodes, delegating each node's field
// parsing to decodeNodeStream so any future per-Node fields only need to be
// added in one place.
func decodeProofNodes(raw []byte) ([]*Node, error) {
	if len(raw) < 4 {
		return nil, fmt.Errorf("truncated proof node list")
	}

	count := binary.BigEndian.Uint32(raw[:4])
	raw = raw[4:]

	if count > MaxProofNodes {
		return nil, fmt.Errorf("proof node count %d exceeds max %d",
			count, MaxProofNodes)
	}

	nodes := make([]*Node, 0, count)
	seen := make(map[chainhash.Hash]struct{}, count)

	for i := uint32(0); i < count; i++ {
		if len(raw) < 4 {
			return nil, fmt.Errorf("truncated proof node "+
				"#%d header", i)
		}

		nodeLen := binary.BigEndian.Uint32(raw[:4])
		raw = raw[4:]

		if uint32(len(raw)) < nodeLen {
			return nil, fmt.Errorf("truncated proof node #%d body",
				i)
		}

		node, err := decodeNodeStream(raw[:nodeLen])
		if err != nil {
			return nil, fmt.Errorf("node #%d: %w", i, err)
		}
		raw = raw[nodeLen:]

		txid := node.Tx.TxHash()
		if _, exists := seen[txid]; exists {
			return nil, fmt.Errorf("duplicate proof node txid %s",
				txid)
		}
		seen[txid] = struct{}{}

		nodes = append(nodes, node)
	}

	if len(raw) != 0 {
		return nil, fmt.Errorf("trailing %d bytes after proof nodes",
			len(raw))
	}

	return nodes, nil
}

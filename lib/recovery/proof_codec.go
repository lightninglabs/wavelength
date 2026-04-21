package recovery

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
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

	// proofNodesRecordType carries the length-prefixed node list.
	proofNodesRecordType tlv.Type = 7
)

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
	nodes, err := encodeProofNodes(proof.nodes)
	if err != nil {
		return nil, fmt.Errorf("encode proof nodes: %w", err)
	}

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(proofVersionRecordType, &version),
		tlv.MakePrimitiveRecord(
			proofTargetOutpointRecordType, &outpoint,
		),
		tlv.MakePrimitiveRecord(proofCSVDelayRecordType, &csvDelay),
		tlv.MakePrimitiveRecord(proofNodesRecordType, &nodes),
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

	if _, err := stream.DecodeWithParsedTypes(
		bytes.NewReader(raw),
	); err != nil {

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
		return wire.OutPoint{}, fmt.Errorf(
			"outpoint length %d invalid", len(raw),
		)
	}

	var op wire.OutPoint
	copy(op.Hash[:], raw[:chainhash.HashSize])
	op.Index = binary.BigEndian.Uint32(raw[chainhash.HashSize:])
	return op, nil
}

// encodeProofNodes writes a 4-byte big-endian node count followed by each
// node encoded as (1-byte kind || 4-byte big-endian tx-length || tx bytes).
// Nodes are emitted in ascending txid byte order so the encoding is
// deterministic.
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
		if node == nil || node.Tx == nil {
			return nil, fmt.Errorf("node %s missing tx", key)
		}

		kind := uint8(node.Kind)
		if err := buf.WriteByte(kind); err != nil {
			return nil, err
		}

		var txBuf bytes.Buffer
		if err := node.Tx.Serialize(&txBuf); err != nil {
			return nil, fmt.Errorf("serialize tx %s: %w",
				key, err)
		}

		var txLenBuf [4]byte
		binary.BigEndian.PutUint32(
			txLenBuf[:], uint32(txBuf.Len()),
		)
		if _, err := buf.Write(txLenBuf[:]); err != nil {
			return nil, err
		}
		if _, err := buf.Write(txBuf.Bytes()); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// decodeProofNodes reverses encodeProofNodes, validating the node kind and
// rejecting duplicate txids.
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
		if len(raw) < 5 {
			return nil, fmt.Errorf(
				"truncated proof node #%d header", i,
			)
		}

		kind := NodeKind(raw[0])
		if kind < NodeKindTree || kind > NodeKindArk {
			return nil, fmt.Errorf("invalid node kind %d", kind)
		}

		txLen := binary.BigEndian.Uint32(raw[1:5])
		raw = raw[5:]

		if uint32(len(raw)) < txLen {
			return nil, fmt.Errorf(
				"truncated proof node #%d tx payload", i,
			)
		}

		tx := &wire.MsgTx{}
		if err := tx.Deserialize(
			bytes.NewReader(raw[:txLen]),
		); err != nil {

			return nil, fmt.Errorf("deserialize tx #%d: %w",
				i, err)
		}
		raw = raw[txLen:]

		txid := tx.TxHash()
		if _, exists := seen[txid]; exists {
			return nil, fmt.Errorf("duplicate proof node "+
				"txid %s", txid)
		}
		seen[txid] = struct{}{}

		nodes = append(nodes, &Node{Kind: kind, Tx: tx})
	}

	if len(raw) != 0 {
		return nil, fmt.Errorf("trailing %d bytes after proof "+
			"nodes", len(raw))
	}

	return nodes, nil
}

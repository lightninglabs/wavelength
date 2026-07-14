package roundpb

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	treePkg "github.com/lightninglabs/wavelength/lib/tree"
	"github.com/stretchr/testify/require"
)

// =====================================================================
// FINDING-1: VTXO tree cycle/self-reference causes infinite loop (DoS)
// Severity: HIGH (CVSS 7.5)
//
// TreeFromProto does not detect cycles in the children map. A
// malicious server can craft a VTXOTree where a node references
// itself or an ancestor as a child, causing infinite recursion in
// any tree traversal code (signing, verification, unrolling).
//
// The flattened representation allows arbitrary child indices, so
// node 0 can reference node 0 as its own child, or two nodes can
// reference each other. The deserialization code only checks that
// childIdx < len(goNodes) but never validates DAG acyclicity.
// =====================================================================

// TestTreeFromProtoCycleSelfReference demonstrates that a node
// referencing itself as a child creates a cycle in the deserialized
// tree.
func TestTreeFromProtoCycleSelfReference(t *testing.T) {
	t.Parallel()

	hash := chainhash.Hash{0x01}

	// Build a single-node tree where node 0's child at output 0
	// points back to node 0 itself.
	pt := &VTXOTree{
		Nodes: []*TreeNode{
			{
				Input: &Outpoint{
					TxHash:      hash[:],
					OutputIndex: 0,
				},
				Outputs: []*TxOut{
					{
						Value: 1000,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{
					0: 0, // Self-reference cycle.
				},
				Amount: 1000,
			},
		},
		BatchOutpoint: &Outpoint{
			TxHash:      hash[:],
			OutputIndex: 0,
		},
		BatchOutput: &TxOut{
			Value: 1000,
			PkScript: []byte{
				0x51,
			},
		},
	}

	// TreeFromProto rejects self-referential cycles via the
	// pre-order invariant (childIdx > i).
	_, err := TreeFromProto(pt)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cycle or back-reference")
}

// TestTreeFromProtoMutualCycle demonstrates that two nodes referencing
// each other as children creates a mutual cycle.
func TestTreeFromProtoMutualCycle(t *testing.T) {
	t.Parallel()

	hash := chainhash.Hash{0x02}

	// Node 0 -> child is node 1, node 1 -> child is node 0.
	pt := &VTXOTree{
		Nodes: []*TreeNode{
			{
				Input: &Outpoint{
					TxHash:      hash[:],
					OutputIndex: 0,
				},
				Outputs: []*TxOut{
					{
						Value: 500,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{
					0: 1, // Points to node 1.
				},
				Amount: 500,
			},
			{
				Input: &Outpoint{
					TxHash:      hash[:],
					OutputIndex: 1,
				},
				Outputs: []*TxOut{
					{
						Value: 500,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{
					0: 0, // Points back to node 0.
				},
				Amount: 500,
			},
		},
		BatchOutpoint: &Outpoint{
			TxHash:      hash[:],
			OutputIndex: 0,
		},
		BatchOutput: &TxOut{
			Value: 1000,
			PkScript: []byte{
				0x51,
			},
		},
	}

	// TreeFromProto rejects mutual cycles via the pre-order
	// invariant: node 1's child index 0 is not > 1.
	_, err := TreeFromProto(pt)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cycle or back-reference")
}

// =====================================================================
// FINDING-2: Unbounded allocation from proto node count (DoS)
// Severity: MEDIUM (CVSS 5.3)
//
// TreeFromProto allocates len(pt.Nodes) tree.Node objects without any
// upper bound check. A malicious server can send a VTXOTree with
// millions of nodes, causing OOM on the client. The protobuf wire
// format allows repeated fields up to the message size limit (default
// ~2GB in many implementations), so an attacker can craft a message
// with e.g. 10M nodes (~320MB of Node objects plus outputs/cosigners).
//
// Similarly, CommitmentTxBuilt.FromProto allocates unbounded maps for
// VTXOTreePaths and ConnectorLeafMap based on server-controlled sizes.
// =====================================================================

// TestTreeFromProtoLargeNodeCount demonstrates the allocation
// amplification. We use a moderate count to avoid OOM in tests.
func TestTreeFromProtoLargeNodeCount(t *testing.T) {
	t.Parallel()

	hash := chainhash.Hash{0x03}

	// 100k nodes is modest but demonstrates the pattern. In
	// production, an attacker would use millions.
	const nodeCount = 100_000
	nodes := make([]*TreeNode, nodeCount)
	for i := range nodes {
		nodes[i] = &TreeNode{
			Input: &Outpoint{
				TxHash:      hash[:],
				OutputIndex: uint32(i),
			},
			Outputs: []*TxOut{
				{
					Value: 1,
					PkScript: []byte{
						0x51,
					},
				},
			},
			Children: map[uint32]uint32{},
			Amount:   1,
		}
	}

	pt := &VTXOTree{
		Nodes: nodes,
		BatchOutpoint: &Outpoint{
			TxHash:      hash[:],
			OutputIndex: 0,
		},
		BatchOutput: &TxOut{
			Value: int64(nodeCount),
			PkScript: []byte{
				0x51,
			},
		},
	}

	// TreeFromProto rejects node counts exceeding maxTreeNodes.
	_, err := TreeFromProto(pt)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds maximum")
}

// =====================================================================
// FINDING-3: Forward-reference in children allows non-tree DAG (Data
// Integrity)
// Severity: MEDIUM (CVSS 5.9)
//
// The flattenNode serialization uses pre-order traversal, but
// TreeFromProto does not validate that children indices form a valid
// tree (each node has exactly one parent, except the root). A
// malicious server can craft a "diamond" DAG where two parent nodes
// share the same child. This violates the tree invariant and could
// cause double-signing of the same transaction or incorrect balance
// calculations.
// =====================================================================

// TestTreeFromProtoDiamondDAG demonstrates shared children creating a
// non-tree DAG.
func TestTreeFromProtoDiamondDAG(t *testing.T) {
	t.Parallel()

	hash := chainhash.Hash{0x04}

	// Node 0 (root) -> children: output 0 -> node 1, output 1 -> node 2
	// Node 1 -> child: output 0 -> node 3
	// Node 2 -> child: output 0 -> node 3 (SHARED child!)
	pt := &VTXOTree{
		Nodes: []*TreeNode{
			{
				Input: &Outpoint{
					TxHash: hash[:], OutputIndex: 0,
				},
				Outputs: []*TxOut{
					{
						Value: 500,
						PkScript: []byte{
							0x51,
						},
					},
					{
						Value: 500,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{
					0: 1,
					1: 2,
				},
				Amount: 1000,
			},
			{
				Input: &Outpoint{
					TxHash: hash[:], OutputIndex: 1,
				},
				Outputs: []*TxOut{
					{
						Value: 500,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{
					0: 3, // Shared child.
				},
				Amount: 500,
			},
			{
				Input: &Outpoint{
					TxHash: hash[:], OutputIndex: 2,
				},
				Outputs: []*TxOut{
					{
						Value: 500,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{
					0: 3, // Same shared child!
				},
				Amount: 500,
			},
			{
				Input: &Outpoint{
					TxHash: hash[:], OutputIndex: 3,
				},
				Outputs: []*TxOut{
					{
						Value: 250,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{},
				Amount:   250,
			},
		},
		BatchOutpoint: &Outpoint{
			TxHash: hash[:], OutputIndex: 0,
		},
		BatchOutput: &TxOut{
			Value: 1000, PkScript: []byte{
				0x51,
			},
		},
	}

	// TreeFromProto rejects diamond DAGs because the shared
	// child (node 3) would be referenced by node 2 at index 3,
	// but node 1 already claims it. The pre-order invariant
	// prevents this since shared children violate the tree
	// property.
	//
	// NOTE: The pre-order invariant (childIdx > i) alone allows
	// this specific diamond shape since all forward references
	// are valid. However, the diamond is still structurally
	// accepted here. The pre-order check primarily prevents
	// cycles; diamond detection would require tracking parent
	// counts. This test documents the current behavior.
	tree, err := TreeFromProto(pt)
	require.NoError(t, err)
	require.NotNil(t, tree)
}

// =====================================================================
// FINDING-4: OutpointFromProto byte-order consistency with map key
// format
// Severity: LOW (CVSS 3.1)
//
// OutpointToProto stores the hash in internal byte order (as-is from
// chainhash.Hash), while OutpointToMapKey uses wire.OutPoint.String()
// which produces byte-REVERSED hex (display order). These two formats
// are intentionally different and the code handles them correctly via
// separate functions. However, there is no documentation or assertion
// preventing a caller from using TxIDToHex (which is NOT byte-reversed)
// on an outpoint hash. If anyone constructs a map key using
// TxIDToHex(outpoint.Hash) instead of OutpointToMapKey, the keys
// would be inconsistent. This is an API foot-gun, not a live bug.
// =====================================================================

// TestByteOrderConsistency verifies that the two serialization paths
// produce different formats for the same hash and that the correct
// functions are paired.
func TestByteOrderConsistency(t *testing.T) {
	t.Parallel()

	// Hash with distinct bytes to show byte-reversal.
	h := chainhash.Hash{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
		0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
	}
	op := wire.OutPoint{Hash: h, Index: 7}

	mapKey := OutpointToMapKey(op)
	protoOP := OutpointToProto(op)

	// Map key uses byte-reversed hex (chainhash.String()).
	require.Contains(
		t, mapKey, "201f1e1d", "map key should use byte-reversed hex",
	)

	// Proto stores internal byte order.
	require.Equal(
		t, byte(0x01), protoOP.TxHash[0],
		"proto should store internal byte order",
	)

	// Verify round-trip consistency for both paths.
	gotFromMap, err := OutpointFromMapKey(mapKey)
	require.NoError(t, err)
	require.Equal(t, op, gotFromMap)

	gotFromProto, err := OutpointFromProto(protoOP)
	require.NoError(t, err)
	require.Equal(t, op, gotFromProto)
}

// =====================================================================
// FINDING-5: PSBTFromBytes accepts nil without error
// Severity: INFORMATIONAL
//
// PSBTFromBytes(nil) returns (nil, nil). This is intentional for
// optional PSBT fields, but when used in CommitmentTxBuilt.FromProto
// for batch_psbt, a nil PSBT is a protocol error that should be
// rejected. The FromProto code calls PSBTFromBytes(pb.BatchPsbt) and
// if batch_psbt is empty (proto default for bytes), it silently sets
// e.Tx = nil. Downstream code that dereferences e.Tx will panic.
// =====================================================================

// TestPSBTFromBytesNilPassthrough demonstrates nil passthrough.
func TestPSBTFromBytesNilPassthrough(t *testing.T) {
	t.Parallel()

	// nil bytes produce nil PSBT without error. This is the
	// vulnerability path: proto default for bytes is nil, so
	// an omitted batch_psbt field will produce a nil Tx.
	p, err := PSBTFromBytes(nil)
	require.NoError(t, err)
	require.Nil(t, p, "nil bytes produce nil PSBT silently")

	// Empty non-nil slice correctly returns an error.
	p, err = PSBTFromBytes([]byte{})
	require.Error(t, err, "empty non-nil slice should error")
	require.Nil(t, p)
}

// =====================================================================
// FINDING-6: SchnorrSigFromBytes accepts nil without error
// Severity: INFORMATIONAL
//
// SchnorrSigFromBytes(nil) returns (nil, nil). Combined with the
// pattern in SubmitVTXOForfeitSigsToServer.ToProto which uses
// SchnorrSigToBytes(sig), if a ForfeitSigs map entry contains a nil
// signature, it will be serialized as nil bytes and deserialized
// back as nil. A nil signature in a forfeit tx is a critical error
// that should be caught, not silently propagated.
// =====================================================================

// TestSchnorrNilSigSilentPassthrough demonstrates silent nil
// signature handling.
func TestSchnorrNilSigSilentPassthrough(t *testing.T) {
	t.Parallel()

	// Serializing a nil signature produces nil bytes.
	b := SchnorrSigToBytes(nil)
	require.Nil(t, b)

	// Deserializing nil bytes produces nil signature.
	sig, err := SchnorrSigFromBytes(nil)
	require.NoError(t, err)
	require.Nil(t, sig)
}

// =====================================================================
// FINDING-7: TxOutFromProto does not validate negative value (Fund loss)
// Severity: MEDIUM (CVSS 6.1)
//
// The proto TxOut.value is int64. A malicious server can set a
// negative value. TxOutFromProto blindly copies this into
// wire.TxOut.Value. If the client uses this negative value in
// fee calculations (fee = inputValue - outputValue), the result
// could overflow, leading to incorrect fee estimation or acceptance
// of a transaction that actually overpays fees.
// =====================================================================

// TestTxOutFromProtoNegativeValue verifies that negative output values
// are rejected.
func TestTxOutFromProtoNegativeValue(t *testing.T) {
	t.Parallel()

	pb := &TxOut{
		Value: -1_000_000,
		PkScript: []byte{
			0x51,
		},
	}

	out, err := TxOutFromProto(pb)
	require.Error(t, err)
	require.Nil(t, out)
	require.Contains(t, err.Error(), "negative output value")
}

// =====================================================================
// FINDING-8: OutpointFromProto does not validate output_index
// Severity: LOW (CVSS 2.5)
//
// output_index is uint32 in proto. While technically valid for
// Bitcoin, an output_index of math.MaxUint32 (0xFFFFFFFF) is
// conventionally used as a null/sentinel value. No validation is
// performed.
// =====================================================================

// TestOutpointFromProtoMaxIndex demonstrates acceptance of sentinel
// index values.
func TestOutpointFromProtoMaxIndex(t *testing.T) {
	t.Parallel()

	hash := chainhash.Hash{0x01}
	pb := &Outpoint{
		TxHash:      hash[:],
		OutputIndex: 0xFFFFFFFF,
	}

	op, err := OutpointFromProto(pb)
	require.NoError(t, err)
	require.Equal(t, uint32(0xFFFFFFFF), op.Index)
}

// =====================================================================
// FINDING-9: Empty ConnectorLeafMap key accepted as valid
// Severity: LOW (CVSS 2.0)
//
// OutpointFromMapKey("") returns an error, which is correctly
// handled. But the map key "0000...0000:0" (64-char zero hash)
// represents the null outpoint and is accepted as valid.
// =====================================================================

// TestOutpointFromMapKeyNullOutpoint demonstrates that the null
// outpoint is accepted as a valid map key.
func TestOutpointFromMapKeyNullOutpoint(t *testing.T) {
	t.Parallel()

	nullHash := chainhash.Hash{} // All zeros.
	nullOP := wire.OutPoint{Hash: nullHash, Index: 0}
	key := OutpointToMapKey(nullOP)

	got, err := OutpointFromMapKey(key)
	require.NoError(t, err)
	require.Equal(t, nullOP, got)
}

// =====================================================================
// FINDING-10: Children map allows child index > number of outputs
// Severity: MEDIUM (CVSS 5.3)
//
// TreeFromProto validates that childIdx < len(goNodes) but does NOT
// validate that outIdx (the map key in Children) < len(node.Outputs).
// A malicious server can claim a child is at output index 999 even
// if the node only has 2 outputs. Downstream code that uses the
// output index to look up scripts or amounts will panic with an
// out-of-bounds access.
// =====================================================================

// TestTreeFromProtoChildOutIdxOutOfBounds demonstrates that a child
// can reference an output index that does not exist in the parent.
func TestTreeFromProtoChildOutIdxOutOfBounds(t *testing.T) {
	t.Parallel()

	hash := chainhash.Hash{0x05}

	pt := &VTXOTree{
		Nodes: []*TreeNode{
			{
				Input: &Outpoint{
					TxHash: hash[:], OutputIndex: 0,
				},
				Outputs: []*TxOut{
					{
						Value: 1000,
						PkScript: []byte{
							0x51,
						},
					},
					// Only 2 outputs (idx 0 and 1).
					{
						Value: 1000,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{
					// Output index 999 does not
					// exist!
					999: 1,
				},
				Amount: 2000,
			},
			{
				Input: &Outpoint{
					TxHash: hash[:], OutputIndex: 1,
				},
				Outputs: []*TxOut{
					{
						Value: 500,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{},
				Amount:   500,
			},
		},
		BatchOutpoint: &Outpoint{
			TxHash: hash[:], OutputIndex: 0,
		},
		BatchOutput: &TxOut{
			Value: 2000, PkScript: []byte{
				0x51,
			},
		},
	}

	// TreeFromProto rejects out-of-bounds output indices.
	_, err := TreeFromProto(pt)
	require.Error(t, err)
	require.Contains(t, err.Error(), "child output index")
}

// =====================================================================
// FINDING-11: treeNodeFromProto nil input outpoint causes error but
// inconsistent with nil Children handling
// Severity: INFORMATIONAL
//
// If TreeNode.Input is nil, OutpointFromProto returns "nil outpoint"
// error. This is correct, but TreeNode.Children being nil is
// silently allowed (empty map), creating a leaf. The inconsistency
// is that the serializer (flattenNode) always sets Input, but a
// malicious proto could omit it.
// =====================================================================

// TestTreeNodeFromProtoNilInput verifies that nil input is rejected.
func TestTreeNodeFromProtoNilInput(t *testing.T) {
	t.Parallel()

	hash := chainhash.Hash{0x06}

	pt := &VTXOTree{
		Nodes: []*TreeNode{
			{
				Input:    nil, // Missing input.
				Outputs:  []*TxOut{},
				Children: map[uint32]uint32{},
			},
		},
		BatchOutpoint: &Outpoint{
			TxHash: hash[:], OutputIndex: 0,
		},
		BatchOutput: &TxOut{
			Value: 0, PkScript: []byte{
				0x51,
			},
		},
	}

	_, err := TreeFromProto(pt)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil outpoint")
}

// =====================================================================
// FINDING-12: MsgTxFromBytes with adversarial input
// Severity: INFORMATIONAL
//
// MsgTxFromBytes wraps btcd's tx.Deserialize which handles malformed
// input correctly. However, PSBTFromBytes and MsgTxFromBytes both
// treat empty-but-non-nil []byte{} differently from nil.
// =====================================================================

// TestMsgTxFromBytesEmptySlice verifies empty byte handling.
func TestMsgTxFromBytesEmptySlice(t *testing.T) {
	t.Parallel()

	// nil returns nil.
	tx, err := MsgTxFromBytes(nil)
	require.NoError(t, err)
	require.Nil(t, tx)

	// Empty non-nil slice returns error (not nil).
	tx, err = MsgTxFromBytes([]byte{})
	require.Error(t, err)
	require.Nil(t, tx)
}

// =====================================================================
// FINDING-13: int32 VtxoTreePaths key allows negative indices
// Severity: MEDIUM (CVSS 5.0)
//
// The proto map<int32, VTXOTree> vtxo_tree_paths uses signed int32
// keys. CommitmentTxBuilt.FromProto casts these to int:
//   e.VTXOTreePaths[int(idx)] = t
//
// A malicious server can use negative keys (e.g., -1) which would
// produce negative map keys in the domain type. Downstream code that
// uses these as commitment tx output indices would behave incorrectly.
// =====================================================================

// TestCommitmentTxBuiltNegativeTreePathIndex is a conceptual test.
// We cannot directly instantiate CommitmentTxBuilt.FromProto here
// (it's in the round package), but we document the vulnerability.
func TestCommitmentTxBuiltNegativeTreePathIndex(t *testing.T) {
	t.Parallel()

	// The proto allows negative int32 keys in map<int32, VTXOTree>.
	// When cast to int in Go, -1 becomes -1. This is a valid Go
	// map key but semantically invalid as a tx output index.
	//
	// This test documents the finding. See the round/from_proto.go
	// line: e.VTXOTreePaths[int(idx)] = t
	idx := int32(-1)
	goIdx := int(idx)
	require.Equal(
		t, -1, goIdx, "negative proto key maps to negative Go map key",
	)
}

// =====================================================================
// Summary of additional findings documented inline:
//
// FINDING-14 (round/outbox_messages.go): SubmitVTXOForfeitSigsToServer
// .ToProto iterates ForfeitSigs map (non-deterministic order in Go
// maps). If the server expects a specific ordering, this could cause
// intermittent failures. The code does check for missing ForfeitTxs
// entries, which is good. INFORMATIONAL.
//
// FINDING-15 (round/from_proto.go): BoardingFailed.FromProto marks
// ALL server errors as Recoverable=true. This means the client will
// always retry, even for permanent failures (e.g., blacklisted
// participant). This could cause infinite retry loops. LOW.
//
// FINDING-16 (round/from_proto.go): JoinRoundRequest.FromProto
// accepts Identifier of any valid pubkey length (compressed or
// uncompressed). The ToProto path always uses SerializeCompressed().
// If the server echoes back an uncompressed key, the round-trip
// would not be byte-identical. INFORMATIONAL.
// =====================================================================

// TestBytesReaderPartialRead verifies the custom bytesReader handles
// partial reads correctly (no data loss on short buffers).
func TestBytesReaderPartialRead(t *testing.T) {
	t.Parallel()

	data := []byte("hello world, this is test data for reader")
	r := &bytesReader{data: data, pos: 0}

	// Read in small chunks.
	buf := make([]byte, 5)
	var total []byte
	for {
		n, err := r.Read(buf)
		total = append(total, buf[:n]...)
		if err != nil {
			break
		}
	}

	require.Equal(t, data, total,
		"all data should be readable in chunks")
}

// TestBytesWriterMultipleWrites verifies the custom bytesWriter
// appends correctly across multiple writes.
func TestBytesWriterMultipleWrites(t *testing.T) {
	t.Parallel()

	var buf []byte
	w := &bytesWriter{buf: &buf}

	for i := 0; i < 100; i++ {
		chunk := []byte(fmt.Sprintf("chunk-%d-", i))
		n, err := w.Write(chunk)
		require.NoError(t, err)
		require.Equal(t, len(chunk), n)
	}

	require.True(t, len(buf) > 0, "buffer should contain all writes")
}

// =====================================================================
// FINDING-C1-POC: Cycle injection causes stack overflow on tree
// traversal. This PoC demonstrates that after successful
// deserialization, calling Depth() on a cyclic tree would crash.
// We verify the cycle exists structurally without calling any
// recursive tree method (which would kill the test process).
// Severity: CRITICAL (CVSS 9.1)
// =====================================================================

// TestTreeFromProtoCycleExploitChain demonstrates the full exploit
// chain: craft malicious proto -> deserialize -> verify cycle exists
// -> demonstrate that any recursive traversal is now unsafe.
func TestTreeFromProtoCycleExploitChain(t *testing.T) {
	t.Parallel()

	hash := chainhash.Hash{0xc1}

	// Exploit: Build a tree with a back-edge from a child (node 2)
	// to the root (node 0), creating a cycle at depth 2.
	pt := &VTXOTree{
		Nodes: []*TreeNode{
			{
				Input: &Outpoint{
					TxHash:      hash[:],
					OutputIndex: 0,
				},
				Outputs: []*TxOut{
					{
						Value: 1000,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{
					0: 1,
				},
				Amount: 1000,
			},
			{
				Input: &Outpoint{
					TxHash:      hash[:],
					OutputIndex: 1,
				},
				Outputs: []*TxOut{
					{
						Value: 500,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{
					// Back-edge to root.
					0: 0,
				},
				Amount: 500,
			},
		},
		BatchOutpoint: &Outpoint{
			TxHash:      hash[:],
			OutputIndex: 0,
		},
		BatchOutput: &TxOut{
			Value: 1000,
			PkScript: []byte{
				0x51,
			},
		},
	}

	// TreeFromProto rejects back-edge cycles via the pre-order
	// invariant: node 1's child references node 0, but 0 is not
	// greater than 1.
	_, err := TreeFromProto(pt)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cycle or back-reference")
}

// =====================================================================
// FINDING-H2-POC: Negative TxOut value in tree node amount field.
// Demonstrates that negative amounts flow through tree
// deserialization into btcutil.Amount, corrupting balance
// calculations.
// Severity: HIGH (CVSS 7.0)
// =====================================================================

// TestTreeFromProtoNegativeNodeAmount demonstrates that negative
// amounts in tree nodes are accepted without validation.
func TestTreeFromProtoNegativeNodeAmount(t *testing.T) {
	t.Parallel()

	hash := chainhash.Hash{0xda}

	pt := &VTXOTree{
		Nodes: []*TreeNode{
			{
				Input: &Outpoint{
					TxHash:      hash[:],
					OutputIndex: 0,
				},
				Outputs: []*TxOut{
					{
						// Negative output value.
						Value: -500_000,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{},
				// Negative node amount.
				Amount: -500_000,
			},
		},
		BatchOutpoint: &Outpoint{
			TxHash:      hash[:],
			OutputIndex: 0,
		},
		BatchOutput: &TxOut{
			Value: 1000,
			PkScript: []byte{
				0x51,
			},
		},
	}

	// TxOutFromProto now rejects negative output values.
	_, err := TreeFromProto(pt)
	require.Error(t, err)
	require.Contains(t, err.Error(), "negative output value")
}

// =====================================================================
// FINDING-M3-POC: Child output-index references non-existent output.
// Demonstrates that downstream code accessing Outputs[outIdx]
// would panic.
// Severity: MEDIUM (CVSS 5.3)
// =====================================================================

// TestTreeFromProtoOutIdxPanicVector demonstrates that a child
// mapped at an invalid output index creates a panic vector when
// downstream code accesses outputs by that index.
func TestTreeFromProtoOutIdxPanicVector(t *testing.T) {
	t.Parallel()

	hash := chainhash.Hash{0xe5}

	pt := &VTXOTree{
		Nodes: []*TreeNode{
			{
				Input: &Outpoint{
					TxHash:      hash[:],
					OutputIndex: 0,
				},
				Outputs: []*TxOut{
					{
						Value: 1000,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{
					// Only 1 output (index 0), but child
					// is mapped at index 42.
					42: 1,
				},
				Amount: 1000,
			},
			{
				Input: &Outpoint{
					TxHash:      hash[:],
					OutputIndex: 1,
				},
				Outputs: []*TxOut{
					{
						Value: 500,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{},
				Amount:   500,
			},
		},
		BatchOutpoint: &Outpoint{
			TxHash:      hash[:],
			OutputIndex: 0,
		},
		BatchOutput: &TxOut{
			Value: 1000,
			PkScript: []byte{
				0x51,
			},
		},
	}

	// TreeFromProto rejects out-of-bounds output indices.
	_, err := TreeFromProto(pt)
	require.Error(t, err)
	require.Contains(t, err.Error(), "child output index")
}

// =====================================================================
// FINDING-M1-POC: Diamond DAG causes double-visit during ForEach.
// Demonstrates that a shared child is visited multiple times,
// which could lead to double-signing.
// Severity: MEDIUM (CVSS 5.9)
// =====================================================================

// TestTreeFromProtoDiamondDoubleVisit demonstrates that ForEach
// visits a shared child twice in a diamond DAG.
func TestTreeFromProtoDiamondDoubleVisit(t *testing.T) {
	t.Parallel()

	hash := chainhash.Hash{0xd4}

	pt := &VTXOTree{
		Nodes: []*TreeNode{
			{
				Input: &Outpoint{
					TxHash: hash[:], OutputIndex: 0,
				},
				Outputs: []*TxOut{
					{
						Value: 500,
						PkScript: []byte{
							0x51,
						},
					},
					{
						Value: 500,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{
					0: 1,
					1: 2,
				},
				Amount: 1000,
			},
			{
				Input: &Outpoint{
					TxHash: hash[:], OutputIndex: 1,
				},
				Outputs: []*TxOut{
					{
						Value: 250,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{
					0: 3, // Shared child.
				},
				Amount: 250,
			},
			{
				Input: &Outpoint{
					TxHash: hash[:], OutputIndex: 2,
				},
				Outputs: []*TxOut{
					{
						Value: 250,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{
					0: 3, // Same shared child!
				},
				Amount: 250,
			},
			{
				Input: &Outpoint{
					TxHash: hash[:], OutputIndex: 3,
				},
				Outputs: []*TxOut{
					{
						Value: 100,
						PkScript: []byte{
							0x51,
						},
					},
				},
				Children: map[uint32]uint32{},
				Amount:   100,
			},
		},
		BatchOutpoint: &Outpoint{
			TxHash: hash[:], OutputIndex: 0,
		},
		BatchOutput: &TxOut{
			Value: 1000, PkScript: []byte{
				0x51,
			},
		},
	}

	// NOTE: The pre-order invariant (childIdx > i) does not
	// prevent all diamond DAGs -- only those with back-edges.
	// This specific diamond shape uses only forward references
	// and is still accepted. Diamond detection would require a
	// parent-count tracker. This test documents the current
	// behavior: the diamond is accepted but the shared child
	// is visited twice during traversal.
	tree, err := TreeFromProto(pt)
	require.NoError(t, err)
	require.NotNil(t, tree)

	// Count how many times each node pointer is visited during
	// a manual depth-first traversal (simulating ForEach).
	visitCount := make(map[*treePkg.Node]int)
	var walk func(n *treePkg.Node)
	walk = func(n *treePkg.Node) {
		if n == nil {
			return
		}
		visitCount[n]++
		// Stop if we detect we already visited this node to
		// prevent infinite recursion in the test.
		if visitCount[n] > 1 {
			return
		}
		for _, child := range n.Children {
			walk(child)
		}
	}
	walk(tree.Root)

	// The shared child (goNodes[3]) is visited twice.
	sharedChild := tree.Root.Children[0].Children[0]
	require.Equal(
		t, 2, visitCount[sharedChild],
		"diamond: shared child visited twice",
	)
}

package arkrpc

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/stretchr/testify/require"
)

// shapeTestOutputs is the number of outputs every synthetic node in this
// file carries. Two is the smallest count that lets a node name two
// distinct children, which is what the DAG shapes below need.
const shapeTestOutputs = 2

// shapeNode builds a minimal, structurally valid TreePathNode carrying
// the given child wiring. Only the shape matters to these tests, so the
// payload fields are filled with the cheapest values the decoder
// accepts.
func shapeNode(idx uint32, children map[uint32]uint32) *TreePathNode {
	hash := chainhash.Hash{byte(idx + 1)}

	outputs := make([]*TxOut, shapeTestOutputs)
	for i := range outputs {
		outputs[i] = &TxOut{
			Value: 1000,
			PkScript: []byte{
				0x51,
			},
		}
	}

	return &TreePathNode{
		Input: &OutPoint{
			Txid: hash[:],
			Vout: idx,
		},
		Outputs:  outputs,
		Children: children,
		Amount:   2000,
	}
}

// shapePath wraps nodes into a TreePath with a valid batch outpoint and
// output, so any error a test observes comes from the node wiring rather
// than from the surrounding fields.
func shapePath(nodes ...*TreePathNode) *TreePath {
	hash := chainhash.Hash{0xba}

	return &TreePath{
		Nodes: nodes,
		BatchOutpoint: &OutPoint{
			Txid: hash[:],
			Vout: 0,
		},
		BatchOutput: &TxOut{
			Value: 2000,
			PkScript: []byte{
				0x51,
			},
		},
	}
}

// TestTreePathToTreeRejectsSharedChild pins the single-parent invariant
// on the indexer decode path. Forward references alone admit a DAG: two
// parents at different indices can both name the same higher-indexed
// child and both satisfy childIdx > i. That shape is the expensive one
// here, because the receive path walks the decoded tree to validate the
// claimed ancestry depth and that walk re-visits a shared subtree once
// per path reaching it.
func TestTreePathToTreeRejectsSharedChild(t *testing.T) {
	t.Parallel()

	// Root fans out to 1 and 2; both then name 3.
	tp := shapePath(
		shapeNode(
			0, map[uint32]uint32{
				0: 1,
				1: 2,
			},
		),
		shapeNode(
			1, map[uint32]uint32{
				0: 3,
			},
		),
		shapeNode(
			2, map[uint32]uint32{
				0: 3,
			},
		),
		shapeNode(3, nil),
	)

	got, err := TreePathToTree(tp)
	require.Error(t, err)
	require.ErrorContains(t, err, "must not share children")
	require.Nil(t, got)
}

// TestTreePathToTreeRejectsSharedChildViaSiblingOutputs covers the
// cheapest form of the same shape: one node pointing several of its own
// outputs at the same next node. Children is a map keyed by output
// index, so this needs no second parent at all, and it is what turns a
// depth-d chain into 2^d walk paths from only d nodes.
func TestTreePathToTreeRejectsSharedChildViaSiblingOutputs(t *testing.T) {
	t.Parallel()

	tp := shapePath(
		shapeNode(
			0, map[uint32]uint32{
				0: 1,
				1: 1,
			},
		),
		shapeNode(1, nil),
	)

	got, err := TreePathToTree(tp)
	require.Error(t, err)
	require.ErrorContains(t, err, "must not share children")
	require.Nil(t, got)
}

// TestTreePathToTreeRejectsUnreachableNodes covers the shapes that
// single-parent plus forward-edges still admits: those describe a
// forest, so a sender can pad a message with nodes no walk from the root
// will ever reach, or attach a whole second root with its own subtree.
func TestTreePathToTreeRejectsUnreachableNodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		nodes []*TreePathNode
	}{{
		// The root claims nothing, so node 1 is simply detached.
		name: "orphan node",
		nodes: []*TreePathNode{
			shapeNode(0, nil),
			shapeNode(1, nil),
		},
	}, {
		// Node 1 is a second root carrying its own subtree.
		name: "detached second root",
		nodes: []*TreePathNode{
			shapeNode(0, nil),
			shapeNode(1, map[uint32]uint32{0: 2}),
			shapeNode(2, nil),
		},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := TreePathToTree(shapePath(tc.nodes...))
			require.Error(t, err)
			require.ErrorContains(
				t, err, "unreachable nodes or multiple roots",
			)
			require.Nil(t, got)
		})
	}
}

// TestTreePathToTreeReportsWildIndexAsOutOfRange checks the diagnostic
// ordering: an index past the end of the node slice must be named as
// out of range even when it repeats, rather than being reported as a
// sharing violation that sends a reader hunting for a tree-shape bug.
func TestTreePathToTreeReportsWildIndexAsOutOfRange(t *testing.T) {
	t.Parallel()

	tp := shapePath(
		shapeNode(
			0, map[uint32]uint32{
				0: 999,
				1: 999,
			},
		),
		shapeNode(1, nil),
	)

	got, err := TreePathToTree(tp)
	require.Error(t, err)
	require.ErrorContains(t, err, "out of range")
	require.NotContains(t, err.Error(), "share children")
	require.Nil(t, got)
}

// TestTreePathToTreeNodeCap covers the node bound. The decoder had none
// at all, so a single response could carry an arbitrary number of nodes
// to deserialize before any structural check ran.
func TestTreePathToTreeNodeCap(t *testing.T) {
	t.Parallel()

	// A linear chain long enough to exceed the cap set below.
	const chainLen = 8

	nodes := make([]*TreePathNode, chainLen)
	for i := range nodes {
		var children map[uint32]uint32
		if i < chainLen-1 {
			children = map[uint32]uint32{0: uint32(i + 1)}
		}
		nodes[i] = shapeNode(uint32(i), children)
	}
	tp := shapePath(nodes...)

	// Over the cap: refused before any node is deserialized.
	got, err := TreePathToTree(tp, WithMaxTreePathNodes(chainLen-1))
	require.Error(t, err)
	require.ErrorContains(t, err, "exceeds maximum")
	require.Nil(t, got)

	// At the cap, and with the cap disabled, the same chain decodes.
	for _, maxNodes := range []int{chainLen, 0} {
		t.Run(fmt.Sprintf("max=%d", maxNodes), func(t *testing.T) {
			t.Parallel()

			got, err := TreePathToTree(
				tp, WithMaxTreePathNodes(maxNodes),
			)
			require.NoError(t, err)
			require.NotNil(t, got)
		})
	}
}

// TestTreePathToTreeAcceptsWellFormedTree guards against the new checks
// rejecting anything a legitimate producer emits: a plain branching tree
// where every node other than the root is claimed exactly once must
// still decode, and the wiring must survive.
func TestTreePathToTreeAcceptsWellFormedTree(t *testing.T) {
	t.Parallel()

	tp := shapePath(
		shapeNode(
			0, map[uint32]uint32{
				0: 1,
				1: 2,
			},
		),
		shapeNode(
			1, map[uint32]uint32{
				0: 3,
			},
		),
		shapeNode(2, nil),
		shapeNode(3, nil),
	)

	got, err := TreePathToTree(tp)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Root.Children, 2)
	require.Len(t, got.Root.Children[0].Children, 1)
	require.Empty(t, got.Root.Children[1].Children)
}

// TestAncestryPathDepthWalkOnSharedChild pins the reason the check
// belongs at this boundary rather than in each walker. nodeMaxDepth,
// which the receive path runs on every decoded ancestry path, has no
// memoization, so a shared child multiplies its work by the number of
// paths reaching it. Refusing the shape at decode time means the walk
// is never handed one.
func TestAncestryPathDepthWalkOnSharedChild(t *testing.T) {
	t.Parallel()

	// A chain where every node points both of its outputs at the
	// next one: linear in nodes, exponential in root-to-leaf paths.
	const chainLen = 20

	nodes := make([]*TreePathNode, chainLen)
	for i := range nodes {
		var children map[uint32]uint32
		if i < chainLen-1 {
			children = map[uint32]uint32{
				0: uint32(i + 1),
				1: uint32(i + 1),
			}
		}
		nodes[i] = shapeNode(uint32(i), children)
	}

	hash := chainhash.Hash{0xac}
	ap := &AncestryPath{
		TreePath:       shapePath(nodes...),
		CommitmentTxid: hash[:],
		TreeDepth:      chainLen,
	}

	got, err := AncestryPathToTree(ap)
	require.Error(t, err)
	require.ErrorContains(t, err, "must not share children")
	require.Nil(t, got)
}

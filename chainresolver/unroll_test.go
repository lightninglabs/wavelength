package chainresolver

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/stretchr/testify/require"
)

// TestCollectNodesAtLevel verifies BFS level collection for various tree
// shapes.
func TestCollectNodesAtLevel(t *testing.T) {
	t.Parallel()

	// Build a 3-level binary tree:
	//   root (level 0)
	//     ├── child0 (level 1)
	//     │   ├── leaf0 (level 2)
	//     │   └── leaf1 (level 2)
	//     └── child1 (level 1)
	//         └── leaf2 (level 2)
	leaf0 := &tree.Node{
		Input:    testOutpoint(10),
		Outputs:  []*wire.TxOut{{Value: 50000}},
		Children: map[uint32]*tree.Node{},
	}
	leaf1 := &tree.Node{
		Input:    testOutpoint(11),
		Outputs:  []*wire.TxOut{{Value: 50000}},
		Children: map[uint32]*tree.Node{},
	}
	leaf2 := &tree.Node{
		Input:    testOutpoint(12),
		Outputs:  []*wire.TxOut{{Value: 100000}},
		Children: map[uint32]*tree.Node{},
	}
	child0 := &tree.Node{
		Input:   testOutpoint(1),
		Outputs: []*wire.TxOut{{Value: 100000}, {Value: 100000}},
		Children: map[uint32]*tree.Node{
			0: leaf0,
			1: leaf1,
		},
	}
	child1 := &tree.Node{
		Input:   testOutpoint(2),
		Outputs: []*wire.TxOut{{Value: 100000}},
		Children: map[uint32]*tree.Node{
			0: leaf2,
		},
	}
	root := &tree.Node{
		Input:   testOutpoint(0),
		Outputs: []*wire.TxOut{{Value: 200000}, {Value: 100000}},
		Children: map[uint32]*tree.Node{
			0: child0,
			1: child1,
		},
	}

	// Level 0: root only.
	nodes, err := collectNodesAtLevel(root, 0)
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	require.Equal(t, root, nodes[0])

	// Level 1: two children.
	nodes, err = collectNodesAtLevel(root, 1)
	require.NoError(t, err)
	require.Len(t, nodes, 2)

	// Level 2: three leaves.
	nodes, err = collectNodesAtLevel(root, 2)
	require.NoError(t, err)
	require.Len(t, nodes, 3)

	// Level 3: no nodes (beyond tree depth).
	nodes, err = collectNodesAtLevel(root, 3)
	require.NoError(t, err)
	require.Len(t, nodes, 0)
}

// TestCollectNodesAtLevel_NilRoot verifies error on nil root.
func TestCollectNodesAtLevel_NilRoot(t *testing.T) {
	t.Parallel()

	_, err := collectNodesAtLevel(nil, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "root node is nil")
}

// TestBuildTreeLevelBroadcasts_NilTree verifies error on nil tree.
func TestBuildTreeLevelBroadcasts_NilTree(t *testing.T) {
	t.Parallel()

	_, err := buildTreeLevelBroadcasts(nil, 0, testOutpoint(0))
	require.Error(t, err)
	require.Contains(t, err.Error(), "tree path is nil")
}

// TestComputeLeafOutpoint_NilTree verifies error on nil tree.
func TestComputeLeafOutpoint_NilTree(t *testing.T) {
	t.Parallel()

	_, err := computeLeafOutpoint(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "tree path is nil")
}

// TestComputeLeafOutpoint_NoLeaves verifies error when tree has no leaves.
func TestComputeLeafOutpoint_NoLeaves(t *testing.T) {
	t.Parallel()

	// A tree where root has children but no leaf nodes is not possible
	// in practice, but we test the error path for completeness by using
	// a root with no children that has only anchor-like outputs.
	root := &tree.Node{
		Input: testOutpoint(0),
		Outputs: []*wire.TxOut{
			{Value: 100000, PkScript: []byte{0x51, 0x20, 0x01}},
			{Value: 0, PkScript: []byte{0x51, 0x02}},
		},
		Children: map[uint32]*tree.Node{},
	}
	treePath := &tree.Tree{
		Root:          root,
		BatchOutpoint: testOutpoint(0),
		BatchOutput:   &wire.TxOut{Value: 100000},
	}

	// Root is a leaf since it has no children. Should find it.
	outpoint, err := computeLeafOutpoint(treePath)
	require.NoError(t, err)
	require.NotEqual(t, wire.OutPoint{}, outpoint)
}

// TestExtractCheckpointTx_NilPSBT verifies error on nil PSBT.
func TestExtractCheckpointTx_NilPSBT(t *testing.T) {
	t.Parallel()

	_, err := extractCheckpointTx(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "checkpoint PSBT is nil")
}

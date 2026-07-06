package fraud

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/stretchr/testify/require"
)

// TestBuildWatchPlanIncludesTreeInputsAndLeafSource verifies passive watches
// cover both tree materialization and checkpoint spend of the source VTXO.
func TestBuildWatchPlanIncludesTreeInputsAndLeafSource(t *testing.T) {
	treePath, source := testBranchTree(t, 5)
	target := testInput(9)

	plan, err := BuildWatchPlan(testDescriptor(target, treePath))
	require.NoError(t, err)
	require.Equal(t, target, plan.TargetOutpoint)

	watched := make(map[wire.OutPoint]struct{}, len(plan.Watches))
	for _, watch := range plan.Watches {
		watched[watch.Outpoint] = struct{}{}
	}

	require.Contains(t, watched, treePath.Root.Input)
	require.Contains(t, watched, treePath.Root.Children[0].Input)
	require.Contains(t, watched, source)
}

// TestBuildWatchPlanIncludesEveryAncestry verifies multi-input OOR targets arm
// passive watches for every ancestry fragment.
func TestBuildWatchPlanIncludesEveryAncestry(t *testing.T) {
	treeOne, sourceOne := testLeafTree(t, 10)
	treeTwo, sourceTwo := testLeafTree(t, 20)
	target := testInput(30)

	plan, err := BuildWatchPlan(testDescriptor(target, treeOne, treeTwo))
	require.NoError(t, err)
	require.Equal(t, target, plan.TargetOutpoint)

	watched := make(map[wire.OutPoint]struct{}, len(plan.Watches))
	for _, watch := range plan.Watches {
		watched[watch.Outpoint] = struct{}{}
	}

	require.Contains(t, watched, treeOne.Root.Input)
	require.Contains(t, watched, sourceOne)
	require.Contains(t, watched, treeTwo.Root.Input)
	require.Contains(t, watched, sourceTwo)
}

// TestBuildWatchPlanRejectsMalformedAncestry verifies invalid persisted
// ancestry is surfaced instead of arming an incomplete passive watch set.
func TestBuildWatchPlanRejectsMalformedAncestry(t *testing.T) {
	treePath, _ := testLeafTree(t, 40)
	desc := testDescriptor(testInput(41), treePath)
	desc.Ancestry[0].TreePath.Root = nil

	_, err := BuildWatchPlan(desc)
	require.ErrorIs(t, err, ErrWatchUnavailable)
}

// testLeafTree creates a one-node tree and returns its non-anchor output.
func testLeafTree(t *testing.T, seed byte) (*tree.Tree, wire.OutPoint) {
	t.Helper()

	root := &tree.Node{
		Input: testInput(seed),
		Outputs: []*wire.TxOut{
			testOut(seed, 1),
		},
		Children: make(map[uint32]*tree.Node),
	}
	source, err := root.GetNonAnchorOutpoint()
	require.NoError(t, err)

	return &tree.Tree{
		Root:        root,
		BatchOutput: testOut(seed, 0),
	}, *source
}

// testBranchTree creates a two-node tree and returns the leaf output.
func testBranchTree(t *testing.T, seed byte) (*tree.Tree, wire.OutPoint) {
	t.Helper()

	root := &tree.Node{
		Input: testInput(seed),
		Outputs: []*wire.TxOut{
			testOut(seed, 1),
		},
		Children: make(map[uint32]*tree.Node),
	}
	rootTx, err := root.ToTx()
	require.NoError(t, err)

	leaf := &tree.Node{
		Input: wire.OutPoint{
			Hash:  rootTx.TxHash(),
			Index: 0,
		},
		Outputs: []*wire.TxOut{
			testOut(seed+1, 1),
		},
		Children: make(map[uint32]*tree.Node),
	}
	root.Children[0] = leaf

	source, err := leaf.GetNonAnchorOutpoint()
	require.NoError(t, err)

	return &tree.Tree{
		Root:        root,
		BatchOutput: testOut(seed, 0),
	}, *source
}

// testDescriptor creates the minimal descriptor shape watcher tests need.
func testDescriptor(target wire.OutPoint,
	trees ...*tree.Tree) *vtxo.Descriptor {

	ancestry := make([]vtxo.Ancestry, 0, len(trees))
	for i := range trees {
		ancestry = append(ancestry, vtxo.Ancestry{
			TreePath:       trees[i],
			CommitmentTxID: trees[i].Root.Input.Hash,
			InputIndices:   []uint32{uint32(i)},
			TreeDepth:      1,
		})
	}

	return &vtxo.Descriptor{
		Outpoint:      target,
		Ancestry:      ancestry,
		ChainDepth:    1,
		CreatedHeight: 7,
		Status:        vtxo.VTXOStatusLive,
	}
}

// testOut returns a deterministic output with a unique pkscript.
func testOut(seed byte, index uint32) *wire.TxOut {
	return &wire.TxOut{
		Value: int64(1000 + index),
		PkScript: []byte{
			0x51, seed, byte(index),
		},
	}
}

// testInput returns a deterministic outpoint for seed.
func testInput(seed byte) wire.OutPoint {
	var h chainhash.Hash
	h[0] = seed

	return wire.OutPoint{
		Hash:  h,
		Index: 0,
	}
}

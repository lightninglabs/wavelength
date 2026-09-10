package tree

import (
	"bytes"
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// assetTestLeaves returns leaves with the given asset amounts.
func assetTestLeaves(t *testing.T, amounts ...uint64) []LeafDescriptor {
	t.Helper()

	leaves := make([]LeafDescriptor, len(amounts))
	for i, amount := range amounts {
		_, owner := createTestKey(t)
		leaves[i] = LeafDescriptor{
			PkScript: []byte{
				0x51,
				byte(i),
			},
			Amount:      10_000,
			CoSignerKey: owner,
			AssetAmount: amount,
		}
	}

	return leaves
}

// TestBuildStructureAssetAggregation tests subtree amount calculation.
func TestBuildStructureAssetAggregation(t *testing.T) {
	t.Parallel()

	_, operatorKey := createTestKey(t)
	cfg := StructureConfig{OperatorKey: operatorKey, Radix: 2}

	t.Run("btc-only trees have no asset context", func(t *testing.T) {
		t.Parallel()

		structure, err := BuildStructure(
			assetTestLeaves(t, 0, 0, 0), cfg,
		)
		require.NoError(t, err)
		require.Nil(t, structure.AssetContext)
	})

	t.Run("subtree totals cover every node", func(t *testing.T) {
		t.Parallel()

		structure, err := BuildStructure(
			assetTestLeaves(t, 800, 200, 500), cfg,
		)
		require.NoError(t, err)
		require.NotNil(t, structure.AssetContext)

		assetCtx := structure.AssetContext
		require.EqualValues(
			t, 1_500, assetCtx.NodeAssetAmount(structure.Root),
		)

		// Every node's total must equal the sum of its children,
		// and each leaf must carry one of the input amounts.
		var leafTotal uint64
		for node := range structure.Root.NodesIter() {
			if len(node.Children) == 0 {
				leafTotal += assetCtx.NodeAssetAmount(node)
				continue
			}

			var childSum uint64
			for _, child := range node.Children {
				childSum += assetCtx.NodeAssetAmount(child)
			}
			require.Equal(
				t, childSum, assetCtx.NodeAssetAmount(node),
			)
		}
		require.EqualValues(t, 1_500, leafTotal)
	})

	t.Run("mixed asset and btc leaves", func(t *testing.T) {
		t.Parallel()

		structure, err := BuildStructure(
			assetTestLeaves(t, 300, 0), cfg,
		)
		require.NoError(t, err)
		require.NotNil(t, structure.AssetContext)
		require.EqualValues(
			t, 300,
			structure.AssetContext.NodeAssetAmount(structure.Root),
		)
	})

	t.Run("overflow rejected", func(t *testing.T) {
		t.Parallel()

		_, err := BuildStructure(
			assetTestLeaves(t, math.MaxUint64, 1), cfg,
		)
		require.ErrorContains(t, err, "overflow")
	})
}

// TestAssetTreeContextTweakLookup tests outpoint lookup and byte ownership.
func TestAssetTreeContextTweakLookup(t *testing.T) {
	t.Parallel()

	assetCtx := NewAssetTreeContext()
	input := wire.OutPoint{Index: 3}
	input.Hash[0] = 0xAB

	tweak := bytes.Repeat([]byte{0x07}, 32)
	assetCtx.SetSigningTweak(input, tweak)
	tweak[0] = 0xFF

	lookup := assetCtx.tweakLookup()
	got := lookup(&Node{Input: input})
	require.Equal(t, bytes.Repeat([]byte{0x07}, 32), got)
	got[0] = 0xEE
	require.Equal(t, byte(0x07), lookup(&Node{Input: input})[0])
	require.Nil(t, lookup(&Node{Input: wire.OutPoint{Index: 9}}))

	pkg := []byte{0x01, 0x02}
	assetCtx.SetSealedPackage(input, pkg)
	pkg[0] = 0xEE
	gotPkg := assetCtx.SealedPackage(input)
	require.Equal(t, []byte{0x01, 0x02}, gotPkg)
	gotPkg[0] = 0xDD
	require.Equal(t, byte(0x01), assetCtx.SealedPackage(input)[0])

	root := []byte{0x03, 0x04}
	assetCtx.SetLeafAssetRoot(input, root)
	root[0] = 0xCC
	gotRoot := assetCtx.LeafAssetRoot(input)
	require.Equal(t, []byte{0x03, 0x04}, gotRoot)
	gotRoot[0] = 0xBB
	require.Equal(t, byte(0x03), assetCtx.LeafAssetRoot(input)[0])
	require.False(t, assetCtx.IsEmpty())
}

// TestAssetTreeContextEmpty tests empty context detection.
func TestAssetTreeContextEmpty(t *testing.T) {
	t.Parallel()

	var nilContext *AssetTreeContext
	require.True(t, nilContext.IsEmpty())

	assetCtx := NewAssetTreeContext()
	require.True(t, assetCtx.IsEmpty())

	assetCtx.SetLeafAssetRoot(wire.OutPoint{Index: 1}, []byte{0x01})
	require.False(t, assetCtx.IsEmpty())
}

// TestAssetTreeContextAmountsByIdentity tests node identity keying.
func TestAssetTreeContextAmountsByIdentity(t *testing.T) {
	t.Parallel()

	assetCtx := NewAssetTreeContext()
	_, key := createTestKey(t)
	nodeA := &Node{CoSigners: []*btcec.PublicKey{key}}
	nodeB := &Node{CoSigners: []*btcec.PublicKey{key}}

	assetCtx.SetNodeAssetAmount(nodeA, 42)
	require.EqualValues(t, 42, assetCtx.NodeAssetAmount(nodeA))
	require.Zero(t, assetCtx.NodeAssetAmount(nodeB))
}

// TestAssetTreeContextAmountsAfterExtraction tests amount lookup on cloned
// path nodes.
func TestAssetTreeContextAmountsAfterExtraction(t *testing.T) {
	t.Parallel()

	_, key := createTestKey(t)
	rootInput := wire.OutPoint{Index: 1}
	rootInput.Hash[0] = 0x01
	leafInput := wire.OutPoint{Index: 2}
	leafInput.Hash[0] = 0x02

	leaf := &Node{
		Input: leafInput,
		CoSigners: []*btcec.PublicKey{
			key,
		},
		Children: make(map[uint32]*Node),
	}
	root := &Node{
		Input: rootInput,
		CoSigners: []*btcec.PublicKey{
			key,
		},
		Children: map[uint32]*Node{
			0: leaf,
		},
	}
	assetCtx := NewAssetTreeContext()
	assetCtx.SetNodeAssetAmount(root, 42)
	assetCtx.SetNodeAssetAmount(leaf, 42)

	extracted, err := (&Tree{
		Root:         root,
		AssetContext: assetCtx,
	}).ExtractPathForCoSigners(key)
	require.NoError(t, err)
	require.NotSame(t, root, extracted.Root)
	require.NotSame(t, assetCtx, extracted.AssetContext)
	require.EqualValues(
		t, 42, extracted.AssetContext.NodeAssetAmount(extracted.Root),
	)

	extractedLeaf := extracted.Root.Children[0]
	require.NotSame(t, leaf, extractedLeaf)
	require.EqualValues(
		t, 42, extracted.AssetContext.NodeAssetAmount(extractedLeaf),
	)

	extracted.AssetContext.SetSealedPackage(rootInput, []byte{1})
	require.Empty(t, assetCtx.SealedPackage(rootInput))
}

// TestAssetTreeContextValidate tests asset tree context validation.
func TestAssetTreeContextValidate(t *testing.T) {
	t.Parallel()

	newContext := func() (*AssetTreeContext, *Node, *Node) {
		rootInput := wire.OutPoint{Index: 1}
		rootInput.Hash[0] = 0x01
		leafInput := wire.OutPoint{Index: 2}
		leafInput.Hash[0] = 0x02

		leaf := &Node{
			Input:    leafInput,
			Children: make(map[uint32]*Node),
		}
		root := &Node{
			Input: rootInput,
			Children: map[uint32]*Node{
				0: leaf,
			},
		}

		ctx := NewAssetTreeContext()
		ctx.SetAssetRef("asset")
		ctx.SetSigningTweak(rootInput, bytes.Repeat([]byte{1}, 32))
		ctx.SetSigningTweak(leafInput, bytes.Repeat([]byte{2}, 32))
		ctx.SetNodeAssetAmount(root, 10)
		ctx.SetNodeAssetAmount(leaf, 10)
		ctx.SetLeafAssetRoot(leafInput, bytes.Repeat([]byte{3}, 32))

		return ctx, root, leaf
	}

	t.Run("valid", func(t *testing.T) {
		ctx, root, _ := newContext()
		require.NoError(t, ctx.Validate(root))
	})

	t.Run("duplicate input", func(t *testing.T) {
		ctx, root, leaf := newContext()
		leaf.Input = root.Input
		require.ErrorContains(t, ctx.Validate(root), "multiple nodes")
	})

	t.Run("child exceeds parent", func(t *testing.T) {
		ctx, root, leaf := newContext()
		ctx.SetNodeAssetAmount(leaf, 11)
		require.ErrorContains(t, ctx.Validate(root), "exceeds")
	})
}

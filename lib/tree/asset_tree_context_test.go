package tree

import (
	"bytes"
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// assetTestLeaves returns n leaf descriptors with distinct owners carrying
// the given asset amounts.
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

// TestBuildStructureAssetAggregation ensures the structure pass folds
// per-leaf asset amounts into subtree totals on the asset context, and that
// BTC-only trees keep a nil context so nothing changes for them.
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

// TestAssetTreeContextTweakLookup ensures the context's lookup feeds signer
// sessions by input outpoint and defensively copies stored bytes.
func TestAssetTreeContextTweakLookup(t *testing.T) {
	t.Parallel()

	assetCtx := NewAssetTreeContext()
	input := wire.OutPoint{Index: 3}
	input.Hash[0] = 0xAB

	tweak := bytes.Repeat([]byte{0x07}, 32)
	assetCtx.SetSigningTweak(input, tweak)
	tweak[0] = 0xFF

	lookup := assetCtx.TweakLookup()
	got := lookup(&Node{Input: input})
	require.Equal(t, bytes.Repeat([]byte{0x07}, 32), got)
	require.Nil(t, lookup(&Node{Input: wire.OutPoint{Index: 9}}))

	pkg := []byte{0x01, 0x02}
	assetCtx.SetSealedPackage(input, pkg)
	pkg[0] = 0xEE
	require.Equal(
		t, []byte{0x01, 0x02}, assetCtx.SealedPackage(input),
	)
}

// TestAssetTreeContextAmountsByIdentity ensures build-phase amounts are
// keyed by node identity, not by content.
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

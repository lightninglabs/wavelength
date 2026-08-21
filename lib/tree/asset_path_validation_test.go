package tree

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// assetPathFixture builds a two-leaf tree with an attached asset context,
// the shape an asset round hands a client, and returns the owner key of
// the first leaf.
func assetPathFixture(t *testing.T) (*Tree, *btcec.PublicKey,
	*btcec.PublicKey) {

	t.Helper()

	_, operatorKey := createTestKey(t)
	_, owner1 := createTestKey(t)
	_, owner2 := createTestKey(t)

	// Composed asset leaf outputs are P2TR; any taproot script stands in
	// for the composed key here since composition is proven at spend
	// time, not path-validation time.
	script1, err := txscript.PayToTaprootScript(owner1)
	require.NoError(t, err)
	script2, err := txscript.PayToTaprootScript(owner2)
	require.NoError(t, err)

	leaves := []LeafDescriptor{
		{
			PkScript:    script1,
			Amount:      1_000,
			CoSignerKey: owner1,
		},
		{
			PkScript:    script2,
			Amount:      2_000,
			CoSignerKey: owner2,
		},
	}

	built, err := NewTree(
		wire.OutPoint{
			Hash: chainhash.HashH([]byte("asset_batch")),
		}, &wire.TxOut{
			Value: 3_000,
		},
		leaves,
		operatorKey,
		make([]byte, 32),
		2,
	)
	require.NoError(t, err)

	assetCtx := NewAssetTreeContext()
	assetCtx.SetAssetRef("group:deadbeef")
	for _, leaf := range built.Root.GetLeafNodes() {
		amount := uint64(200)
		if ContainsCosigner(leaf.CoSigners, owner1) {
			amount = 100
		}
		assetCtx.SetNodeAssetAmount(leaf, amount)
	}
	built.AssetContext = assetCtx

	return built, owner1, operatorKey
}

// TestValidatePathForAsset covers the accept path and each rejected
// corruption of an asset VTXO path.
func TestValidatePathForAsset(t *testing.T) {
	t.Parallel()

	expectedLeaf := func(owner *btcec.PublicKey) LeafDescriptor {
		return LeafDescriptor{Amount: 1_000, CoSignerKey: owner}
	}

	t.Run("valid asset path", func(t *testing.T) {
		t.Parallel()

		built, owner, operator := assetPathFixture(t)
		clientTree, err := built.ValidatePathForAsset(
			owner, expectedLeaf(owner), operator, "group:deadbeef",
			100,
		)
		require.NoError(t, err)
		require.NotNil(t, clientTree)
		require.NotNil(t, clientTree.AssetContext)
	})

	t.Run("missing asset context", func(t *testing.T) {
		t.Parallel()

		built, owner, operator := assetPathFixture(t)
		built.AssetContext = nil
		_, err := built.ValidatePathForAsset(
			owner, expectedLeaf(owner), operator, "group:deadbeef",
			100,
		)
		require.ErrorContains(t, err, "no asset context")
	})

	t.Run("asset ref mismatch", func(t *testing.T) {
		t.Parallel()

		built, owner, operator := assetPathFixture(t)
		_, err := built.ValidatePathForAsset(
			owner, expectedLeaf(owner), operator, "group:feedface",
			100,
		)
		require.ErrorContains(t, err, "requested asset")
	})

	t.Run("leaf asset amount mismatch", func(t *testing.T) {
		t.Parallel()

		built, owner, operator := assetPathFixture(t)
		_, err := built.ValidatePathForAsset(
			owner, expectedLeaf(owner), operator, "group:deadbeef",
			250,
		)
		require.ErrorContains(t, err, "leaf asset amount")
	})

	t.Run("bitcoin amount mismatch", func(t *testing.T) {
		t.Parallel()

		built, owner, operator := assetPathFixture(t)
		wrongLeaf := LeafDescriptor{Amount: 999, CoSignerKey: owner}
		_, err := built.ValidatePathForAsset(
			owner, wrongLeaf, operator, "group:deadbeef", 100,
		)
		require.ErrorContains(t, err, "VTXO output value")
	})

	t.Run("non-taproot leaf output", func(t *testing.T) {
		t.Parallel()

		built, owner, operator := assetPathFixture(t)
		for _, leaf := range built.Root.GetLeafNodes() {
			leaf.Outputs[0].PkScript = []byte("not_p2tr")
		}
		_, err := built.ValidatePathForAsset(
			owner, expectedLeaf(owner), operator, "group:deadbeef",
			100,
		)
		require.ErrorContains(t, err, "not P2TR")
	})

	t.Run("missing operator cosigner", func(t *testing.T) {
		t.Parallel()

		built, owner, _ := assetPathFixture(t)
		_, foreign := createTestKey(t)
		_, err := built.ValidatePathForAsset(
			owner, expectedLeaf(owner), foreign, "group:deadbeef",
			100,
		)
		require.ErrorContains(t, err, "operator key")
	})
}

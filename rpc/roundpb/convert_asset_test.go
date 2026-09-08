package roundpb

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/stretchr/testify/require"
)

// newAssetTestTree returns a materialized two-node asset tree.
func newAssetTestTree(t *testing.T) *tree.Tree {
	t.Helper()

	rootKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	leafKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	rootInput := wire.OutPoint{
		Hash: chainhash.HashH([]byte("batch")),
	}
	leafInput := wire.OutPoint{
		Hash: chainhash.HashH([]byte("root-tx")),
	}
	leaf := &tree.Node{
		Input: leafInput,
		Outputs: []*wire.TxOut{{
			Value: 1_000,
			PkScript: []byte{
				0x51,
			},
		}},
		CoSigners: []*btcec.PublicKey{
			leafKey.PubKey(), rootKey.PubKey(),
		},
		Children: make(map[uint32]*tree.Node),
	}
	root := &tree.Node{
		Input: rootInput,
		Outputs: []*wire.TxOut{{
			Value: 1_000,
			PkScript: []byte{
				0x52,
			},
		}},
		CoSigners: []*btcec.PublicKey{
			leafKey.PubKey(), rootKey.PubKey(),
		},
		Children: map[uint32]*tree.Node{
			0: leaf,
		},
	}

	assetCtx := tree.NewAssetTreeContext()
	assetCtx.SetAssetRef(
		tapsdk.AssetRefFromAssetID(tapsdk.AssetID{0xaa}).String(),
	)
	assetCtx.SetSigningTweak(rootInput, assetTestHash("root-tweak"))
	assetCtx.SetSigningTweak(leafInput, assetTestHash("leaf-tweak"))
	assetCtx.SetNodeAssetAmount(root, 500)
	assetCtx.SetNodeAssetAmount(leaf, 500)
	assetCtx.SetLeafAssetRoot(leafInput, assetTestHash("leaf-root"))

	return &tree.Tree{
		Root:          root,
		BatchOutpoint: rootInput,
		BatchOutput: &wire.TxOut{
			Value: 1_000,
			PkScript: []byte{
				0x52,
			},
		},
		SweepTapscriptRoot: assetTestHash("sweep"),
		AssetContext:       assetCtx,
	}
}

// TestTreeProtoAssetRoundTrip tests asset tree wire encoding.
func TestTreeProtoAssetRoundTrip(t *testing.T) {
	t.Parallel()

	original := newAssetTestTree(t)
	encoded, err := TreeToProto(original)
	require.NoError(t, err)
	require.Equal(t, original.AssetContext.AssetRef(), encoded.AssetRef)
	require.Len(t, encoded.Nodes, 2)
	require.Empty(t, encoded.Nodes[0].AssetCommitmentRoot)
	require.NotEmpty(t, encoded.Nodes[1].AssetCommitmentRoot)

	decoded, err := TreeFromProto(encoded)
	require.NoError(t, err)
	require.NotNil(t, decoded.AssetContext)
	require.Equal(
		t, original.AssetContext.AssetRef(),
		decoded.AssetContext.AssetRef(),
	)

	decodedLeaf := decoded.Root.Children[0]
	for _, node := range []*tree.Node{decoded.Root, decodedLeaf} {
		require.EqualValues(
			t, 500, decoded.AssetContext.NodeAssetAmount(node),
		)

		finalKey, keyErr := tree.ComputeFinalKey(
			append(
				[]*btcec.PublicKey(nil), node.CoSigners...,
			),
			decoded.AssetContext.SigningTweak(node.Input),
		)
		require.NoError(t, keyErr)
		require.True(t, finalKey.IsEqual(node.FinalKey))
	}
	require.Equal(
		t, original.AssetContext.LeafAssetRoot(
			original.Root.Children[0].Input,
		),
		decoded.AssetContext.LeafAssetRoot(decodedLeaf.Input),
	)
}

// TestTreeProtoBitcoinRoundTrip tests that Bitcoin trees remain asset-free.
func TestTreeProtoBitcoinRoundTrip(t *testing.T) {
	t.Parallel()

	original := newAssetTestTree(t)
	original.AssetContext = nil

	encoded, err := TreeToProto(original)
	require.NoError(t, err)
	require.Empty(t, encoded.AssetRef)
	for _, node := range encoded.Nodes {
		require.Empty(t, node.SigningTweak)
		require.Zero(t, node.AssetAmount)
		require.Empty(t, node.AssetCommitmentRoot)
	}

	decoded, err := TreeFromProto(encoded)
	require.NoError(t, err)
	require.Nil(t, decoded.AssetContext)
}

// TestTreeFromProtoRejectsIncompleteAssetData tests asset field validation.
func TestTreeFromProtoRejectsIncompleteAssetData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*VTXOTree)
		wantErr string
	}{
		{
			name: "missing asset ref",
			mutate: func(encoded *VTXOTree) {
				encoded.AssetRef = ""
			},
			wantErr: "require asset_ref",
		},
		{
			name: "invalid asset ref",
			mutate: func(encoded *VTXOTree) {
				encoded.AssetRef = "invalid"
			},
			wantErr: "asset_ref",
		},
		{
			name: "missing signing tweak",
			mutate: func(encoded *VTXOTree) {
				encoded.Nodes[0].SigningTweak = nil
			},
			wantErr: "signing tweak",
		},
		{
			name: "missing asset amount",
			mutate: func(encoded *VTXOTree) {
				encoded.Nodes[1].AssetAmount = 0
			},
			wantErr: "asset amount",
		},
		{
			name: "missing leaf root",
			mutate: func(encoded *VTXOTree) {
				encoded.Nodes[1].AssetCommitmentRoot = nil
			},
			wantErr: "asset commitment root",
		},
		{
			name: "branch leaf root",
			mutate: func(encoded *VTXOTree) {
				encoded.Nodes[0].AssetCommitmentRoot =
					assetTestHash("unexpected")
			},
			wantErr: "branch",
		},
		{
			name: "child exceeds parent",
			mutate: func(encoded *VTXOTree) {
				encoded.Nodes[1].AssetAmount = 501
			},
			wantErr: "exceeds parent",
		},
		{
			name: "duplicate input",
			mutate: func(encoded *VTXOTree) {
				encoded.Nodes[1].Input = encoded.Nodes[0].Input
			},
			wantErr: "multiple nodes",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			encoded, err := TreeToProto(newAssetTestTree(t))
			require.NoError(t, err)
			test.mutate(encoded)

			_, err = TreeFromProto(encoded)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

// assetTestHash returns a deterministic 32-byte value.
func assetTestHash(value string) []byte {
	hash := chainhash.HashH([]byte(value))

	return hash[:]
}

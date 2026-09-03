package db

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/stretchr/testify/require"
)

// newAssetCodecTree returns an asset tree with persistent node metadata.
func newAssetCodecTree(t *testing.T) *tree.Tree {
	t.Helper()

	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	rootInput := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("batch")),
		Index: 1,
	}
	leafInput := wire.OutPoint{
		Hash: chainhash.HashH([]byte("root-tx")),
	}
	leaf := &tree.Node{
		Input: leafInput,
		Outputs: []*wire.TxOut{{
			Value: 2_000,
			PkScript: []byte{
				0x51,
			},
		}},
		CoSigners: []*btcec.PublicKey{
			key.PubKey(),
		},
		Children: make(map[uint32]*tree.Node),
	}
	root := &tree.Node{
		Input: rootInput,
		Outputs: []*wire.TxOut{{
			Value: 2_000,
			PkScript: []byte{
				0x52,
			},
		}},
		CoSigners: []*btcec.PublicKey{
			key.PubKey(),
		},
		Children: map[uint32]*tree.Node{
			0: leaf,
		},
	}

	assetCtx := tree.NewAssetTreeContext()
	assetCtx.SetAssetRef(
		tapsdk.AssetRefFromAssetID(tapsdk.AssetID{0xbb}).String(),
	)
	assetCtx.SetSigningTweak(rootInput, assetCodecHash("root-tweak"))
	assetCtx.SetSigningTweak(leafInput, assetCodecHash("leaf-tweak"))
	assetCtx.SetSealedPackage(rootInput, []byte{0xaa, 0xbb})
	assetCtx.SetSealedPackage(leafInput, []byte{0xcc})
	assetCtx.SetNodeAssetAmount(root, 700)
	assetCtx.SetNodeAssetAmount(leaf, 700)
	assetCtx.SetLeafAssetRoot(leafInput, assetCodecHash("leaf-root"))

	return &tree.Tree{
		Root:          root,
		BatchOutpoint: rootInput,
		BatchOutput: &wire.TxOut{
			Value: 2_000,
			PkScript: []byte{
				0x52,
			},
		},
		SweepTapscriptRoot: assetCodecHash("sweep"),
		AssetContext:       assetCtx,
	}
}

// TestTreeCodecAssetRoundTrip tests asset tree persistence.
func TestTreeCodecAssetRoundTrip(t *testing.T) {
	t.Parallel()

	original := newAssetCodecTree(t)
	encoded, err := SerializeTree(original)
	require.NoError(t, err)

	decoded, err := DeserializeTree(encoded)
	require.NoError(t, err)
	require.NotNil(t, decoded.AssetContext)
	require.Equal(
		t, original.AssetContext.AssetRef(),
		decoded.AssetContext.AssetRef(),
	)

	originalLeaf := original.Root.Children[0]
	decodedLeaf := decoded.Root.Children[0]
	for _, pair := range [][2]*tree.Node{
		{
			original.Root,
			decoded.Root,
		},
		{
			originalLeaf,
			decodedLeaf,
		},
	} {
		require.Equal(
			t, original.AssetContext.SigningTweak(pair[0].Input),
			decoded.AssetContext.SigningTweak(pair[1].Input),
		)
		require.Equal(
			t, original.AssetContext.SealedPackage(pair[0].Input),
			decoded.AssetContext.SealedPackage(pair[1].Input),
		)
		require.Equal(
			t, original.AssetContext.NodeAssetAmount(pair[0]),
			decoded.AssetContext.NodeAssetAmount(pair[1]),
		)
	}
	require.Equal(
		t, original.AssetContext.LeafAssetRoot(originalLeaf.Input),
		decoded.AssetContext.LeafAssetRoot(decodedLeaf.Input),
	)
}

// TestTreeCodecBitcoinEncodingHasNoAssetContext tests optional records.
func TestTreeCodecBitcoinEncodingHasNoAssetContext(t *testing.T) {
	t.Parallel()

	original := newAssetCodecTree(t)
	original.AssetContext = nil

	encoded, err := SerializeTree(original)
	require.NoError(t, err)
	decoded, err := DeserializeTree(encoded)
	require.NoError(t, err)
	require.Nil(t, decoded.AssetContext)
}

// assetCodecHash returns a deterministic 32-byte value.
func assetCodecHash(value string) []byte {
	hash := chainhash.HashH([]byte(value))

	return hash[:]
}

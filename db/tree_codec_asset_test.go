package db

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/stretchr/testify/require"
)

// newAssetCodecTree builds a two-node asset-aware tree whose context
// carries tweaks, sealed packages, and amounts, and whose nodes use the
// flow-V2 sequence.
func newAssetCodecTree(t *testing.T) *tree.Tree {
	t.Helper()

	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	rootInput := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("batch")),
		Index: 1,
	}
	leafInput := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("root-tx")),
		Index: 0,
	}

	leaf := &tree.Node{
		Input: leafInput,
		Outputs: []*wire.TxOut{
			{
				Value: 2_000,
				PkScript: []byte{
					0x51,
				},
			},
		},
		CoSigners: []*btcec.PublicKey{
			key.PubKey(),
		},
		Children: make(map[uint32]*tree.Node),
		Sequence: tree.SequenceV2,
	}
	root := &tree.Node{
		Input: rootInput,
		Outputs: []*wire.TxOut{
			{
				Value: 2_000,
				PkScript: []byte{
					0x52,
				},
			},
		},
		CoSigners: []*btcec.PublicKey{
			key.PubKey(),
		},
		Children: map[uint32]*tree.Node{
			0: leaf,
		},
		Sequence: tree.SequenceV2,
	}

	ctx := tree.NewAssetTreeContext()
	ctx.SetAssetRef("group:cafe")
	ctx.SetSigningTweak(rootInput, hashOf("t1"))
	ctx.SetSigningTweak(leafInput, hashOf("t2"))
	ctx.SetSealedPackage(rootInput, []byte{0xaa, 0xbb})
	ctx.SetSealedPackage(leafInput, []byte{0xcc})
	ctx.SetNodeAssetAmount(root, 700)
	ctx.SetNodeAssetAmount(leaf, 700)

	return &tree.Tree{
		Root:          root,
		BatchOutpoint: rootInput,
		BatchOutput: &wire.TxOut{
			Value: 2_000,
			PkScript: []byte{
				0x52,
			},
		},
		SweepTapscriptRoot: hashOf("sweep"),
		AssetContext:       ctx,
	}
}

// TestTreeCodecAssetRoundTrip proves an asset-aware tree survives TLV
// persistence: asset ref, per-node tweaks, sealed packages, subtree
// amounts, and the tree-wide node sequence all round-trip.
func TestTreeCodecAssetRoundTrip(t *testing.T) {
	t.Parallel()

	original := newAssetCodecTree(t)
	ctx := original.AssetContext

	data, err := SerializeTree(original)
	require.NoError(t, err)

	got, err := DeserializeTree(data)
	require.NoError(t, err)
	require.NotNil(t, got.AssetContext)
	require.Equal(t, "group:cafe", got.AssetContext.AssetRef())

	gotLeaf := got.Root.Children[0]
	for _, input := range []wire.OutPoint{
		got.Root.Input, gotLeaf.Input,
	} {
		require.Equal(
			t, ctx.SigningTweak(input),
			got.AssetContext.SigningTweak(input),
		)
		require.Equal(
			t, ctx.SealedPackage(input),
			got.AssetContext.SealedPackage(input),
		)
	}
	require.EqualValues(
		t, 700, got.AssetContext.NodeAssetAmount(got.Root),
	)
	require.EqualValues(
		t, 700, got.AssetContext.NodeAssetAmount(gotLeaf),
	)

	// The tree-wide sequence is stamped back onto every node, so node
	// txids are preserved across persistence.
	require.Equal(t, tree.SequenceV2, got.Root.Sequence)
	require.Equal(t, tree.SequenceV2, gotLeaf.Sequence)

	wantTxID, err := original.Root.TXID()
	require.NoError(t, err)
	gotTxID, err := got.Root.TXID()
	require.NoError(t, err)
	require.Equal(t, wantTxID, gotTxID)
}

// TestTreeCodecBitcoinOnlyNoAssetContext proves Bitcoin-only trees do not
// grow an asset context through persistence.
func TestTreeCodecBitcoinOnlyNoAssetContext(t *testing.T) {
	t.Parallel()

	original := newAssetCodecTree(t)
	original.AssetContext = nil
	original.Root.Sequence = 0
	original.Root.Children[0].Sequence = 0

	data, err := SerializeTree(original)
	require.NoError(t, err)

	got, err := DeserializeTree(data)
	require.NoError(t, err)
	require.Nil(t, got.AssetContext)
	require.Zero(t, got.Root.Sequence)
}

// hashOf returns a deterministic 32-byte slice for test fixtures.
func hashOf(s string) []byte {
	h := chainhash.HashH([]byte(s))

	return h[:]
}

package roundpb

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/stretchr/testify/require"
)

// newAssetTestTree builds a two-node asset-aware tree with a populated
// context: distinct signing tweaks per node input and subtree amounts.
func newAssetTestTree(t *testing.T) *tree.Tree {
	t.Helper()

	rootKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	leafKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	rootInput := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("batch")),
		Index: 0,
	}
	leafInput := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("root-tx")),
		Index: 0,
	}

	leaf := &tree.Node{
		Input: leafInput,
		Outputs: []*wire.TxOut{
			{
				Value: 1_000,
				PkScript: []byte{
					0x51,
				},
			},
			{
				Value: 0,
				PkScript: []byte{
					0x51,
					0x02,
					0x4e,
					0x73,
				},
			},
		},
		CoSigners: []*btcec.PublicKey{
			leafKey.PubKey(), rootKey.PubKey(),
		},
		Children: make(map[uint32]*tree.Node),
	}
	root := &tree.Node{
		Input: rootInput,
		Outputs: []*wire.TxOut{
			{
				Value: 1_000,
				PkScript: []byte{
					0x52,
				},
			},
			{
				Value: 0,
				PkScript: []byte{
					0x51,
					0x02,
					0x4e,
					0x73,
				},
			},
		},
		CoSigners: []*btcec.PublicKey{
			leafKey.PubKey(), rootKey.PubKey(),
		},
		Children: map[uint32]*tree.Node{
			0: leaf,
		},
	}

	ctx := tree.NewAssetTreeContext()
	ctx.SetAssetRef("group:deadbeef")
	ctx.SetSigningTweak(rootInput, hashOf("t1"))
	ctx.SetSigningTweak(leafInput, hashOf("t2"))
	ctx.SetNodeAssetAmount(root, 500)
	ctx.SetNodeAssetAmount(leaf, 500)

	return &tree.Tree{
		Root:          root,
		BatchOutpoint: rootInput,
		BatchOutput: &wire.TxOut{
			Value: 1_000,
			PkScript: []byte{
				0x52,
			},
		},
		SweepTapscriptRoot: hashOf("sweep"),
		AssetContext:       ctx,
	}
}

// TestTreeProtoAssetRoundTrip proves an asset-aware tree carries its
// context over the wire: asset ref, per-node signing tweaks and amounts
// survive the round trip, and node keys are recomputed with the per-node
// tweak rather than the tree-wide sweep root.
func TestTreeProtoAssetRoundTrip(t *testing.T) {
	t.Parallel()

	original := newAssetTestTree(t)
	ctx := original.AssetContext

	pb, err := TreeToProto(original)
	require.NoError(t, err)
	require.Equal(t, "group:deadbeef", pb.AssetRef)
	require.Len(t, pb.Nodes, 2)
	for _, pn := range pb.Nodes {
		require.NotEmpty(t, pn.SigningTweak)
		require.EqualValues(t, 500, pn.AssetAmount)
	}

	got, err := TreeFromProto(pb)
	require.NoError(t, err)
	require.NotNil(t, got.AssetContext)
	require.Equal(t, "group:deadbeef", got.AssetContext.AssetRef())

	require.Equal(
		t, ctx.SigningTweak(original.Root.Input),
		got.AssetContext.SigningTweak(got.Root.Input),
	)
	gotLeaf := got.Root.Children[0]
	require.Equal(
		t, ctx.SigningTweak(original.Root.Children[0].Input),
		got.AssetContext.SigningTweak(gotLeaf.Input),
	)
	require.EqualValues(
		t, 500, got.AssetContext.NodeAssetAmount(got.Root),
	)
	require.EqualValues(
		t, 500, got.AssetContext.NodeAssetAmount(gotLeaf),
	)

	// The recomputed key must be the composed one: cosigners tweaked
	// with the node's own tweak, not the sweep root.
	wantKey, err := tree.ComputeFinalKey(
		append(
			[]*btcec.PublicKey(nil), got.Root.CoSigners...,
		),
		got.AssetContext.SigningTweak(got.Root.Input),
	)
	require.NoError(t, err)
	require.True(t, wantKey.IsEqual(got.Root.FinalKey))
}

// TestTreeProtoBitcoinOnlyNoAssetContext proves Bitcoin-only trees stay
// asset-free across the round trip.
func TestTreeProtoBitcoinOnlyNoAssetContext(t *testing.T) {
	t.Parallel()

	original := newAssetTestTree(t)
	original.AssetContext = nil

	pb, err := TreeToProto(original)
	require.NoError(t, err)
	require.Empty(t, pb.AssetRef)
	for _, pn := range pb.Nodes {
		require.Empty(t, pn.SigningTweak)
		require.Zero(t, pn.AssetAmount)
	}

	got, err := TreeFromProto(pb)
	require.NoError(t, err)
	require.Nil(t, got.AssetContext)
}

// TestTreeFromProtoNodeSequence proves the flow-version-derived sequence
// stamps every node, and its absence keeps the V1 default.
func TestTreeFromProtoNodeSequence(t *testing.T) {
	t.Parallel()

	pb, err := TreeToProto(newAssetTestTree(t))
	require.NoError(t, err)

	v1, err := TreeFromProto(pb)
	require.NoError(t, err)
	v2, err := TreeFromProto(pb, WithNodeSequence(tree.SequenceV2))
	require.NoError(t, err)

	v1Tx, err := v1.Root.ToTx()
	require.NoError(t, err)
	v2Tx, err := v2.Root.ToTx()
	require.NoError(t, err)

	require.Equal(t, wire.MaxTxInSequenceNum, v1Tx.TxIn[0].Sequence)
	require.Equal(t, tree.SequenceV2, v2Tx.TxIn[0].Sequence)

	// The sequence is consensus-visible: the txid must change.
	require.NotEqual(t, v1Tx.TxHash(), v2Tx.TxHash())

	// Every node is stamped, not only the root.
	require.Equal(
		t, tree.SequenceV2, v2.Root.Children[0].Sequence,
	)
}

// hashOf returns a deterministic 32-byte slice for test fixtures.
func hashOf(s string) []byte {
	h := chainhash.HashH([]byte(s))

	return h[:]
}

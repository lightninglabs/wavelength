package tree

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// TestMetadataDoesNotAffectVerification ensures that attaching asset metadata
// to leaves does not change structural verification of the tree.
func TestMetadataDoesNotAffectVerification(t *testing.T) {
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	leaf := LeafDescriptor{
		PkScript:    []byte{0x51},
		Amount:      1000,
		CoSignerKey: priv.PubKey(),
		Asset: &AssetMetadata{
			InputProof: []byte{0x01, 0x02, 0x03},
			Labels: map[string]string{
				"client": "alice",
			},
		},
	}

	rootOut := wire.OutPoint{}
	rootOutput := &wire.TxOut{Value: 1000, PkScript: []byte{0x51}}

	tree, err := NewTree(
		rootOut, rootOutput, []LeafDescriptor{leaf}, operatorPriv.PubKey(),
		nil, /* sweepTapscriptRoot */
		2,   /* radix */
	)
	require.NoError(t, err)
	require.NotNil(t, tree)

	// Metadata should have been attached to the leaf (root is a leaf in
	// the single-leaf case).
	require.NotNil(t, tree.Root.Metadata)
	require.Equal(t, []byte{0x01, 0x02, 0x03}, tree.Root.Metadata.AssetProof)

	// Structural verification should still succeed.
	require.NoError(t, tree.Verify())
}

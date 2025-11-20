package tree

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

func TestFundingModeString(t *testing.T) {
	require.Equal(t, "unknown", FundingModeUnknown.String())
	require.Equal(t, "operator", FundingModeOperatorProvided.String())
	require.Equal(t, "client", FundingModeClientGas.String())
	require.Equal(t, "unknown", FundingMode(42).String())
}

// newTestKey returns a fresh pubkey for tests.
func newTestKey(t *testing.T) *btcec.PublicKey {
	t.Helper()
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	return priv.PubKey()
}

func TestNewLeafNodeMetadata(t *testing.T) {
	operatorKey := newTestKey(t)
	coSigner := newTestKey(t)

	input := wire.OutPoint{
		Hash:  chainhash.Hash{},
		Index: 0,
	}

	leafDesc := LeafDescriptor{
		PkScript:    []byte{0x51},
		Amount:      1000,
		CoSignerKey: coSigner,
		Asset:       nil,
	}

	leaf, err := NewLeafNode(input, leafDesc, operatorKey, nil)
	require.NoError(t, err)
	require.NotNil(t, leaf.Metadata)
	require.Nil(t, leaf.Metadata.AssetProof)
	require.Nil(t, leaf.Metadata.Leaf)
}

func TestNewLeafNodeAssetMetadata(t *testing.T) {
	operatorKey := newTestKey(t)
	coSigner := newTestKey(t)

	origProof := []byte{0x01, 0x02, 0x03}
	assetMeta := &AssetMetadata{
		Funding: LeafFunding{
			Mode:   FundingModeClientGas,
			Amount: 123,
		},
		InputProof:     origProof,
		ChangePkScript: []byte{0xaa, 0xbb},
		ExitRebalance:  10,
		Labels: map[string]string{
			"client": "alice",
		},
	}

	leaf, err := NewLeafNode(
		wire.OutPoint{}, LeafDescriptor{
			PkScript:    []byte{0x51},
			Amount:      2000,
			CoSignerKey: coSigner,
			Asset:       assetMeta,
		}, operatorKey, nil,
	)
	require.NoError(t, err)

	require.NotNil(t, leaf.Metadata)
	require.NotNil(t, leaf.Metadata.AssetProof)
	require.Equal(t, origProof, leaf.Metadata.AssetProof)
	// Ensure the proof was copied, not referenced.
	origProof[0] = 0xff
	require.Equal(t, []byte{0x01, 0x02, 0x03}, leaf.Metadata.AssetProof)

	require.NotNil(t, leaf.Metadata.Leaf)
	require.Equal(t, assetMeta, leaf.Metadata.Leaf)
}

func TestNewBranchNodeMetadata(t *testing.T) {
	operatorKey := newTestKey(t)
	coSigner1 := newTestKey(t)
	coSigner2 := newTestKey(t)
	coSigner3 := newTestKey(t)

	groups := [][]LeafDescriptor{
		{
			{
				PkScript:    []byte{0x51},
				Amount:      1000,
				CoSignerKey: coSigner1,
				Asset: &AssetMetadata{
					Funding: LeafFunding{
						Mode:   FundingModeOperatorProvided,
						Amount: 100,
					},
				},
			},
			{
				PkScript:    []byte{0x51},
				Amount:      2000,
				CoSignerKey: coSigner2,
				Asset:       nil,
			},
		},
		{
			{
				PkScript:    []byte{0x51},
				Amount:      1500,
				CoSignerKey: coSigner3,
				Asset: &AssetMetadata{
					Funding: LeafFunding{
						Mode:   FundingModeClientGas,
						Amount: 50,
					},
				},
			},
		},
	}

	branch, err := NewBranchNode(
		wire.OutPoint{}, groups, operatorKey, nil,
	)
	require.NoError(t, err)

	require.NotNil(t, branch.Metadata)
	require.Nil(t, branch.Metadata.AssetProof)
	require.Nil(t, branch.Metadata.Leaf)
}

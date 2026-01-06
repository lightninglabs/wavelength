package tree

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

func TestNewAssetLeafDescriptorCopiesProofAndChange(t *testing.T) {
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	origProof := []byte{0x01, 0x02}
	origChange := []byte{0xaa, 0xbb}

	desc := NewAssetLeafDescriptor(
		[]byte{0x51}, 5000, priv.PubKey(),
		origProof, 777, 123, origChange,
	)

	require.NotNil(t, desc.Asset)
	require.Equal(t, origProof, desc.Asset.InputProof)
	require.Equal(t, origChange, desc.Asset.ChangePkScript)

	// Mutate originals and ensure the descriptor does not change.
	origProof[0] = 0xff
	origChange[0] = 0xcc

	require.Equal(t, []byte{0x01, 0x02}, desc.Asset.InputProof)
	require.Equal(t, []byte{0xaa, 0xbb}, desc.Asset.ChangePkScript)
}

func TestNewAssetLeafDescriptorFields(t *testing.T) {
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	desc := NewAssetLeafDescriptor(
		[]byte{0x51}, 1000, priv.PubKey(), nil, 0, 42, nil,
	)

	require.Equal(t, []byte{0x51}, desc.PkScript)
	require.Equal(t, btcutil.Amount(1000), desc.Amount)
	require.Equal(t, priv.PubKey(), desc.CoSignerKey)

	require.NotNil(t, desc.Asset)
	require.Equal(t, uint64(0), desc.Asset.AssetAmount)
	require.Equal(t, btcutil.Amount(42), desc.Asset.Funding)
	require.Nil(t, desc.Asset.InputProof)
	require.Nil(t, desc.Asset.ChangePkScript)
}

func TestAnchorPlanToLeafDescriptor(t *testing.T) {
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	output := &wire.TxOut{
		Value:    2100,
		PkScript: []byte{0x51},
	}

	trRoot := []byte{0x05, 0x06}
	desc, err := AnchorPlanToLeafDescriptor(
		output, priv.PubKey(), trRoot, 555, 2100, []byte{0xaa},
	)
	require.NoError(t, err)

	require.Equal(t, output.PkScript, desc.PkScript)
	require.Equal(t, btcutil.Amount(2100), desc.Amount)
	require.Equal(t, uint64(555), desc.Asset.AssetAmount)

	// Validation: nil output or cosigner.
	_, err = AnchorPlanToLeafDescriptor(nil, priv.PubKey(), nil, 0, 0, nil)
	require.Error(t, err)
	_, err = AnchorPlanToLeafDescriptor(output, nil, nil, 0, 0, nil)
	require.Error(t, err)
}

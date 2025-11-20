package tree

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

func TestNewAssetLeafDescriptorCopiesProofAndLabels(t *testing.T) {
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	origProof := []byte{0x01, 0x02}
	origChange := []byte{0xaa, 0xbb}
	origLabels := map[string]string{"client": "alice"}

	desc := NewAssetLeafDescriptor(
		[]byte{0x51}, 5000, priv.PubKey(),
		origProof, 777, LeafFunding{
			Mode:   FundingModeClientGas,
			Amount: 123,
		}, origChange, 10, origLabels,
	)

	require.NotNil(t, desc.Asset)
	require.Equal(t, origProof, desc.Asset.InputProof)
	require.Equal(t, origChange, desc.Asset.ChangePkScript)
	require.Equal(t, origLabels, desc.Asset.Labels)

	// Mutate originals and ensure the descriptor does not change.
	origProof[0] = 0xff
	origChange[0] = 0xcc
	origLabels["client"] = "bob"

	require.Equal(t, []byte{0x01, 0x02}, desc.Asset.InputProof)
	require.Equal(t, []byte{0xaa, 0xbb}, desc.Asset.ChangePkScript)
	require.Equal(t, map[string]string{"client": "alice"}, desc.Asset.Labels)
}

func TestNewAssetLeafDescriptorFields(t *testing.T) {
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	desc := NewAssetLeafDescriptor(
		[]byte{0x51}, 1000, priv.PubKey(), nil,
		0,
		LeafFunding{
			Mode:   FundingModeOperatorProvided,
			Amount: 42,
		}, nil, -5, nil,
	)

	require.Equal(t, []byte{0x51}, desc.PkScript)
	require.Equal(t, btcutil.Amount(1000), desc.Amount)
	require.Equal(t, priv.PubKey(), desc.CoSignerKey)

	require.NotNil(t, desc.Asset)
	require.Equal(t, uint64(0), desc.Asset.AssetAmount)
	require.Equal(t, LeafFunding{
		Mode:   FundingModeOperatorProvided,
		Amount: 42,
	}, desc.Asset.Funding)
	require.Equal(t, btcutil.Amount(-5), desc.Asset.ExitRebalance)
	require.Nil(t, desc.Asset.InputProof)
	require.Nil(t, desc.Asset.ChangePkScript)
	require.Nil(t, desc.Asset.Labels)
}

func TestAnchorPlanToLeafDescriptor(t *testing.T) {
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	output := &wire.TxOut{
		Value:    2100,
		PkScript: []byte{0x51},
	}

	desc, err := AnchorPlanToLeafDescriptor(
		output, priv.PubKey(), []byte{0x05, 0x06}, 555,
		LeafFunding{
			Mode:   FundingModeClientGas,
			Amount: 2100,
		}, []byte{0xaa}, 0, map[string]string{"client": "alice"},
	)
	require.NoError(t, err)

	require.Equal(t, output.PkScript, desc.PkScript)
	require.Equal(t, btcutil.Amount(2100), desc.Amount)
	require.Equal(t, uint64(555), desc.Asset.AssetAmount)
	require.Equal(t, map[string]string{"client": "alice"}, desc.Asset.Labels)

	// Validation: nil output or cosigner.
	_, err = AnchorPlanToLeafDescriptor(
		nil, priv.PubKey(), nil, 0, LeafFunding{}, nil, 0, nil,
	)
	require.Error(t, err)
	_, err = AnchorPlanToLeafDescriptor(
		output, nil, nil, 0, LeafFunding{}, nil, 0, nil,
	)
	require.Error(t, err)
}

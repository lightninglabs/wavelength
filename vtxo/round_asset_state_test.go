package vtxo

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/round"
	"github.com/stretchr/testify/require"
)

// TestRoundAssetDescriptor retains independent asset state when a confirmed
// round hands its leaves to the VTXO manager.
func TestRoundAssetDescriptor(t *testing.T) {
	t.Parallel()
	root := chainhash.Hash{1}
	cv := &round.ClientVTXO{
		TaprootAssetRoot:   &root,
		TaprootAssetRef:    "asset",
		TaprootAssetAmount: 100,
		TaprootAssetSealedPackage: []byte{
			1,
			2,
		},
	}
	desc, err := clientVTXOToDescriptor(
		cv, &round.VTXOCreatedNotification{},
	).Unpack()
	require.NoError(t, err)
	require.Equal(t, cv.TaprootAssetRoot, desc.TaprootAssetRoot)
	require.Equal(t, cv.TaprootAssetRef, desc.TaprootAssetRef)
	require.Equal(t, cv.TaprootAssetAmount, desc.TaprootAssetAmount)
	require.Equal(
		t, cv.TaprootAssetSealedPackage, desc.TaprootAssetSealedPackage,
	)
	require.Nil(t, desc.TapScript)
	cv.TaprootAssetRoot[0] = 2
	cv.TaprootAssetSealedPackage[0] = 3
	require.EqualValues(t, 1, desc.TaprootAssetRoot[0])
	require.EqualValues(t, 1, desc.TaprootAssetSealedPackage[0])
}

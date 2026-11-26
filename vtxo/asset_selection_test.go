package vtxo

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// makeAssetDescriptor creates a live test asset VTXO descriptor for assetRef
// carrying assetUnits and carrierSat.
func makeAssetDescriptor(t *testing.T, assetRef string, assetUnits uint64,
	carrierSat btcutil.Amount, idx uint32) *Descriptor {

	t.Helper()

	desc := makeDescriptor(t, carrierSat, idx)
	root := chainhash.HashH([]byte{byte(idx + 1), byte(assetUnits)})
	desc.TaprootAssetRoot = &root
	desc.TaprootAssetRef = assetRef
	desc.TaprootAssetAmount = assetUnits
	desc.TaprootAssetSealedPackage = []byte("sealed-pkg-" + assetRef)

	return desc
}

// TestSelectAssetVTXOCandidates verifies largest-first merging, asset
// isolation (ignoring plain Bitcoin, other assets, and unpackaged VTXOs), and
// the max-inputs merge cap.
func TestSelectAssetVTXOCandidates(t *testing.T) {
	t.Parallel()

	const assetA = "asset-a"
	const assetB = "asset-b"

	btcOnly := makeDescriptor(t, 50_000, 1)
	otherAsset := makeAssetDescriptor(t, assetB, 900, 1_000, 2)
	noPkg := makeAssetDescriptor(t, assetA, 800, 1_000, 3)
	noPkg.TaprootAssetSealedPackage = nil

	aSmall := makeAssetDescriptor(t, assetA, 100, 1_000, 4)
	aMed := makeAssetDescriptor(t, assetA, 300, 1_000, 5)
	aLarge := makeAssetDescriptor(t, assetA, 500, 1_000, 6)

	candidates := []*Descriptor{
		btcOnly, otherAsset, noPkg, aSmall, aMed, aLarge,
	}

	selected, total, err := SelectAssetVTXOCandidates(
		candidates, assetA, 750, 8,
	)
	require.NoError(t, err)
	require.EqualValues(t, 800, total)
	require.Len(t, selected, 2)
	require.Equal(t, aLarge.Outpoint, selected[0].Outpoint)
	require.Equal(t, aMed.Outpoint, selected[1].Outpoint)

	// Enforcing maxInputs = 1 fails with ErrInsufficientSpendableFunds
	// even though uncapped live balance exceeds the target.
	_, _, err = SelectAssetVTXOCandidates(candidates, assetA, 750, 1)
	require.ErrorIs(t, err, ErrInsufficientSpendableFunds)
	require.ErrorContains(t, err, "top 1")

	// Requesting more than total live balance fails.
	_, _, err = SelectAssetVTXOCandidates(candidates, assetA, 1_000, 8)
	require.ErrorIs(t, err, ErrInsufficientSpendableFunds)
}

// TestManagerSelectAndReserveAssetSpend verifies that the manager selects,
// merges, and reserves matching live asset VTXOs while isolating them from
// plain Bitcoin and other assets, and classifies reserved asset VTXOs as
// locked liquidity.
func TestManagerSelectAndReserveAssetSpend(t *testing.T) {
	t.Parallel()

	const assetRef = "asset-usd"

	btc := makeDescriptor(t, 100_000, 0)
	a1 := makeAssetDescriptor(t, assetRef, 400, 1_000, 1)
	a2 := makeAssetDescriptor(t, assetRef, 300, 1_000, 2)
	other := makeAssetDescriptor(t, "asset-eur", 900, 1_000, 3)

	all := []*Descriptor{btc, a1, a2, other}
	mgr, store := newTestManager(t, all)

	store.On("ListVTXOsByStatus", mock.Anything, VTXOStatusLive).
		Return(all, nil).Twice()
	store.On("ListLiveVTXOs", mock.Anything).Return(all, nil).Once()

	res := mgr.handleSelectAndReserveAssetSpend(
		t.Context(), &SelectAndReserveAssetSpendRequest{
			AssetRef:     assetRef,
			TargetAmount: 650,
			MaxInputs:    8,
		},
	)
	require.True(t, res.IsOk(), "unexpected error: %v", res.Err())

	respVal, _ := res.Unpack()
	resp, ok := respVal.(*SelectAndReserveAssetSpendResponse)
	require.True(t, ok)
	require.EqualValues(t, 700, resp.TotalAssetSelected)
	require.Len(t, resp.SelectedVTXOs, 2)
	require.Equal(t, a1.Outpoint, resp.SelectedVTXOs[0].Outpoint)
	require.Equal(t, a2.Outpoint, resp.SelectedVTXOs[1].Outpoint)
	require.NotZero(t, resp.SelectedVTXOs[0].ReserveEpoch)
	require.NotZero(t, resp.SelectedVTXOs[1].ReserveEpoch)

	// A second selection while both asset VTXOs are reserved in memory
	// classifies the shortfall as temporarily locked liquidity.
	res2 := mgr.handleSelectAndReserveAssetSpend(
		t.Context(), &SelectAndReserveAssetSpendRequest{
			AssetRef:     assetRef,
			TargetAmount: 100,
			MaxInputs:    8,
		},
	)
	require.True(t, res2.IsErr())
	require.ErrorIs(t, res2.Err(), ErrVTXOLiquidityLocked)
}

package db

import (
	"testing"

	"github.com/lightninglabs/wavelength/round"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestRoundAssetVTXOState persists the asset marker in the round transaction,
// before any manager notification, and preserves it through both read APIs.
func TestRoundAssetVTXOState(t *testing.T) {
	t.Parallel()
	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()
	roundID := round.RoundID{1}
	require.NoError(
		t,
		roundStore.CommitState(
			ctx, createTestRound(t, roundID),
			&round.InputSigSentState{
				RoundID: roundID,
			},
		),
	)
	desc := testAssetDescriptor(t, 1)
	cv := &round.ClientVTXO{
		Outpoint:                  desc.Outpoint,
		Amount:                    desc.Amount,
		PolicyTemplate:            desc.PolicyTemplate,
		PkScript:                  desc.PkScript,
		OwnerKey:                  desc.ClientKey,
		OperatorKey:               desc.OperatorKey,
		Expiry:                    desc.RelativeExpiry,
		RoundID:                   fn.Some(roundID),
		TaprootAssetRoot:          desc.TaprootAssetRoot,
		TaprootAssetRef:           desc.TaprootAssetRef,
		TaprootAssetAmount:        desc.TaprootAssetAmount,
		TaprootAssetSealedPackage: desc.TaprootAssetSealedPackage,
	}
	require.NoError(t, roundStore.SaveVTXOs(ctx, []*round.ClientVTXO{cv}))
	got, err := vtxoStore.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.Equal(t, desc.TaprootAssetAmount, got.TaprootAssetAmount)
	require.Equal(
		t, desc.TaprootAssetSealedPackage,
		got.TaprootAssetSealedPackage,
	)
	require.Nil(t, got.TapScript)
	fromRound, err := roundStore.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.Equal(t, cv.TaprootAssetRoot, fromRound.TaprootAssetRoot)
	require.Equal(t, cv.TaprootAssetRef, fromRound.TaprootAssetRef)
	require.Equal(t, cv.TaprootAssetAmount, fromRound.TaprootAssetAmount)
	require.Equal(
		t, cv.TaprootAssetSealedPackage,
		fromRound.TaprootAssetSealedPackage,
	)
	require.NoError(t, vtxoStore.SaveVTXO(ctx, desc))
	cv.TaprootAssetAmount--
	require.Error(t, roundStore.SaveVTXOs(ctx, []*round.ClientVTXO{cv}))
}

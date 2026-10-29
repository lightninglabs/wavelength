package db

import (
	"testing"

	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// TestRecoveredVTXOHoldsInconsistentLegacyExpiry verifies an inconsistent
// legacy deadline cannot trigger an exit or expire a live coin after upgrade.
func TestRecoveredVTXOHoldsInconsistentLegacyExpiry(t *testing.T) {
	t.Parallel()

	store, _, _ := newVTXOStoreForTest(t)
	desc := createTestVTXODescriptor(
		t, testRoundIDDB("expiry-recovery"), 0,
	)
	desc.CreatedHeight = 700
	desc.BatchExpiry = 600
	require.NoError(t, store.SaveVTXO(t.Context(), desc))

	// Recover through the same store listing used at daemon startup. The
	// legacy deadline has no durable authentication marker, so its positive
	// value alone cannot override the existing consistency guard.
	recovered, err := store.ListRecoverableVTXOs(t.Context())
	require.NoError(t, err)
	require.Len(t, recovered, 1)
	require.Equal(t, desc.CreatedHeight, recovered[0].CreatedHeight)

	cfg := vtxo.DefaultExpiryConfig()
	child := vtxo.NewVTXOActor(t.Context(), &vtxo.VTXOActorConfig{
		VTXO:         recovered[0],
		Store:        store,
		ExpiryConfig: cfg,
	})
	require.IsType(t, &vtxo.LiveState{}, child.CurrentState())

	criticalBlocks := cfg.CalculateCriticalThreshold(recovered[0])
	for _, height := range []int32{
		desc.BatchExpiry - criticalBlocks, desc.BatchExpiry,
	} {
		event := &vtxo.BlockEpochEvent{Height: height}
		response, err := child.Receive(t.Context(), event).Unpack()
		require.NoError(t, err)
		require.IsType(t, &vtxo.LiveState{}, child.CurrentState())

		result, ok := response.(vtxo.VTXOActorResponse)
		require.True(t, ok)
		require.Empty(t, result.Outbox)
	}

	stored, err := store.GetVTXO(t.Context(), desc.Outpoint)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusLive, stored.Status)
	require.Equal(t, desc.BatchExpiry, stored.BatchExpiry)
	require.Equal(
		t, vtxo.ExpiryStatusUnknown,
		cfg.CheckExpiry(stored, stored.BatchExpiry),
	)
	require.Equal(t, desc.CreatedHeight, stored.CreatedHeight)
}

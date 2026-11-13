package db

import (
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/vhtlcrecovery"
	"github.com/stretchr/testify/require"
)

// TestVHTLCRecoveryRequestLookup includes terminal jobs so an idempotent
// restore cannot hide a cancelled destination and allocate a replacement.
func TestVHTLCRecoveryRequestLookup(t *testing.T) {
	t.Parallel()
	store := newVHTLCRecoveryStoreForTest(t, time.Unix(1_700_000_000, 0))
	job := sampleVHTLCRecoveryJob("indexed-job", "indexed-request")
	_, _, err := store.ArmRecovery(t.Context(), job)
	require.NoError(t, err)
	got, err := store.GetRecoveryByRequestID(t.Context(), job.RequestID)
	require.NoError(t, err)
	require.Equal(t, job.ID, got.ID)
	require.NoError(
		t,
		store.CancelRecovery(
			t.Context(), job.ID, "cooperative settlement", nil,
		),
	)
	got, err = store.GetRecoveryByRequestID(t.Context(), job.RequestID)
	require.NoError(t, err)
	require.Equal(t, vhtlcrecovery.StateCancelled, got.State)
	_, err = store.GetRecoveryByRequestID(t.Context(), "missing")
	require.ErrorIs(t, err, ErrVHTLCRecoveryJobNotFound)
}

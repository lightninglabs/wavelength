package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestAdmissionDeadlinePersistence proves retries and quote reseals cannot
// extend admission, closure survives reconstruction, and deadline records never
// masquerade as signature checkpoints during reservation recovery.
func TestAdmissionDeadlinePersistence(t *testing.T) {
	t.Parallel()

	store, _ := newRoundStoreForTest(t)
	ctx := t.Context()
	id := testRoundIDDB("admission-budget")
	expiry := time.Unix(1_800_000_000, 123).UTC()

	first, err := store.ConstrainAdmissionDeadline(ctx, id, expiry)
	require.NoError(t, err)
	require.Equal(t, expiry, first.ExpiresAt)
	require.False(t, first.Closed)

	// A retry uses the original durable budget, even with a later clock.
	retry, err := store.ConstrainAdmissionDeadline(
		ctx, id, expiry.Add(time.Hour),
	)
	require.NoError(t, err)
	require.Equal(t, first, retry)

	// A quote may shorten authorization, but a reseal cannot extend it.
	shorter := expiry.Add(-time.Minute)
	quote, err := store.ConstrainAdmissionDeadline(ctx, id, shorter)
	require.NoError(t, err)
	require.Equal(t, shorter, quote.ExpiresAt)

	checkpointed, err := store.HasForfeitRoundCheckpoint(ctx, id.String())
	require.NoError(t, err)
	require.False(t, checkpointed)

	require.NoError(t, store.CloseAdmissionDeadline(ctx, id))
	require.NoError(t, store.CloseAdmissionDeadline(ctx, id))
	restored := NewRoundPersistenceStore(
		store.db, store.chainParams, store.clock,
	)
	closed, err := restored.ConstrainAdmissionDeadline(ctx, id, expiry)
	require.NoError(t, err)
	require.True(t, closed.Closed)
	require.Equal(t, shorter, closed.ExpiresAt)
}

// TestAdmissionDeadlineRestartFence proves startup abandons future and overdue
// ephemeral attempts alike, without creating a signature-bearing checkpoint.
func TestAdmissionDeadlineRestartFence(t *testing.T) {
	t.Parallel()

	store, _ := newRoundStoreForTest(t)
	ctx := t.Context()
	for _, offset := range []time.Duration{-time.Hour, time.Hour} {
		id := testRoundIDDB(offset.String())
		expiry := time.Now().Add(offset)
		_, err := store.ConstrainAdmissionDeadline(ctx, id, expiry)
		require.NoError(t, err)
	}

	require.NoError(t, store.AbandonAdmissionDeadlines(ctx))
	require.NoError(t, store.AbandonAdmissionDeadlines(ctx))
	for _, offset := range []time.Duration{-time.Hour, time.Hour} {
		id := testRoundIDDB(offset.String())
		deadline, err := store.ConstrainAdmissionDeadline(
			ctx, id, time.Now().Add(time.Hour),
		)
		require.NoError(t, err)
		require.True(t, deadline.Closed)
		checkpointed, err := store.HasForfeitRoundCheckpoint(
			ctx, id.String(),
		)
		require.NoError(t, err)
		require.False(t, checkpointed)
	}
}

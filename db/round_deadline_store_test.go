package db

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightningnetwork/lnd/clock"
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

// TestAdmissionDeadlineDatabaseReopen proves a committed budget survives
// closing and reopening the SQLite database. A missing pre-write record has no
// effect; a post-write crash leaves the original budget for startup to fence.
func TestAdmissionDeadlineDatabaseReopen(t *testing.T) {
	t.Parallel()

	cfg := DefaultSqliteConfig(t.TempDir())
	first, err := NewSqliteStore(cfg, btclog.Disabled)
	require.NoError(t, err)
	store := NewStore(
		first.DB, first.Queries, first.Backend(), btclog.Disabled,
	)
	rounds := store.NewRoundStore(
		&chaincfg.RegressionNetParams, clock.NewDefaultClock(),
	)
	id := testRoundIDDB("disk-admission")
	expiry := time.Now().Add(time.Minute).UTC()
	_, err = rounds.ConstrainAdmissionDeadline(t.Context(), id, expiry)
	require.NoError(t, err)
	require.NoError(t, first.Close())

	second, err := NewSqliteStore(cfg, btclog.Disabled)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	store = NewStore(
		second.DB, second.Queries, second.Backend(), btclog.Disabled,
	)
	rounds = store.NewRoundStore(
		&chaincfg.RegressionNetParams, clock.NewDefaultClock(),
	)
	recovered, err := rounds.ConstrainAdmissionDeadline(
		t.Context(), id, expiry.Add(time.Hour),
	)
	require.NoError(t, err)
	require.Equal(t, expiry, recovered.ExpiresAt)
	require.False(t, recovered.Closed)
	require.NoError(t, rounds.AbandonAdmissionDeadlines(t.Context()))
	recovered, err = rounds.ConstrainAdmissionDeadline(
		t.Context(), id, expiry,
	)
	require.NoError(t, err)
	require.True(t, recovered.Closed)
}

// TestAdmissionCheckpointWinsRestart covers a crash after checkpoint commit
// but before the caller knows whether signature delivery completed. Restart
// must preserve that checkpoint regardless of the admission deadline's age.
func TestAdmissionCheckpointWinsRestart(t *testing.T) {
	t.Parallel()

	store, _ := newRoundStoreForTest(t)
	ctx := t.Context()
	id := testRoundIDDB("admission-checkpoint-handoff")
	_, err := store.ConstrainAdmissionDeadline(
		ctx, id, time.Now().Add(-time.Minute),
	)
	require.NoError(t, err)
	r := createTestRound(t, id)
	require.NoError(
		t,
		store.CommitState(
			ctx, r, &round.InputSigSentState{
				RoundID: id,
			},
		),
	)
	require.NoError(t, store.AbandonAdmissionDeadlines(ctx))
	deadline, err := store.ConstrainAdmissionDeadline(ctx, id, time.Now())
	require.NoError(t, err)
	require.True(t, deadline.Closed)
	checkpoint, err := store.HasForfeitRoundCheckpoint(ctx, id.String())
	require.NoError(t, err)
	require.True(t, checkpoint)
	active, err := store.ListActiveRounds(ctx)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, id, active[0].RoundID)
}

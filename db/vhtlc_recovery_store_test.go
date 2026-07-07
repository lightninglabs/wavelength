package db

import (
	"bytes"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/vhtlcrecovery"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

func newVHTLCRecoveryStoreForTest(t *testing.T,
	now time.Time) *VHTLCRecoveryStoreDB {

	t.Helper()

	sqlDB := NewTestDB(t)
	store := NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), btclog.Disabled,
	)

	return NewVHTLCRecoveryStore(store, clock.NewTestClock(now))
}

func TestVHTLCRecoveryStoreArmIdempotent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	now := time.Unix(1_700_000_000, 0)
	store := newVHTLCRecoveryStoreForTest(t, now)

	job := sampleVHTLCRecoveryJob("recovery-a", "request-a")
	stored, created, err := store.ArmRecovery(ctx, job)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, job.ID, stored.ID)
	require.Equal(t, vhtlcrecovery.StateArmed, stored.State)
	require.Equal(
		t, vhtlcrecovery.ExitPolicyKindClaim, stored.ExitPolicyKind,
	)
	require.Equal(t, now.UTC(), stored.CreatedAt)
	require.NotNil(t, stored.ArmedAt)
	require.Equal(t, now.UTC(), *stored.ArmedAt)

	replay := job
	replay.ID = "different-local-id"
	stored, created, err = store.ArmRecovery(ctx, replay)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, job.ID, stored.ID)

	conflict := job
	conflict.VTXOAmountSat++
	_, _, err = store.ArmRecovery(ctx, conflict)
	require.ErrorIs(t, err, ErrVHTLCRecoveryIdempotencyConflict)
}

func TestVHTLCRecoveryStoreSeparatesSwapActionByVTXO(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newVHTLCRecoveryStoreForTest(
		t, time.Unix(1_700_000_100, 0),
	)

	first := sampleVHTLCRecoveryJob("recovery-a", "request-a")
	_, created, err := store.ArmRecovery(ctx, first)
	require.NoError(t, err)
	require.True(t, created)

	second := first
	second.ID = "recovery-b"
	second.RequestID = "request-b"
	second.DestinationScript = []byte{0x51}

	_, _, err = store.ArmRecovery(ctx, second)
	require.ErrorIs(t, err, ErrVHTLCRecoveryIdempotencyConflict)

	refreshed := first
	refreshed.ID = "recovery-c"
	refreshed.RequestID = "request-c"
	refreshed.VTXOOutpoint.Index++
	refreshed.VTXOAmountSat--
	refreshed.DestinationScript = []byte{0x51}

	stored, created, err := store.ArmRecovery(ctx, refreshed)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, refreshed.ID, stored.ID)
	require.Equal(t, refreshed.VTXOOutpoint, stored.VTXOOutpoint)
}

func TestVHTLCRecoveryStoreRejectsActionPolicyMismatch(t *testing.T) {
	t.Parallel()

	store := newVHTLCRecoveryStoreForTest(
		t, time.Unix(1_700_000_150, 0),
	)

	job := sampleVHTLCRecoveryJob("recovery-a", "request-a")
	job.ExitPolicyKind = vhtlcrecovery.ExitPolicyKindRefundWithoutReceiver

	_, _, err := store.ArmRecovery(t.Context(), job)
	require.ErrorContains(t, err, "does not match action")
}

func TestVHTLCRecoveryStoreTransitions(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newVHTLCRecoveryStoreForTest(
		t, time.Unix(1_700_000_200, 0),
	)

	active := sampleVHTLCRecoveryJob("recovery-active", "request-active")
	cancelled := sampleVHTLCRecoveryJob(
		"recovery-cancelled", "request-cancelled",
	)
	cancelled.SwapID = []byte("swap-b")
	failed := sampleVHTLCRecoveryJob("recovery-failed", "request-failed")
	failed.SwapID = []byte("swap-c")

	_, _, err := store.ArmRecovery(ctx, active)
	require.NoError(t, err)
	_, _, err = store.ArmRecovery(ctx, cancelled)
	require.NoError(t, err)
	_, _, err = store.ArmRecovery(ctx, failed)
	require.NoError(t, err)

	claimPreimage := bytes.Repeat([]byte{0x42}, 32)
	require.NoError(
		t, store.EscalateRecovery(
			ctx, active.ID, claimPreimage,
		),
	)
	require.NoError(
		t,
		store.CancelRecovery(
			ctx, cancelled.ID, "cooperative_completed",
			bytes.Repeat(
				[]byte{0xCC}, chainhash.HashSize,
			),
		),
	)
	require.NoError(t, store.FailRecovery(ctx, failed.ID, nil))

	jobs, err := store.ListNonTerminalRecoveries(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.Equal(t, active.ID, jobs[0].ID)
	require.Equal(t, claimPreimage, jobs[0].ClaimPreimage)
	require.Equal(t, vhtlcrecovery.StateUnrollStarted, jobs[0].State)
	require.NotNil(t, jobs[0].EscalatedAt)
	require.NoError(t, store.EscalateRecovery(ctx, active.ID, nil))
	replayedActive, err := store.GetRecovery(ctx, active.ID)
	require.NoError(t, err)
	require.Equal(t, vhtlcrecovery.StateUnrollStarted, replayedActive.State)
	require.Equal(t, claimPreimage, replayedActive.ClaimPreimage)

	cancelledJob, err := store.GetRecovery(ctx, cancelled.ID)
	require.NoError(t, err)
	require.Equal(t, vhtlcrecovery.StateCancelled, cancelledJob.State)
	require.Equal(t, "cooperative_completed", cancelledJob.CancelReason)
	require.NotNil(t, cancelledJob.TerminalAt)
	require.True(t, cancelledJob.IsTerminal())

	failedJob, err := store.GetRecovery(ctx, failed.ID)
	require.NoError(t, err)
	require.Equal(t, vhtlcrecovery.StateFailed, failedJob.State)
	require.Empty(t, failedJob.LastError)
	require.NotNil(t, failedJob.TerminalAt)
	require.True(t, failedJob.IsTerminal())

	require.NoError(t, store.CompleteRecovery(ctx, active.ID))
	// A second terminal transition on the same row must surface
	// ErrVHTLCRecoveryAlreadyTerminal rather than silently succeeding so
	// racing terminal paths can't mask a lost-update bug.
	require.ErrorIs(
		t, store.CancelRecovery(ctx, active.ID, "late", nil),
		ErrVHTLCRecoveryAlreadyTerminal,
	)
	require.ErrorIs(
		t, store.CompleteRecovery(ctx, "missing"),
		ErrVHTLCRecoveryJobNotFound,
	)
	require.ErrorIs(
		t, store.FailRecovery(ctx, "missing", nil),
		ErrVHTLCRecoveryJobNotFound,
	)
	require.ErrorIs(
		t, store.CancelRecovery(ctx, "missing", "stale", nil),
		ErrVHTLCRecoveryJobNotFound,
	)
	require.ErrorIs(
		t, store.EscalateRecovery(ctx, active.ID, nil),
		ErrVHTLCRecoveryCannotEscalate,
	)
	require.ErrorIs(
		t, store.EscalateRecovery(ctx, "missing", nil),
		ErrVHTLCRecoveryJobNotFound,
	)

	completed, err := store.GetRecovery(ctx, active.ID)
	require.NoError(t, err)
	require.Equal(t, vhtlcrecovery.StateCompleted, completed.State)
	require.Empty(t, completed.CancelReason)

	jobs, err = store.ListNonTerminalRecoveries(ctx)
	require.NoError(t, err)
	require.Empty(t, jobs)
}

func TestVHTLCRecoveryStoreNotFound(t *testing.T) {
	t.Parallel()

	store := newVHTLCRecoveryStoreForTest(
		t, time.Unix(1_700_000_300, 0),
	)

	_, err := store.GetRecovery(t.Context(), "missing")
	require.ErrorIs(t, err, ErrVHTLCRecoveryJobNotFound)
}

func TestVHTLCRecoveryStoreRequiresRequestID(t *testing.T) {
	t.Parallel()

	store := newVHTLCRecoveryStoreForTest(
		t, time.Unix(1_700_000_400, 0),
	)

	job := sampleVHTLCRecoveryJob("recovery-a", "")
	_, _, err := store.ArmRecovery(t.Context(), job)
	require.ErrorContains(t, err, "request id is required")
}

func sampleVHTLCRecoveryJob(id, requestID string) vhtlcrecovery.RecoveryJob {
	return vhtlcrecovery.RecoveryJob{
		ID:        id,
		RequestID: requestID,
		SwapID:    []byte("swap-a"),
		Direction: vhtlcrecovery.DirectionReceive,
		Action:    vhtlcrecovery.ActionClaim,
		VTXOOutpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0x11,
			},
			Index: 7,
		},
		VTXOAmountSat: 250_000,
		SenderPubkey: bytes.Repeat(
			[]byte{0x02}, 33,
		),
		ReceiverPubkey: bytes.Repeat(
			[]byte{0x03}, 33,
		),
		ServerPubkey: bytes.Repeat(
			[]byte{0x04}, 33,
		),
		RefundLocktime:                       500,
		UnilateralClaimDelay:                 12,
		UnilateralRefundDelay:                24,
		UnilateralRefundWithoutReceiverDelay: 36,
		PreimageHash: bytes.Repeat(
			[]byte{0xAA}, 32,
		),
		SignerKeyFamily: 6,
		SignerKeyIndex:  9,
		DestinationScript: []byte{
			0x00,
			0x14,
			0x01,
		},
		MaxFeeRateSatPerKWeight: 2_500,
	}
}

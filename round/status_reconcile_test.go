package round

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/stretchr/testify/require"
)

// reconcileRoundID builds a deterministic round id for the reconcile tests.
func reconcileRoundID(seed byte) RoundID {
	var id RoundID
	id[0] = seed

	return id
}

// reconcileState builds an InputSigSentState carrying the given forfeits,
// mirroring the checkpointed point-of-no-return state of a refresh round.
func reconcileState(roundID RoundID,
	forfeits []types.ForfeitRequest) *InputSigSentState {

	return &InputSigSentState{
		RoundID: roundID,
		Intents: Intents{
			Forfeits: forfeits,
		},
	}
}

// reconcileEnv builds a minimal environment with the status reconcile
// enabled.
func reconcileEnv() *ClientEnvironment {
	return &ClientEnvironment{
		Log:                    btclog.Disabled,
		StatusReconcileTimeout: time.Minute,
	}
}

// reconcileOutpoint builds a deterministic VTXO outpoint.
func reconcileOutpoint(seed byte) wire.OutPoint {
	return wire.OutPoint{
		Hash: chainhash.Hash{
			seed,
		},
		Index: 0,
	}
}

// TestPostSigningFailureParksAndProbes is the wavelength#844 core: a round
// failure arriving in InputSigSentState with forfeit signatures already out
// must NOT fail the round or release the reservations on the notification
// alone. The FSM parks the failure in the state and probes the operator
// with a QueryRoundStatus, arming the reconcile retry timeout.
func TestPostSigningFailureParksAndProbes(t *testing.T) {
	t.Parallel()

	roundID := reconcileRoundID(0xa1)
	s := reconcileState(roundID, []types.ForfeitRequest{
		mkForfeit(reconcileOutpoint(0x01), 10_000),
	})

	failure := &BoardingFailed{
		Reason:      "input signature collection timeout",
		Recoverable: true,
	}

	tr, err := s.ProcessEvent(context.Background(), failure, reconcileEnv())
	require.NoError(t, err)

	// The round must still be in InputSigSentState with the failure
	// parked, not failed.
	next, ok := tr.NextState.(*InputSigSentState)
	require.True(t, ok, "expected InputSigSentState, got %T", tr.NextState)
	require.NotNil(t, next.PendingFailure)
	require.Equal(t, failure.Reason, next.PendingFailure.Reason)

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox

	// A probe went out and the retry window was armed.
	probe, ok := findOutbox[*QueryRoundStatusOutbox](outbox)
	require.True(t, ok, "no QueryRoundStatusOutbox emitted")
	require.Equal(t, roundID, probe.RoundID)

	timeoutReq, ok := findOutbox[*StartTimeoutReq](outbox)
	require.True(t, ok, "no StartTimeoutReq emitted")
	require.Equal(t, TimeoutPhaseStatusReconcile, timeoutReq.Phase)

	// Crucially, NO release rode the notification.
	_, released := findOutbox[*ReleaseForfeitReservation](outbox)
	require.False(t, released, "release emitted on unreconciled failure")
}

// TestPostSigningFailureNoForfeitsFailsImmediately pins the boarding-only
// behavior: with no forfeit reservations at stake there is nothing to
// strand, so a round failure in InputSigSentState fails the round
// immediately exactly as before the reconcile existed.
func TestPostSigningFailureNoForfeitsFailsImmediately(t *testing.T) {
	t.Parallel()

	s := reconcileState(reconcileRoundID(0xa2), nil)

	failure := &BoardingFailed{
		Reason:      "round failed",
		Recoverable: true,
	}

	tr, err := s.ProcessEvent(context.Background(), failure, reconcileEnv())
	require.NoError(t, err)

	failed, ok := tr.NextState.(*ClientFailedState)
	require.True(t, ok, "expected ClientFailedState, got %T", tr.NextState)
	require.Equal(t, failure.Reason, failed.Reason)
}

// TestPostSigningFailureReconcileDisabledFailsImmediately pins the opt-out:
// a non-positive StatusReconcileTimeout restores the pre-#844 behavior of
// failing straight into ClientFailedState with no release (the #823
// startup sweep remains the only rescue).
func TestPostSigningFailureReconcileDisabledFailsImmediately(t *testing.T) {
	t.Parallel()

	s := reconcileState(reconcileRoundID(0xa3), []types.ForfeitRequest{
		mkForfeit(reconcileOutpoint(0x02), 10_000),
	})
	env := &ClientEnvironment{
		Log:                    btclog.Disabled,
		StatusReconcileTimeout: -1,
	}

	tr, err := s.ProcessEvent(
		context.Background(), &BoardingFailed{
			Reason:      "round failed",
			Recoverable: true,
		},
		env,
	)
	require.NoError(t, err)

	_, ok := tr.NextState.(*ClientFailedState)
	require.True(t, ok, "expected ClientFailedState, got %T", tr.NextState)
}

// TestDeadStatusFailsAndReleases proves the reconciled release: an
// authoritative ROUND_STATUS_DEAD answer fails the round with the parked
// failure and emits the ReleaseForfeitReservation returning the inputs to
// LiveState, plus the reconcile-timeout cancel.
func TestDeadStatusFailsAndReleases(t *testing.T) {
	t.Parallel()

	roundID := reconcileRoundID(0xa4)
	op := reconcileOutpoint(0x03)
	s := reconcileState(roundID, []types.ForfeitRequest{
		mkForfeit(op, 10_000),
	})
	s.PendingFailure = &BoardingFailed{
		Reason:      "input signature collection timeout",
		Recoverable: true,
	}

	tr, err := s.ProcessEvent(
		context.Background(),
		&RoundStatusReported{
			RoundID: roundID,
			Status:  roundpb.RoundLifecycleStatus_ROUND_STATUS_DEAD,
		},
		reconcileEnv(),
	)
	require.NoError(t, err)

	failed, ok := tr.NextState.(*ClientFailedState)
	require.True(t, ok, "expected ClientFailedState, got %T", tr.NextState)
	require.Equal(t, s.PendingFailure.Reason, failed.Reason)
	require.True(t, failed.Recoverable)

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox

	release, ok := findOutbox[*ReleaseForfeitReservation](outbox)
	require.True(t, ok, "no ReleaseForfeitReservation emitted")
	require.Equal(t, []wire.OutPoint{op}, release.Outpoints)

	cancel, ok := findOutbox[*CancelTimeoutReq](outbox)
	require.True(t, ok, "no CancelTimeoutReq emitted")
	require.Equal(t, TimeoutPhaseStatusReconcile, cancel.Phase)
}

// TestDeadStatusWithoutParkedFailureSynthesizesReason covers the lumos#618
// silence door: the round died with no failure notification at all (server
// crash), so the dead answer itself must carry the round into a
// recoverable failure with the release.
func TestDeadStatusWithoutParkedFailureSynthesizesReason(t *testing.T) {
	t.Parallel()

	roundID := reconcileRoundID(0xa5)
	op := reconcileOutpoint(0x04)
	s := reconcileState(roundID, []types.ForfeitRequest{
		mkForfeit(op, 10_000),
	})

	tr, err := s.ProcessEvent(
		context.Background(),
		&RoundStatusReported{
			RoundID: roundID,
			Status:  roundpb.RoundLifecycleStatus_ROUND_STATUS_DEAD,
			Detail:  "round unknown to operator",
		},
		reconcileEnv(),
	)
	require.NoError(t, err)

	failed, ok := tr.NextState.(*ClientFailedState)
	require.True(t, ok, "expected ClientFailedState, got %T", tr.NextState)
	require.Equal(t, "round unknown to operator", failed.Reason)
	require.True(t, failed.Recoverable)

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	_, released := findOutbox[*ReleaseForfeitReservation](outbox)
	require.True(t, released, "no release on dead answer")
}

// TestNonDeadStatusHoldsReservations pins the safety half: any answer other
// than dead (in-flight, broadcast, confirmed) means the commitment may
// still confirm, so the FSM must hold the reservations and keep waiting.
func TestNonDeadStatusHoldsReservations(t *testing.T) {
	t.Parallel()

	statuses := []roundpb.RoundLifecycleStatus{
		roundpb.RoundLifecycleStatus_ROUND_STATUS_IN_FLIGHT,
		roundpb.RoundLifecycleStatus_ROUND_STATUS_BROADCAST,
		roundpb.RoundLifecycleStatus_ROUND_STATUS_CONFIRMED,
		roundpb.RoundLifecycleStatus_ROUND_STATUS_UNSPECIFIED,
	}

	for _, status := range statuses {
		roundID := reconcileRoundID(0xa6)
		s := reconcileState(roundID, []types.ForfeitRequest{
			mkForfeit(reconcileOutpoint(0x05), 10_000),
		})
		s.PendingFailure = &BoardingFailed{Reason: "parked"}

		tr, err := s.ProcessEvent(
			context.Background(),
			&RoundStatusReported{
				RoundID: roundID,
				Status:  status,
			},
			reconcileEnv(),
		)
		require.NoError(t, err)

		next, ok := tr.NextState.(*InputSigSentState)
		require.True(
			t, ok, "status %v: expected InputSigSentState, got %T",
			status, tr.NextState,
		)
		require.NotNil(
			t, next.PendingFailure, "status %v: parked failure "+
				"lost", status,
		)

		outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
		_, released := findOutbox[*ReleaseForfeitReservation](outbox)
		require.False(
			t, released, "status %v: released on a non-dead answer",
			status,
		)
	}
}

// TestMismatchedReportIgnored pins the routing guard: a status report for a
// different round must not touch this round's state.
func TestMismatchedReportIgnored(t *testing.T) {
	t.Parallel()

	s := reconcileState(reconcileRoundID(0xa7), []types.ForfeitRequest{
		mkForfeit(reconcileOutpoint(0x06), 10_000),
	})

	tr, err := s.ProcessEvent(
		context.Background(),
		&RoundStatusReported{
			RoundID: reconcileRoundID(0xff),
			Status:  roundpb.RoundLifecycleStatus_ROUND_STATUS_DEAD,
		},
		reconcileEnv(),
	)
	require.NoError(t, err)

	_, ok := tr.NextState.(*InputSigSentState)
	require.True(t, ok, "expected InputSigSentState, got %T", tr.NextState)

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	_, released := findOutbox[*ReleaseForfeitReservation](outbox)
	require.False(t, released, "released on a mismatched report")
}

// TestReconcileTimeoutReprobes pins the retry loop that covers both the
// lost-answer case and the lumos#618 silence door: every expiry of the
// reconcile window re-emits the probe and re-arms the window, and never
// fails the round by itself.
func TestReconcileTimeoutReprobes(t *testing.T) {
	t.Parallel()

	roundID := reconcileRoundID(0xa8)
	s := reconcileState(roundID, []types.ForfeitRequest{
		mkForfeit(reconcileOutpoint(0x07), 10_000),
	})

	tr, err := s.ProcessEvent(
		context.Background(), &StatusReconcileTimedOut{
			RoundID: roundID,
		},
		reconcileEnv(),
	)
	require.NoError(t, err)

	_, ok := tr.NextState.(*InputSigSentState)
	require.True(t, ok, "expected InputSigSentState, got %T", tr.NextState)

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox

	probe, ok := findOutbox[*QueryRoundStatusOutbox](outbox)
	require.True(t, ok, "no re-probe emitted")
	require.Equal(t, roundID, probe.RoundID)

	timeoutReq, ok := findOutbox[*StartTimeoutReq](outbox)
	require.True(t, ok, "reconcile window not re-armed")
	require.Equal(t, TimeoutPhaseStatusReconcile, timeoutReq.Phase)

	_, released := findOutbox[*ReleaseForfeitReservation](outbox)
	require.False(t, released, "released on a bare timeout")
}

// TestReconcileReprobeBacksOff proves the re-arm duration doubles with each
// unanswered probe and caps at base<<statusReconcileMaxBackoffShift, so a
// parked round facing an operator that never answers (e.g. one predating the
// QueryRoundStatus RPC) converges on a bounded probe cadence instead of an
// unbounded fixed-rate loop.
func TestReconcileReprobeBacksOff(t *testing.T) {
	t.Parallel()

	roundID := reconcileRoundID(0xaa)
	env := reconcileEnv()
	base := env.StatusReconcileTimeout

	var state ClientState = reconcileState(
		roundID, []types.ForfeitRequest{
			mkForfeit(reconcileOutpoint(0x09), 10_000),
		},
	)

	// Drive enough timeouts to walk past the backoff ceiling, checking
	// the re-armed duration at every step.
	for probe := 0; probe < statusReconcileMaxBackoffShift+3; probe++ {
		tr, err := state.ProcessEvent(
			context.Background(), &StatusReconcileTimedOut{
				RoundID: roundID,
			},
			env,
		)
		require.NoError(t, err)

		next, ok := tr.NextState.(*InputSigSentState)
		require.True(
			t, ok, "expected InputSigSentState, got %T",
			tr.NextState,
		)

		outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
		timeoutReq, ok := findOutbox[*StartTimeoutReq](outbox)
		require.True(t, ok, "reconcile window not re-armed")

		shift := min(
			uint32(probe), statusReconcileMaxBackoffShift,
		)
		require.Equal(
			t, base<<shift, timeoutReq.Duration, "probe %d "+
				"re-armed with the wrong backoff", probe,
		)

		state = next
	}
}

// TestDeadStatusTerminalCodeRetiresJob proves the terminal-for-job path
// composes with the reconcile: when the parked failure carries a
// terminal-for-job code, the dead answer emits the
// TerminalJobFailedNotification alongside the release so the originating
// job is retired rather than replayed.
func TestDeadStatusTerminalCodeRetiresJob(t *testing.T) {
	t.Parallel()

	roundID := reconcileRoundID(0xa9)
	op := reconcileOutpoint(0x08)
	s := reconcileState(roundID, []types.ForfeitRequest{
		mkForfeit(op, 10_000),
	})
	s.PendingFailure = &BoardingFailed{
		Reason:      "operator cannot fund the commitment tx",
		Recoverable: true,
		FailureCode: RoundFailureInsufficientOperatorFunds,
	}

	tr, err := s.ProcessEvent(
		context.Background(),
		&RoundStatusReported{
			RoundID: roundID,
			Status:  roundpb.RoundLifecycleStatus_ROUND_STATUS_DEAD,
		},
		reconcileEnv(),
	)
	require.NoError(t, err)

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox

	notify, ok := findOutbox[*TerminalJobFailedNotification](outbox)
	require.True(t, ok, "no TerminalJobFailedNotification emitted")
	require.Equal(t, []wire.OutPoint{op}, notify.ForfeitOutpoints)
	require.Equal(
		t, RoundFailureInsufficientOperatorFunds, notify.FailureCode,
	)
}

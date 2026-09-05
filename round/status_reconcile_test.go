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
	fn "github.com/lightningnetwork/lnd/fn/v2"
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

	// The release names the dead round so a VTXO that has since signed
	// its forfeit for a newer round refuses it.
	require.Equal(t, roundID.String(), release.RoundID)

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

	// The disarm is cleanup and must trail the job drop. A rejected
	// CancelTimeoutReq aborts the rest of the outbox, so a cancel sitting
	// ahead of the notification would let a saturated timeout actor leave
	// the pending intent in recoverable replay, which is exactly what
	// retiring the job prevents.
	cancelIdx := outboxIndexOf[*CancelTimeoutReq](outbox)
	require.NotEqual(t, -1, cancelIdx, "dead answer left the clock armed")

	notifyIdx := outboxIndexOf[*TerminalJobFailedNotification](outbox)
	require.Less(
		t, notifyIdx, cancelIdx, "disarm precedes the job drop, so "+
			"a rejected cancel would suppress it",
	)
}

// outboxIndexOf returns the position of the first outbox message of type T,
// or -1 when the outbox carries none. Ordering assertions need the position
// rather than mere presence: processOutbox abandons the rest of the outbox on
// the first failing Tell, so whether a message is dispatched before or after a
// fallible send decides what a mid-flight error can strand.
func outboxIndexOf[T ClientOutMsg](outbox []ClientOutMsg) int {
	for i, msg := range outbox {
		if _, ok := msg.(T); ok {
			return i
		}
	}

	return -1
}

// TestBoardingOnlyReconcileTimeoutProbes pins the wavelength#1051 fix on the
// timeout handler. A boarding-only round holds no forfeit reservations, so the
// old forfeit-count gate self-looped its expiry as a defensive no-op. But the
// probe is that round's sole exit from InputSigSentState once the operator has
// rolled the round back before broadcast: no commitment can confirm and no
// failure will ever be delivered. The expiry must therefore probe and re-arm
// exactly as a forfeit-bearing round does.
func TestBoardingOnlyReconcileTimeoutProbes(t *testing.T) {
	t.Parallel()

	roundID := reconcileRoundID(0xb1)
	s := reconcileState(roundID, nil)

	tr, err := s.ProcessEvent(
		context.Background(), &StatusReconcileTimedOut{
			RoundID: roundID,
		},
		reconcileEnv(),
	)
	require.NoError(t, err)

	next, ok := tr.NextState.(*InputSigSentState)
	require.True(t, ok, "expected InputSigSentState, got %T", tr.NextState)
	require.Equal(t, uint32(1), next.ReconcileProbes)

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox

	probe, ok := findOutbox[*QueryRoundStatusOutbox](outbox)
	require.True(t, ok, "boarding-only round did not re-probe")
	require.Equal(t, roundID, probe.RoundID)

	timeoutReq, ok := findOutbox[*StartTimeoutReq](outbox)
	require.True(t, ok, "boarding-only reconcile window not re-armed")
	require.Equal(t, TimeoutPhaseStatusReconcile, timeoutReq.Phase)
}

// TestBoardingOnlyDeliveredFailureDisarms pins the disarm half of the
// wavelength#1051 invariant: the clock is armed for the whole of
// InputSigSentState, so every exit disarms it. The delivered-failure shortcut
// still fails a boarding-only round immediately (it signed nothing away, so
// nothing can strand), but it must now cancel the timer it armed on the way
// in rather than leave a one-shot running against a terminal round.
func TestBoardingOnlyDeliveredFailureDisarms(t *testing.T) {
	t.Parallel()

	roundID := reconcileRoundID(0xb2)
	s := reconcileState(roundID, nil)

	failure := &BoardingFailed{
		Reason:      "operator rolled the round back",
		Recoverable: true,
	}

	tr, err := s.ProcessEvent(context.Background(), failure, reconcileEnv())
	require.NoError(t, err)

	failed, ok := tr.NextState.(*ClientFailedState)
	require.True(t, ok, "expected ClientFailedState, got %T", tr.NextState)
	require.Equal(t, failure.Reason, failed.Reason)

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox

	cancel, ok := findOutbox[*CancelTimeoutReq](outbox)
	require.True(t, ok, "delivered failure left the reconcile clock armed")
	require.Equal(t, TimeoutPhaseStatusReconcile, cancel.Phase)
	require.Equal(t, RoundKeyStr(roundID.KeyString()), cancel.RoundKey)
}

// TestDeliveredFailureNoDisarmWhenReconcileDisabled pins the other side of
// that gate: with the reconcile opted out no clock was ever armed, so the
// failure path must not emit a cancel for a timer that does not exist.
func TestDeliveredFailureNoDisarmWhenReconcileDisabled(t *testing.T) {
	t.Parallel()

	s := reconcileState(reconcileRoundID(0xb3), nil)

	env := reconcileEnv()
	env.StatusReconcileTimeout = 0

	tr, err := s.ProcessEvent(context.Background(), &BoardingFailed{
		Reason:      "round failed",
		Recoverable: true,
	}, env)
	require.NoError(t, err)

	_, ok := tr.NextState.(*ClientFailedState)
	require.True(t, ok, "expected ClientFailedState, got %T", tr.NextState)

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	_, cancelled := findOutbox[*CancelTimeoutReq](outbox)
	require.False(t, cancelled, "cancelled a timer that was never armed")
}

// TestReconcileArmedBeforeFallibleSends pins the ordering the liveness clock
// depends on. processOutbox abandons the rest of the outbox on the first
// failing Tell, and the FSM has already checkpointed into InputSigSentState by
// the time the outbox is dispatched. Arming after the server sends would
// therefore let a mid-flight send error leave a checkpointed round with no
// clock for the rest of the session, reopening the wavelength#1051 strand
// through a different door.
func TestReconcileArmedBeforeFallibleSends(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	h.env.StatusReconcileTimeout = time.Minute

	intent := h.newTestBoardingIntent()
	state := h.newForfeitCollectingState(
		testRoundIDTr("round-arm-order"), Intents{
			Boarding: []BoardingIntent{intent},
		},
		nil,
	)

	outbox := state.forfeitCollectionOutbox(
		h.env, nil, []*types.BoardingInputSignature{{}},
	)

	armIdx := outboxIndexOf[*StartTimeoutReq](outbox)
	require.NotEqual(t, -1, armIdx, "reconcile clock never armed")

	forfeitSigsIdx := outboxIndexOf[*SubmitVTXOForfeitSigsToServer](outbox)
	boardingSigsIdx := outboxIndexOf[*SubmitForfeitSigRequest](outbox)
	confRegIdx := outboxIndexOf[*RegisterConfirmationRequest](outbox)

	fallible := map[string]int{
		"SubmitVTXOForfeitSigsToServer": forfeitSigsIdx,
		"SubmitForfeitSigRequest":       boardingSigsIdx,
		"RegisterConfirmationRequest":   confRegIdx,
	}

	for name, idx := range fallible {
		require.NotEqual(t, -1, idx, "%s missing from outbox", name)
		require.Lessf(
			t, armIdx, idx, "reconcile clock armed after %s, so "+
				"a failed send strands the checkpointed round",
			name,
		)
	}
}

// TestConfirmationDisarmTrailsNotifications pins the mirror-image ordering on
// the way out. The confirmation resolves the round's fate, but the disarm is
// cleanup and must never gate delivery: dispatching the cancel first lets a
// saturated or down timeout actor short-circuit processOutbox before
// VTXOCreatedNotification and RoundCompletedNotification, withholding
// already-persisted VTXOs from the manager and leaving onRoundComplete
// unfinalized. A cancel that never lands only leaks a one-shot timer, which
// fires into a terminal state and self-loops.
func TestConfirmationDisarmTrailsNotifications(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	h.setupMockVTXOStoreForSave()
	h.env.StatusReconcileTimeout = time.Minute

	intent := h.newTestBoardingIntent()
	state := h.newInputSigSentState(
		testRoundIDTr("round-conf-order"), []BoardingIntent{intent},
	)
	h.withState(state)

	_, err := h.sendEvent(&BoardingConfirmed{
		TxID:          state.CommitmentTx.UnsignedTx.TxHash(),
		BlockHeight:   101,
		BlockHash:     chainhash.Hash{0x01, 0x02},
		Confirmations: 6,
	})
	require.NoError(t, err)

	cancelIdx := outboxIndexOf[*CancelTimeoutReq](h.outboxMessages)
	require.NotEqual(t, -1, cancelIdx, "confirmation left the clock armed")

	doneIdx := outboxIndexOf[*RoundCompletedNotification](h.outboxMessages)
	require.NotEqual(t, -1, doneIdx, "no RoundCompletedNotification")

	require.Less(
		t, doneIdx, cancelIdx, "disarm precedes the terminal "+
			"notifications, so a rejected cancel would "+
			"withhold confirmed funds",
	)
}

// TestDeadStatusAnnouncesRoundFailure pins the emission the durable
// retirement hangs off. The actor retires a dead round's checkpoint row and
// releases its deposits when it sees a RoundFailedNotification, so a dead
// answer that fails the round in memory without announcing it leaves the row
// active and the deposit adopted forever: the wavelength#1051 strand, moved
// from the FSM into the database. Every other exit from InputSigSentState
// stays silent on purpose, so this is the only site that closes it.
func TestDeadStatusAnnouncesRoundFailure(t *testing.T) {
	t.Parallel()

	roundID := reconcileRoundID(0xc1)
	s := reconcileState(roundID, nil)

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

	_, ok := tr.NextState.(*ClientFailedState)
	require.True(t, ok, "expected ClientFailedState, got %T", tr.NextState)

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox

	notify, ok := findOutbox[*RoundFailedNotification](outbox)
	require.True(
		t, ok, "dead answer did not announce the failure, so the "+
			"round is never retired and its deposit stays adopted",
	)
	require.Equal(t, fn.Some(roundID), notify.RoundID)

	// The announcement is a delivery, so the disarm trails it for the same
	// reason it trails the release and the job drop.
	notifyIdx := outboxIndexOf[*RoundFailedNotification](outbox)
	cancelIdx := outboxIndexOf[*CancelTimeoutReq](outbox)
	require.NotEqual(t, -1, cancelIdx, "dead answer left the clock armed")
	require.Less(
		t, notifyIdx, cancelIdx, "disarm precedes the "+
			"announcement, so a rejected cancel would suppress "+
			"the retirement",
	)
}

// TestDeliveredFailureDoesNotAnnounce pins the deliberate silence on the
// delivered-failure shortcut. handleCancelRound injects a synthetic
// BoardingFailed onto this same branch, and a local cancel proves nothing
// about the operator: the commitment may still broadcast. Announcing here
// would retire the round and hand the deposit back to the boardable pool
// while it can still be spent by that commitment.
func TestDeliveredFailureDoesNotAnnounce(t *testing.T) {
	t.Parallel()

	s := reconcileState(reconcileRoundID(0xc2), nil)

	tr, err := s.ProcessEvent(context.Background(), &BoardingFailed{
		Reason:      "User requested cancellation",
		Recoverable: true,
	}, reconcileEnv())
	require.NoError(t, err)

	_, ok := tr.NextState.(*ClientFailedState)
	require.True(t, ok, "expected ClientFailedState, got %T", tr.NextState)

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	_, announced := findOutbox[*RoundFailedNotification](outbox)
	require.False(
		t, announced, "a delivered failure retired the round, "+
			"which releases the deposit on a verdict that does "+
			"not rule out the commitment",
	)
}

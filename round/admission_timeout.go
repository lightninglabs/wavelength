package round

import (
	"context"
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/lib/types"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// defaultAdmissionTimeout bounds the entire interactive ceremony after the
// operator's acceptance, including the wait for the first fee quote.
const defaultAdmissionTimeout = 30 * time.Minute

// admissionDeadlinePoll bounds detection of forward wall-clock corrections.
// Timers are wakeups; the durable absolute deadline remains authoritative.
const admissionDeadlinePoll = time.Second

// TimeoutPhaseAdmission covers every accepted state before InputSigSent.
const TimeoutPhaseAdmission TimeoutPhase = "admission"

// AdmissionTimedOut asks the FSM to check its original admission budget. A
// wakeup for another attempt cannot release the current attempt's inputs.
type AdmissionTimedOut struct {
	// RoundID identifies the attempt that scheduled this wakeup.
	RoundID RoundID
}

// clientEventSealed marks the deadline wakeup as a round event.
func (*AdmissionTimedOut) clientEventSealed() {}

// admissionBudget holds one FSM's durable deadline and process-local monotonic
// cutoff. The latter prevents a backward clock correction extending a live
// attempt. Restart abandons ephemeral attempts instead of resetting this clock.
type admissionBudget struct {
	roundID  RoundID
	deadline AdmissionDeadline
	cutoff   time.Time
}

// expired observes both absolute expiry and elapsed process time. Real Now
// values carry monotonic time, so either direction of wall-clock correction
// can shorten, but cannot extend, the original participation budget.
func (b *admissionBudget) expired(now time.Time) bool {
	return b.deadline.Closed ||
		now.UnixNano() >= b.deadline.ExpiresAt.UnixNano() ||
		!now.Before(b.cutoff)
}

// admissionWakeup arms an independent check before any fallible remote send.
func admissionWakeup(roundID RoundID) ClientOutMsg {
	return &StartTimeoutReq{
		RoundKey: RoundKeyStr(roundID.KeyString()),
		Phase:    TimeoutPhaseAdmission,
		Duration: admissionDeadlinePoll,
	}
}

// constrainAdmission persists the budget before processing admission.
// Repeated admission may shorten, but never renew, either clock.
func (e *ClientEnvironment) constrainAdmission(ctx context.Context,
	roundID RoundID, expiresAt time.Time) error {

	now := e.now()
	deadline, err := e.RoundStore.ConstrainAdmissionDeadline(
		ctx, roundID, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("persist admission deadline: %w", err)
	}

	cutoff := now.Add(deadline.ExpiresAt.Sub(now))
	if e.admission != nil && e.admission.cutoff.Before(cutoff) {
		cutoff = e.admission.cutoff
	}
	e.admission = &admissionBudget{
		roundID:  roundID,
		deadline: deadline,
		cutoff:   cutoff,
	}

	return nil
}

// processPreCheckpointEvent checks expiry at the owning FSM boundary, including
// internal signing events. It deliberately has no checkpointed-state caller:
// timeout cannot prove that signatures already handed off are unspendable.
func processPreCheckpointEvent(ctx context.Context, state ClientState,
	event ClientEvent, env *ClientEnvironment, roundID RoundID,
	forfeits []types.ForfeitRequest,
	process func(context.Context, ClientEvent, *ClientEnvironment) (
		*ClientStateTransition, error)) (*ClientStateTransition,
	error) {

	budget := env.admission
	wake, isWake := event.(*AdmissionTimedOut)
	if isWake && (budget == nil || wake.RoundID != budget.roundID) {
		return selfLoop(state), nil
	}

	if budget != nil && budget.expired(env.now()) {
		// No input/forfeit signature has left the client at this
		// boundary. Cleanup stays on the existing failure path,
		// including custom forfeit actors. Ephemeral nonce sessions are
		// never reused.
		cleanupErr := cleanupSignerSessions(
			signingSessionsFromState(state),
		)
		tr := failWithNotification(
			"round admission deadline expired", cleanupErr, true,
			fn.Some(budget.roundID),
		)

		return finishAdmissionTransition(
			ctx, env, tr, nil, budget.roundID, forfeits,
		)
	}

	if isWake {
		return &ClientStateTransition{
			NextState: state,
			NewEvents: fn.Some(
				ClientEmittedEvent{
					Outbox: []ClientOutMsg{
						admissionWakeup(budget.roundID),
					},
				},
			),
		}, nil
	}

	tr, err := process(ctx, event, env)

	return finishAdmissionTransition(ctx, env, tr, err, roundID, forfeits)
}

// finishAdmissionTransition uses the existing rollback path and closes failed
// admission. A failed fence write must not suppress safe local rollback; the
// terminal FSM rejects late messages and startup retries the durable fence.
func finishAdmissionTransition(ctx context.Context, env *ClientEnvironment,
	tr *ClientStateTransition, err error, roundID RoundID,
	forfeits []types.ForfeitRequest) (*ClientStateTransition, error) {

	tr, err = releaseForfeitsOnFailure(tr, err, fn.Some(roundID), forfeits)
	if tr == nil || env.admission == nil {
		return tr, err
	}
	if _, failed := tr.NextState.(*ClientFailedState); !failed {
		return tr, err
	}

	closeErr := env.RoundStore.CloseAdmissionDeadline(
		ctx, env.admission.roundID,
	)
	if closeErr != nil {
		env.Log.WarnS(ctx, "Failed to close round admission", closeErr)
	}

	return tr, err
}

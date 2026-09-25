package round

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/lightninglabs/wavelength/lib/batchschedule"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// scheduledTermsRefreshTimeout bounds the discovery refresh performed before a
// scheduled attempt selects its slot. The refresh runs while the round actor
// waits on this FSM, so it is kept short even though the actor's own context
// is long-lived.
const scheduledTermsRefreshTimeout = 5 * time.Second

// scheduledAttempt is the slot a registration attempt pinned when it first
// asked to join a scheduled operator, together with everything needed to hit
// that slot's window on the operator's clock. It is fixed for the life of the
// attempt: a missed window fails the attempt rather than sliding it to a later
// slot, because the selection is bound into the signed join.
type scheduledAttempt struct {
	// selection is the schedule identity and cutoff signed into the join.
	selection batchschedule.Selection

	// slot is the published window the selection refers to.
	slot batchschedule.Slot

	// clockOffset is the estimated operator clock minus the local clock,
	// taken from the discovery refresh that produced the selection.
	clockOffset time.Duration

	// wake is the instant, on the operator's clock, at which the attempt
	// sends its join. It is drawn uniformly from the window's wake bounds
	// so the slot's clients do not all arrive in the same instant.
	wake time.Time

	// margin is the minimum time that must remain before the cutoff, on
	// the operator's clock, for a join to be sent at all. It covers the
	// residual error in clockOffset.
	margin time.Duration
}

// operatorNow returns the current time on the operator's clock as estimated
// for this attempt.
func (a *scheduledAttempt) operatorNow(env *ClientEnvironment) time.Time {
	return env.now().Add(a.clockOffset)
}

// newScheduledAttempt selects the first published slot that still has at least
// MaxWakeMargin left before its cutoff on the operator's clock, and draws the
// attempt's wake time within that slot's window.
func newScheduledAttempt(published batchschedule.Published,
	localNow time.Time) (*scheduledAttempt, error) {

	// Skip a slot that closes within the send margin: an attempt pinned to
	// it could only fail, while the next slot can still be reached.
	offset := published.ClockOffset()
	slot, err := published.Next(
		localNow.Add(offset).Add(batchschedule.MaxWakeMargin),
	)
	if err != nil {
		return nil, err
	}

	lo, hi := batchschedule.WakeBounds(slot.Window())
	jitter := lo
	if hi > lo {
		jitter += rand.N(hi - lo)
	}

	return &scheduledAttempt{
		selection: batchschedule.Selection{
			ScheduleID: published.ID(),
			Cutoff:     slot.Cutoff,
		},
		slot:        slot,
		clockOffset: offset,
		wake:        slot.Opens.Add(jitter),
		margin:      lo,
	}, nil
}

// batchSlot returns the selection to sign into the join, or None when the
// operator uses event-driven registration.
func (e *ClientEnvironment) batchSlot() fn.Option[batchschedule.Selection] {
	if e.scheduled == nil {
		return fn.None[batchschedule.Selection]()
	}

	return fn.Some(e.scheduled.selection)
}

// refreshOperatorTerms fetches fresh discovery for a new scheduled selection.
// A scheduled operator's published list rolls forward every interval, so the
// terms cached at startup may no longer list the next slot.
func (e *ClientEnvironment) refreshOperatorTerms(ctx context.Context) error {
	if e.OperatorTermsSource == nil {
		return nil
	}

	refreshCtx, cancel := context.WithTimeout(
		ctx, scheduledTermsRefreshTimeout,
	)
	defer cancel()

	terms, err := e.OperatorTermsSource.FreshOperatorTerms(refreshCtx)
	if err != nil {
		return fmt.Errorf("refresh batch schedule: %w", err)
	}

	if terms == nil {
		return fmt.Errorf("missing refreshed operator terms")
	}

	e.OperatorTerms = terms

	return nil
}

// waitForScheduledSlot holds a registration attempt until its scheduled wake
// time. It returns a nil transition when the attempt may proceed to build its
// join: either the operator is event-driven, or the wake time has arrived.
// Otherwise it returns a self-transition that arms a wakeup timer.
//
// The first call on an attempt refreshes discovery and pins a slot; later
// calls, driven by the wakeup timer, reuse that pin. This bounds a connected
// attempt only; durable waiting across restarts is a separate mechanism.
func (s *PendingRoundAssembly) waitForScheduledSlot(ctx context.Context,
	env *ClientEnvironment) (*ClientStateTransition, error) {

	if env.scheduled == nil {
		if err := env.refreshOperatorTerms(ctx); err != nil {
			return nil, err
		}

		// An operator without a published schedule keeps the
		// event-driven flow.
		terms := env.OperatorTerms
		if terms == nil || terms.BatchSchedule.IsNone() {
			return nil, nil
		}

		attempt, err := newScheduledAttempt(
			terms.BatchSchedule.UnsafeFromSome(), env.now(),
		)
		if err != nil {
			return nil, err
		}
		env.scheduled = attempt
	}

	attempt := env.scheduled
	now := attempt.operatorNow(env)

	switch {
	// The pinned window has closed, or so little of it remains that the
	// join could land after the cutoff. The selection never slides to a
	// later slot.
	case attempt.slot.Cutoff.Sub(now) < attempt.margin:
		return nil, fmt.Errorf("scheduled registration window for "+
			"cutoff %v closed", attempt.slot.Cutoff)

	// The wake time has arrived, so build and send the join.
	case !now.Before(attempt.wake):
		return nil, nil
	}

	// Sleep until the wake time. The timer measures local time, and the
	// offset is constant, so the operator-clock gap is the same duration.
	return &ClientStateTransition{
		NextState: s,
		NewEvents: fn.Some(ClientEmittedEvent{Outbox: []ClientOutMsg{
			&StartTimeoutReq{
				RoundKey: env.RoundKey,
				Phase:    TimeoutPhaseScheduledRegistration,
				Duration: attempt.wake.Sub(now),
			},
		}}),
	}, nil
}

// validateScheduledSend checks the pinned cutoff again after the wallet and
// chain calls that build the join, which can take long enough to cross it.
// The operator still enforces the window on receipt; this check only avoids
// sending a join that is already known to be late.
func (e *ClientEnvironment) validateScheduledSend() error {
	if e.scheduled == nil {
		return nil
	}

	attempt := e.scheduled
	remaining := attempt.slot.Cutoff.Sub(attempt.operatorNow(e))
	if remaining < attempt.margin {
		return fmt.Errorf("scheduled registration window for cutoff "+
			"%v closed during preparation", attempt.slot.Cutoff)
	}

	return nil
}

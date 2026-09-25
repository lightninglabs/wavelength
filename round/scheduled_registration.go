package round

import (
	"context"
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/lib/batchschedule"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// waitForScheduledSlot delays authentication until the registration window.
// A chosen cutoff never slides forward after a missed wakeup. This bounds the
// connected attempt; durable waiting-intent replay is a separate concern.
func (s *PendingRoundAssembly) waitForScheduledSlot(ctx context.Context,
	env *ClientEnvironment) (*ClientStateTransition, error) {

	if env.scheduledSlot == nil && env.OperatorTermsSource != nil {
		// Discovery belongs to this selection, not a background worker.
		// Bound the RPC even when the FSM has a long-lived actor
		// context.
		refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		terms, err := env.OperatorTermsSource(refreshCtx)
		if err != nil {
			return nil, fmt.Errorf("refresh batch schedule: %w",
				err)
		}
		if terms == nil {
			return nil, fmt.Errorf("missing refreshed operator " +
				"terms")
		}
		env.OperatorTerms = terms
	}
	if env.OperatorTerms == nil || env.OperatorTerms.BatchSchedule == nil {
		return nil, nil
	}
	schedule := env.OperatorTerms.BatchSchedule
	now := env.now()
	if env.scheduledSlot == nil {
		slot, err := schedule.Next(now)
		if err != nil {
			return nil, err
		}
		env.scheduledWindow = slot
		env.scheduledSlot = &batchschedule.Selection{
			ScheduleID: schedule.ID(),
			CutoffUnix: uint64(
				slot.Cutoff.Unix(),
			),
		}
	}
	cutoff := time.Unix(int64(env.scheduledSlot.CutoffUnix), 0)
	if !now.Before(cutoff) {
		return nil, fmt.Errorf("scheduled registration window closed")
	}
	opens := env.scheduledWindow.Opens
	if !now.Before(opens) {
		return nil, nil
	}

	return &ClientStateTransition{
		NextState: s,
		NewEvents: fn.Some(ClientEmittedEvent{Outbox: []ClientOutMsg{
			&StartTimeoutReq{
				RoundKey: env.RoundKey,
				Phase:    TimeoutPhaseScheduledRegistration,
				Duration: opens.Sub(now),
			},
		}}),
	}, nil
}

// validateScheduledSend checks the deadline again after wallet and chain calls.
// The server still enforces receipt time; this avoids sending a known-late
// join.
func (e *ClientEnvironment) validateScheduledSend() error {
	if e.scheduledSlot == nil {
		return nil
	}
	cutoff := time.Unix(int64(e.scheduledSlot.CutoffUnix), 0)
	if !e.now().Before(cutoff) {
		return fmt.Errorf("scheduled registration window closed " +
			"during preparation")
	}

	return nil
}

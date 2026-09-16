package round

import (
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/lib/batchschedule"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// waitForScheduledSlot delays authentication until the registration window.
// A chosen cutoff never slides forward after a missed wakeup. This bounds the
// connected attempt; durable waiting-intent replay is a separate concern.
func (s *PendingRoundAssembly) waitForScheduledSlot(env *ClientEnvironment) (
	*ClientStateTransition, error) {

	if env.OperatorTerms == nil || env.OperatorTerms.BatchSchedule == nil {
		return nil, nil
	}
	schedule := env.OperatorTerms.BatchSchedule
	now := env.now()
	if env.scheduledSlot == nil {
		after := now
		if cursor := env.OperatorTerms.NextBatchCutoff; cursor.After(
			after,
		) {

			after = cursor.Add(-time.Nanosecond)
		}
		slot, err := schedule.Next(after)
		if err != nil {
			return nil, err
		}
		env.scheduledSlot = &batchschedule.Selection{
			ScheduleID: schedule.ID(), CutoffUnix: uint64(
				slot.Cutoff.Unix(),
			),
		}
	}
	cutoff := time.Unix(int64(env.scheduledSlot.CutoffUnix), 0)
	if !now.Before(cutoff) {
		return nil, fmt.Errorf("scheduled registration window closed")
	}
	opens := cutoff.Add(-schedule.Window())
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

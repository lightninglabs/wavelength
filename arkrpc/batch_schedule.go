package arkrpc

import (
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/lib/batchschedule"
)

// batchScheduleVersion is the only discovery format this package understands:
// an opaque schedule identity plus a list of concrete registration windows.
const batchScheduleVersion = 1

// PublishedBatchSlots is how many upcoming slots an interval-based operator
// publishes in each discovery response, starting at its admission cursor.
const PublishedBatchSlots = 8

// ParseBatchSchedule converts a discovery message into the client's view of
// the operator's published slots. A nil message means the operator uses
// event-driven registration, and yields a nil result with no error.
//
// Validation is deliberately independent of any interval formula: the list
// must be well formed on its own terms, and batchschedule.NewPublished checks
// ordering and window bounds. A malformed schedule is an error rather than a
// silent fallback to event-driven registration, because the operator will
// reject joins that carry no slot.
func ParseBatchSchedule(p *BatchSchedule) (*batchschedule.Published, error) {
	if p == nil {
		return nil, nil
	}

	if p.Version != batchScheduleVersion {
		return nil, fmt.Errorf("unsupported schedule version %d",
			p.Version)
	}

	id, err := batchschedule.IDFromBytes(p.ScheduleId)
	if err != nil {
		return nil, err
	}

	if len(p.Slots) == 0 || len(p.Slots) > batchschedule.MaxPublishedSlots {
		return nil, fmt.Errorf("published schedule has %d slots, "+
			"want 1 to %d", len(p.Slots),
			batchschedule.MaxPublishedSlots)
	}

	slots := make([]batchschedule.Slot, 0, len(p.Slots))
	for _, slot := range p.Slots {
		if slot == nil {
			return nil, fmt.Errorf("nil published slot")
		}

		slots = append(slots, batchschedule.Slot{
			Opens:  time.Unix(slot.RegistrationOpensUnix, 0),
			Cutoff: time.Unix(slot.CutoffUnix, 0),
		})
	}

	published, err := batchschedule.NewPublished(id, slots)
	if err != nil {
		return nil, err
	}

	// Keep the operator's clock reading so a caller that timed the
	// request can estimate the clock offset. A non-positive value means
	// the operator did not report one.
	if p.ServerTimeUnix > 0 {
		published = published.WithServerTime(
			time.Unix(p.ServerTimeUnix, 0).UTC(),
		)
	}

	return published, nil
}

// BatchScheduleToProto builds the discovery message an interval-based
// operator publishes. It lists PublishedBatchSlots slots starting at cutoff,
// the operator's durable admission cursor, and stamps now as the server time.
// The interval itself is never published, so clients cannot extrapolate past
// the list. A nil schedule yields nil, preserving event-driven registration.
func BatchScheduleToProto(s *batchschedule.Schedule,
	now, cutoff time.Time) *BatchSchedule {

	if s == nil {
		return nil
	}

	id := s.ID()
	p := &BatchSchedule{
		Version:        batchScheduleVersion,
		ScheduleId:     id[:],
		ServerTimeUnix: now.Unix(),
	}

	// Walk forward from the cursor. A cursor that is off the grid, or a
	// slot past the supported timestamp range, ends the list early.
	slot, err := s.At(cutoff)
	for i := 0; i < PublishedBatchSlots && err == nil; i++ {
		p.Slots = append(p.Slots, &BatchSlot{
			RegistrationOpensUnix: slot.Opens.Unix(),
			CutoffUnix:            slot.Cutoff.Unix(),
		})

		slot, err = s.Next(slot.Cutoff)
	}

	return p
}

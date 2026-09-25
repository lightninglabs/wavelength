package arkrpc

import (
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/lib/batchschedule"
)

// PublishedBatchSlots is the rolling discovery horizon for interval operators.
const PublishedBatchSlots = 8

// ParseBatchSchedule validates concrete opportunities without deriving slots
// from a cadence. Absence preserves legacy event-driven registration.
func ParseBatchSchedule(p *BatchSchedule) (*batchschedule.Published, error) {
	if p == nil {
		return nil, nil
	}
	if p.Version != 1 {
		return nil, fmt.Errorf("unsupported schedule version %d",
			p.Version)
	}
	if len(p.ScheduleId) != 32 || len(p.Slots) == 0 ||
		len(p.Slots) > batchschedule.MaxPublishedSlots {
		return nil, fmt.Errorf("invalid published schedule identity " +
			"or size")
	}
	var id [32]byte
	copy(id[:], p.ScheduleId)
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

	return batchschedule.NewPublished(id, slots)
}

// BatchScheduleToProto publishes concrete slots from the operator's durable
// admission cursor. The interval remains an operator implementation detail.
func BatchScheduleToProto(s *batchschedule.Schedule,
	now, cutoff time.Time) *BatchSchedule {

	if s == nil {
		return nil
	}
	id := s.ID()
	p := &BatchSchedule{
		Version:        1,
		ScheduleId:     id[:],
		ServerTimeUnix: now.Unix(),
	}
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

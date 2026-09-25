package batchschedule

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// MaxPublishedSlots bounds discovery parsing and storage.
const MaxPublishedSlots = 32

// ErrScheduleExhausted requires rediscovery instead of extrapolating slots.
var ErrScheduleExhausted = errors.New("published batch schedule exhausted")

// Published is an immutable set of advertised registration opportunities.
// Its identity is opaque: clients need not know how an operator schedules work.
type Published struct {
	id    [32]byte
	slots []Slot
}

// NewPublished validates a bounded, ordered list without requiring a cadence.
func NewPublished(id [32]byte, slots []Slot) (*Published, error) {
	if id == ([32]byte{}) || len(slots) == 0 ||
		len(slots) > MaxPublishedSlots {
		return nil, fmt.Errorf("invalid published schedule identity " +
			"or size")
	}
	for i, slot := range slots {
		if slot.Opens.Unix() < 0 ||
			slot.Cutoff.Unix() > maxUnixSecond ||
			slot.Opens.Nanosecond() != 0 ||
			slot.Cutoff.Nanosecond() != 0 ||
			!slot.Opens.Before(slot.Cutoff) {
			return nil, fmt.Errorf("invalid published slot %d", i)
		}
		if slot.Cutoff.Sub(slot.Opens) > MaxRegistrationWindow {
			return nil, fmt.Errorf("published slot %d window "+
				"too long", i)
		}
		if i > 0 && slot.Opens.Before(slots[i-1].Cutoff) {
			return nil, fmt.Errorf("published slots overlap or " +
				"are unordered")
		}
	}

	return &Published{
		id:    id,
		slots: slices.Clone(slots),
	}, nil
}

// ID returns the operator's timing-policy identity, stable as the list rolls.
func (p *Published) ID() [32]byte { return p.id }

// Next selects a listed future cutoff, never synthesizing another opportunity.
func (p *Published) Next(after time.Time) (Slot, error) {
	for _, slot := range p.slots {
		if after.Before(slot.Cutoff) {
			return slot, nil
		}
	}

	return Slot{}, ErrScheduleExhausted
}

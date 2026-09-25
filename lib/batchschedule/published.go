package batchschedule

import (
	"errors"
	"fmt"
	"time"
)

// MaxPublishedSlots bounds how many slots a client will parse and store from
// a single discovery response.
const MaxPublishedSlots = 32

// ErrScheduleExhausted is returned when every published slot has passed. The
// client must fetch fresh discovery rather than extrapolate a new slot.
var ErrScheduleExhausted = errors.New("published batch schedule exhausted")

// Published is the client's immutable view of an operator's advertised
// registration opportunities. Its identity is opaque: a client selects from
// the listed slots and never needs to know how the operator generated them.
type Published struct {
	// id is the operator's timing-policy identity. It stays the same as
	// the published list rolls forward under an unchanged policy.
	id ID

	// slots is the chronological, non-overlapping list of opportunities.
	slots []Slot
}

// NewPublished validates an advertised slot list. The list must be non-empty,
// no longer than MaxPublishedSlots, ordered and non-overlapping, and every
// window must be a positive whole number of seconds no longer than
// MaxRegistrationWindow. Gaps between windows are allowed, so an operator is
// free to publish an irregular calendar.
func NewPublished(id ID, slots []Slot) (*Published, error) {
	if id.IsZero() || len(slots) == 0 ||
		len(slots) > MaxPublishedSlots {
		return nil, fmt.Errorf("invalid published schedule identity " +
			"or size")
	}

	for i, slot := range slots {
		switch {
		case !isWholeSecond(slot.Opens) || slot.Opens.Unix() < 0 ||
			!inRange(slot.Cutoff) ||
			!slot.Opens.Before(slot.Cutoff):
			return nil, fmt.Errorf("invalid published slot %d", i)

		case slot.Window() > MaxRegistrationWindow:
			return nil, fmt.Errorf("published slot %d window "+
				"too long", i)

		// Each window must open no earlier than the previous cutoff,
		// which rules out both overlap and misordering.
		case i > 0 && slot.Opens.Before(slots[i-1].Cutoff):
			return nil, fmt.Errorf("published slots overlap or " +
				"are unordered")
		}
	}

	normalized := make([]Slot, len(slots))
	for i, slot := range slots {
		normalized[i] = Slot{
			Opens:  slot.Opens.UTC(),
			Cutoff: slot.Cutoff.UTC(),
		}
	}

	return &Published{
		id:    id,
		slots: normalized,
	}, nil
}

// ID returns the operator's timing-policy identity.
func (p *Published) ID() ID {
	return p.id
}

// Next returns the first listed slot whose cutoff is strictly after the given
// instant. It never synthesizes a slot beyond the list.
func (p *Published) Next(after time.Time) (Slot, error) {
	for _, slot := range p.slots {
		if after.Before(slot.Cutoff) {
			return slot, nil
		}
	}

	return Slot{}, ErrScheduleExhausted
}

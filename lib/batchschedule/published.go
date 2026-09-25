package batchschedule

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// MaxPublishedSlots bounds how many slots a client will parse and store from
// a single discovery response.
const MaxPublishedSlots = 32

// maxWakeMargin caps the earliest point after a window opens at which a client
// sends its join. It absorbs the residual error of the clock offset estimate:
// up to half a second from the operator truncating its clock reading to whole
// seconds, plus the asymmetry of the discovery round trip.
const maxWakeMargin = 2 * time.Second

// ErrScheduleExhausted is returned when every published slot has passed. The
// client must fetch fresh discovery rather than extrapolate a new slot.
var ErrScheduleExhausted = errors.New("published batch schedule exhausted")

// Published is the client's immutable view of an operator's advertised
// registration opportunities. It is a value type: copies share the slot slice,
// which nothing mutates after construction. Its identity is opaque: a client
// selects from the listed slots and never needs to know how the operator
// generated them.
type Published struct {
	// id is the operator's timing-policy identity. It stays the same as
	// the published list rolls forward under an unchanged policy.
	id ID

	// slots is the chronological, non-overlapping list of opportunities.
	slots []Slot

	// serverTime is the operator's clock reading when it built the
	// response, truncated to whole seconds. It is the zero time when the
	// operator did not report one.
	serverTime time.Time

	// clockOffset is the estimated amount to add to the local clock to
	// obtain the operator's clock. It is zero until a caller that timed
	// the discovery round trip attaches an estimate with WithClockOffset.
	clockOffset time.Duration
}

// NewPublished validates an advertised slot list. The list must be non-empty,
// no longer than MaxPublishedSlots, ordered and non-overlapping, and every
// window must be a positive whole number of seconds no longer than
// MaxRegistrationWindow. Gaps between windows are allowed, so an operator is
// free to publish an irregular calendar.
func NewPublished(id ID, slots []Slot) (Published, error) {
	if id.IsZero() || len(slots) == 0 ||
		len(slots) > MaxPublishedSlots {
		return Published{}, fmt.Errorf("invalid published schedule " +
			"identity or size")
	}

	for i, slot := range slots {
		switch {
		case !isWholeSecond(slot.Opens) || slot.Opens.Unix() < 0 ||
			!inRange(slot.Cutoff) ||
			!slot.Opens.Before(slot.Cutoff):
			return Published{}, fmt.Errorf("invalid "+
				"published slot %d", i)

		case slot.Window() > MaxRegistrationWindow:
			return Published{}, fmt.Errorf("published slot %d "+
				"window too long", i)

		// Each window must open no earlier than the previous cutoff,
		// which rules out both overlap and misordering.
		case i > 0 && slot.Opens.Before(slots[i-1].Cutoff):
			return Published{}, fmt.Errorf("published slots " +
				"overlap or are unordered")
		}
	}

	normalized := make([]Slot, len(slots))
	for i, slot := range slots {
		normalized[i] = Slot{
			Opens:  slot.Opens.UTC(),
			Cutoff: slot.Cutoff.UTC(),
		}
	}

	return Published{
		id:    id,
		slots: normalized,
	}, nil
}

// WithServerTime returns a copy of the list that records the operator's
// clock reading from the same discovery response. A zero time means the
// operator did not report one.
func (p Published) WithServerTime(serverTime time.Time) Published {
	p.slots = slices.Clone(p.slots)
	p.serverTime = serverTime

	return p
}

// WithClockOffset returns a copy of the list that carries an estimate of how
// far the operator's clock is ahead of the local clock.
func (p Published) WithClockOffset(offset time.Duration) Published {
	p.slots = slices.Clone(p.slots)
	p.clockOffset = offset

	return p
}

// ID returns the operator's timing-policy identity.
func (p Published) ID() ID {
	return p.id
}

// ServerTime returns the operator's clock reading from discovery, or the zero
// time if it was not reported.
func (p Published) ServerTime() time.Time {
	return p.serverTime
}

// ClockOffset returns the estimated operator clock minus local clock.
func (p Published) ClockOffset() time.Duration {
	return p.clockOffset
}

// Next returns the first listed slot whose cutoff is strictly after the given
// instant, which must already be expressed on the operator's clock. It never
// synthesizes a slot beyond the list.
func (p Published) Next(after time.Time) (Slot, error) {
	for _, slot := range p.slots {
		if after.Before(slot.Cutoff) {
			return slot, nil
		}
	}

	return Slot{}, ErrScheduleExhausted
}

// EstimateClockOffset estimates how far the operator's clock is ahead of the
// local clock from one discovery round trip. The operator stamped serverTime
// somewhere between sent and received, and truncated it to whole seconds, so
// the estimate compares serverTime plus half a second against the midpoint of
// the round trip. A zero serverTime yields a zero offset.
func EstimateClockOffset(serverTime, sent, received time.Time) time.Duration {
	if serverTime.IsZero() || received.Before(sent) {
		return 0
	}

	midpoint := sent.Add(received.Sub(sent) / 2)

	return serverTime.Add(time.Second / 2).Sub(midpoint)
}

// WakeBounds returns the range, measured from a window's opening on the
// operator's clock, within which a client should send its join.
//
// The lower bound is a safety margin: a join sent any earlier could still
// arrive before the window opens once the residual clock offset error is
// counted. The upper bound is half the window, which spreads a slot's joins
// across the first half of the window instead of the same instant and
// leaves the second half for preparation and transit. A join with less than
// the lower bound remaining before the cutoff is treated as closed.
func WakeBounds(window time.Duration) (lo, hi time.Duration) {
	lo = min(maxWakeMargin, window/4)
	hi = max(lo, window/2)

	return lo, hi
}

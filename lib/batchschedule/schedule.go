// Package batchschedule defines anchored UTC registration opportunities for
// scheduled batches.
//
// An operator that opts into scheduled batches runs a fixed UTC timetable:
// cutoffs fall at anchor + n*interval, and each cutoff is preceded by a short
// registration window. At a cutoff the operator stops admitting joins and
// starts quoting and signing; the cutoff says nothing about when the batch
// transaction is broadcast or confirmed.
//
// The package has two halves. Schedule is the operator's view: the anchored
// arithmetic that generates slots from configuration. Published is the
// client's view: an immutable list of concrete slots received through
// discovery, which the client selects from without ever reconstructing the
// timetable. Selection binds one chosen slot into a join authorization.
//
// Every instant handled here is a whole UTC second. Schedules and selections
// reject sub-second precision at construction, so equality, hashing and
// wire encoding never depend on a monotonic clock reading or a time zone.
package batchschedule

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// MaxRegistrationWindow bounds how long a registration window may stay open.
// A connected client holds its inputs from registration until the cutoff, so
// the window caps that reservation. Longer waiting belongs in a durable
// waiting room outside the signing attempt.
const MaxRegistrationWindow = 5 * time.Minute

// maxUnixSecond is the last second of year 9999. Capping timestamps here keeps
// every published instant representable in RFC 3339 and keeps the interval
// arithmetic in Next well away from int64 overflow.
const maxUnixSecond int64 = 253402300799

// ID is the opaque identity of an operator's timing policy. It stays the same
// while the policy is unchanged, so together with a cutoff it names one slot.
// Clients never interpret its contents.
type ID [32]byte

// IsZero reports whether the identity is unset.
func (id ID) IsZero() bool {
	return id == ID{}
}

// IDFromBytes parses an identity from its wire form, rejecting anything other
// than exactly 32 bytes so a malformed value is never truncated or padded.
func IDFromBytes(b []byte) (ID, error) {
	var id ID
	if len(b) != len(id) {
		return ID{}, fmt.Errorf("schedule identity must be %d "+
			"bytes, got %d", len(id), len(b))
	}

	copy(id[:], b)

	return id, nil
}

// String returns the identity in hex for logs and errors.
func (id ID) String() string {
	return hex.EncodeToString(id[:])
}

// Schedule is an operator's immutable UTC timetable. Cutoffs fall at
// Anchor + n*Interval for every integer n >= 0, and registration for each
// cutoff opens Window before it. Use New to construct a validated Schedule.
type Schedule struct {
	// anchor is the reference cutoff. It is a whole UTC second between the
	// Unix epoch and the end of year 9999.
	anchor time.Time

	// interval is the positive, whole-second spacing between consecutive
	// cutoffs.
	interval time.Duration

	// window is how long before each cutoff registration opens. It is a
	// positive whole number of seconds, no longer than interval and no
	// longer than MaxRegistrationWindow.
	window time.Duration

	// id is the timetable's identity, derived from the three parameters
	// above so that any change to them produces a different identity.
	id ID
}

// Slot is one registration opportunity. Registration is accepted over the
// half-open interval [Opens, Cutoff). Cutoff marks the start of quoting, not
// transaction broadcast or confirmation.
type Slot struct {
	// Opens is the first instant at which a join for this slot is
	// accepted.
	Opens time.Time

	// Cutoff is the first instant at which joins for this slot are
	// rejected and the operator begins quoting.
	Cutoff time.Time
}

// Window returns how long registration for the slot stays open.
func (s Slot) Window() time.Duration {
	return s.Cutoff.Sub(s.Opens)
}

// Selection names the single slot a join authorization is valid for. It is
// bound into the signed join-authorization message, so the operator can admit
// the join only into the collector for that exact schedule and cutoff.
type Selection struct {
	// ScheduleID is the opaque timetable identity published in discovery.
	ScheduleID ID

	// Cutoff is the selected slot's cutoff, a whole UTC second.
	Cutoff time.Time
}

// Validate rejects a selection with no schedule identity or a cutoff that
// cannot be encoded as a positive whole Unix second.
func (s *Selection) Validate() error {
	switch {
	case s.ScheduleID.IsZero():
		return fmt.Errorf("scheduled slot selection has no schedule " +
			"identity")

	case !inRange(s.Cutoff):
		return fmt.Errorf("scheduled slot selection has invalid "+
			"cutoff %v", s.Cutoff)
	}

	return nil
}

// Matches reports whether the selection names the given schedule and cutoff.
// Instants are compared with time.Time.Equal, so location and monotonic clock
// readings never affect the result.
func (s *Selection) Matches(id ID, cutoff time.Time) bool {
	return s.ScheduleID == id && s.Cutoff.Equal(cutoff)
}

// SelectionFromUnix builds a selection from its wire form, where the cutoff
// travels as Unix seconds. The result is validated.
func SelectionFromUnix(id ID, cutoffUnix uint64) (*Selection, error) {
	if cutoffUnix > uint64(maxUnixSecond) {
		return nil, fmt.Errorf("scheduled slot cutoff %d is out "+
			"of range", cutoffUnix)
	}

	selection := &Selection{
		ScheduleID: id,
		Cutoff:     time.Unix(int64(cutoffUnix), 0).UTC(),
	}
	if err := selection.Validate(); err != nil {
		return nil, err
	}

	return selection, nil
}

// CutoffUnix returns the cutoff in its wire form. It must only be called on a
// validated selection, which guarantees a positive value.
func (s *Selection) CutoffUnix() uint64 {
	return uint64(s.Cutoff.Unix())
}

// New constructs a timetable from a UTC anchor, an interval and a registration
// window. All three must have whole-second precision; the window must be
// positive, no longer than the interval and no longer than
// MaxRegistrationWindow.
func New(anchor time.Time, interval, window time.Duration) (*Schedule, error) {
	switch {
	// The anchor must be a representable whole second so that every
	// cutoff derived from it is too.
	case !isWholeSecond(anchor) || anchor.Unix() < 0 ||
		anchor.Unix() > maxUnixSecond:
		return nil, fmt.Errorf("anchor must be a whole UTC second " +
			"from 1970 through 9999")

	case interval <= 0 || interval%time.Second != 0:
		return nil, fmt.Errorf("interval must be a positive whole " +
			"number of seconds")

	case window <= 0 || window%time.Second != 0:
		return nil, fmt.Errorf("registration window must be a "+
			"positive whole number of seconds, got %v", window)

	// A window longer than the interval would let consecutive slots
	// overlap, and one longer than the cap would hold inputs for too
	// long inside a single connected attempt.
	case window > interval:
		return nil, fmt.Errorf("registration window %v exceeds "+
			"interval %v", window, interval)

	case window > MaxRegistrationWindow:
		return nil, fmt.Errorf("registration window %v exceeds "+
			"maximum %v", window, MaxRegistrationWindow)
	}

	s := &Schedule{
		anchor:   anchor.UTC(),
		interval: interval,
		window:   window,
	}

	// The identity preimage is fixed text over integer seconds so it is
	// independent of the anchor's time zone and of Go's duration
	// formatting. Changing this format changes every schedule identity
	// and invalidates outstanding selections.
	s.id = sha256.Sum256(
		[]byte(
			fmt.Sprintf(
				"batch-schedule-v1:%d:%d:%d", s.anchor.Unix(),
				int64(interval/time.Second),
				int64(window/time.Second),
			),
		),
	)

	return s, nil
}

// ID returns the timetable's identity. It depends only on the anchor instant,
// interval and window, never on server startup time or location.
func (s *Schedule) ID() ID {
	return s.id
}

// Anchor returns the timetable's UTC reference cutoff.
func (s *Schedule) Anchor() time.Time {
	return s.anchor
}

// Interval returns the time between consecutive cutoffs.
func (s *Schedule) Interval() time.Duration {
	return s.interval
}

// Window returns how long before each cutoff registration opens.
func (s *Schedule) Window() time.Duration {
	return s.window
}

// slotAt builds the slot whose cutoff is the given whole Unix second.
func (s *Schedule) slotAt(cutoffSec int64) Slot {
	cutoff := time.Unix(cutoffSec, 0).UTC()

	return Slot{
		Opens:  cutoff.Add(-s.window),
		Cutoff: cutoff,
	}
}

// Next returns the first slot whose cutoff is strictly after the given
// instant. An instant exactly at a cutoff therefore belongs to the following
// slot, never to the one that is closing.
//
// The comparison is done on whole seconds: any instant within the second
// before a cutoff selects that cutoff. Callers that need to keep a slot they
// already hold should compare the result against it rather than nudging the
// input instant.
func (s *Schedule) Next(after time.Time) (Slot, error) {
	sec := after.Unix()
	if sec < 0 || sec > maxUnixSecond {
		return Slot{}, fmt.Errorf("time outside supported schedule " +
			"range")
	}

	// Before the anchor, the anchor itself is the next cutoff.
	anchor := s.anchor.Unix()
	if sec < anchor {
		return s.slotAt(anchor), nil
	}

	// Otherwise advance to the next grid point strictly after sec. The
	// distance is in (0, interval], so an instant on the grid moves a
	// full interval forward.
	interval := int64(s.interval / time.Second)
	delta := interval - (sec-anchor)%interval
	if delta > maxUnixSecond-sec {
		return Slot{}, fmt.Errorf("next slot exceeds supported " +
			"timestamp range")
	}

	return s.slotAt(sec + delta), nil
}

// At returns the slot for a cutoff, failing if the cutoff does not lie on this
// timetable's grid.
func (s *Schedule) At(cutoff time.Time) (Slot, error) {
	sec := cutoff.Unix()
	anchor := s.anchor.Unix()
	interval := int64(s.interval / time.Second)

	switch {
	case !inRange(cutoff):
		return Slot{}, fmt.Errorf("cutoff %v is not a supported "+
			"whole second", cutoff)

	case sec < anchor || (sec-anchor)%interval != 0:
		return Slot{}, fmt.Errorf("cutoff %v does not belong to "+
			"schedule", cutoff)
	}

	return s.slotAt(sec), nil
}

// isWholeSecond reports whether t has no sub-second component.
func isWholeSecond(t time.Time) bool {
	return t.Nanosecond() == 0
}

// inRange reports whether t is a whole second after the Unix epoch and no
// later than the end of year 9999, the range every cutoff must fall in.
func inRange(t time.Time) bool {
	sec := t.Unix()

	return isWholeSecond(t) && sec > 0 && sec <= maxUnixSecond
}

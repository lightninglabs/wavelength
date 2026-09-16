// Package batchschedule defines anchored UTC registration opportunities.
package batchschedule

import (
	"crypto/sha256"
	"fmt"
	"time"
)

// MaxRegistrationWindow bounds scheduled waiting inside an admitted ceremony.
// Longer-lived waiting intentions belong outside the signing attempt.
const MaxRegistrationWindow = 5 * time.Minute

// maxUnixSecond keeps published timestamps within the RFC3339 year range.
const maxUnixSecond int64 = 253402300799

// Schedule is an immutable timetable. Use New to validate its parameters.
type Schedule struct {
	anchor   int64
	interval int64
	window   int64
	id       [32]byte
}

// Slot identifies one registration opportunity. Cutoff starts quoting, not
// transaction broadcast or confirmation. Registration is [Opens, Cutoff).
type Slot struct {
	Opens  time.Time
	Cutoff time.Time
}

// Selection authorizes admission to one cutoff under a particular timetable.
type Selection struct {
	ScheduleID [32]byte
	CutoffUnix uint64
}

// Validate rejects incomplete selections and unrepresentable timestamps.
func (s *Selection) Validate() error {
	if s.ScheduleID == ([32]byte{}) || s.CutoffUnix == 0 ||
		s.CutoffUnix > uint64(maxUnixSecond) {
		return fmt.Errorf("invalid scheduled slot selection")
	}

	return nil
}

// New constructs a timetable with whole-second precision and a UTC anchor.
func New(anchor time.Time, interval, window time.Duration) (*Schedule, error) {
	if anchor.Unix() < 0 || anchor.Unix() > maxUnixSecond ||
		anchor.Nanosecond() != 0 {
		return nil, fmt.Errorf("anchor must be a whole UTC second " +
			"from 1970 through 9999")
	}
	if interval <= 0 || interval%time.Second != 0 {
		return nil, fmt.Errorf("interval must be a positive whole " +
			"number of seconds")
	}
	if window <= 0 || window%time.Second != 0 || window > interval ||
		window > MaxRegistrationWindow {
		return nil, fmt.Errorf("registration window must be positive "+
			"whole seconds, at most the interval and %s",
			MaxRegistrationWindow)
	}
	s := &Schedule{
		anchor:   anchor.Unix(),
		interval: int64(interval / time.Second),
		window:   int64(window / time.Second),
	}
	s.id = sha256.Sum256(
		[]byte(
			fmt.Sprintf("batch-schedule-v1:%d:%d:%d", s.anchor,
				s.interval, s.window),
		),
	)

	return s, nil
}

// ID identifies the timing policy independently of server startup or location.
func (s *Schedule) ID() [32]byte { return s.id }

// Anchor returns the timetable's UTC reference instant.
func (s *Schedule) Anchor() time.Time { return time.Unix(s.anchor, 0).UTC() }

// Interval returns the time between consecutive cutoffs.
func (s *Schedule) Interval() time.Duration {
	return time.Duration(s.interval) * time.Second
}

// Window returns the duration for which a slot accepts registrations.
func (s *Schedule) Window() time.Duration {
	return time.Duration(s.window) * time.Second
}

// Next returns the first cutoff strictly after the supplied instant. Exact
// cutoff arrivals belong to the next opportunity, never the closing slot.
func (s *Schedule) Next(after time.Time) (Slot, error) {
	sec := after.Unix()
	if sec < 0 || sec > maxUnixSecond {
		return Slot{}, fmt.Errorf("time outside supported schedule " +
			"range")
	}
	cutoff := s.anchor
	if sec >= cutoff {
		delta := s.interval - (sec-s.anchor)%s.interval
		if delta > maxUnixSecond-sec {
			return Slot{}, fmt.Errorf("next slot exceeds " +
				"supported timestamp range")
		}
		cutoff = sec + delta
	}

	return Slot{
		Opens:  time.Unix(cutoff-s.window, 0).UTC(),
		Cutoff: time.Unix(cutoff, 0).UTC(),
	}, nil
}

// At validates that cutoff belongs to this timetable.
func (s *Schedule) At(cutoff time.Time) (Slot, error) {
	sec := cutoff.Unix()
	if sec <= 0 || sec > maxUnixSecond || cutoff.Nanosecond() != 0 ||
		sec < s.anchor || (sec-s.anchor)%s.interval != 0 {
		return Slot{}, fmt.Errorf("cutoff does not belong to schedule")
	}

	return Slot{
		Opens:  cutoff.Add(-s.Window()).UTC(),
		Cutoff: cutoff.UTC(),
	}, nil
}

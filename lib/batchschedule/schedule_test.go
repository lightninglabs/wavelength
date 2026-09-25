package batchschedule

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestAnchoredSlots pins the UTC boundaries a schedule produces. Operators and
// clients in different time zones, and an operator that restarts mid-day, must
// all agree on which cutoff an instant belongs to, or a join would be admitted
// into a slot the client never selected. It also pins that the schedule
// identity depends only on the anchor instant and parameters, so a restart in
// a different zone does not invalidate outstanding selections, while a real
// policy change does.
func TestAnchoredSlots(t *testing.T) {
	t.Parallel()

	// An hourly timetable with a one-minute registration window gives
	// round numbers that make each boundary case easy to read.
	anchor := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	s, err := New(anchor, time.Hour, time.Minute)
	require.NoError(t, err)

	// Each case names an instant and the cutoff Next must pick for it.
	// The interesting cases sit right on or just beside a boundary, since
	// an instant exactly at a cutoff belongs to the following slot.
	testCases := []struct {
		name   string
		now    time.Time
		cutoff time.Time
	}{
		{
			name:   "before anchor",
			now:    anchor.Add(-time.Second),
			cutoff: anchor,
		},
		{
			name:   "exact anchor",
			now:    anchor,
			cutoff: anchor.Add(time.Hour),
		},
		{
			name:   "just before cutoff",
			now:    anchor.Add(time.Hour - time.Nanosecond),
			cutoff: anchor.Add(time.Hour),
		},
		{
			name:   "at cutoff",
			now:    anchor.Add(time.Hour),
			cutoff: anchor.Add(2 * time.Hour),
		},
		{
			name:   "day boundary",
			now:    anchor.Add(24*time.Hour - time.Second),
			cutoff: anchor.Add(24 * time.Hour),
		},
		{
			name: "non-UTC local zone",
			now: anchor.Add(15 * time.Minute).In(
				time.FixedZone("elsewhere", 7200),
			),
			cutoff: anchor.Add(time.Hour),
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			slot, err := s.Next(tc.now)
			require.NoError(t, err)

			// Next always returns UTC values, so a deep equality
			// check against the UTC expectation also pins the
			// location. Opens must sit one window before Cutoff.
			require.Equal(t, tc.cutoff, slot.Cutoff)
			require.Equal(
				t, tc.cutoff.Add(-time.Minute), slot.Opens,
			)
		})
	}

	// The same anchor instant expressed in another zone must hash to the
	// same identity, since the operator's host zone is not policy.
	otherZone, err := New(
		anchor.In(
			time.FixedZone("same instant", -3600),
		),
		time.Hour,
		time.Minute,
	)
	require.NoError(t, err)
	require.Equal(t, s.ID(), otherZone.ID())

	// Changing any parameter must change the identity, otherwise a client
	// holding a selection under the old policy would be admitted into a
	// slot with different timing.
	changed, err := New(anchor, time.Hour, 2*time.Minute)
	require.NoError(t, err)
	require.NotEqual(t, s.ID(), changed.ID())
}

// TestScheduleBounds pins the parameters New rejects and the instants Next
// refuses. Sub-second precision would make equality and hashing depend on
// clock readings, oversized windows would hold client inputs too long or let
// slots overlap, and out-of-range instants would overflow the interval
// arithmetic.
func TestScheduleBounds(t *testing.T) {
	t.Parallel()

	anchor := time.Unix(0, 0)

	// Every case here is an invalid combination of anchor, interval and
	// window that New must refuse outright.
	testCases := []struct {
		name     string
		anchor   time.Time
		interval time.Duration
		window   time.Duration
	}{
		{
			name:     "zero interval",
			anchor:   anchor,
			interval: 0,
			window:   time.Second,
		},
		{
			name:     "negative interval",
			anchor:   anchor,
			interval: -time.Second,
			window:   time.Second,
		},
		{
			name:     "sub-second interval",
			anchor:   anchor,
			interval: time.Nanosecond,
			window:   time.Second,
		},
		{
			name:     "zero window",
			anchor:   anchor,
			interval: time.Hour,
			window:   0,
		},
		{
			name:     "negative window",
			anchor:   anchor,
			interval: time.Hour,
			window:   -time.Second,
		},
		{
			name:     "sub-second window",
			anchor:   anchor,
			interval: time.Hour,
			window:   time.Nanosecond,
		},
		{
			name:     "window above maximum",
			anchor:   anchor,
			interval: time.Hour,
			window:   6 * time.Minute,
		},
		{
			name:     "window longer than interval",
			anchor:   anchor,
			interval: time.Second,
			window:   2 * time.Second,
		},
		{
			name:     "anchor before epoch",
			anchor:   time.Unix(-1, 0),
			interval: time.Hour,
			window:   time.Minute,
		},
		{
			name:     "sub-second anchor",
			anchor:   anchor.Add(time.Nanosecond),
			interval: time.Hour,
			window:   time.Minute,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.anchor, tc.interval, tc.window)
			require.Error(t, err)
		})
	}

	// A valid schedule must still refuse instants whose next cutoff would
	// fall outside the supported range, rather than wrapping around.
	s, err := New(anchor, time.Hour, time.Minute)
	require.NoError(t, err)

	_, err = s.Next(time.Unix(maxUnixSecond, 0))
	require.Error(t, err)

	_, err = s.Next(time.Unix(-1, 0))
	require.Error(t, err)
}

// TestScheduleRestartProperty proves that the operator's startup time cannot
// move a slot. For any anchor and interval, the cutoff chosen for an instant
// is always the next grid point after it, so two operator processes that
// started at different times agree on every slot.
func TestScheduleRestartProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Draw a timetable and an instant expressed as a whole number
		// of intervals past the anchor plus an in-interval remainder.
		interval := rapid.Int64Range(1, 86400).Draw(t, "interval")
		anchor := rapid.Int64Range(0, 2000000000).Draw(t, "anchor")
		step := rapid.Int64Range(0, 1000000).Draw(t, "step")
		offset := rapid.Int64Range(0, interval-1).Draw(t, "offset")

		s, err := New(
			time.Unix(anchor, 0),
			time.Duration(interval)*time.Second, time.Second,
		)
		require.NoError(t, err)

		// The chosen cutoff must be exactly one grid step past the
		// step the instant falls in, regardless of the remainder.
		slot, err := s.Next(time.Unix(anchor+step*interval+offset, 0))
		require.NoError(t, err)
		require.Equal(t, anchor+(step+1)*interval, slot.Cutoff.Unix())
	})
}

// TestIDEncoding pins the wire and log forms of a schedule identity. A
// malformed identity must be rejected instead of being truncated or padded,
// because a silently altered identity would name a different timetable, and
// IsZero must only report the unset value so validation can rely on it.
func TestIDEncoding(t *testing.T) {
	t.Parallel()

	// The zero value is the only unset identity.
	require.True(t, ID{}.IsZero())
	require.False(t, ID{1}.IsZero())
	require.False(t, ID{31: 1}.IsZero())

	// A 32-byte input round trips exactly, and String renders every byte
	// in hex so two identities that differ anywhere log differently.
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	id, err := IDFromBytes(raw)
	require.NoError(t, err)
	require.Equal(t, raw, id[:])
	require.Equal(
		t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b"+
			"1c1d1e1f", id.String(),
	)
	require.Equal(t, strings.Repeat("0", 64), ID{}.String())

	// Any other length is rejected, including the empty and nil slices a
	// missing protobuf field decodes to.
	for _, length := range []int{0, 1, 31, 33, 64} {
		_, err := IDFromBytes(make([]byte, length))
		require.Error(t, err, "length %d", length)
	}
	_, err = IDFromBytes(nil)
	require.Error(t, err)
}

// TestSelectionValidate pins which selections may be bound into a join
// authorization. A selection must name a timetable and a cutoff that
// survives the whole-Unix-second wire encoding unchanged, otherwise the
// operator would verify a signature over a different slot than the client
// intended.
func TestSelectionValidate(t *testing.T) {
	t.Parallel()

	cutoff := time.Unix(1800000000, 0).UTC()

	testCases := []struct {
		name      string
		selection Selection
		valid     bool
	}{
		{
			name: "valid",
			selection: Selection{
				ScheduleID: ID{
					1,
				},
				Cutoff: cutoff,
			},
			valid: true,
		},
		{
			name: "last supported second",
			selection: Selection{
				ScheduleID: ID{
					1,
				},
				Cutoff: time.Unix(maxUnixSecond, 0),
			},
			valid: true,
		},
		{
			name: "zero schedule identity",
			selection: Selection{
				Cutoff: cutoff,
			},
		},
		{
			name: "zero cutoff",
			selection: Selection{
				ScheduleID: ID{
					1,
				},
			},
		},
		{
			name: "epoch cutoff",
			selection: Selection{
				ScheduleID: ID{
					1,
				},
				Cutoff: time.Unix(0, 0),
			},
		},
		{
			name: "sub-second cutoff",
			selection: Selection{
				ScheduleID: ID{
					1,
				},
				Cutoff: cutoff.Add(time.Millisecond),
			},
		},
		{
			name: "cutoff past supported range",
			selection: Selection{
				ScheduleID: ID{
					1,
				},
				Cutoff: time.Unix(maxUnixSecond+1, 0),
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.selection.Validate()
			if tc.valid {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
		})
	}
}

// TestSelectionMatches pins that matching compares instants rather than
// time.Time representations. The operator and client may hold the same
// cutoff in different locations, and a spurious mismatch would reject a
// correctly targeted join.
func TestSelectionMatches(t *testing.T) {
	t.Parallel()

	cutoff := time.Unix(1800000000, 0).UTC()
	selection := &Selection{
		ScheduleID: ID{
			1,
		},
		Cutoff: cutoff,
	}

	// The same instant in another zone is still the same slot.
	elsewhere := cutoff.In(time.FixedZone("elsewhere", -5*3600))
	require.True(t, selection.Matches(ID{1}, cutoff))
	require.True(t, selection.Matches(ID{1}, elsewhere))

	// A different cutoff or a different timetable must never match, so a
	// selection cannot be replayed into a neighboring slot or a changed
	// policy.
	require.False(t, selection.Matches(ID{1}, cutoff.Add(time.Second)))
	require.False(t, selection.Matches(ID{2}, cutoff))
}

// TestSelectionFromUnix pins decoding a selection from its wire form. The
// decoded cutoff must be UTC and round trip through CutoffUnix, and values
// that do not fit the supported range must be rejected before any conversion
// to int64 can wrap them.
func TestSelectionFromUnix(t *testing.T) {
	t.Parallel()

	// A valid wire value decodes to a UTC instant and encodes back to the
	// same number, so a signed message is reproducible after decoding.
	selection, err := SelectionFromUnix(ID{1}, 1800000000)
	require.NoError(t, err)
	require.Equal(t, time.UTC, selection.Cutoff.Location())
	require.True(
		t, selection.Cutoff.Equal(time.Unix(1800000000, 0)),
	)
	require.EqualValues(t, 1800000000, selection.CutoffUnix())

	// Invalid identities and cutoffs are rejected by the same rules as
	// Validate, including a value too large to fit in an int64.
	testCases := []struct {
		name   string
		id     ID
		cutoff uint64
	}{
		{
			name:   "zero identity",
			id:     ID{},
			cutoff: 1800000000,
		},
		{
			name: "zero cutoff",
			id: ID{
				1,
			},
			cutoff: 0,
		},
		{
			name: "cutoff past supported range",
			id: ID{
				1,
			},
			cutoff: uint64(maxUnixSecond) + 1,
		},
		{
			name: "cutoff past int64 range",
			id: ID{
				1,
			},
			cutoff: 1 << 63,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SelectionFromUnix(tc.id, tc.cutoff)
			require.Error(t, err)
		})
	}
}

// TestPublishedNormalizesAndCopies pins that a published list is immutable
// from the caller's point of view. NewPublished stores UTC copies of the
// slots, and WithServerTime and WithClockOffset return new values, so an
// attempt that attaches its own clock estimate can never change the list a
// concurrent attempt is reading from the shared operator terms.
func TestPublishedNormalizesAndCopies(t *testing.T) {
	t.Parallel()

	// Build the input slots in a non-UTC zone so the normalization is
	// observable.
	zone := time.FixedZone("elsewhere", 3600)
	cutoff := time.Unix(1800000000, 0).In(zone)
	input := []Slot{{
		Opens:  cutoff.Add(-time.Minute),
		Cutoff: cutoff,
	}}

	base, err := NewPublished(ID{1}, input)
	require.NoError(t, err)

	// The stored slot is the same instant in UTC, and later edits to the
	// caller's slice do not reach the published list.
	slot, err := base.Next(cutoff.Add(-time.Second))
	require.NoError(t, err)
	require.Equal(t, time.UTC, slot.Cutoff.Location())
	require.Equal(t, time.UTC, slot.Opens.Location())
	require.True(t, slot.Cutoff.Equal(cutoff))

	input[0].Cutoff = cutoff.Add(time.Hour)
	require.True(t, base.slots[0].Cutoff.Equal(cutoff))

	// A fresh list carries no clock information until a caller attaches
	// it.
	require.True(t, base.ServerTime().IsZero())
	require.Zero(t, base.ClockOffset())

	// WithServerTime returns a copy with the reading set and leaves the
	// receiver untouched, including its slot backing array.
	serverTime := time.Unix(1800000000, 0).UTC()
	withTime := base.WithServerTime(serverTime)
	require.NotSame(t, base, withTime)
	require.Equal(t, serverTime, withTime.ServerTime())
	require.True(t, base.ServerTime().IsZero())
	require.Equal(t, base.ID(), withTime.ID())

	withTime.slots[0].Cutoff = cutoff.Add(time.Hour)
	require.True(t, base.slots[0].Cutoff.Equal(cutoff))

	// WithClockOffset behaves the same way and preserves the server time
	// recorded by the earlier copy.
	withOffset := withTime.WithClockOffset(3 * time.Second)
	require.NotSame(t, withTime, withOffset)
	require.Equal(t, 3*time.Second, withOffset.ClockOffset())
	require.Equal(t, serverTime, withOffset.ServerTime())
	require.Zero(t, withTime.ClockOffset())

	withOffset.slots[0].Opens = cutoff
	require.True(
		t,
		withTime.slots[0].Opens.Equal(
			cutoff.Add(-time.Minute),
		),
	)
}

// TestEstimateClockOffsetEdges pins the inputs for which no estimate can be
// made. Without a server reading, or with a round trip that ran backwards
// because the local clock stepped, the only safe answer is no correction.
func TestEstimateClockOffsetEdges(t *testing.T) {
	t.Parallel()

	sent := time.Unix(1800000000, 0)
	received := sent.Add(200 * time.Millisecond)

	// An operator that did not report its clock yields no offset even
	// with a well-formed round trip.
	require.Zero(t, EstimateClockOffset(time.Time{}, sent, received))

	// A round trip whose receive stamp precedes its send stamp is
	// discarded rather than producing a nonsense midpoint.
	require.Zero(
		t,
		EstimateClockOffset(
			sent.Add(time.Hour), received, sent,
		),
	)

	// A reading equal to the round-trip midpoint is assumed to have been
	// truncated, so the estimate adds the half-second correction.
	offset := EstimateClockOffset(sent, sent, sent)
	require.Equal(t, time.Second/2, offset)
}

// TestEstimateClockOffsetProperty proves that for a symmetric round trip the
// estimate lands within the truncation error of the true offset. The
// operator truncates its reading to whole seconds, so if its clock is X
// ahead, the estimate must fall in (X-0.5s, X+0.5s]. That bound is what
// WakeBounds' lower margin is sized to absorb.
func TestEstimateClockOffsetProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Draw a send time with sub-second precision, a round-trip
		// time and a true offset of up to a day in either direction.
		sentSec := rapid.Int64Range(1e9, 2e9).Draw(t, "sent_sec")
		sentNano := rapid.Int64Range(0, 999_999_999).Draw(
			t, "sent_nano",
		)
		rtt := time.Duration(
			rapid.Int64Range(
				0, int64(10*time.Second),
			).Draw(t, "rtt"),
		)
		trueOffset := time.Duration(
			rapid.Int64Range(
				-int64(24*time.Hour), int64(24*time.Hour),
			).Draw(t, "true_offset"),
		)

		sent := time.Unix(sentSec, sentNano)
		received := sent.Add(rtt)

		// The operator stamps its clock at the midpoint of the round
		// trip and truncates the reading to whole seconds.
		midpoint := sent.Add(rtt / 2)
		serverTime := midpoint.Add(trueOffset).Truncate(time.Second)

		offset := EstimateClockOffset(serverTime, sent, received)
		require.Greater(t, offset, trueOffset-time.Second/2)
		require.LessOrEqual(t, offset, trueOffset+time.Second/2)
	})
}

// TestWakeBounds pins the join send range for representative windows. The
// lower bound is capped at the clock-error margin so long windows do not
// waste their opening, and scales down for short windows so the range never
// inverts; the upper bound leaves the second half of the window for the
// operator to prepare.
func TestWakeBounds(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		window time.Duration
		lo     time.Duration
		hi     time.Duration
	}{
		{
			name:   "one second scales below margin",
			window: time.Second,
			lo:     250 * time.Millisecond,
			hi:     500 * time.Millisecond,
		},
		{
			name:   "one minute uses full margin",
			window: time.Minute,
			lo:     2 * time.Second,
			hi:     30 * time.Second,
		},
		{
			name:   "maximum window uses full margin",
			window: MaxRegistrationWindow,
			lo:     2 * time.Second,
			hi:     150 * time.Second,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			lo, hi := WakeBounds(tc.window)
			require.Equal(t, tc.lo, lo)
			require.Equal(t, tc.hi, hi)
		})
	}
}

// TestWakeBoundsProperty proves the send range is well formed for every
// window a published schedule may carry. A client picks a wake time in
// [lo, hi], so the range must be non-empty, start no later than the margin,
// and end at the window's midpoint, well before the cutoff.
func TestWakeBoundsProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		window := time.Duration(
			rapid.Int64Range(
				int64(time.Second), int64(MaxRegistrationWindow),
			).Draw(t, "window"),
		)

		lo, hi := WakeBounds(window)

		// The range is non-empty and the lower bound never exceeds
		// either the margin or a quarter of the window.
		require.LessOrEqual(t, lo, hi)
		require.Positive(t, lo)
		require.LessOrEqual(t, lo, maxWakeMargin)
		require.LessOrEqual(t, lo, window/4)

		// The upper bound is the window's midpoint, leaving the second
		// half for preparation and transit.
		require.Equal(t, window/2, hi)
		require.Less(t, hi, window)
	})
}

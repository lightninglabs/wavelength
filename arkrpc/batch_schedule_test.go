package arkrpc

import (
	"math"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/lib/batchschedule"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestBatchScheduleDiscovery pins the discovery round trip between an
// interval-based operator and a client. The operator publishes from its
// persisted admission cursor rather than from the current time, so a cursor
// that was fenced forward after a rollback is what the client sees. The
// client, in turn, accepts any well-formed irregular list without assuming
// an interval formula, never extrapolates past it, and does not alias the
// protobuf message it parsed.
func TestBatchScheduleDiscovery(t *testing.T) {
	t.Parallel()

	// Build expected values in UTC, since parsed slots are normalized to
	// UTC and are compared by deep equality below.
	now := time.Unix(1800000000, 0).UTC()
	s, err := batchschedule.New(now, time.Hour, time.Minute)
	require.NoError(t, err)

	// Publish from a cursor two intervals ahead of now, as an operator
	// whose cursor was fenced forward would, and push the message through
	// the wire encoding so the test covers what a client actually
	// receives.
	p := BatchScheduleToProto(s, now, now.Add(2*time.Hour))
	require.Len(t, p.Slots, PublishedBatchSlots)

	encoded, err := proto.Marshal(p)
	require.NoError(t, err)

	p = &BatchSchedule{}
	require.NoError(t, proto.Unmarshal(encoded, p))

	// The parsed view keeps the operator's identity, and the first slot a
	// client can select is the cursor, not the next grid point after now.
	parsedOpt, err := ParseBatchSchedule(p)
	require.NoError(t, err)
	parsed := parsedOpt.UnwrapOrFail(t)
	require.Equal(t, s.ID(), parsed.ID())

	slot, err := parsed.Next(now)
	require.NoError(t, err)
	require.Equal(t, now.Add(2*time.Hour), slot.Cutoff)
	require.Equal(t, slot.Cutoff.Add(-time.Minute), slot.Opens)

	// The operator stamped its clock, so the parsed view carries it for
	// the caller's clock offset estimate.
	require.True(t, parsed.ServerTime().Equal(now))

	// Replace the list with an irregular calendar: windows of different
	// lengths separated by a gap. No interval formula describes it, and
	// the client must accept it anyway.
	p.Slots = []*BatchSlot{
		{
			RegistrationOpensUnix: now.Unix() + 10,
			CutoffUnix:            now.Unix() + 30,
		},
		{
			RegistrationOpensUnix: now.Unix() + 170,
			CutoffUnix:            now.Unix() + 220,
		},
	}
	parsedOpt, err = ParseBatchSchedule(p)
	require.NoError(t, err)
	parsed = parsedOpt.UnwrapOrFail(t)

	// An instant at the first cutoff moves to the second slot, whose
	// edges come straight from the list rather than any formula.
	slot, err = parsed.Next(now.Add(30 * time.Second))
	require.NoError(t, err)
	require.Equal(t, now.Add(170*time.Second), slot.Opens)
	require.Equal(t, now.Add(220*time.Second), slot.Cutoff)

	// Past the last listed cutoff the client must refetch discovery
	// instead of synthesizing a slot.
	_, err = parsed.Next(slot.Cutoff)
	require.ErrorIs(t, err, batchschedule.ErrScheduleExhausted)

	// Mutating the source message after parsing must not reach the parsed
	// view, so the list stays exhausted at the original cutoff.
	p.Slots[1].CutoffUnix++
	_, err = parsed.Next(slot.Cutoff)
	require.ErrorIs(t, err, batchschedule.ErrScheduleExhausted)

	// A missing schedule means event-driven registration, which is not an
	// error.
	parsedOpt, err = ParseBatchSchedule(nil)
	require.NoError(t, err)
	require.True(t, parsedOpt.IsNone())
}

// TestParseBatchScheduleServerTime pins how the operator's clock reading is
// carried from discovery. A positive reading is kept as a UTC instant so the
// caller can estimate the clock offset, while a missing or negative reading
// leaves the zero time, which EstimateClockOffset treats as no estimate. In
// every case parsing itself never attaches an offset, because only the
// caller timed the round trip.
func TestParseBatchScheduleServerTime(t *testing.T) {
	t.Parallel()

	anchor := time.Unix(1800000000, 0)
	schedule, err := batchschedule.New(anchor, time.Hour, time.Minute)
	require.NoError(t, err)

	testCases := []struct {
		name       string
		serverTime int64
		want       time.Time
	}{
		{
			name:       "reported server time",
			serverTime: 1800000123,
			want:       time.Unix(1800000123, 0).UTC(),
		},
		{
			name:       "unreported server time",
			serverTime: 0,
		},
		{
			name:       "negative server time",
			serverTime: -1,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			p := BatchScheduleToProto(
				schedule, anchor, anchor.Add(time.Hour),
			)
			p.ServerTimeUnix = tc.serverTime

			parsedOpt, err := ParseBatchSchedule(p)
			require.NoError(t, err)
			parsed := parsedOpt.UnwrapOrFail(t)

			// Deep equality also pins that a reported reading is
			// stored in UTC.
			require.Equal(t, tc.want, parsed.ServerTime())
			require.Zero(t, parsed.ClockOffset())
		})
	}
}

// TestBatchScheduleDiscoveryBounds pins every malformed discovery message the
// client rejects before selecting a slot. A malformed schedule must be an
// error rather than a fallback to event-driven registration, since the
// operator rejects joins that carry no slot. Each case starts from a valid
// published list and breaks exactly one property.
func TestBatchScheduleDiscoveryBounds(t *testing.T) {
	t.Parallel()

	anchor := time.Unix(1800000000, 0)
	schedule, err := batchschedule.New(anchor, time.Hour, time.Minute)
	require.NoError(t, err)

	testCases := []struct {
		name   string
		mutate func(*BatchSchedule)
	}{
		{
			name: "window below minimum",
			mutate: func(p *BatchSchedule) {
				p.Slots[0].RegistrationOpensUnix =
					p.Slots[0].CutoffUnix - 9
			},
		},
		{
			name: "unset version",
			mutate: func(p *BatchSchedule) {
				p.Version = 0
			},
		},
		{
			name: "unknown future version",
			mutate: func(p *BatchSchedule) {
				p.Version = 2
			},
		},
		{
			name: "missing schedule id",
			mutate: func(p *BatchSchedule) {
				p.ScheduleId = nil
			},
		},
		{
			name: "short schedule id",
			mutate: func(p *BatchSchedule) {
				p.ScheduleId = []byte{
					1,
				}
			},
		},
		{
			name: "long schedule id",
			mutate: func(p *BatchSchedule) {
				p.ScheduleId = append(p.ScheduleId, 1)
			},
		},
		{
			name: "zero schedule id",
			mutate: func(p *BatchSchedule) {
				p.ScheduleId = make([]byte, 32)
			},
		},
		{
			name: "no slots",
			mutate: func(p *BatchSchedule) {
				p.Slots = nil
			},
		},
		{
			name: "too many slots",
			mutate: func(p *BatchSchedule) {
				p.Slots = make(
					[]*BatchSlot,
					batchschedule.MaxPublishedSlots+1,
				)
			},
		},
		{
			name: "nil slot",
			mutate: func(p *BatchSchedule) {
				p.Slots[0] = nil
			},
		},
		{
			name: "zero cutoff",
			mutate: func(p *BatchSchedule) {
				p.Slots[0].CutoffUnix = 0
			},
		},
		{
			name: "negative opening",
			mutate: func(p *BatchSchedule) {
				p.Slots[0].RegistrationOpensUnix = -1
			},
		},
		{
			name: "cutoff past supported range",
			mutate: func(p *BatchSchedule) {
				p.Slots[0].CutoffUnix = math.MaxInt64
			},
		},
		{
			name: "empty window",
			mutate: func(p *BatchSchedule) {
				slot := p.Slots[0]
				slot.RegistrationOpensUnix = slot.CutoffUnix
			},
		},
		{
			name: "window above maximum",
			mutate: func(p *BatchSchedule) {
				slot := p.Slots[0]
				maxWindow := int64(
					batchschedule.MaxRegistrationWindow /
						time.Second,
				)
				slot.RegistrationOpensUnix =
					slot.CutoffUnix - maxWindow - 1
			},
		},
		{
			name: "duplicate slot",
			mutate: func(p *BatchSchedule) {
				p.Slots[1] = p.Slots[0]
			},
		},
		{
			name: "unordered slots",
			mutate: func(p *BatchSchedule) {
				p.Slots[0], p.Slots[1] = p.Slots[1], p.Slots[0]
			},
		},
		{
			name: "overlapping slots",
			mutate: func(p *BatchSchedule) {
				cutoff := p.Slots[0].CutoffUnix
				p.Slots[1].RegistrationOpensUnix = cutoff - 1
				p.Slots[1].CutoffUnix = cutoff + 10
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Start from a list the client would accept, so the
			// single mutation is the only reason for rejection.
			p := BatchScheduleToProto(
				schedule, anchor, anchor.Add(time.Hour),
			)
			_, err := ParseBatchSchedule(p)
			require.NoError(t, err)

			tc.mutate(p)

			_, err = ParseBatchSchedule(p)
			require.Error(t, err)
		})
	}
}

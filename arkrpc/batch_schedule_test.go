package arkrpc

import (
	"math"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/lib/batchschedule"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestBatchScheduleDiscovery publishes from the persisted cursor, including
// its fence after rollback, and accepts irregular windows without a formula.
func TestBatchScheduleDiscovery(t *testing.T) {
	t.Parallel()
	now := time.Unix(1800000000, 0).UTC()
	s, err := batchschedule.New(now, time.Hour, time.Minute)
	require.NoError(t, err)
	p := BatchScheduleToProto(s, now, now.Add(2*time.Hour))
	require.Len(t, p.Slots, PublishedBatchSlots)
	encoded, err := proto.Marshal(p)
	require.NoError(t, err)
	p = &BatchSchedule{}
	require.NoError(t, proto.Unmarshal(encoded, p))
	parsed, err := ParseBatchSchedule(p)
	require.NoError(t, err)
	require.Equal(t, s.ID(), parsed.ID())
	slot, err := parsed.Next(now)
	require.NoError(t, err)
	require.Equal(t, now.Add(2*time.Hour), slot.Cutoff)
	require.Equal(t, slot.Cutoff.Add(-time.Minute), slot.Opens)

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
	parsed, err = ParseBatchSchedule(p)
	require.NoError(t, err)
	slot, err = parsed.Next(now.Add(30 * time.Second))
	require.NoError(t, err)
	require.Equal(t, now.Add(170*time.Second), slot.Opens)
	require.Equal(t, now.Add(220*time.Second), slot.Cutoff)
	_, err = parsed.Next(slot.Cutoff)
	require.ErrorIs(t, err, batchschedule.ErrScheduleExhausted)
	p.Slots[1].CutoffUnix++
	_, err = parsed.Next(slot.Cutoff)
	require.ErrorIs(t, err, batchschedule.ErrScheduleExhausted)
	parsed, err = ParseBatchSchedule(nil)
	require.NoError(t, err)
	require.Nil(t, parsed)
}

// TestBatchScheduleDiscoveryBounds rejects malformed lists before selection.
func TestBatchScheduleDiscoveryBounds(t *testing.T) {
	t.Parallel()
	anchor := time.Unix(1800000000, 0)
	schedule, err := batchschedule.New(anchor, time.Hour, time.Minute)
	require.NoError(t, err)
	cases := map[string]func(*BatchSchedule){
		"unset version": func(p *BatchSchedule) {
			p.Version = 0
		},
		"unknown version": func(p *BatchSchedule) {
			p.Version = 2
		},
		"short id": func(p *BatchSchedule) {
			p.ScheduleId = []byte{
				1,
			}
		},
		"zero id": func(p *BatchSchedule) {
			p.ScheduleId = make([]byte, 32)
		},
		"empty": func(p *BatchSchedule) {
			p.Slots = nil
		},
		"oversized": func(p *BatchSchedule) {
			p.Slots = make(
				[]*BatchSlot, batchschedule.MaxPublishedSlots+1,
			)
		},
		"nil slot": func(p *BatchSchedule) {
			p.Slots[0] = nil
		},
		"zero cutoff": func(p *BatchSchedule) {
			p.Slots[0].CutoffUnix = 0
		},
		"negative opening": func(p *BatchSchedule) {
			p.Slots[0].RegistrationOpensUnix = -1
		},
		"overflow cutoff": func(p *BatchSchedule) {
			p.Slots[0].CutoffUnix = math.MaxInt64
		},
		"empty window": func(p *BatchSchedule) {
			p.Slots[0].RegistrationOpensUnix = p.Slots[0].CutoffUnix
		},
		"unbounded window": func(p *BatchSchedule) {
			slot := p.Slots[0]
			slot.RegistrationOpensUnix = slot.CutoffUnix - 301
		},
		"duplicate": func(p *BatchSchedule) {
			p.Slots[1] = p.Slots[0]
		},
		"unordered": func(p *BatchSchedule) {
			p.Slots[0], p.Slots[1] = p.Slots[1], p.Slots[0]
		},
		"overlap": func(p *BatchSchedule) {
			cutoff := p.Slots[0].CutoffUnix
			p.Slots[1].RegistrationOpensUnix = cutoff - 1
			p.Slots[1].CutoffUnix = p.Slots[0].CutoffUnix + 10
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := BatchScheduleToProto(
				schedule, anchor, anchor.Add(time.Hour),
			)
			mutate(p)
			_, err := ParseBatchSchedule(p)
			require.Error(t, err)
		})
	}
}

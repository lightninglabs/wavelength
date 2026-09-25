package batchschedule

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestAnchoredSlots pins UTC boundaries independently of restart and location.
func TestAnchoredSlots(t *testing.T) {
	t.Parallel()
	anchor := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	s, err := New(anchor, time.Hour, time.Minute)
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		now    time.Time
		cutoff time.Time
	}{
		{
			"before anchor",
			anchor.Add(-time.Second),
			anchor,
		},
		{
			"exact anchor",
			anchor,
			anchor.Add(time.Hour),
		},
		{
			"before cutoff",
			anchor.Add(time.Hour - time.Nanosecond),
			anchor.Add(time.Hour),
		},
		{
			"at cutoff",
			anchor.Add(time.Hour),
			anchor.Add(2 * time.Hour),
		},
		{
			"day boundary",
			anchor.Add(24*time.Hour - time.Second),
			anchor.Add(24 * time.Hour),
		},
		{
			"local zone",
			anchor.Add(15 * time.Minute).In(
				time.FixedZone("elsewhere", 7200),
			),
			anchor.Add(time.Hour),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slot, err := s.Next(tc.now)
			require.NoError(t, err)
			require.Equal(t, tc.cutoff, slot.Cutoff)
			require.Equal(
				t, tc.cutoff.Add(-time.Minute), slot.Opens,
			)
		})
	}
	otherZone, err := New(
		anchor.In(
			time.FixedZone("same instant", -3600),
		),
		time.Hour,
		time.Minute,
	)
	require.NoError(t, err)
	require.Equal(t, s.ID(), otherZone.ID())
	changed, err := New(anchor, time.Hour, 2*time.Minute)
	require.NoError(t, err)
	require.NotEqual(t, s.ID(), changed.ID())
}

// TestScheduleBounds rejects ambiguous precision and arithmetic overflow.
func TestScheduleBounds(t *testing.T) {
	t.Parallel()
	anchor := time.Unix(0, 0)
	for _, interval := range []time.Duration{
		0,
		-time.Second,
		time.Nanosecond,
	} {
		_, err := New(anchor, interval, time.Second)
		require.Error(t, err)
	}
	for _, window := range []time.Duration{
		0,
		-time.Second,
		time.Nanosecond,
		6 * time.Minute,
	} {
		_, err := New(anchor, time.Hour, window)
		require.Error(t, err)
	}
	_, err := New(anchor, time.Second, 2*time.Second)
	require.Error(t, err)
	_, err = New(time.Unix(-1, 0), time.Hour, time.Minute)
	require.Error(t, err)
	_, err = New(anchor.Add(time.Nanosecond), time.Hour, time.Minute)
	require.Error(t, err)
	s, err := New(anchor, time.Hour, time.Minute)
	require.NoError(t, err)
	_, err = s.Next(time.Unix(maxUnixSecond, 0))
	require.Error(t, err)
	_, err = s.Next(time.Unix(-1, 0))
	require.Error(t, err)
}

// TestScheduleRestartProperty proves startup time cannot change a slot's grid.
func TestScheduleRestartProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		interval := rapid.Int64Range(1, 86400).Draw(t, "interval")
		anchor := rapid.Int64Range(0, 2000000000).Draw(t, "anchor")
		step := rapid.Int64Range(0, 1000000).Draw(t, "step")
		offset := rapid.Int64Range(0, interval-1).Draw(t, "offset")
		s, err := New(
			time.Unix(anchor, 0),
			time.Duration(interval)*time.Second, time.Second,
		)
		require.NoError(t, err)
		slot, err := s.Next(time.Unix(anchor+step*interval+offset, 0))
		require.NoError(t, err)
		require.Equal(t, anchor+(step+1)*interval, slot.Cutoff.Unix())
	})
}

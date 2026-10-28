package timeout

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/stretchr/testify/require"
)

// TestMailboxPressureEpisode covers the grace boundary, episode deduplication,
// wrapped queue errors, and a changed failure that must remain visible.
func TestMailboxPressureEpisode(t *testing.T) {
	t.Parallel()

	var pressure mailboxPressure
	full := fmt.Errorf("enqueue: %w", actor.ErrMailboxFull)
	require.False(t, pressure.shouldWarn(startEpoch, full))
	require.False(
		t,
		pressure.shouldWarn(
			startEpoch.Add(mailboxPressureGrace-time.Nanosecond),
			full,
		),
	)
	require.True(
		t,
		pressure.shouldWarn(
			startEpoch.Add(mailboxPressureGrace), full,
		),
	)
	require.False(t, pressure.shouldWarn(startEpoch.Add(time.Hour), full))

	for _, err := range []error{
		context.DeadlineExceeded, actor.ErrNoActorsAvailable,
		errors.New("mailbox full"), errors.New("connection reset"),
	} {
		require.True(
			t,
			pressure.shouldWarn(
				startEpoch.Add(time.Hour), err,
			),
		)
		require.Zero(t, pressure)
	}

	// A later full-mailbox episode must earn its own grace and warning.
	now := startEpoch.Add(2 * time.Hour)
	require.False(t, pressure.shouldWarn(now, full))
	require.True(
		t,
		pressure.shouldWarn(
			now.Add(mailboxPressureGrace), full,
		),
	)
}

// TestTimeoutPressureRecovery retains a burst of independent cleanup messages
// across brief or sustained queue pressure, then delivers each exactly once.
func TestTimeoutPressureRecovery(t *testing.T) {
	t.Parallel()

	for _, blocked := range []time.Duration{time.Second, time.Minute} {
		t.Run(blocked.String(), func(t *testing.T) {
			t.Parallel()

			clock := newFakeClock(startEpoch)
			a := newTestActor(clock)
			cb := newWedgeableCallbackRef(
				"manager", actor.ErrMailboxFull,
			)
			for i := range 12 {
				result := a.Receive(
					t.Context(), &ScheduleTimeoutRequest{
						ID: ID(
							fmt.Sprintf(
								"cleanup-%d",
								i),
						),
						Duration: time.Millisecond,
						Callback: cb,
					},
				)
				require.True(t, result.IsOk())
			}

			clock.Advance(blocked)
			require.Len(t, a.oneshots, 12)
			for _, entry := range a.oneshots {
				require.Equal(
					t, blocked >= mailboxPressureGrace,
					entry.pressure.warned,
				)
			}

			cb.release()
			clock.Advance(retryMaxDelay)
			msgs, _ := cb.snapshot()
			require.Len(t, msgs, 12)
			ids := make(map[ID]bool)
			for _, msg := range msgs {
				require.False(
					t, ids[msg.ID], "duplicate delivery",
				)
				ids[msg.ID] = true
			}
			require.Empty(t, a.oneshots)
		})
	}
}

// TestTickPressureRecovery proves a sustained failure retains the original tick
// and successful delivery resets the grace period for the next blocked tick.
func TestTickPressureRecovery(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	cb := newWedgeableTickRef("manager")
	result := a.Receive(t.Context(), &ScheduleRecurringTickRequest{
		ID: "tick", Interval: time.Second, Callback: cb,
	})
	require.True(t, result.IsOk())

	// Failures occur at seconds 1, 2, 4, 8, 16 and 32. Only the last
	// crosses the grace boundary; the next retry is scheduled at second 62.
	clock.Advance(32 * time.Second)
	entry := a.recurring["tick"]
	require.True(t, entry.pressure.warned)
	require.Equal(t, startEpoch.Add(time.Second), entry.pending.FiredAt)

	cb.release()
	clock.Advance(retryMaxDelay)
	msgs, attempts := cb.snapshot()
	require.Len(t, msgs, 1)
	require.Equal(t, 7, attempts)
	require.Equal(t, startEpoch.Add(time.Second), msgs[0].FiredAt)
	require.Zero(t, entry.pressure)
	require.Zero(t, entry.attempts)
	require.Nil(t, entry.pending)

	cb.mu.Lock()
	cb.wedged = true
	cb.mu.Unlock()
	clock.Advance(time.Second)
	require.False(t, entry.pressure.warned)
	require.Equal(t, clock.Now(), entry.pressure.since)
	clock.Advance(31 * time.Second)
	require.True(t, entry.pressure.warned)

	// Cancelling the blocked episode removes both its timer and its policy
	// state. Reusing the ID must not inherit the old warning suppression.
	result = a.Receive(t.Context(), &CancelTimeoutRequest{ID: "tick"})
	require.True(t, result.IsOk())
	require.Empty(t, a.recurring)
	result = a.Receive(t.Context(), &ScheduleRecurringTickRequest{
		ID: "tick", Interval: time.Second, Callback: cb,
	})
	require.True(t, result.IsOk())
	clock.Advance(time.Second)
	require.False(t, a.recurring["tick"].pressure.warned)
	require.Equal(t, clock.Now(), a.recurring["tick"].pressure.since)
}

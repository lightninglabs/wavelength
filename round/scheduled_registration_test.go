package round

import (
	"context"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lib/batchschedule"
	"github.com/lightninglabs/wavelength/lib/types"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// testAnchor is a whole UTC second used as the reference cutoff throughout
// these tests.
var testAnchor = time.Unix(1800000000, 0).UTC()

// newTestSchedule returns an hourly timetable with a one minute window.
func newTestSchedule(t require.TestingT) *batchschedule.Schedule {
	schedule, err := batchschedule.New(testAnchor, time.Hour, time.Minute)
	require.NoError(t, err)

	return schedule
}

// requireWakeup asserts that a transition parks the attempt on the scheduled
// registration timer and returns the requested delay.
func requireWakeup(t *testing.T, s *PendingRoundAssembly,
	tr *ClientStateTransition) time.Duration {

	t.Helper()

	require.NotNil(t, tr)
	require.Same(t, s, tr.NextState)

	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	require.Len(t, outbox, 1)

	wakeup, ok := outbox[0].(*StartTimeoutReq)
	require.True(t, ok)
	require.Equal(t, TimeoutPhaseScheduledRegistration, wakeup.Phase)

	return wakeup.Duration
}

// TestScheduledRegistrationWindow walks one attempt through waiting, sending
// and a missed cutoff. The wake time is jittered, so the test asserts it lies
// within the window's wake bounds rather than at the exact opening.
func TestScheduledRegistrationWindow(t *testing.T) {
	t.Parallel()

	schedule := newTestSchedule(t)
	cutoff := testAnchor.Add(time.Hour)
	opens := cutoff.Add(-time.Minute)
	lo, hi := batchschedule.WakeBounds(time.Minute)

	now := testAnchor.Add(10 * time.Minute)
	env := &ClientEnvironment{
		OperatorTerms: &types.OperatorTerms{
			BatchSchedule: fn.Some(
				publishedTestSchedule(
					t, schedule, cutoff,
				),
			),
		},
		Now:      func() time.Time { return now },
		RoundKey: "pending",
	}
	s := &PendingRoundAssembly{}

	// Selection pins the next cutoff and sleeps until a wake time inside
	// the first half of the window, never at the opening itself.
	tr, err := s.waitForScheduledSlot(t.Context(), env)
	require.NoError(t, err)

	delay := requireWakeup(t, s, tr)
	require.GreaterOrEqual(t, delay, opens.Sub(now)+lo)
	require.LessOrEqual(t, delay, opens.Sub(now)+hi)

	selection := env.batchSlot().UnwrapOrFail(t)
	require.True(t, selection.Matches(schedule.ID(), cutoff))

	// Opening the window is not enough; the attempt waits for its own
	// wake time so the margin absorbs clock error.
	now = opens
	tr, err = s.waitForScheduledSlot(t.Context(), env)
	require.NoError(t, err)
	require.GreaterOrEqual(t, requireWakeup(t, s, tr), lo)

	// At the wake time the attempt proceeds, keeping its selection.
	now = testAnchor.Add(10 * time.Minute).Add(delay)
	tr, err = s.waitForScheduledSlot(t.Context(), env)
	require.NoError(t, err)
	require.Nil(t, tr)
	require.Equal(t, selection, env.batchSlot().UnwrapOrFail(t))

	// With less than the margin left the window counts as closed, and the
	// selection never slides to the next slot.
	now = cutoff.Add(-lo / 2)
	_, err = s.waitForScheduledSlot(t.Context(), env)
	require.ErrorContains(t, err, "closed")
	require.Equal(t, selection, env.batchSlot().UnwrapOrFail(t))

	// An operator without a published schedule keeps event-driven
	// registration.
	tr, err = s.waitForScheduledSlot(t.Context(), &ClientEnvironment{})
	require.NoError(t, err)
	require.Nil(t, tr)
}

// TestScheduledSelectionSkipsClosingSlot checks that an attempt triggered
// within the send margin of a cutoff pins the following slot and waits for it,
// rather than pinning a slot it can only fail to reach.
func TestScheduledSelectionSkipsClosingSlot(t *testing.T) {
	t.Parallel()

	schedule := newTestSchedule(t)
	cutoff := testAnchor.Add(time.Hour)

	// Trigger one second before the first cutoff, inside its window but
	// within the send margin.
	now := cutoff.Add(-time.Second)
	env := &ClientEnvironment{
		OperatorTerms: &types.OperatorTerms{
			BatchSchedule: fn.Some(
				publishedTestSchedule(
					t, schedule, cutoff,
				),
			),
		},
		Now:      func() time.Time { return now },
		RoundKey: "pending",
	}
	s := &PendingRoundAssembly{}

	// The attempt pins the next hourly cutoff and sleeps until inside
	// that slot's window.
	tr, err := s.waitForScheduledSlot(t.Context(), env)
	require.NoError(t, err)
	require.Greater(t, requireWakeup(t, s, tr), time.Hour-time.Minute)
	require.True(
		t,
		env.batchSlot().UnwrapOrFail(t).Cutoff.Equal(
			cutoff.Add(time.Hour),
		),
	)
}

// TestScheduledJoinsSpread checks that independent attempts for the same slot
// do not all wake at the same instant.
func TestScheduledJoinsSpread(t *testing.T) {
	t.Parallel()

	schedule := newTestSchedule(t)
	published := publishedTestSchedule(
		t, schedule, testAnchor.Add(time.Hour),
	)

	wakes := make(map[time.Time]struct{})
	for range 16 {
		attempt, err := newScheduledAttempt(published, testAnchor)
		require.NoError(t, err)

		wakes[attempt.wake] = struct{}{}
	}

	require.Greater(t, len(wakes), 1)
}

// TestScheduledClockOffsetProperty checks that, for any clock offset and any
// selection time, the join is sent inside the operator's window on the
// operator's clock: no earlier than the opening plus the margin, no later than
// the upper wake bound, and never within the margin of the cutoff.
func TestScheduledClockOffsetProperty(t *testing.T) {
	t.Parallel()

	schedule := newTestSchedule(t)
	cutoff := testAnchor.Add(8 * time.Hour)
	opens := cutoff.Add(-time.Minute)
	lo, hi := batchschedule.WakeBounds(time.Minute)

	rapid.Check(t, func(rt *rapid.T) {
		// A positive offset means the operator's clock is ahead.
		offset := time.Duration(
			rapid.Int64Range(
				int64(-2*time.Hour), int64(2*time.Hour),
			).Draw(rt, "offset"),
		)

		// Select at some operator-clock instant before the window
		// opens, expressed on the local clock.
		lead := time.Duration(
			rapid.Int64Range(
				int64(time.Second), int64(50*time.Minute),
			).Draw(rt, "lead"),
		)
		local := opens.Add(-lead).Add(-offset)

		published := publishedTestSchedule(rt, schedule, cutoff).
			WithClockOffset(offset)
		env := &ClientEnvironment{
			OperatorTerms: &types.OperatorTerms{
				BatchSchedule: fn.Some(published),
			},
			Now:      func() time.Time { return local },
			RoundKey: "pending",
		}
		s := &PendingRoundAssembly{}

		tr, err := s.waitForScheduledSlot(context.Background(), env)
		require.NoError(rt, err)
		require.NotNil(rt, tr)

		outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
		require.Len(rt, outbox, 1)
		wakeup, ok := outbox[0].(*StartTimeoutReq)
		require.True(rt, ok)
		delay := wakeup.Duration

		// Fire the timer and translate the send instant to the
		// operator's clock.
		local = local.Add(delay)
		tr, err = s.waitForScheduledSlot(context.Background(), env)
		require.NoError(rt, err)
		require.Nil(rt, tr)
		require.NoError(rt, env.validateScheduledSend())

		sent := local.Add(offset)
		require.False(rt, sent.Before(opens.Add(lo)))
		require.False(rt, sent.After(opens.Add(hi)))
		require.GreaterOrEqual(rt, cutoff.Sub(sent), lo)
	})
}

// TestScheduledRegistrationCursor selects from the published list: a list
// starting at a later cursor is honored, and a stale list does not hold the
// client on an expired slot.
func TestScheduledRegistrationCursor(t *testing.T) {
	t.Parallel()

	schedule := newTestSchedule(t)
	now := testAnchor.Add(10 * time.Minute)
	env := &ClientEnvironment{
		OperatorTerms: &types.OperatorTerms{
			BatchSchedule: fn.Some(
				publishedTestSchedule(
					t, schedule,
					testAnchor.Add(2*time.Hour),
				),
			),
		},
		Now: func() time.Time { return now },
	}
	s := &PendingRoundAssembly{}

	_, err := s.waitForScheduledSlot(t.Context(), env)
	require.NoError(t, err)
	require.True(
		t,
		env.batchSlot().UnwrapOrFail(t).Cutoff.Equal(
			testAnchor.Add(2*time.Hour),
		),
	)

	// A new attempt three hours later skips every published cutoff that
	// has already passed.
	now = testAnchor.Add(3 * time.Hour)
	env.scheduled = nil
	_, err = s.waitForScheduledSlot(t.Context(), env)
	require.NoError(t, err)
	require.True(
		t,
		env.batchSlot().UnwrapOrFail(t).Cutoff.Equal(
			testAnchor.Add(4*time.Hour),
		),
	)
}

// TestScheduledRegistrationLatestTerms pins the refreshed terms at selection
// and keeps them for the rest of the attempt, even when the shared cache
// changes while the attempt waits.
func TestScheduledRegistrationLatestTerms(t *testing.T) {
	t.Parallel()

	schedule := newTestSchedule(t)
	cutoff := testAnchor.Add(2 * time.Hour)
	latest := &types.OperatorTerms{
		BatchSchedule: fn.Some(
			publishedTestSchedule(t, schedule, cutoff),
		),
	}

	now := testAnchor.Add(10 * time.Minute)
	env := &ClientEnvironment{
		OperatorTerms: &types.OperatorTerms{},
		OperatorTermsSource: termsSourceFunc(func(context.Context) (
			*types.OperatorTerms, error) {

			return latest, nil
		}),
		Now: func() time.Time { return now },
	}
	s := &PendingRoundAssembly{}

	_, err := s.waitForScheduledSlot(t.Context(), env)
	require.NoError(t, err)
	require.Same(t, latest, env.OperatorTerms)
	require.True(t, env.batchSlot().UnwrapOrFail(t).Cutoff.Equal(cutoff))

	// A later wakeup neither refreshes again nor adopts the new cache.
	selected := env.OperatorTerms
	latest = &types.OperatorTerms{}
	_, hi := batchschedule.WakeBounds(time.Minute)
	now = cutoff.Add(-time.Minute).Add(hi)

	tr, err := s.waitForScheduledSlot(t.Context(), env)
	require.NoError(t, err)
	require.Nil(t, tr)
	require.Same(t, selected, env.OperatorTerms)
}

// TestScheduledPreparationCrossesCutoff drives the real assembly transition
// with a wallet key derivation that consumes the rest of the window. The
// attempt must fail locally without emitting a join.
func TestScheduledPreparationCrossesCutoff(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	schedule := newTestSchedule(t)
	cutoff := testAnchor.Add(time.Hour)
	_, hi := batchschedule.WakeBounds(time.Minute)

	// Start past the latest possible wake time so the attempt proceeds
	// straight to building its join.
	now := cutoff.Add(-time.Minute).Add(hi)
	h.env.Now = func() time.Time { return now }
	h.env.OperatorTerms.BatchSchedule = fn.Some(
		publishedTestSchedule(
			t, schedule, cutoff,
		),
	)

	for _, call := range h.wallet.ExpectedCalls {
		if call.Method == "DeriveNextKey" {
			call.Run(func(_ mock.Arguments) {
				now = cutoff
			})
		}
	}

	intent := h.newTestBoardingIntent()
	h.withState(&PendingRoundAssembly{
		Boarding: []BoardingIntent{intent},
		VTXOs: []types.VTXORequest{
			h.newTestVTXORequestForIntent(intent),
		},
	})

	tr, err := h.sendEvent(&IntentRequested{})
	require.NoError(t, err)
	require.IsType(t, &ClientFailedState{}, tr.NextState)

	for _, msg := range tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox {
		_, isJoin := msg.(*JoinRoundRequest)
		require.False(t, isJoin)
	}
}

// TestScheduledRegistrationExhausted never extrapolates an unpublished cutoff,
// and a fresh discovery may publish an irregular slot that the client then
// uses as listed.
func TestScheduledRegistrationExhausted(t *testing.T) {
	t.Parallel()

	now := testAnchor
	p, err := batchschedule.NewPublished(
		batchschedule.ID{1}, []batchschedule.Slot{{
			Opens:  now.Add(-time.Minute),
			Cutoff: now,
		}},
	)
	require.NoError(t, err)

	env := &ClientEnvironment{
		OperatorTerms: &types.OperatorTerms{
			BatchSchedule: fn.Some(p),
		},
		Now: func() time.Time { return now },
	}
	s := &PendingRoundAssembly{}

	_, err = s.waitForScheduledSlot(t.Context(), env)
	require.ErrorIs(t, err, batchschedule.ErrScheduleExhausted)
	require.True(t, env.batchSlot().IsNone())

	// A new discovery supplies a two minute window after the old list.
	// The wake time is measured from its explicit opening.
	opens := now.Add(17 * time.Minute)
	cutoff := now.Add(19 * time.Minute)
	p, err = batchschedule.NewPublished(
		batchschedule.ID{2}, []batchschedule.Slot{{
			Opens:  opens,
			Cutoff: cutoff,
		}},
	)
	require.NoError(t, err)

	calls := 0
	env.OperatorTermsSource = termsSourceFunc(func(ctx context.Context) (
		*types.OperatorTerms, error) {

		_, bounded := ctx.Deadline()
		require.True(t, bounded)
		calls++

		return &types.OperatorTerms{BatchSchedule: fn.Some(p)}, nil
	})

	tr, err := s.waitForScheduledSlot(t.Context(), env)
	require.NoError(t, err)

	lo, hi := batchschedule.WakeBounds(2 * time.Minute)
	delay := requireWakeup(t, s, tr)
	require.GreaterOrEqual(t, delay, 17*time.Minute+lo)
	require.LessOrEqual(t, delay, 17*time.Minute+hi)
	require.True(t, env.batchSlot().UnwrapOrFail(t).Cutoff.Equal(cutoff))

	now = now.Add(delay)
	tr, err = s.waitForScheduledSlot(t.Context(), env)
	require.NoError(t, err)
	require.Nil(t, tr)
	require.Equal(t, 1, calls)
}

// TestScheduledDiscoveryFailure rejects an unavailable refresh rather than
// selecting from old cached slots or falling back to event-driven
// registration.
func TestScheduledDiscoveryFailure(t *testing.T) {
	t.Parallel()

	env := &ClientEnvironment{
		OperatorTermsSource: termsSourceFunc(func(context.Context) (
			*types.OperatorTerms, error) {

			return nil, context.DeadlineExceeded
		}),
	}
	state := &PendingRoundAssembly{}

	_, err := state.waitForScheduledSlot(t.Context(), env)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.True(t, env.batchSlot().IsNone())
}

// publishedTestSchedule builds the same bounded discovery an operator
// publishes, starting at cutoff, and parses it as a client would.
func publishedTestSchedule(t require.TestingT, schedule *batchschedule.Schedule,
	cutoff time.Time) batchschedule.Published {

	p, err := arkrpc.ParseBatchSchedule(
		arkrpc.BatchScheduleToProto(
			schedule, cutoff.Add(-time.Hour), cutoff,
		),
	)
	require.NoError(t, err)
	require.True(t, p.IsSome())

	return p.UnsafeFromSome()
}

// termsSourceFunc adapts a function to the OperatorTermsSource interface.
type termsSourceFunc func(context.Context) (*types.OperatorTerms, error)

// FreshOperatorTerms calls the wrapped function.
func (f termsSourceFunc) FreshOperatorTerms(ctx context.Context) (
	*types.OperatorTerms, error) {

	return f(ctx)
}

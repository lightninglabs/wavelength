package timeout

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/stretchr/testify/require"
)

// mockCallbackRef implements actor.TellOnlyRef[*ExpiredMsg] for testing.
// It captures all ExpiredMsg messages sent to it.
type mockCallbackRef struct {
	t        *testing.T
	id       string
	messages []ExpiredMsg
	mu       sync.Mutex
}

func newMockCallbackRef(t *testing.T, id string) *mockCallbackRef {
	return &mockCallbackRef{
		t:        t,
		id:       id,
		messages: make([]ExpiredMsg, 0),
	}
}

func (m *mockCallbackRef) ID() string {
	return m.id
}

func (m *mockCallbackRef) Tell(_ context.Context, msg *ExpiredMsg) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if msg == nil {
		return nil
	}

	m.messages = append(m.messages, *msg)

	return nil
}

// getMessages returns a copy of all received messages.
func (m *mockCallbackRef) getMessages() []ExpiredMsg {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]ExpiredMsg{}, m.messages...)
}

// assertNoMessages verifies that no messages have been received.
func (m *mockCallbackRef) assertNoMessages(t *testing.T) {
	t.Helper()

	m.mu.Lock()
	defer m.mu.Unlock()

	require.Empty(
		t, m.messages, "expected no messages, got %d", len(m.messages),
	)
}

// TestActorScheduleWithoutStartDropsTimerFire verifies direct tests that forget
// to inject the self-ref do not panic from timer goroutines.
func TestActorScheduleWithoutStartDropsTimerFire(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := NewActorWithClock(clock)
	callback := newMockCallbackRef(t, "callback")

	result := a.Receive(ctx, &ScheduleTimeoutRequest{
		ID:       "missing-self",
		Duration: 50 * time.Millisecond,
		Callback: callback,
	})
	require.True(t, result.IsOk(), "schedule should succeed")

	require.NotPanics(t, func() {
		clock.Advance(50 * time.Millisecond)
	})
	callback.assertNoMessages(t)
}

// scheduleStep schedules a one-shot timeout on the shared callback.
type scheduleStep struct {
	id       ID
	duration time.Duration
}

// advanceStep advances the fake clock by d.
type advanceStep struct{ d time.Duration }

// cancelStep cancels the timeout with the given ID.
type cancelStep struct{ id ID }

// oneShotStep is one action in a one-shot scheduling scenario: exactly
// one of its fields is non-nil.
type oneShotStep struct {
	schedule *scheduleStep
	advance  *advanceStep
	cancel   *cancelStep
}

// TestActorOneShotScheduling drives a single shared callback through a
// sequence of schedule / advance / cancel steps and asserts the IDs of
// the ExpiredMsg deliveries observed at the end. It folds the basic
// schedule-and-expire, cancel-before/after-expiry, cancel-non-existent,
// duplicate-ID replacement, zero-duration, and reschedule-after-expiry
// cases into one data-driven table: each row differs only in its step
// list and expected message IDs, so the shared runner replaces the
// per-case boilerplate.
func TestActorOneShotScheduling(t *testing.T) {
	t.Parallel()

	const id ID = "test-timeout"

	sched := func(id ID, d time.Duration) oneShotStep {
		return oneShotStep{schedule: &scheduleStep{id, d}}
	}
	adv := func(d time.Duration) oneShotStep {
		return oneShotStep{advance: &advanceStep{d}}
	}
	cancel := func(id ID) oneShotStep {
		return oneShotStep{cancel: &cancelStep{id}}
	}

	const ms = time.Millisecond

	tests := []struct {
		name  string
		steps []oneShotStep
		want  []ID
	}{
		{
			name: "schedule and expire",
			steps: []oneShotStep{
				sched(id, 50*ms),
				adv(50 * ms),
			},
			want: []ID{
				id,
			},
		},
		{
			name: "cancel before expiry",
			steps: []oneShotStep{
				sched(id, 500*ms), cancel(id),
				adv(time.Second),
			},
			want: nil,
		},
		{
			name: "cancel non-existent",
			steps: []oneShotStep{
				cancel("non-existent"),
			},
			want: nil,
		},
		{
			name: "duplicate id replaces",
			steps: []oneShotStep{
				sched("duplicate-id", 500*ms),
				sched("duplicate-id", 50*ms),
				adv(time.Second),
			},
			want: []ID{
				"duplicate-id",
			},
		},
		{
			name: "zero duration",
			steps: []oneShotStep{
				sched("zero", 0),
				adv(0),
			},
			want: []ID{
				"zero",
			},
		},
		{
			name: "cancel after expiry",
			steps: []oneShotStep{
				sched(id, 50*ms), adv(50 * ms), cancel(id),
			},
			want: []ID{
				id,
			},
		},
		{
			name: "reschedule after expiry",
			steps: []oneShotStep{
				sched(id, 50*ms), adv(50 * ms),
				sched(id, 50*ms), adv(50 * ms),
			},
			want: []ID{
				id,
				id,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			clock := newFakeClock(startEpoch)
			a := newTestActor(clock)
			cb := newMockCallbackRef(t, "callback")

			for _, step := range tc.steps {
				switch {
				case step.schedule != nil:
					res := a.Receive(
						ctx, &ScheduleTimeoutRequest{
							ID: step.schedule.id,
							Duration: step.
								schedule.
								duration,
							Callback: cb,
						},
					)
					require.True(t, res.IsOk())

				case step.advance != nil:
					clock.Advance(step.advance.d)

				case step.cancel != nil:
					res := a.Receive(
						ctx, &CancelTimeoutRequest{
							ID: step.cancel.id,
						},
					)
					require.True(t, res.IsOk())
				}
			}

			var got []ID
			for _, m := range cb.getMessages() {
				got = append(got, m.ID)
			}
			require.Equal(t, tc.want, got)
		})
	}
}

// TestActorMultipleConcurrentTimeouts tests scheduling multiple timeouts that
// fire concurrently.
func TestActorMultipleConcurrentTimeouts(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)

	callback1 := newMockCallbackRef(t, "callback1")
	callback2 := newMockCallbackRef(t, "callback2")
	callback3 := newMockCallbackRef(t, "callback3")

	timeouts := []struct {
		id       ID
		duration time.Duration
		callback *mockCallbackRef
	}{
		{
			"timeout-1",
			50 * time.Millisecond,
			callback1,
		},
		{
			"timeout-2",
			75 * time.Millisecond,
			callback2,
		},
		{
			"timeout-3",
			100 * time.Millisecond,
			callback3,
		},
	}

	for _, tc := range timeouts {
		req := &ScheduleTimeoutRequest{
			ID:       tc.id,
			Duration: tc.duration,
			Callback: tc.callback,
		}

		result := a.Receive(ctx, req)
		require.True(t, result.IsOk())
	}

	// Advance past the longest deadline.
	clock.Advance(100 * time.Millisecond)

	require.Len(t, callback1.getMessages(), 1)
	require.Equal(t, ID("timeout-1"), callback1.getMessages()[0].ID)
	require.Len(t, callback2.getMessages(), 1)
	require.Equal(t, ID("timeout-2"), callback2.getMessages()[0].ID)
	require.Len(t, callback3.getMessages(), 1)
	require.Equal(t, ID("timeout-3"), callback3.getMessages()[0].ID)
}

// TestActorConcurrentSendsViaActorSystem exercises the actor through a
// real actor.ActorSystem mailbox, with multiple goroutines Tell-ing
// schedule and cancel requests in parallel. Under the self-tell model
// the actor itself has no internal mutex; the framework's mailbox is
// the serialization point. This test fails if a regression
// reintroduces concurrent state mutation.
func TestActorConcurrentSendsViaActorSystem(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		_ = system.Shutdown(context.Background())
	})

	behavior := NewActor()
	key := actor.NewServiceKey[Msg, Resp]("test-timeout")
	ref := actor.RegisterWithSystem(system, "test-timeout", key, behavior)
	behavior.Start(ref)

	const numGoroutines = 10
	const numOperations = 50

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()

			callback := newMockCallbackRef(
				t, fmt.Sprintf("callback-%d", id),
			)

			for j := 0; j < numOperations; j++ {
				timeoutID := ID(
					fmt.Sprintf("timeout-%d-%d", id, j),
				)

				err := ref.Tell(ctx, &ScheduleTimeoutRequest{
					ID:       timeoutID,
					Duration: 100 * time.Millisecond,
					Callback: callback,
				})
				require.NoError(t, err)

				if j%2 == 0 {
					cancel := &CancelTimeoutRequest{
						ID: timeoutID,
					}

					err = ref.Tell(ctx, cancel)
					require.NoError(t, err)
				}
			}
		}(i)
	}

	wg.Wait()

	// No assertions on callback contents — the goal is to surface
	// races in the actor's state mutation under -race. The test
	// passes if the mailbox processes every message without data
	// races and without panics.
}

// TestActorDifferentCallbacks tests that different callbacks can be used for
// different timeouts.
func TestActorDifferentCallbacks(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)

	callback1 := newMockCallbackRef(t, "callback1")
	callback2 := newMockCallbackRef(t, "callback2")

	req1 := &ScheduleTimeoutRequest{
		ID:       "timeout-1",
		Duration: 50 * time.Millisecond,
		Callback: callback1,
	}

	result := a.Receive(ctx, req1)
	require.True(t, result.IsOk())

	req2 := &ScheduleTimeoutRequest{
		ID:       "timeout-2",
		Duration: 50 * time.Millisecond,
		Callback: callback2,
	}

	result = a.Receive(ctx, req2)
	require.True(t, result.IsOk())

	clock.Advance(50 * time.Millisecond)

	msgs1 := callback1.getMessages()
	require.Len(t, msgs1, 1)
	require.Equal(t, ID("timeout-1"), msgs1[0].ID)

	msgs2 := callback2.getMessages()
	require.Len(t, msgs2, 1)
	require.Equal(t, ID("timeout-2"), msgs2[0].ID)
}

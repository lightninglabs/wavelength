package timeout

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

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

// waitForMessage waits for a message with the given ID to arrive, or times out.
func (m *mockCallbackRef) waitForMessage(t *testing.T, id ID,
	timeout time.Duration) ExpiredMsg {

	t.Helper()

	var receivedMsg ExpiredMsg
	require.Eventually(
		t,
		func() bool {
			m.mu.Lock()
			defer m.mu.Unlock()

			for _, msg := range m.messages {
				if msg.ID == id {
					receivedMsg = msg
					return true
				}
			}

			return false
		},
		timeout,
		10*time.Millisecond,
		"timed out waiting for message with ID %v", id,
	)

	return receivedMsg
}

// assertNoMessages verifies that no messages have been received.
func (m *mockCallbackRef) assertNoMessages(t *testing.T) {
	t.Helper()

	m.mu.Lock()
	defer m.mu.Unlock()

	require.Empty(t, m.messages,
		"expected no messages, got %d", len(m.messages))
}

// TestActorScheduleAndExpire tests basic timeout scheduling and expiration.
func TestActorScheduleAndExpire(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	actor := NewActor()
	callback := newMockCallbackRef(t, "callback")

	// Schedule a timeout with a short duration.
	req := &ScheduleTimeoutRequest{
		ID:       "test-timeout",
		Duration: 50 * time.Millisecond,
		Callback: callback,
	}

	result := actor.Receive(ctx, req)
	require.True(t, result.IsOk(), "schedule should succeed")

	resp, ok := result.UnwrapOrFail(t).(*AckResponse)
	require.True(t, ok, "response should be AckResponse")
	require.True(t, resp.Success)

	// Wait for the timeout to expire and callback to be invoked.
	msg := callback.waitForMessage(t, "test-timeout", 200*time.Millisecond)
	require.Equal(t, ID("test-timeout"), msg.ID)
}

// TestActorCancelBeforeExpiry tests cancelling a timeout before it fires.
func TestActorCancelBeforeExpiry(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	actor := NewActor()
	callback := newMockCallbackRef(t, "callback")

	// Schedule a timeout with a longer duration.
	scheduleReq := &ScheduleTimeoutRequest{
		ID:       "test-timeout",
		Duration: 500 * time.Millisecond,
		Callback: callback,
	}

	result := actor.Receive(ctx, scheduleReq)
	require.True(t, result.IsOk())

	// Cancel the timeout before it expires.
	cancelReq := &CancelTimeoutRequest{
		ID: "test-timeout",
	}

	result = actor.Receive(ctx, cancelReq)
	require.True(t, result.IsOk())

	resp, ok := result.UnwrapOrFail(t).(*AckResponse)
	require.True(t, ok, "response should be AckResponse")
	require.True(t, resp.Success)

	// Wait a bit to ensure the timeout doesn't fire.
	time.Sleep(100 * time.Millisecond)

	// Verify no messages were received.
	callback.assertNoMessages(t)
}

// TestActorCancelNonExistent tests cancelling a timeout that doesn't exist.
func TestActorCancelNonExistent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	actor := NewActor()

	// Cancel a timeout that was never scheduled.
	cancelReq := &CancelTimeoutRequest{
		ID: "non-existent",
	}

	result := actor.Receive(ctx, cancelReq)
	require.True(t, result.IsOk(), "cancel non-existent should succeed")

	resp, ok := result.UnwrapOrFail(t).(*AckResponse)
	require.True(t, ok, "response should be AckResponse")
	require.True(t, resp.Success)
}

// TestActorMultipleConcurrentTimeouts tests scheduling multiple timeouts that
// fire concurrently.
func TestActorMultipleConcurrentTimeouts(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	actor := NewActor()

	// Create multiple callbacks.
	callback1 := newMockCallbackRef(t, "callback1")
	callback2 := newMockCallbackRef(t, "callback2")
	callback3 := newMockCallbackRef(t, "callback3")

	// Schedule multiple timeouts with different durations.
	timeouts := []struct {
		id       ID
		duration time.Duration
		callback *mockCallbackRef
	}{
		{"timeout-1", 50 * time.Millisecond, callback1},
		{"timeout-2", 75 * time.Millisecond, callback2},
		{"timeout-3", 100 * time.Millisecond, callback3},
	}

	for _, tc := range timeouts {
		req := &ScheduleTimeoutRequest{
			ID:       tc.id,
			Duration: tc.duration,
			Callback: tc.callback,
		}

		result := actor.Receive(ctx, req)
		require.True(t, result.IsOk())
	}

	// Wait for all timeouts to fire.
	msg1 := callback1.waitForMessage(t, "timeout-1", 200*time.Millisecond)
	require.Equal(t, ID("timeout-1"), msg1.ID)

	msg2 := callback2.waitForMessage(t, "timeout-2", 200*time.Millisecond)
	require.Equal(t, ID("timeout-2"), msg2.ID)

	msg3 := callback3.waitForMessage(t, "timeout-3", 200*time.Millisecond)
	require.Equal(t, ID("timeout-3"), msg3.ID)
}

// TestActorDuplicateIDReplacesTimeout tests that scheduling a timeout with a
// duplicate ID cancels the previous timeout and replaces it.
func TestActorDuplicateIDReplacesTimeout(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	actor := NewActor()
	callback := newMockCallbackRef(t, "callback")

	// Schedule first timeout with longer duration.
	req1 := &ScheduleTimeoutRequest{
		ID:       "duplicate-id",
		Duration: 500 * time.Millisecond,
		Callback: callback,
	}

	result := actor.Receive(ctx, req1)
	require.True(t, result.IsOk())

	// Immediately schedule another timeout with the same ID but shorter
	// duration.
	req2 := &ScheduleTimeoutRequest{
		ID:       "duplicate-id",
		Duration: 50 * time.Millisecond,
		Callback: callback,
	}

	result = actor.Receive(ctx, req2)
	require.True(t, result.IsOk())

	// Wait for the second (shorter) timeout to fire.
	msg := callback.waitForMessage(t, "duplicate-id", 200*time.Millisecond)
	require.Equal(t, ID("duplicate-id"), msg.ID)

	// Verify only one message was received (first timeout was cancelled).
	time.Sleep(100 * time.Millisecond)
	msgs := callback.getMessages()
	require.Len(t, msgs, 1, "should only receive one message")
}

// TestActorCancelAfterExpiry tests cancelling a timeout after it has already
// fired.
func TestActorCancelAfterExpiry(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	actor := NewActor()
	callback := newMockCallbackRef(t, "callback")

	// Schedule a timeout with a short duration.
	scheduleReq := &ScheduleTimeoutRequest{
		ID:       "test-timeout",
		Duration: 50 * time.Millisecond,
		Callback: callback,
	}

	result := actor.Receive(ctx, scheduleReq)
	require.True(t, result.IsOk())

	// Wait for the timeout to expire.
	_ = callback.waitForMessage(t, "test-timeout", 200*time.Millisecond)

	// Try to cancel after expiry - should succeed but be a no-op.
	cancelReq := &CancelTimeoutRequest{
		ID: "test-timeout",
	}

	result = actor.Receive(ctx, cancelReq)
	require.True(t, result.IsOk())

	resp, ok := result.UnwrapOrFail(t).(*AckResponse)
	require.True(t, ok, "response should be AckResponse")
	require.True(t, resp.Success)
}

// TestActorThreadSafety tests concurrent access to the actor from multiple
// goroutines.
func TestActorThreadSafety(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	actor := NewActor()

	const numGoroutines = 10
	const numOperations = 50

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Launch multiple goroutines that schedule and cancel timeouts
	// concurrently.
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()

			callback := newMockCallbackRef(
				t, "callback",
			)

			for j := 0; j < numOperations; j++ {
				timeoutID := ID(fmt.Sprintf(
					"timeout-%d-%d", id, j,
				))

				// Schedule a timeout.
				scheduleReq := &ScheduleTimeoutRequest{
					ID:       timeoutID,
					Duration: 100 * time.Millisecond,
					Callback: callback,
				}

				result := actor.Receive(ctx, scheduleReq)
				require.True(t, result.IsOk())

				// Randomly cancel some timeouts.
				if j%2 == 0 {
					cancelReq := &CancelTimeoutRequest{
						ID: timeoutID,
					}

					result = actor.Receive(ctx, cancelReq)
					require.True(t, result.IsOk())
				}
			}
		}(i)
	}

	wg.Wait()

	// No assertions needed - test passes if no race conditions occur.
}

// TestActorDifferentCallbacks tests that different callbacks can be used for
// different timeouts.
func TestActorDifferentCallbacks(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	actor := NewActor()

	callback1 := newMockCallbackRef(t, "callback1")
	callback2 := newMockCallbackRef(t, "callback2")

	// Schedule two timeouts with different callbacks.
	req1 := &ScheduleTimeoutRequest{
		ID:       "timeout-1",
		Duration: 50 * time.Millisecond,
		Callback: callback1,
	}

	result := actor.Receive(ctx, req1)
	require.True(t, result.IsOk())

	req2 := &ScheduleTimeoutRequest{
		ID:       "timeout-2",
		Duration: 50 * time.Millisecond,
		Callback: callback2,
	}

	result = actor.Receive(ctx, req2)
	require.True(t, result.IsOk())

	// Wait for both timeouts to fire.
	msg1 := callback1.waitForMessage(t, "timeout-1", 200*time.Millisecond)
	require.Equal(t, ID("timeout-1"), msg1.ID)

	msg2 := callback2.waitForMessage(t, "timeout-2", 200*time.Millisecond)
	require.Equal(t, ID("timeout-2"), msg2.ID)

	// Verify each callback only received its own message.
	msgs1 := callback1.getMessages()
	require.Len(t, msgs1, 1)
	require.Equal(t, ID("timeout-1"), msgs1[0].ID)

	msgs2 := callback2.getMessages()
	require.Len(t, msgs2, 1)
	require.Equal(t, ID("timeout-2"), msgs2[0].ID)
}

// TestActorZeroDuration tests scheduling a timeout with zero duration.
func TestActorZeroDuration(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	actor := NewActor()
	callback := newMockCallbackRef(t, "callback")

	// Schedule a timeout with zero duration - should fire immediately.
	req := &ScheduleTimeoutRequest{
		ID:       "zero-duration",
		Duration: 0,
		Callback: callback,
	}

	result := actor.Receive(ctx, req)
	require.True(t, result.IsOk())

	// Wait for the timeout to fire immediately.
	msg := callback.waitForMessage(t, "zero-duration", 100*time.Millisecond)
	require.Equal(t, ID("zero-duration"), msg.ID)
}

// TestActorRescheduleAfterExpiry tests rescheduling a timeout after it has
// already expired.
func TestActorRescheduleAfterExpiry(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	actor := NewActor()
	callback := newMockCallbackRef(t, "callback")

	// Schedule first timeout.
	req1 := &ScheduleTimeoutRequest{
		ID:       "test-timeout",
		Duration: 50 * time.Millisecond,
		Callback: callback,
	}

	result := actor.Receive(ctx, req1)
	require.True(t, result.IsOk())

	// Wait for it to expire.
	_ = callback.waitForMessage(t, "test-timeout", 200*time.Millisecond)

	// Schedule the same ID again.
	req2 := &ScheduleTimeoutRequest{
		ID:       "test-timeout",
		Duration: 50 * time.Millisecond,
		Callback: callback,
	}

	result = actor.Receive(ctx, req2)
	require.True(t, result.IsOk())

	// Wait for the second timeout to fire.
	time.Sleep(100 * time.Millisecond)

	// Should have received two messages total.
	msgs := callback.getMessages()
	require.Len(t, msgs, 2)
	require.Equal(t, ID("test-timeout"), msgs[0].ID)
	require.Equal(t, ID("test-timeout"), msgs[1].ID)
}

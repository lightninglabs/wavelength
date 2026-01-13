//go:build systest

package systest

import (
	"context"
	"fmt"
	"sync"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo/timeout"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// scheduledTimeout tracks a scheduled timeout and its callback.
type scheduledTimeout struct {
	callback actor.TellOnlyRef[*timeout.ExpiredMsg]
}

// MockTimeoutActor provides instant timeout triggering for tests. Instead of
// using real timers, it stores callbacks that can be manually triggered.
type MockTimeoutActor struct {
	mu        sync.Mutex
	scheduled map[timeout.ID]*scheduledTimeout
}

// NewMockTimeoutActor creates a new mock timeout actor.
func NewMockTimeoutActor() *MockTimeoutActor {
	return &MockTimeoutActor{
		scheduled: make(map[timeout.ID]*scheduledTimeout),
	}
}

// Receive processes timeout requests. Instead of scheduling real timers, it
// stores the callback for manual triggering via TriggerTimeout.
func (a *MockTimeoutActor) Receive(ctx context.Context,
	msg timeout.Msg) fn.Result[timeout.Resp] {

	switch m := msg.(type) {
	case *timeout.ScheduleTimeoutRequest:
		return a.handleSchedule(ctx, m)

	case *timeout.CancelTimeoutRequest:
		return a.handleCancel(ctx, m)

	default:
		return fn.Err[timeout.Resp](fmt.Errorf(
			"unknown message type: %T", msg,
		))
	}
}

// handleSchedule stores the timeout callback for later manual triggering.
func (a *MockTimeoutActor) handleSchedule(_ context.Context,
	req *timeout.ScheduleTimeoutRequest) fn.Result[timeout.Resp] {

	a.mu.Lock()
	defer a.mu.Unlock()

	// Store the callback, replacing any existing one with the same ID.
	a.scheduled[req.ID] = &scheduledTimeout{
		callback: req.Callback,
	}

	return fn.Ok[timeout.Resp](&timeout.AckResponse{
		Success: true,
	})
}

// handleCancel removes a pending timeout.
func (a *MockTimeoutActor) handleCancel(_ context.Context,
	req *timeout.CancelTimeoutRequest) fn.Result[timeout.Resp] {

	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.scheduled, req.ID)

	return fn.Ok[timeout.Resp](&timeout.AckResponse{
		Success: true,
	})
}

// TriggerTimeout manually fires a timeout by ID. Returns an error if the
// timeout ID is not found.
func (a *MockTimeoutActor) TriggerTimeout(ctx context.Context,
	id timeout.ID) error {

	a.mu.Lock()
	entry, ok := a.scheduled[id]
	if !ok {
		a.mu.Unlock()
		return fmt.Errorf("timeout %s not found", id)
	}

	// Remove from scheduled before firing to prevent double-trigger.
	delete(a.scheduled, id)
	a.mu.Unlock()

	// Send the expiry message to the callback.
	entry.callback.Tell(ctx, &timeout.ExpiredMsg{
		ID: id,
	})

	return nil
}

// TriggerAll fires all pending timeouts. Useful for tests that want to advance
// through all timeouts at once.
func (a *MockTimeoutActor) TriggerAll(ctx context.Context) {
	a.mu.Lock()

	// Collect all entries before releasing the lock.
	entries := make(map[timeout.ID]*scheduledTimeout)
	for id, entry := range a.scheduled {
		entries[id] = entry
	}

	// Clear all scheduled timeouts.
	a.scheduled = make(map[timeout.ID]*scheduledTimeout)
	a.mu.Unlock()

	// Fire all callbacks outside the lock.
	for id, entry := range entries {
		entry.callback.Tell(ctx, &timeout.ExpiredMsg{
			ID: id,
		})
	}
}

// PendingCount returns the number of pending timeouts.
func (a *MockTimeoutActor) PendingCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return len(a.scheduled)
}

// HasPendingTimeout returns true if a timeout with the given ID is pending.
func (a *MockTimeoutActor) HasPendingTimeout(id timeout.ID) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	_, ok := a.scheduled[id]
	return ok
}

// PendingTimeoutIDs returns all pending timeout IDs.
func (a *MockTimeoutActor) PendingTimeoutIDs() []timeout.ID {
	a.mu.Lock()
	defer a.mu.Unlock()

	ids := make([]timeout.ID, 0, len(a.scheduled))
	for id := range a.scheduled {
		ids = append(ids, id)
	}

	return ids
}

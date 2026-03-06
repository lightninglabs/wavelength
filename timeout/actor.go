package timeout

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// timeoutEntry tracks a scheduled timeout and its associated timer.
type timeoutEntry struct {
	timer    *time.Timer
	callback actor.TellOnlyRef[*ExpiredMsg]
}

// Actor is the timeout scheduling actor. It manages timers and sends
// expiry notifications when timeouts expire.
type Actor struct {
	// mu protects the timers map for concurrent access.
	mu sync.Mutex

	// timers maps timeout IDs to their active timers and callbacks.
	timers map[ID]*timeoutEntry
}

// NewActor creates a new timeout actor.
func NewActor() *Actor {
	return &Actor{
		timers: make(map[ID]*timeoutEntry),
	}
}

// Receive processes incoming messages.
func (a *Actor) Receive(ctx context.Context, msg Msg) fn.Result[Resp] {
	switch m := msg.(type) {
	case *ScheduleTimeoutRequest:
		return a.handleSchedule(ctx, m)

	case *CancelTimeoutRequest:
		return a.handleCancel(ctx, m)

	default:
		return fn.Err[Resp](fmt.Errorf(
			"unknown message type: %T", msg))
	}
}

// handleSchedule schedules a new timeout. If a timeout with the same ID
// already exists, it will be cancelled and replaced with the new one.
func (a *Actor) handleSchedule(_ context.Context,
	req *ScheduleTimeoutRequest) fn.Result[Resp] {

	a.mu.Lock()
	defer a.mu.Unlock()

	// Cancel existing timer if one exists for this ID.
	if existing, ok := a.timers[req.ID]; ok {
		existing.timer.Stop()
		delete(a.timers, req.ID)
	}

	// Create a new timer that will fire after the specified duration.
	// We need to store the timer in a variable that can be captured by
	// the closure before the closure runs.
	var timer *time.Timer
	timer = time.AfterFunc(req.Duration, func() {
		// When the timer fires, acquire the lock to verify this
		// timer is still active (not cancelled or replaced).
		a.mu.Lock()

		entry, ok := a.timers[req.ID]
		if !ok || entry.timer != timer {
			// Timer was cancelled or rescheduled. Do nothing.
			a.mu.Unlock()

			return
		}

		// Capture the callback and clean up the entry while
		// holding the lock. Release the lock before calling Tell
		// to avoid blocking concurrent Schedule/Cancel operations
		// if the receiving mailbox applies backpressure.
		cb := entry.callback
		delete(a.timers, req.ID)
		a.mu.Unlock()

		// We use context.Background() because the timer callback
		// runs in its own goroutine, potentially long after the
		// original request context was cancelled. The receiving
		// actor uses its own context when processing the message.
		_ = cb.Tell(context.Background(), &ExpiredMsg{
			ID: req.ID,
		})
	})

	// Store the timer and callback.
	a.timers[req.ID] = &timeoutEntry{
		timer:    timer,
		callback: req.Callback,
	}

	return fn.Ok[Resp](&AckResponse{
		Success: true,
	})
}

// handleCancel cancels a pending timeout. If the timeout doesn't exist or has
// already fired, this is a no-op.
func (a *Actor) handleCancel(_ context.Context,
	req *CancelTimeoutRequest) fn.Result[Resp] {

	a.mu.Lock()
	defer a.mu.Unlock()

	// Look up the timer for this ID.
	entry, ok := a.timers[req.ID]
	if !ok {
		// Timeout doesn't exist - this is not an error.
		return fn.Ok[Resp](&AckResponse{
			Success: true,
		})
	}

	// Cancel the timer and remove it from the map.
	entry.timer.Stop()
	delete(a.timers, req.ID)

	return fn.Ok[Resp](&AckResponse{
		Success: true,
	})
}

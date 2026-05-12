package clientconn

import (
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/clock"
)

const (
	// defaultStaleThreshold is the default duration after which a
	// client with no observed inbound activity is considered offline.
	// This is 2× the expected 30 s heartbeat interval, giving one
	// missed heartbeat as grace.
	defaultStaleThreshold = 60 * time.Second

	// defaultSweepInterval is how often the background goroutine
	// checks for stale clients. A shorter interval gives faster
	// offline detection at the cost of slightly more CPU.
	defaultSweepInterval = 10 * time.Second
)

// TrackerOption is a functional option for configuring a
// PullActivityTracker.
type TrackerOption func(*trackerOptions)

// trackerOptions holds optional configuration for the tracker.
type trackerOptions struct {
	staleThreshold time.Duration
	sweepInterval  time.Duration
	clock          clock.Clock
}

// WithStaleThreshold sets the duration after which a client with no
// observed activity transitions to offline.
func WithStaleThreshold(d time.Duration) TrackerOption {
	return func(o *trackerOptions) {
		if d > 0 {
			o.staleThreshold = d
		}
	}
}

// WithSweepInterval sets how often the background staleness sweep runs.
func WithSweepInterval(d time.Duration) TrackerOption {
	return func(o *trackerOptions) {
		if d > 0 {
			o.sweepInterval = d
		}
	}
}

// WithClock overrides the clock used for timestamps and tickers. This
// is intended for testing with a fake clock.
func WithClock(c clock.Clock) TrackerOption {
	return func(o *trackerOptions) {
		if c != nil {
			o.clock = c
		}
	}
}

// clientState tracks the liveness state of a single client.
type clientState struct {
	lastSeen time.Time
	status   ClientStatus
}

// PullActivityTracker implements StatusTracker. It derives client
// liveness from inbound envelope activity observed by the server's
// ingress loop.
//
// A background sweep goroutine periodically checks all tracked clients
// against a configurable staleness threshold, transitioning them from
// online to offline when no recent activity is observed.
type PullActivityTracker struct {
	mu        sync.RWMutex
	clients   map[ClientID]*clientState
	callbacks []func(ClientID, ClientStatus)

	staleThreshold time.Duration
	clock          clock.Clock

	stopOnce sync.Once
	stopCh   chan struct{}
}

// NewPullActivityTracker creates a new tracker and starts its
// background sweep goroutine. The caller must call Stop when the
// tracker is no longer needed.
func NewPullActivityTracker(opts ...TrackerOption) *PullActivityTracker {
	o := &trackerOptions{
		staleThreshold: defaultStaleThreshold,
		sweepInterval:  defaultSweepInterval,
		clock:          clock.NewDefaultClock(),
	}
	for _, opt := range opts {
		opt(o)
	}

	t := &PullActivityTracker{
		clients:        make(map[ClientID]*clientState),
		staleThreshold: o.staleThreshold,
		clock:          o.clock,
		stopCh:         make(chan struct{}),
	}

	go t.sweepLoop(o.sweepInterval)

	return t
}

// Status returns the current liveness status of the given client. If
// the client is not tracked, StatusUnknown is returned.
func (t *PullActivityTracker) Status(clientID ClientID) ClientStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()

	cs, ok := t.clients[clientID]
	if !ok {
		return StatusUnknown
	}

	return cs.status
}

// OnStatusChange registers a callback that is invoked whenever a
// client's status transitions. The callback is called synchronously
// from the goroutine that detects the transition (either MarkActive
// or the sweep goroutine), so it must not block.
func (t *PullActivityTracker) OnStatusChange(fn func(ClientID, ClientStatus)) {
	if fn == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.callbacks = append(t.callbacks, fn)
}

// MarkActive records that the given client has sent inbound traffic.
// If the client is tracked and was not already online, the status is
// set to online and callbacks are fired outside the lock.
func (t *PullActivityTracker) MarkActive(clientID ClientID) {
	var fire bool

	t.mu.Lock()
	cs, ok := t.clients[clientID]
	if ok {
		cs.lastSeen = t.clock.Now()

		if cs.status != StatusOnline {
			cs.status = StatusOnline
			fire = true
		}
	}
	t.mu.Unlock()

	if fire {
		t.fireCallbacks(clientID, StatusOnline)
	}
}

// RegisterClient initialises tracking state for a newly registered
// client. The client starts in StatusUnknown until the first
// MarkActive call.
func (t *PullActivityTracker) RegisterClient(clientID ClientID) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Idempotent — don't overwrite existing state if the client
	// is already tracked.
	if _, ok := t.clients[clientID]; ok {
		return
	}

	t.clients[clientID] = &clientState{
		status: StatusUnknown,
	}
}

// DeregisterClient removes tracking state for a departing client and
// fires an offline callback outside the lock if the client was online.
func (t *PullActivityTracker) DeregisterClient(clientID ClientID) {
	var wasOnline bool

	t.mu.Lock()
	cs, ok := t.clients[clientID]
	if ok {
		wasOnline = cs.status == StatusOnline
		delete(t.clients, clientID)
	}
	t.mu.Unlock()

	if wasOnline {
		t.fireCallbacks(clientID, StatusOffline)
	}
}

// Stop shuts down the background sweep goroutine. Safe to call
// multiple times.
func (t *PullActivityTracker) Stop() {
	t.stopOnce.Do(func() {
		close(t.stopCh)
	})
}

// sweepLoop periodically checks all tracked clients for staleness.
// Clients whose last activity is older than the threshold are
// transitioned to offline.
func (t *PullActivityTracker) sweepLoop(interval time.Duration) {
	for {
		select {
		case <-t.stopCh:
			return

		case <-t.clock.TickAfter(interval):
			t.sweep()
		}
	}
}

// sweep checks all tracked clients and transitions any stale ones to
// offline. Callbacks are fired outside the lock.
func (t *PullActivityTracker) sweep() {
	var stale []ClientID

	t.mu.Lock()
	cutoff := t.clock.Now().Add(-t.staleThreshold)

	for id, cs := range t.clients {
		if cs.status != StatusOnline {
			continue
		}

		if cs.lastSeen.Before(cutoff) {
			cs.status = StatusOffline
			stale = append(stale, id)
		}
	}
	t.mu.Unlock()

	for _, id := range stale {
		t.fireCallbacks(id, StatusOffline)
	}
}

// fireCallbacks invokes all registered callbacks. Must be called
// WITHOUT t.mu held to avoid deadlock with callers that query bridge
// or tracker state from the callback.
func (t *PullActivityTracker) fireCallbacks(clientID ClientID,
	status ClientStatus) {

	// Copy the callback slice under the read lock so concurrent
	// registrations cannot mutate the shared backing array while we
	// iterate outside the lock.
	t.mu.RLock()
	cbs := append(
		[]func(ClientID, ClientStatus){}, t.callbacks...,
	)
	t.mu.RUnlock()

	for _, fn := range cbs {
		fn(clientID, status)
	}
}

// Compile-time interface checks.
var (
	_ StatusTracker   = (*PullActivityTracker)(nil)
	_ ActivityMarker  = (*PullActivityTracker)(nil)
	_ ClientRegistrar = (*PullActivityTracker)(nil)
)

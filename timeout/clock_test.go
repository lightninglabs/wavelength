package timeout

import (
	"sync"
	"time"
)

// fakeClock is a deterministic Clock implementation for tests. Time only
// moves forward when Advance is called; AfterFunc callbacks fire in
// time-ordered fashion when their fire time has been reached.
type fakeClock struct {
	mu sync.Mutex

	now    time.Time
	afters []*fakeAfter
}

// newFakeClock returns a fakeClock pinned at start.
func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

// Now returns the current logical time.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

// AfterFunc registers f to run after d. The returned Stoppable cancels the
// scheduled call.
func (c *fakeClock) AfterFunc(d time.Duration, f func()) Stoppable {
	c.mu.Lock()
	defer c.mu.Unlock()

	a := &fakeAfter{
		fireAt: c.now.Add(d),
		f:      f,
	}
	c.afters = append(c.afters, a)

	return a
}

// Advance moves logical time forward by d. Due AfterFunc callbacks are
// invoked synchronously, in fire-time order; callbacks may register
// further AfterFunc entries (this is exactly what the recurring-tick
// chain does on each fire) and any new entries with a fireAt inside
// the [start, end] window are picked up in the same Advance call.
//
// Each fire advances the clock to that fire's fireAt before the
// callback runs, so a callback that consults Now() observes the
// timestamp at which its timer was supposed to fire — not the eventual
// end time. This keeps tick timestamps interval-aligned for tests.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	end := c.now.Add(d)
	c.mu.Unlock()

	for {
		c.mu.Lock()

		var next *fakeAfter
		for _, a := range c.afters {
			if a.isCancelledOrFired() {
				continue
			}

			if a.fireAt.After(end) {
				continue
			}

			if next == nil || a.fireAt.Before(next.fireAt) {
				next = a
			}
		}

		if next == nil {
			c.now = end
			c.mu.Unlock()

			return
		}

		c.now = next.fireAt
		c.mu.Unlock()

		if next.markFired() {
			next.f()
		}
	}
}

// fakeAfter is a one-shot Stoppable handle managed by fakeClock.
type fakeAfter struct {
	mu sync.Mutex

	fireAt    time.Time
	f         func()
	cancelled bool
	fired     bool
}

// Stop returns true if the call cancelled a pending fire.
func (a *fakeAfter) Stop() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cancelled || a.fired {
		return false
	}

	a.cancelled = true

	return true
}

func (a *fakeAfter) isCancelledOrFired() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.cancelled || a.fired
}

func (a *fakeAfter) markFired() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cancelled || a.fired {
		return false
	}

	a.fired = true

	return true
}


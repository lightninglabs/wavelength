package waved

import (
	"sync"

	"github.com/lightninglabs/wavelength/oor"
)

// oorTerminalWaiters provides per-session, one-shot post-commit wakeups. Its
// zero value is ready for use; it retains no completed session history.
type oorTerminalWaiters struct {
	mu      sync.Mutex
	waiters map[oor.SessionID]map[chan struct{}]struct{}
}

// subscribe registers before the caller reads status. The caller must cancel
// on every exit, including timeout and cancellation, to release its entry.
func (w *oorTerminalWaiters) subscribe(id oor.SessionID) (<-chan struct{},
	func()) {

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.waiters == nil {
		w.waiters = make(map[oor.SessionID]map[chan struct{}]struct{})
	}
	if w.waiters[id] == nil {
		w.waiters[id] = make(map[chan struct{}]struct{})
	}
	wake := make(chan struct{})
	w.waiters[id][wake] = struct{}{}

	return wake, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.waiters[id], wake)
		if len(w.waiters[id]) == 0 {
			delete(w.waiters, id)
		}
	}
}

// notify wakes current readers without blocking the committing actor. A later
// reader observes the terminal row itself and needs no retained notification.
func (w *oorTerminalWaiters) notify(id oor.SessionID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for wake := range w.waiters[id] {
		close(wake)
	}
	delete(w.waiters, id)
}

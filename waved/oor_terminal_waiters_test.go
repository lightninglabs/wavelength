package waved

import (
	"sync"
	"testing"

	"github.com/lightninglabs/wavelength/oor"
	"github.com/stretchr/testify/require"
)

// TestOORTerminalWaiters verifies broadcast, session isolation, notification
// before the caller starts waiting, and removal of canceled registrations.
func TestOORTerminalWaiters(t *testing.T) {
	t.Parallel()
	var waiters oorTerminalWaiters
	first := oor.SessionID{1}
	second := oor.SessionID{2}
	a, cancelA := waiters.subscribe(first)
	b, cancelB := waiters.subscribe(first)
	other, cancelOther := waiters.subscribe(second)
	waiters.notify(first)
	<-a
	<-b
	select {
	case <-other:
		t.Fatal("unrelated session woke")

	default:
	}
	// A cancellation from an earlier generation cannot remove a newer
	// registration for the same session.
	fresh, cancelFresh := waiters.subscribe(first)
	cancelA()
	cancelB()
	waiters.notify(first)
	<-fresh
	cancelFresh()
	cancelOther()
	require.Empty(t, waiters.waiters)
}

// TestOORTerminalWaitersConcurrentCancel verifies notification and repeated
// cancellation can race without double-closing channels or leaking entries.
func TestOORTerminalWaitersConcurrentCancel(t *testing.T) {
	t.Parallel()
	var waiters oorTerminalWaiters
	var workers sync.WaitGroup
	id := oor.SessionID{3}
	for range 100 {
		_, cancel := waiters.subscribe(id)
		workers.Go(func() {
			cancel()
			cancel()
		})
		workers.Go(func() { waiters.notify(id) })
	}
	workers.Wait()
	require.Empty(t, waiters.waiters)
}

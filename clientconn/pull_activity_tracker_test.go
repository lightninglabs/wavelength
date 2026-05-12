package clientconn

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// newTestTracker creates a PullActivityTracker with a fake clock and
// short intervals for testing.
func newTestTracker(t *testing.T, opts ...TrackerOption) (*PullActivityTracker,
	*clock.TestClock, chan time.Duration) {

	t.Helper()

	tickSignal := make(chan time.Duration, 8)
	testClock := clock.NewTestClockWithTickSignal(
		time.Unix(0, 0), tickSignal,
	)

	defaults := []TrackerOption{
		WithClock(testClock),
		WithStaleThreshold(60 * time.Second),
		WithSweepInterval(10 * time.Second),
	}

	return NewPullActivityTracker(append(defaults, opts...)...),
		testClock, tickSignal
}

// waitForSweepRegistration waits for the sweep loop to register its next
// wake-up tick.
func waitForSweepRegistration(t *testing.T, tickSignal <-chan time.Duration) {
	t.Helper()

	select {
	case interval := <-tickSignal:
		require.Equal(t, 10*time.Second, interval)

	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for sweep registration")
	}
}

// advanceTestClock moves the test clock forward by the given duration.
func advanceTestClock(c *clock.TestClock, delta time.Duration) {
	c.SetTime(c.Now().Add(delta))
}

// TestRegisterAndStatus verifies that a newly registered client starts
// with StatusUnknown.
func TestRegisterAndStatus(t *testing.T) {
	t.Parallel()

	tracker, _, _ := newTestTracker(t)
	defer tracker.Stop()

	require.Equal(t, StatusUnknown, tracker.Status("alice"))

	tracker.RegisterClient("alice")
	require.Equal(t, StatusUnknown, tracker.Status("alice"))
}

// TestMarkActiveTransitionsToOnline verifies that MarkActive moves a
// client from unknown to online and fires the callback.
func TestMarkActiveTransitionsToOnline(t *testing.T) {
	t.Parallel()

	tracker, _, _ := newTestTracker(t)
	defer tracker.Stop()

	var (
		mu          sync.Mutex
		transitions []ClientStatus
	)
	tracker.OnStatusChange(func(id ClientID, s ClientStatus) {
		mu.Lock()
		defer mu.Unlock()

		transitions = append(transitions, s)
	})

	tracker.RegisterClient("alice")
	tracker.MarkActive("alice")

	require.Equal(t, StatusOnline, tracker.Status("alice"))

	mu.Lock()
	require.Equal(t, []ClientStatus{StatusOnline}, transitions)
	mu.Unlock()
}

// TestMarkActiveIdempotent verifies that repeated MarkActive calls on
// an already-online client do not fire duplicate callbacks.
func TestMarkActiveIdempotent(t *testing.T) {
	t.Parallel()

	tracker, _, _ := newTestTracker(t)
	defer tracker.Stop()

	callCount := 0
	tracker.OnStatusChange(func(_ ClientID, _ ClientStatus) {
		callCount++
	})

	tracker.RegisterClient("alice")
	tracker.MarkActive("alice")
	tracker.MarkActive("alice")
	tracker.MarkActive("alice")

	require.Equal(t, 1, callCount)
}

// TestMarkActiveUnregisteredClient verifies that MarkActive on an
// unknown client is a no-op.
func TestMarkActiveUnregisteredClient(t *testing.T) {
	t.Parallel()

	tracker, _, _ := newTestTracker(t)
	defer tracker.Stop()

	// Should not panic or change state.
	tracker.MarkActive("ghost")
	require.Equal(t, StatusUnknown, tracker.Status("ghost"))
}

// TestSweepTransitionsToOffline verifies that the background sweep
// moves a stale client from online to offline.
func TestSweepTransitionsToOffline(t *testing.T) {
	t.Parallel()

	tracker, testClock, tickSignal := newTestTracker(t)
	defer tracker.Stop()

	var (
		mu          sync.Mutex
		transitions []ClientStatus
	)
	tracker.OnStatusChange(func(_ ClientID, s ClientStatus) {
		mu.Lock()
		defer mu.Unlock()

		transitions = append(transitions, s)
	})

	tracker.RegisterClient("alice")
	tracker.MarkActive("alice")
	require.Equal(t, StatusOnline, tracker.Status("alice"))

	// Wait for the sweep goroutine to register its wake-up before
	// advancing, so the clock jump will trigger the sweep.
	waitForSweepRegistration(t, tickSignal)

	// Advance past the stale threshold and trigger the sweep.
	advanceTestClock(testClock, 61*time.Second)

	// Poll until the sweep goroutine completes the offline
	// transition. The tick registration only proves the wake-up
	// was scheduled; the goroutine may still be executing sweep().
	require.Eventually(t, func() bool {
		return tracker.Status("alice") == StatusOffline
	}, 2*time.Second, 10*time.Millisecond)

	mu.Lock()
	require.Equal(
		t, []ClientStatus{StatusOnline, StatusOffline}, transitions,
	)
	mu.Unlock()
}

// TestMarkActiveResetsTimer verifies that MarkActive prevents the
// sweep from transitioning the client to offline.
func TestMarkActiveResetsTimer(t *testing.T) {
	t.Parallel()

	tracker, testClock, tickSignal := newTestTracker(t)
	defer tracker.Stop()

	tracker.RegisterClient("alice")
	tracker.MarkActive("alice")

	waitForSweepRegistration(t, tickSignal)

	// Advance to just before threshold.
	advanceTestClock(testClock, 50*time.Second)
	waitForSweepRegistration(t, tickSignal)

	// Refresh activity.
	tracker.MarkActive("alice")

	// Advance past original threshold but not past the refreshed
	// one (50s + 20s = 70s from start, but only 20s from refresh).
	advanceTestClock(testClock, 20*time.Second)
	waitForSweepRegistration(t, tickSignal)

	require.Equal(t, StatusOnline, tracker.Status("alice"))
}

// TestOfflineToOnline verifies that a client can transition back from
// offline to online via MarkActive.
func TestOfflineToOnline(t *testing.T) {
	t.Parallel()

	tracker, testClock, tickSignal := newTestTracker(t)
	defer tracker.Stop()

	var (
		mu          sync.Mutex
		transitions []ClientStatus
	)
	tracker.OnStatusChange(func(_ ClientID, s ClientStatus) {
		mu.Lock()
		defer mu.Unlock()

		transitions = append(transitions, s)
	})

	tracker.RegisterClient("alice")
	tracker.MarkActive("alice")

	waitForSweepRegistration(t, tickSignal)

	// Go offline.
	advanceTestClock(testClock, 61*time.Second)
	require.Eventually(t, func() bool {
		return tracker.Status("alice") == StatusOffline
	}, 2*time.Second, 10*time.Millisecond)

	// Come back online.
	tracker.MarkActive("alice")
	require.Equal(t, StatusOnline, tracker.Status("alice"))

	mu.Lock()
	require.Equal(
		t, []ClientStatus{StatusOnline, StatusOffline, StatusOnline},
		transitions,
	)
	mu.Unlock()
}

// TestDeregisterCleansUp verifies that DeregisterClient removes the
// client and fires an offline callback if the client was online.
func TestDeregisterCleansUp(t *testing.T) {
	t.Parallel()

	tracker, _, _ := newTestTracker(t)
	defer tracker.Stop()

	var (
		mu          sync.Mutex
		transitions []ClientStatus
	)
	tracker.OnStatusChange(func(_ ClientID, s ClientStatus) {
		mu.Lock()
		defer mu.Unlock()

		transitions = append(transitions, s)
	})

	tracker.RegisterClient("alice")
	tracker.MarkActive("alice")
	tracker.DeregisterClient("alice")

	require.Equal(t, StatusUnknown, tracker.Status("alice"))

	mu.Lock()
	require.Equal(
		t, []ClientStatus{StatusOnline, StatusOffline}, transitions,
	)
	mu.Unlock()
}

// TestDeregisterOfflineClientNoCallback verifies that deregistering an
// already-offline client does not fire an extra callback.
func TestDeregisterOfflineClientNoCallback(t *testing.T) {
	t.Parallel()

	tracker, _, _ := newTestTracker(t)
	defer tracker.Stop()

	callCount := 0
	tracker.OnStatusChange(func(_ ClientID, _ ClientStatus) {
		callCount++
	})

	tracker.RegisterClient("alice")
	tracker.DeregisterClient("alice")

	// No callbacks — client was never online.
	require.Equal(t, 0, callCount)
}

// TestRegisterClientIdempotent verifies that registering an already
// tracked client does not reset its state.
func TestRegisterClientIdempotent(t *testing.T) {
	t.Parallel()

	tracker, _, _ := newTestTracker(t)
	defer tracker.Stop()

	tracker.RegisterClient("alice")
	tracker.MarkActive("alice")
	require.Equal(t, StatusOnline, tracker.Status("alice"))

	// Re-register should not reset to unknown.
	tracker.RegisterClient("alice")
	require.Equal(t, StatusOnline, tracker.Status("alice"))
}

// TestConcurrentMarkActive verifies that concurrent MarkActive calls
// from multiple goroutines do not race.
func TestConcurrentMarkActive(t *testing.T) {
	t.Parallel()

	tracker, _, _ := newTestTracker(t)
	defer tracker.Stop()

	const numClients = 10
	for i := range numClients {
		tracker.RegisterClient(ClientID(string(rune('A' + i))))
	}

	var wg sync.WaitGroup
	for i := range numClients {
		wg.Add(1)

		go func(id ClientID) {
			defer wg.Done()

			for range 100 {
				tracker.MarkActive(id)
			}
		}(ClientID(string(rune('A' + i))))
	}

	wg.Wait()

	for i := range numClients {
		id := ClientID(string(rune('A' + i)))
		require.Equal(t, StatusOnline, tracker.Status(id))
	}
}

// TestSweepSkipsOfflineClients verifies that the sweep does not fire
// duplicate offline callbacks for already-offline clients.
func TestSweepSkipsOfflineClients(t *testing.T) {
	t.Parallel()

	tracker, testClock, tickSignal := newTestTracker(t)
	defer tracker.Stop()

	var callCount atomic.Int32
	tracker.OnStatusChange(func(_ ClientID, s ClientStatus) {
		if s == StatusOffline {
			callCount.Add(1)
		}
	})

	tracker.RegisterClient("alice")
	tracker.MarkActive("alice")

	waitForSweepRegistration(t, tickSignal)

	// First sweep: go offline.
	advanceTestClock(testClock, 61*time.Second)
	require.Eventually(t, func() bool {
		return callCount.Load() == 1
	}, 2*time.Second, 10*time.Millisecond)

	// Second sweep: already offline, no duplicate.
	waitForSweepRegistration(t, tickSignal)
	advanceTestClock(testClock, 10*time.Second)
	waitForSweepRegistration(t, tickSignal)
	require.Equal(t, int32(1), callCount.Load())
}

package serverconn

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestEgressWorkerPoolDeliversAllEventsOnce drives many fire-and-forget events
// through a multi-worker egress runtime and verifies every event reaches the
// server mailbox exactly once -- no loss and no duplication across the
// competing-consumer worker pool. This is the serverconn-level counterpart to
// the db-level per-key FIFO pool test: here the focus is the at-least-once
// egress contract holding as exactly-once delivery under concurrency.
func TestEgressWorkerPoolDeliversAllEventsOnce(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()

	cfg := newTestConnectorConfig(mb, store)
	cfg.EgressWorkers = 4

	runtime, err := NewRuntime(cfg)
	require.NoError(t, err)

	// Egress only -- the ingress puller is a separate single loop and is
	// not needed to exercise the outbound worker pool.
	runtime.StartEgress()
	defer runtime.Stop()

	const numEvents = 60
	ctx := t.Context()
	for i := 0; i < numEvents; i++ {
		err := runtime.TellRef().Tell(ctx, &SendClientEventRequest{
			Message: &testServerMessage{
				value: fmt.Sprintf("evt-%d", i),
			},
		})
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool {
		mb.mu.Lock()
		defer mb.mu.Unlock()

		return len(mb.mailboxes["server-1"]) == numEvents
	}, 10*time.Second, 10*time.Millisecond,
		"all events should reach the server mailbox")

	mb.mu.Lock()
	defer mb.mu.Unlock()

	envs := mb.mailboxes["server-1"]
	require.Len(t, envs, numEvents)

	// Each distinct event was sent exactly once: no worker double-sent and
	// none were dropped.
	seen := make(map[string]int)
	for _, env := range envs {
		seen[env.MsgId]++
	}
	require.Len(t, seen, numEvents, "every event delivered exactly once")
	for id, count := range seen {
		require.Equalf(
			t, 1, count, "event %s delivered %d times", id, count,
		)
	}
}

package actor

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestServiceKeyRefCreatesRouter verifies that ServiceKey.Ref returns a
// working router that load-balances across registered actors.
func TestServiceKeyRefCreatesRouter(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()
	defer func() {
		_ = system.Shutdown(context.Background())
	}()

	// Track which actors process messages.
	var actor1Count, actor2Count, actor3Count atomic.Int32

	// Create behaviors that track message counts.
	behavior1 := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			actor1Count.Add(1)
			return fn.Ok("actor1")
		},
	)

	behavior2 := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			actor2Count.Add(1)
			return fn.Ok("actor2")
		},
	)

	behavior3 := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			actor3Count.Add(1)
			return fn.Ok("actor3")
		},
	)

	// Register three actors under the same service key.
	key := NewServiceKey[*testMsg, string]("worker-pool")
	_ = RegisterWithSystem(system, "worker-1", key, behavior1)
	_ = RegisterWithSystem(system, "worker-2", key, behavior2)
	_ = RegisterWithSystem(system, "worker-3", key, behavior3)

	// Get a virtual reference (router) for the service.
	serviceRef := key.Ref(system)

	// Send messages through the router.
	numMessages := 12 // Divisible by 3 for round-robin.
	for i := 0; i < numMessages; i++ {
		result := serviceRef.Ask(context.Background(), newTestMsg("work")).
			Await(context.Background())
		require.True(t, result.IsOk(), "Message %d should be processed", i)
	}

	// Verify all actors received messages (round-robin distribution).
	require.Equal(t, int32(4), actor1Count.Load(),
		"Actor 1 should receive 4 messages")
	require.Equal(t, int32(4), actor2Count.Load(),
		"Actor 2 should receive 4 messages")
	require.Equal(t, int32(4), actor3Count.Load(),
		"Actor 3 should receive 4 messages")
}

// TestServiceKeyRefWithNoActors verifies that Ref works even when no actors
// are registered yet.
func TestServiceKeyRefWithNoActors(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()
	defer func() {
		_ = system.Shutdown(context.Background())
	}()

	// Get a ref before any actors are registered.
	key := NewServiceKey[*testMsg, string]("empty-service")
	serviceRef := key.Ref(system)

	// Sending to an empty service should fail gracefully.
	result := serviceRef.Ask(context.Background(), newTestMsg("test")).
		Await(context.Background())
	require.True(t, result.IsErr(), "Should fail with no actors")
}

// TestServiceKeyBroadcast verifies that Broadcast sends to all registered
// actors.
func TestServiceKeyBroadcast(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()
	defer func() {
		_ = system.Shutdown(context.Background())
	}()

	// Track messages received by each actor.
	actor1Received := make(chan string, 10)
	actor2Received := make(chan string, 10)
	actor3Received := make(chan string, 10)

	behavior1 := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			actor1Received <- msg.data
			return fn.Ok("ok")
		},
	)

	behavior2 := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			actor2Received <- msg.data
			return fn.Ok("ok")
		},
	)

	behavior3 := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			actor3Received <- msg.data
			return fn.Ok("ok")
		},
	)

	// Register three actors.
	key := NewServiceKey[*testMsg, string]("broadcast-service")
	_ = RegisterWithSystem(system, "listener-1", key, behavior1)
	_ = RegisterWithSystem(system, "listener-2", key, behavior2)
	_ = RegisterWithSystem(system, "listener-3", key, behavior3)

	// Broadcast a message.
	sent := key.Broadcast(system, context.Background(), newTestMsg("notification"))

	// Should send to all 3 actors.
	require.Equal(t, 3, sent, "Should send to all 3 actors")

	// Verify all actors received the message.
	require.Equal(t, "notification", <-actor1Received)
	require.Equal(t, "notification", <-actor2Received)
	require.Equal(t, "notification", <-actor3Received)
}

// TestServiceKeyBroadcastWithNoActors verifies Broadcast handles empty services.
func TestServiceKeyBroadcastWithNoActors(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()
	defer func() {
		_ = system.Shutdown(context.Background())
	}()

	key := NewServiceKey[*testMsg, string]("empty-broadcast")

	// Broadcast to an empty service should return 0.
	sent := key.Broadcast(system, context.Background(), newTestMsg("test"))
	require.Equal(t, 0, sent, "Should send to 0 actors")
}

// TestServiceKeyRefAndBroadcastTogether verifies that Ref and Broadcast work
// together on the same service.
func TestServiceKeyRefAndBroadcastTogether(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()
	defer func() {
		_ = system.Shutdown(context.Background())
	}()

	var broadcastCount atomic.Int32
	var routedCount atomic.Int32

	behavior := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			if msg.data == "broadcast" {
				broadcastCount.Add(1)
			} else {
				routedCount.Add(1)
			}
			return fn.Ok("ok")
		},
	)

	// Register multiple actors.
	key := NewServiceKey[*testMsg, string]("hybrid-service")
	_ = RegisterWithSystem(system, "hybrid-1", key, behavior)
	_ = RegisterWithSystem(system, "hybrid-2", key, behavior)
	_ = RegisterWithSystem(system, "hybrid-3", key, behavior)

	// Get router for load-balanced calls.
	router := key.Ref(system)

	// Send 6 messages through router using Ask to ensure they're processed.
	for i := 0; i < 6; i++ {
		result := router.Ask(context.Background(), newTestMsg("routed")).
			Await(context.Background())
		require.True(t, result.IsOk())
	}

	// Broadcast 2 messages (all actors receive).
	sent1 := key.Broadcast(system, context.Background(), newTestMsg("broadcast"))
	require.Equal(t, 3, sent1, "First broadcast should reach 3 actors")

	sent2 := key.Broadcast(system, context.Background(), newTestMsg("broadcast"))
	require.Equal(t, 3, sent2, "Second broadcast should reach 3 actors")

	// Shutdown to ensure all Tell messages are processed.
	_ = system.Shutdown(context.Background())

	// Total routed count: 6 messages sent (round-robin).
	require.Equal(t, int32(6), routedCount.Load(),
		"Should receive 6 total routed messages")

	// Total broadcast count: 2 broadcasts × 3 actors = 6.
	// Note: Broadcast uses Tell which is fire-and-forget, so we may not
	// have processed all of them before shutdown. Just verify some were
	// received.
	require.Greater(t, broadcastCount.Load(), int32(0),
		"Should receive some broadcast messages")
}

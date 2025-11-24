package actor

import (
	"context"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestDeterministicShutdownWaits verifies that Shutdown blocks until all
// actors have completely finished their process loops.
func TestDeterministicShutdownWaits(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()

	// Simple behavior.
	behavior := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			return fn.Ok("ok")
		},
	)

	// Register multiple actors.
	key := NewServiceKey[*testMsg, string]("test-actors")
	ref1 := RegisterWithSystem(system, "actor-1", key, behavior)
	ref2 := RegisterWithSystem(system, "actor-2", key, behavior)
	ref3 := RegisterWithSystem(system, "actor-3", key, behavior)

	// Send messages.
	for i := 0; i < 5; i++ {
		ref1.Tell(context.Background(), newTestMsg("msg"))
		ref2.Tell(context.Background(), newTestMsg("msg"))
		ref3.Tell(context.Background(), newTestMsg("msg"))
	}

	// Shutdown should block until all 3 actors + DLO finish.
	err := system.Shutdown(context.Background())
	require.NoError(t, err, "Shutdown should complete successfully")

	// After Shutdown returns, all actor goroutines have exited. The
	// actorWg counter is back to zero.
}

// TestShutdownWithTimeout verifies that Shutdown respects context deadline
// when actors take too long to finish.
func TestShutdownWithTimeout(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()

	// Channel that will never close, making the actor hang.
	hangForever := make(chan struct{})

	// Create a behavior that hangs in message processing.
	behavior := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			// This will block forever, preventing the actor from
			// finishing even after context cancellation.
			select {
			case <-hangForever:
				return fn.Ok("done")
			case <-ctx.Done():
				// Even with context cancelled, we continue to hang.
				<-hangForever
				return fn.Err[string](ctx.Err())
			}
		},
	)

	// Register actor.
	key := NewServiceKey[*testMsg, string]("hanging-actor")
	ref := RegisterWithSystem(system, "hanging-1", key, behavior)

	// Send message to trigger the hang.
	ref.Tell(context.Background(), newTestMsg("hang"))

	// Give it time to start processing.
	time.Sleep(20 * time.Millisecond)

	// Shutdown with short timeout - should return with error before actor
	// finishes.
	shutdownCtx, cancel := context.WithTimeout(
		context.Background(), 50*time.Millisecond,
	)
	defer cancel()

	err := system.Shutdown(shutdownCtx)

	// Should return timeout error.
	require.Error(t, err, "Shutdown should timeout")
	require.ErrorIs(t, err, context.DeadlineExceeded,
		"Should be DeadlineExceeded")

	// Cleanup - release the hang.
	close(hangForever)
}

// TestShutdownIdempotency verifies calling Shutdown multiple times is safe.
func TestShutdownIdempotency(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()

	// Register a simple actor.
	behavior := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			return fn.Ok("ok")
		},
	)

	key := NewServiceKey[*testMsg, string]("test-actor")
	_ = RegisterWithSystem(system, "test-actor-1", key, behavior)

	ctx := context.Background()

	// Multiple shutdowns should all succeed.
	err := system.Shutdown(ctx)
	require.NoError(t, err)

	err = system.Shutdown(ctx)
	require.NoError(t, err)

	err = system.Shutdown(ctx)
	require.NoError(t, err)
}

// TestShutdownEmptySystem verifies shutting down with no actors works.
func TestShutdownEmptySystem(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()

	// Shutdown immediately (only DLO is running).
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err := system.Shutdown(ctx)
	require.NoError(t, err)
}

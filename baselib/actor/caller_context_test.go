package actor

import (
	"context"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestCallerDeadlineRespected verifies that actors can detect and respect
// caller deadlines passed through the merged context.
func TestCallerDeadlineRespected(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()
	defer func() {
		err := system.Shutdown(context.Background())
		require.NoError(t, err)
	}()

	// Track whether the behavior detected context cancellation.
	ctxCancelDetected := make(chan struct{})

	// Create a behavior that checks for context cancellation.
	behavior := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			// Simulate work that might take a while.
			select {
			case <-time.After(500 * time.Millisecond):
				// Work completed.
				return fn.Ok("completed")
			case <-ctx.Done():
				// Context cancelled before work finished.
				close(ctxCancelDetected)
				return fn.Err[string](ctx.Err())
			}
		},
	)

	// Register the actor.
	key := NewServiceKey[*testMsg, string]("deadline-aware")
	ref := RegisterWithSystem(system, "deadline-actor", key, behavior)

	// Send Ask with a short deadline (50ms).
	askCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	future := ref.Ask(askCtx, newTestMsg("work"))
	result := future.Await(context.Background())

	// The Ask should fail due to deadline.
	require.True(t, result.IsErr(), "Ask should fail due to deadline")

	// The behavior should have detected the context cancellation.
	select {
	case <-ctxCancelDetected:
		// Good - actor detected the caller's deadline.
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Actor did not detect caller deadline")
	}
}

// TestCallerContextCancellation verifies that actors detect when the caller
// cancels their context.
func TestCallerContextCancellation(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()
	defer func() {
		err := system.Shutdown(context.Background())
		require.NoError(t, err)
	}()

	// Signal when actor detects cancellation.
	cancelDetected := make(chan struct{})

	behavior := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			select {
			case <-time.After(1 * time.Second):
				return fn.Ok("done")
			case <-ctx.Done():
				close(cancelDetected)
				return fn.Err[string](ctx.Err())
			}
		},
	)

	key := NewServiceKey[*testMsg, string]("cancel-aware")
	ref := RegisterWithSystem(system, "cancel-actor", key, behavior)

	// Create cancellable context.
	askCtx, cancel := context.WithCancel(context.Background())

	// Send Ask.
	future := ref.Ask(askCtx, newTestMsg("work"))

	// Cancel immediately.
	cancel()

	// Actor should detect the cancellation.
	select {
	case <-cancelDetected:
		// Good.
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Actor didn't detect cancellation")
	}

	// Result should be an error.
	result := future.Await(context.Background())
	require.True(t, result.IsErr())
}

// TestActorShutdownOverridesCallerDeadline verifies that actor shutdown takes
// precedence even if the caller's deadline is longer.
func TestActorShutdownOverridesCallerDeadline(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()

	shutdownDetected := make(chan struct{})

	behavior := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			select {
			case <-time.After(2 * time.Second):
				return fn.Ok("done")
			case <-ctx.Done():
				close(shutdownDetected)
				return fn.Err[string](ctx.Err())
			}
		},
	)

	key := NewServiceKey[*testMsg, string]("shutdown-test")
	ref := RegisterWithSystem(system, "shutdown-actor", key, behavior)

	// Send Ask with a LONG deadline (5 seconds).
	askCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	future := ref.Ask(askCtx, newTestMsg("work"))

	// Give time for message to be received.
	time.Sleep(10 * time.Millisecond)

	// Shutdown the system (which cancels actor context).
	shutdownCtx, shutdownCancel := context.WithTimeout(
		context.Background(), 1*time.Second,
	)
	defer shutdownCancel()

	err := system.Shutdown(shutdownCtx)
	require.NoError(t, err)

	// Actor should have detected shutdown despite long caller deadline.
	select {
	case <-shutdownDetected:
		// Good - actor context took precedence.
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Actor didn't detect shutdown")
	}

	// Result should reflect the error.
	result := future.Await(context.Background())
	require.True(t, result.IsErr())
}

// TestTellIgnoresCallerContextAfterEnqueue verifies that Tell preserves
// fire-and-forget semantics. Once a Tell message is enqueued, cancelling the
// caller's context should not prevent the message from being processed.
func TestTellIgnoresCallerContextAfterEnqueue(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()
	defer func() {
		err := system.Shutdown(context.Background())
		require.NoError(t, err)
	}()

	processed := make(chan struct{})

	behavior := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			time.Sleep(50 * time.Millisecond)

			select {
			case <-ctx.Done():
				return fn.Err[string](ctx.Err())
			default:
				close(processed)
				return fn.Ok("completed")
			}
		},
	)

	key := NewServiceKey[*testMsg, string]("tell-test")
	ref := RegisterWithSystem(system, "tell-actor", key, behavior)

	tellCtx, cancel := context.WithTimeout(
		context.Background(), 100*time.Millisecond,
	)
	defer cancel()

	ref.Tell(tellCtx, newTestMsg("fire-and-forget"))

	time.Sleep(10 * time.Millisecond)

	cancel()

	select {
	case <-processed:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Tell message was not processed despite being enqueued")
	}
}

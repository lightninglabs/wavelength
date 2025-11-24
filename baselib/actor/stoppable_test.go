package actor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// stoppableBehavior implements both ActorBehavior and Stoppable for testing.
type stoppableBehavior struct {
	onStopCalled atomic.Bool
	cleanupDone  chan struct{}
}

func newStoppableBehavior() *stoppableBehavior {
	return &stoppableBehavior{
		cleanupDone: make(chan struct{}),
	}
}

func (b *stoppableBehavior) Receive(ctx context.Context, msg *testMsg) fn.Result[string] {
	return fn.Ok("processed")
}

func (b *stoppableBehavior) OnStop(ctx context.Context) error {
	b.onStopCalled.Store(true)
	close(b.cleanupDone)
	return nil
}

// TestStoppableInterfaceInvoked verifies that OnStop is called during actor
// shutdown.
func TestStoppableInterfaceInvoked(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()

	behavior := newStoppableBehavior()

	key := NewServiceKey[*testMsg, string]("stoppable-actor")
	_ = RegisterWithSystem(system, "stoppable-1", key, behavior)

	// Shutdown the system.
	err := system.Shutdown(context.Background())
	require.NoError(t, err)

	// Verify OnStop was called.
	require.True(t, behavior.onStopCalled.Load(),
		"OnStop should have been called")

	// Verify cleanup completed.
	select {
	case <-behavior.cleanupDone:
		// Good.
	case <-time.After(100 * time.Millisecond):
		t.Fatal("OnStop cleanup didn't complete")
	}
}

// stoppableCleanupBehavior has slow cleanup.
type stoppableCleanupBehavior struct {
	cleanupStarted  chan struct{}
	cleanupFinished chan struct{}
}

func (b *stoppableCleanupBehavior) Receive(ctx context.Context, msg *testMsg) fn.Result[string] {
	return fn.Ok("ok")
}

func (b *stoppableCleanupBehavior) OnStop(ctx context.Context) error {
	close(b.cleanupStarted)
	// Simulate slow cleanup.
	time.Sleep(100 * time.Millisecond)
	close(b.cleanupFinished)
	return nil
}

// TestStoppableOnStopCompletes verifies that OnStop cleanup completes even with
// slow operations.
func TestStoppableOnStopCompletes(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()

	cleanupBehavior := &stoppableCleanupBehavior{
		cleanupStarted:  make(chan struct{}),
		cleanupFinished: make(chan struct{}),
	}

	key := NewServiceKey[*testMsg, string]("cleanup-test")
	ref := RegisterWithSystem(system, "cleanup-actor", key, cleanupBehavior)

	// Send a message to ensure actor is running.
	result := ref.Ask(context.Background(), newTestMsg("test")).Await(context.Background())
	require.True(t, result.IsOk())

	// Shutdown the system.
	err := system.Shutdown(context.Background())
	require.NoError(t, err)

	// Verify cleanup started.
	select {
	case <-cleanupBehavior.cleanupStarted:
		// Good.
	default:
		t.Fatal("Cleanup didn't start")
	}

	// Verify cleanup finished.
	select {
	case <-cleanupBehavior.cleanupFinished:
		// Good.
	default:
		t.Fatal("Cleanup didn't finish")
	}
}

// TestNonStoppableBehaviorWorksNormally verifies that behaviors that don't
// implement Stoppable continue to work without OnStop hooks.
func TestNonStoppableBehaviorWorksNormally(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()

	// Use a regular function behavior (doesn't implement Stoppable).
	behavior := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			return fn.Ok("normal")
		},
	)

	key := NewServiceKey[*testMsg, string]("normal-actor")
	ref := RegisterWithSystem(system, "normal-1", key, behavior)

	// Should work normally.
	result := ref.Ask(context.Background(), newTestMsg("test")).Await(context.Background())
	require.True(t, result.IsOk())

	// Shutdown should work normally.
	err := system.Shutdown(context.Background())
	require.NoError(t, err)
}

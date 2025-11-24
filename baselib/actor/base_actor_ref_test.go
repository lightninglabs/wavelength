package actor

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestBaseActorRefStrongerTyping verifies that BaseActorRef provides stronger
// typing than any in the Receptionist.
func TestBaseActorRefStrongerTyping(t *testing.T) {
	t.Parallel()

	receptionist := newReceptionist()

	behavior := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			return fn.Ok("ok")
		},
	)

	actor := NewActor(ActorConfig[*testMsg, string]{
		ID:          "test-actor",
		Behavior:    behavior,
		MailboxSize: 10,
	})
	actor.Start()
	defer actor.Stop()

	key := NewServiceKey[*testMsg, string]("test-service")
	err := RegisterWithReceptionist(receptionist, key, actor.Ref())
	require.NoError(t, err)

	// Verify we can access the registrations as BaseActorRef.
	receptionist.mu.RLock()
	baseRefs := receptionist.registrations["test-service"]
	receptionist.mu.RUnlock()

	require.Len(t, baseRefs, 1)

	// BaseActorRef provides ID() method directly.
	require.Equal(t, "test-actor", baseRefs[0].ID())
}

// TestActorRefImplementsBaseActorRef verifies that ActorRef satisfies
// BaseActorRef.
func TestActorRefImplementsBaseActorRef(t *testing.T) {
	t.Parallel()

	behavior := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			return fn.Ok("ok")
		},
	)

	actor := NewActor(ActorConfig[*testMsg, string]{
		ID:          "base-test",
		Behavior:    behavior,
		MailboxSize: 10,
	})
	actor.Start()
	defer actor.Stop()

	// ActorRef should be assignable to BaseActorRef.
	var baseRef BaseActorRef = actor.Ref()
	require.NotNil(t, baseRef)
	require.Equal(t, "base-test", baseRef.ID())
}

// TestRouterImplementsBaseActorRef verifies that Router satisfies BaseActorRef.
func TestRouterImplementsBaseActorRef(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()
	defer func() {
		_ = system.Shutdown(context.Background())
	}()

	key := NewServiceKey[*testMsg, string]("router-test")

	// Create a router using key.Ref.
	router := key.Ref(system)

	// Router should be assignable to BaseActorRef.
	var baseRef BaseActorRef = router
	require.NotNil(t, baseRef)
	require.Contains(t, baseRef.ID(), "router")
}

// firstActorStrategy always selects the first available actor.
type firstActorStrategy[M Message, R any] struct{}

func (s *firstActorStrategy[M, R]) Select(actors []ActorRef[M, R]) (ActorRef[M, R], error) {
	if len(actors) == 0 {
		return nil, ErrNoActorsAvailable
	}
	return actors[0], nil
}

// TestFunctionalOptionsWithCustomStrategy verifies that WithStrategy option
// works correctly.
func TestFunctionalOptionsWithCustomStrategy(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()
	defer func() {
		_ = system.Shutdown(context.Background())
	}()

	// Track which actors get selected.
	var actor1Selected, actor2Selected atomic.Int32

	behavior1 := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			actor1Selected.Add(1)
			return fn.Ok("actor1")
		},
	)

	behavior2 := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			actor2Selected.Add(1)
			return fn.Ok("actor2")
		},
	)

	key := NewServiceKey[*testMsg, string]("custom-strategy-test")
	_ = RegisterWithSystem(system, "actor-1", key, behavior1)
	_ = RegisterWithSystem(system, "actor-2", key, behavior2)

	// Get ref with custom strategy that always picks first actor.
	customStrategy := &firstActorStrategy[*testMsg, string]{}
	ref := key.Ref(system, WithStrategy[*testMsg, string](customStrategy))

	// Send multiple messages - all should go to first actor.
	for i := 0; i < 10; i++ {
		result := ref.Ask(context.Background(), newTestMsg("test")).
			Await(context.Background())
		require.True(t, result.IsOk())
	}

	// Verify only actor1 was selected (custom strategy working).
	require.Equal(t, int32(10), actor1Selected.Load(),
		"Actor 1 should receive all 10 messages")
	require.Equal(t, int32(0), actor2Selected.Load(),
		"Actor 2 should receive no messages")
}

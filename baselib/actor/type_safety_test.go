package actor

import (
	"context"
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// testMsgA is a distinct message type for testing type conflicts.
type testMsgA struct {
	BaseMessage
	value string
}

func (m *testMsgA) MessageType() string {
	return "testMsgA"
}

// testMsgB is a different message type with the same purpose.
type testMsgB struct {
	BaseMessage
	count int
}

func (m *testMsgB) MessageType() string {
	return "testMsgB"
}

// TestReceptionistTypeSafetyPreventsConflicts verifies that the Receptionist
// prevents registering actors with the same service name but different types.
func TestReceptionistTypeSafetyPreventsConflicts(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()
	defer func() {
		_ = system.Shutdown(context.Background())
	}()

	// Create behaviors for different message types.
	behaviorA := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsgA) fn.Result[string] {
			return fn.Ok("A: " + msg.value)
		},
	)

	behaviorB := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsgB) fn.Result[int] {
			return fn.Ok(msg.count)
		},
	)

	// Register first actor with name "conflict-test" for type A.
	keyA := NewServiceKey[*testMsgA, string]("conflict-test")
	refA := RegisterWithSystem(system, "actor-a", keyA, behaviorA)

	// Verify actor A is registered and working.
	resultA := refA.Ask(context.Background(), &testMsgA{value: "test"}).
		Await(context.Background())
	require.True(t, resultA.IsOk(), "Actor A should work")

	// Attempt to register second actor with SAME name but DIFFERENT type.
	keyB := NewServiceKey[*testMsgB, int]("conflict-test")
	refB := RegisterWithSystem(system, "actor-b", keyB, behaviorB)

	// The second actor should fail to respond because it wasn't actually
	// registered (type mismatch detected).
	resultB := refB.Ask(context.Background(), &testMsgB{count: 42}).
		Await(context.Background())
	require.True(t, resultB.IsErr(), "Actor B should fail (type conflict)")
	require.ErrorIs(t, resultB.Err(), ErrActorTerminated,
		"Should return ErrActorTerminated for conflicted actor")

	// Verify only actor A is in the receptionist.
	foundA := FindInReceptionist(system.Receptionist(), keyA)
	require.Len(t, foundA, 1, "Only actor A should be registered")

	foundB := FindInReceptionist(system.Receptionist(), keyB)
	require.Len(t, foundB, 0, "Actor B should not be in receptionist")
}

// TestReceptionistAllowsSameTypeRegistrations verifies that multiple actors
// with the SAME types can register under the same name (load balancing).
func TestReceptionistAllowsSameTypeRegistrations(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()
	defer func() {
		_ = system.Shutdown(context.Background())
	}()

	behavior := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsg) fn.Result[string] {
			return fn.Ok("processed")
		},
	)

	// Register multiple actors with the SAME service key (same name + types).
	key := NewServiceKey[*testMsg, string]("load-balanced-service")
	ref1 := RegisterWithSystem(system, "worker-1", key, behavior)
	ref2 := RegisterWithSystem(system, "worker-2", key, behavior)
	ref3 := RegisterWithSystem(system, "worker-3", key, behavior)

	// All should be registered and working.
	result1 := ref1.Ask(context.Background(), newTestMsg("test")).
		Await(context.Background())
	require.True(t, result1.IsOk())

	result2 := ref2.Ask(context.Background(), newTestMsg("test")).
		Await(context.Background())
	require.True(t, result2.IsOk())

	result3 := ref3.Ask(context.Background(), newTestMsg("test")).
		Await(context.Background())
	require.True(t, result3.IsOk())

	// All three should be found in the receptionist.
	found := FindInReceptionist(system.Receptionist(), key)
	require.Len(t, found, 3, "All three actors should be registered")
}

// TestReceptionistDirectRegistrationValidation verifies type safety when
// calling RegisterWithReceptionist directly.
func TestReceptionistDirectRegistrationValidation(t *testing.T) {
	t.Parallel()

	receptionist := newReceptionist()

	behaviorA := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsgA) fn.Result[string] {
			return fn.Ok("A")
		},
	)

	behaviorB := NewFunctionBehavior(
		func(ctx context.Context, msg *testMsgB) fn.Result[int] {
			return fn.Ok(1)
		},
	)

	// Create actors directly (not through system).
	actorA := NewActor(ActorConfig[*testMsgA, string]{
		ID:          "actor-a",
		Behavior:    behaviorA,
		MailboxSize: 10,
	})
	actorA.Start()
	defer actorA.Stop()

	actorB := NewActor(ActorConfig[*testMsgB, int]{
		ID:          "actor-b",
		Behavior:    behaviorB,
		MailboxSize: 10,
	})
	actorB.Start()
	defer actorB.Stop()

	// Register first actor.
	keyA := NewServiceKey[*testMsgA, string]("direct-test")
	err := RegisterWithReceptionist(receptionist, keyA, actorA.Ref())
	require.NoError(t, err, "First registration should succeed")

	// Attempt to register second actor with different types.
	keyB := NewServiceKey[*testMsgB, int]("direct-test")
	err = RegisterWithReceptionist(receptionist, keyB, actorB.Ref())
	require.Error(t, err, "Second registration should fail")
	require.ErrorIs(t, err, ErrServiceKeyTypeMismatch,
		"Should return ErrServiceKeyTypeMismatch")
	require.Contains(t, err.Error(), "direct-test",
		"Error should mention the conflicting service name")
}

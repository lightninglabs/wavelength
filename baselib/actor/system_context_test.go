package actor

import (
	"context"
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestActorSystemImplementsSystemContext verifies that ActorSystem satisfies
// the SystemContext interface.
func TestActorSystemImplementsSystemContext(t *testing.T) {
	t.Parallel()

	system := NewActorSystem()
	defer func() {
		_ = system.Shutdown(context.Background())
	}()

	// Verify ActorSystem can be used as SystemContext.
	var sysCtx SystemContext = system

	// Should be able to call interface methods.
	receptionist := sysCtx.Receptionist()
	require.NotNil(t, receptionist, "Receptionist should not be nil")

	deadLetters := sysCtx.DeadLetters()
	require.NotNil(t, deadLetters, "DeadLetters should not be nil")
}

// mockSystemContext is a test implementation of SystemContext for unit testing.
type mockSystemContext struct {
	receptionist *Receptionist
	deadLetters  ActorRef[Message, any]
}

func newMockSystemContext(t *testing.T) *mockSystemContext {
	// Create a minimal DLO for the mock.
	dloBehavior := NewFunctionBehavior(
		func(ctx context.Context, msg Message) fn.Result[any] {
			return fn.Ok[any](nil)
		},
	)

	dloCfg := ActorConfig[Message, any]{
		ID:          "mock-dlo",
		Behavior:    dloBehavior,
		MailboxSize: 10,
	}
	dloActor := NewActor(dloCfg)
	dloActor.Start()
	t.Cleanup(dloActor.Stop)

	return &mockSystemContext{
		receptionist: newReceptionist(),
		deadLetters:  dloActor.Ref(),
	}
}

func (m *mockSystemContext) Receptionist() *Receptionist {
	return m.receptionist
}

func (m *mockSystemContext) DeadLetters() ActorRef[Message, any] {
	return m.deadLetters
}

// TestMockSystemContextForUnitTesting demonstrates how SystemContext enables
// unit testing without a full ActorSystem.
func TestMockSystemContextForUnitTesting(t *testing.T) {
	t.Parallel()

	// Create a mock system context for testing.
	mockSys := newMockSystemContext(t)

	// Components can accept SystemContext instead of *ActorSystem.
	testComponent := func(sys SystemContext) *Receptionist {
		return sys.Receptionist()
	}

	// Test the component with the mock.
	receptionist := testComponent(mockSys)
	require.NotNil(t, receptionist)

	// Can also test with real ActorSystem.
	realSystem := NewActorSystem()
	defer func() {
		_ = realSystem.Shutdown(context.Background())
	}()

	receptionistReal := testComponent(realSystem)
	require.NotNil(t, receptionistReal)
}

// TestSystemContextEnablesDecoupling demonstrates using SystemContext for
// better separation of concerns.
func TestSystemContextEnablesDecoupling(t *testing.T) {
	t.Parallel()

	// Simulate a component that only needs to find actors, not manage them.
	type actorConsumer struct {
		sys SystemContext
	}

	newActorConsumer := func(sys SystemContext) *actorConsumer {
		return &actorConsumer{sys: sys}
	}

	// Can use with real system.
	system := NewActorSystem()
	defer func() {
		_ = system.Shutdown(context.Background())
	}()

	consumer := newActorConsumer(system)
	require.NotNil(t, consumer.sys.Receptionist())

	// Or with mock for isolated testing.
	mockSys := newMockSystemContext(t)
	mockConsumer := newActorConsumer(mockSys)
	require.NotNil(t, mockConsumer.sys.Receptionist())
}

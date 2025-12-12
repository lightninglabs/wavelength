package rounds

import (
	"context"

	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// State is a sealed interface for all states in the round state machine.
// Each state implements ProcessEvent to handle events and transition to the
// next state.
type State interface {
	protofsm.State[Event, OutboxEvent, *Environment]

	// stateSealed is an unexported method that marks this interface as
	// sealed, preventing external implementations.
	stateSealed()
}

// CreatedState is the initial state of the round. No clients have joined yet.
type CreatedState struct{}

// ProcessEvent handles events in the CreatedState.
func (s *CreatedState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	// No-op for now - return same state.
	return &StateTransition{
		NextState: s,
		NewEvents: fn.None[EmittedEvent](),
	}, nil
}

// IsTerminal returns false as CreatedState is not a terminal state.
func (s *CreatedState) IsTerminal() bool {
	return false
}

// String returns a human-readable representation of CreatedState.
func (s *CreatedState) String() string {
	return "CreatedState"
}

// stateSealed marks CreatedState as implementing the sealed State interface.
func (s *CreatedState) stateSealed() {}

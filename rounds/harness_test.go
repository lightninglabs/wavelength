package rounds

import (
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/stretchr/testify/require"
)

// fsmTestHarness is the central test harness housing all common setup,
// mocks, fixtures, and helper functions for round FSM tests.
type fsmTestHarness struct {
	*testing.T

	// Environment for FSM.
	env *Environment

	// fsm is the state machine instance under test.
	fsm *StateMachine

	// outboxMessages accumulates outbox events from the last sendEvent
	// call.
	outboxMessages []OutboxEvent
}

// newTestHarness creates a new test harness with default configuration.
// It initializes and starts a new state machine for testing.
func newTestHarness(t *testing.T) *fsmTestHarness {
	t.Helper()

	roundID, err := NewRoundID()
	require.NoError(t, err)

	env := Environment{
		RoundID: roundID,
	}

	fsmCfg := StateMachineCfg{
		InitialState: &CreatedState{},
		Env:          &env,
		Logger:       btclog.Disabled,
	}
	fsm := protofsm.NewStateMachine(fsmCfg)
	fsm.Start(t.Context())

	h := &fsmTestHarness{
		T:              t,
		env:            &env,
		fsm:            &fsm,
		outboxMessages: make([]OutboxEvent, 0),
	}

	return h
}

// sendEvent sends an event to the state machine and accumulates outbox
// messages. The state machine executor automatically handles dispatching
// internal events, so this method simply awaits the result and captures
// the accumulated outbox events.
//
//nolint:unused
func (h *fsmTestHarness) sendEvent(event Event) error {
	h.Helper()

	// Use AskEvent to send the event and wait for all state transitions
	// (including those triggered by internal events) to complete.
	future := h.fsm.AskEvent(h.Context(), event)
	result := future.Await(h.Context())

	// Extract outbox events or return the error.
	outbox, err := result.Unpack()
	if err != nil {
		return err
	}

	// Accumulate the outbox messages from this event.
	h.outboxMessages = append(h.outboxMessages, outbox...)

	return nil
}

// clearOutbox clears the captured outbox messages. Useful between multiple
// event sends when testing specific sequences.
//
//nolint:unused
func (h *fsmTestHarness) clearOutbox() {
	h.outboxMessages = nil
}

// assertStateType asserts the current state is of the expected type and
// returns it cast to that type.
func assertStateType[T State](h *fsmTestHarness) T {
	h.Helper()

	currentState, err := h.fsm.CurrentState()
	require.NoError(h, err, "failed to query current state")

	state, ok := currentState.(T)
	require.True(h, ok, "current state is not of expected type %T, got "+
		"%T", *new(T), currentState)

	return state
}

// assertOutboxLen asserts that exactly n outbox messages were emitted.
func (h *fsmTestHarness) assertOutboxLen(n int) {
	h.Helper()

	require.Len(h, h.outboxMessages, n)
}

// assertOutboxMessageType asserts that the outbox contains a message of the
// given type at the specified index and returns it cast to that type.
//
//nolint:unused
func assertOutboxMessageType[T OutboxEvent](h *fsmTestHarness,
	index int) T {

	h.Helper()

	require.Greater(h, len(h.outboxMessages), index)

	msg, ok := h.outboxMessages[index].(T)
	require.True(h, ok)

	return msg
}

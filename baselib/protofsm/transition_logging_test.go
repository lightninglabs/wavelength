package protofsm

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btclog/v2"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// logTestState models polling that replaces the state value and re-arms a
// timer while keeping the displayed phase unchanged.
type logTestState string

// String returns the displayed phase.
func (s logTestState) String() string { return string(s) }

// IsTerminal keeps the test machine available for the next event.
func (logTestState) IsTerminal() bool { return false }

// ProcessEvent emits an outbox marker and selects the requested phase.
func (logTestState) ProcessEvent(_ context.Context, next string,
	_ struct{}) (*StateTransition[string, string, struct{}], error) {

	return &StateTransition[string, string, struct{}]{
		NextState: logTestState(next),
		NewEvents: fn.Some(EmittedEvent[string, string]{
			Outbox: []string{"rearm"},
		}),
	}, nil
}

// TestTransitionLogging keeps polling out of INFO without losing its outbox,
// and retains both real phase changes at INFO and self-transitions at DEBUG.
func TestTransitionLogging(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	handler := btclog.NewDefaultHandler(&logs, btclog.WithNoTimestamp())
	handler.SetLevel(btclog.LevelInfo)
	machine := NewStateMachine(StateMachineCfg[string, string, struct{}]{
		Logger: btclog.NewSLogger(handler),
	})
	state := State[string, string, struct{}](logTestState("IntentSent"))
	for range 3 {
		next, outbox, err := machine.applyEvents(
			t.Context(), state, "IntentSent",
		)
		require.NoError(t, err)
		require.Equal(t, "IntentSent", next.String())
		require.Equal(t, []string{"rearm"}, outbox)
		state = next
	}
	require.Empty(t, logs.String())

	_, _, err := machine.applyEvents(t.Context(), state, "QuoteReceived")
	require.NoError(t, err)
	require.Contains(t, logs.String(), "State transition")
	require.Contains(t, logs.String(), "from_state=IntentSent")
	require.Contains(t, logs.String(), "to_state=QuoteReceived")

	logs.Reset()
	handler.SetLevel(btclog.LevelDebug)
	_, _, err = machine.applyEvents(t.Context(), state, "IntentSent")
	require.NoError(t, err)
	require.Contains(t, logs.String(), "State transition")
	require.Contains(t, logs.String(), "to_state=IntentSent")
}

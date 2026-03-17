package oor

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTerminalStatesIgnoreUnexpectedEvents asserts terminal states ignore
// late/unexpected deliveries and remain unchanged.
func TestTerminalStatesIgnoreUnexpectedEvents(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	completed := &Completed{}
	transition, err := completed.ProcessEvent(
		ctx, &FailEvent{Reason: "late"}, nil,
	)
	require.NoError(t, err)
	require.Equal(t, completed, transition.NextState)
	require.True(t, transition.NewEvents.IsNone())

	failed := &Failed{Reason: "boom"}
	transition, err = failed.ProcessEvent(
		ctx, &FailEvent{Reason: "late"}, nil,
	)
	require.NoError(t, err)
	require.Equal(t, failed, transition.NextState)
	require.True(t, transition.NewEvents.IsNone())
}

// TestReceiveStatesIgnoreUnhandledEvents asserts receive FSM states keep their
// current state and emit no work when fed unrelated late/out-of-order events.
func TestReceiveStatesIgnoreUnhandledEvents(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		state ReceiveState
		event Event
	}{
		{
			name:  "ReceiveIdle ignores ArkSignedEvent",
			state: &ReceiveIdle{},
			event: &ArkSignedEvent{},
		},
		{
			name:  "ReceiveResolving ignores checkpoint event",
			state: &ReceiveResolving{},
			event: &CheckpointsSignedEvent{},
		},
		{
			name:  "ReceiveAwaitingAck ignores StartTransferEvent",
			state: &ReceiveAwaitingAck{},
			event: &StartTransferEvent{},
		},
		{
			name:  "ReceiveCompleted ignores IncomingHandledEvent",
			state: &ReceiveCompleted{},
			event: &IncomingHandledEvent{},
		},
	}

	for _, testCase := range testCases {
		tc := testCase
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()

			transition, err := tc.state.ProcessEvent(
				ctx, tc.event, nil,
			)
			require.NoError(t, err)
			require.Equal(t, tc.state, transition.NextState)
			require.True(t, transition.NewEvents.IsNone())
		})
	}
}

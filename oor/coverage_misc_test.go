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

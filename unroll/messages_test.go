package unroll

import (
	"io"
	"testing"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/stretchr/testify/require"
)

// TestMessagesAreInMemoryActorCommands verifies the unroll actor command
// surface no longer requires serialized durable-mailbox messages.
func TestMessagesAreInMemoryActorCommands(t *testing.T) {
	t.Parallel()

	var msg actor.Message = &StartUnrollRequest{
		Height:  12345,
		Trigger: TriggerCriticalExpiry,
	}
	require.Equal(t, "StartUnrollRequest", msg.MessageType())

	_, serializable := msg.(interface {
		Encode(io.Writer) error

		Decode(io.Reader) error
	})
	require.False(t, serializable)
}

// TestMessagePriorityOrdering verifies that concrete progress notifications
// are delivered ahead of lossy block ticks and read-only status probes.
func TestMessagePriorityOrdering(t *testing.T) {
	t.Parallel()

	require.Greater(
		t, (&TxConfirmedMsg{}).Priority(),
		(&HeightObservedMsg{}).Priority(),
	)
	require.Greater(
		t, (&TxFailedMsg{}).Priority(),
		(&HeightObservedMsg{}).Priority(),
	)
	require.Greater(
		t, (&SpendObservedMsg{}).Priority(),
		(&HeightObservedMsg{}).Priority(),
	)
	require.Greater(
		t, (&HeightObservedMsg{}).Priority(),
		(&GetStateRequest{}).Priority(),
	)
	require.Less(t, (&HeightObservedMsg{}).Priority(), 0)
	require.Less(t, (&GetStateRequest{}).Priority(), 0)
}

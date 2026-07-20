package round

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRouteServerMessageNoPendingRoundTyped pins that a server message
// delivered with no pending round to route it to — the shape an
// IntentRequested join trigger takes when nothing was queued — fails
// with the typed ErrNoPendingRound rather than a bare error. JoinNextRound
// relies on errors.Is against this sentinel to turn the benign "nothing
// queued to join" case into a no-op instead of an INTERNAL fault.
func TestRouteServerMessageNoPendingRoundTyped(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)

	result := h.receive(&ServerMessageNotification{
		Message: &IntentRequested{},
	})

	require.Error(t, result.Err())
	require.ErrorIs(t, result.Err(), ErrNoPendingRound)
}

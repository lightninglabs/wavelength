package rounds

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFSMCreatedState verifies that a new test harness starts in the
// CreatedState and has no outbox messages.
func TestFSMCreatedState(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	// Verify initial state is CreatedState.
	_ = assertStateType[*CreatedState](h)

	// Verify no outbox messages initially.
	h.assertOutboxLen(0)

	// Verify environment has a valid round ID.
	require.NotEmpty(t, h.env.RoundID, "round ID should not be empty")
}

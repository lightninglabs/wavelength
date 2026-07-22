package waveclicommands

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDecideAutoJoinDefault verifies that the default path (neither
// --dry-run nor --no-join) opts in to the implicit JoinNextRound RPC.
// This is the one-shot ergonomics fix: refresh / leave should not
// require a follow-up `ark rounds join` for the common case.
func TestDecideAutoJoinDefault(t *testing.T) {
	t.Parallel()

	d := decideAutoJoin(false, false)
	require.True(t, d.Join)
	require.Contains(t, d.Notice, "auto-joined")
	require.Contains(t, d.Notice, "--no-join")
}

// TestDecideAutoJoinDryRunSkips verifies that a dry-run preview never
// commits a round, regardless of --no-join. The notice tells the user
// the join was skipped because of the dry run.
func TestDecideAutoJoinDryRunSkips(t *testing.T) {
	t.Parallel()

	d := decideAutoJoin(true, false)
	require.False(t, d.Join)
	require.Contains(t, d.Notice, "dry run")

	// --no-join is redundant under --dry-run but must not flip
	// Join back on or cause a surprising notice.
	d = decideAutoJoin(true, true)
	require.False(t, d.Join)
	require.Contains(t, d.Notice, "dry run")
}

// TestDecideAutoJoinNoJoinDefers verifies the batching opt-out: with
// --no-join the caller takes responsibility for invoking `ark rounds
// join` explicitly, and the stderr notice points at that follow-up
// command.
func TestDecideAutoJoinNoJoinDefers(t *testing.T) {
	t.Parallel()

	d := decideAutoJoin(false, true)
	require.False(t, d.Join)
	require.Contains(t, d.Notice, "--no-join")
	require.Contains(t, d.Notice, "ark rounds join")
}

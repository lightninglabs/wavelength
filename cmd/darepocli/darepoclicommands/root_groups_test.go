package darepoclicommands

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// findRootCommand returns the direct subcommand of root with the given name,
// failing the test if it is not registered.
func findRootCommand(t *testing.T, root *cobra.Command,
	name string) *cobra.Command {

	t.Helper()

	for _, sub := range root.Commands() {
		if sub.Name() == name {
			return sub
		}
	}

	t.Fatalf("command %q not registered on root", name)

	return nil
}

// rootGroupIDs returns the set of cobra group IDs registered on root.
func rootGroupIDs(root *cobra.Command) map[string]bool {
	ids := make(map[string]bool, len(root.Groups()))
	for _, group := range root.Groups() {
		ids[group.ID] = true
	}

	return ids
}

// executeHelp renders the root command's default help into a buffer.
func executeHelp(t *testing.T, root *cobra.Command) string {
	t.Helper()

	var buf bytes.Buffer
	root.SetArgs([]string{"--help"})
	root.SetOut(&buf)
	root.SetErr(&buf)
	require.NoError(t, root.Execute())

	return buf.String()
}

// TestRootHelpShowsOnlyWalletAndIntrospection verifies that the default help
// face is limited to the Wallet and Introspection groups with no advanced
// subtrees and no "Additional Commands" stragglers.
func TestRootHelpShowsOnlyWalletAndIntrospection(t *testing.T) {
	t.Parallel()

	out := executeHelp(t, newRootCmd(false))

	require.Contains(t, out, "Wallet:")
	require.Contains(t, out, "Introspection:")
	require.NotContains(t, out, "Advanced:")
	require.NotContains(t, out, "Additional Commands:")

	// The hidden completion command must not surface in the default face.
	// (Swap's absence is pinned structurally in
	// TestSwapCommandRemovedFromRoot, not by a brittle buffer scan here —
	// "swap" appears in send's own help text.)
	require.NotContains(t, out, "completion")
}

// TestDarepoDevRevealsAdvancedGroup verifies that dev mode surfaces the
// advanced subtrees under an Advanced group without disturbing the everyday
// face.
func TestDarepoDevRevealsAdvancedGroup(t *testing.T) {
	t.Parallel()

	out := executeHelp(t, newRootCmd(true))

	// "Advanced:" and the distinctive "recovery" row confirm the group
	// renders; ark/dev group membership is pinned structurally in
	// TestAdvancedCommandsGroupedUnderDevMode (a bare "dev" substring is
	// satisfied by the "(dev/regtest)" flag help, so it proves nothing).
	require.Contains(t, out, "Wallet:")
	require.Contains(t, out, "Introspection:")
	require.Contains(t, out, "Advanced:")
	require.Contains(t, out, "recovery")
}

// TestAdvancedCommandsHiddenByDefault verifies that ark, dev, and recovery are
// hidden and carry no group by default, so the empty-heading and group-panic
// cobra edges never trigger.
func TestAdvancedCommandsHiddenByDefault(t *testing.T) {
	t.Parallel()

	root := newRootCmd(false)
	for _, name := range []string{"ark", "dev", "recovery"} {
		sub := findRootCommand(t, root, name)
		require.Truef(t, sub.Hidden, "%q should be hidden", name)
		require.Emptyf(
			t, sub.GroupID, "%q should carry no group", name,
		)
	}

	require.NotContains(t, rootGroupIDs(root), groupAdvanced)
}

// TestAdvancedCommandsGroupedUnderDevMode verifies that dev mode makes the
// advanced subtrees visible members of the Advanced group.
func TestAdvancedCommandsGroupedUnderDevMode(t *testing.T) {
	t.Parallel()

	root := newRootCmd(true)
	require.Contains(t, rootGroupIDs(root), groupAdvanced)

	for _, name := range []string{"ark", "dev", "recovery"} {
		sub := findRootCommand(t, root, name)
		require.Falsef(t, sub.Hidden, "%q should be visible", name)
		require.Equalf(
			t, groupAdvanced, sub.GroupID, "%q should be in the "+
				"advanced group", name,
		)
	}
}

// TestEveryVisibleRootCommandHasGroup guards against a future command being
// registered without a group, which would silently reappear under an
// "Additional Commands" heading. Hidden commands are exempt by design.
func TestEveryVisibleRootCommandHasGroup(t *testing.T) {
	t.Parallel()

	root := newRootCmd(false)

	// The built-in help and completion commands are added lazily; force
	// them so the check covers them too.
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()

	groups := rootGroupIDs(root)
	for _, sub := range root.Commands() {
		if sub.Hidden {
			continue
		}

		require.NotEmptyf(
			t, sub.GroupID, "visible command %q has no group",
			sub.Name(),
		)
		require.Containsf(
			t, groups, sub.GroupID, "command %q references "+
				"unregistered group %q", sub.Name(),
			sub.GroupID,
		)
	}
}

// TestHiddenCommandsRemainDispatchable verifies that hiding a subtree from
// --help does not gate its execution: the commands still resolve and run.
func TestHiddenCommandsRemainDispatchable(t *testing.T) {
	t.Parallel()

	root := newRootCmd(false)

	// A nested advanced command still resolves through Find.
	cmd, _, err := root.Find([]string{"ark", "vtxos", "list"})
	require.NoError(t, err)
	require.Equal(t, "list", cmd.Name())

	// A hidden subtree's own help still executes end-to-end.
	var buf bytes.Buffer
	root.SetArgs([]string{"recovery", "--help"})
	root.SetOut(&buf)
	root.SetErr(&buf)
	require.NoError(t, root.Execute())
	require.Contains(t, buf.String(), "escalate")

	// The completion command is hidden from --help via
	// CompletionOptions.HiddenDefaultCmd, but must stay runnable — the
	// same hidden-not-gated contract. It is registered lazily, so force
	// init before resolving it; this pins that we did not switch to
	// DisableDefaultCmd, which would remove it entirely.
	root.InitDefaultCompletionCmd()
	completion, _, err := root.Find([]string{"completion"})
	require.NoError(t, err)
	require.Equal(t, "completion", completion.Name())
	require.True(t, completion.Hidden)
}

// TestDarepoDevEnvRevealsAdvanced verifies the DAREPO_DEV env plumbing into
// NewRootCmd: only the exact value "1" reveals the advanced subtrees; any
// other value leaves them hidden. It intentionally does not run in parallel:
// t.Setenv is incompatible with t.Parallel.
func TestDarepoDevEnvRevealsAdvanced(t *testing.T) {
	t.Setenv(devModeEnvVar, "1")

	root := NewRootCmd()
	require.Contains(t, rootGroupIDs(root), groupAdvanced)

	ark := findRootCommand(t, root, "ark")
	require.False(t, ark.Hidden)
	require.Equal(t, groupAdvanced, ark.GroupID)

	// The contract is an exact match on "1"; any other value is a no-op
	// (visibility only, never execution).
	for _, v := range []string{"0", ""} {
		t.Setenv(devModeEnvVar, v)
		require.NotContainsf(
			t,
			rootGroupIDs(
				NewRootCmd(),
			),
			groupAdvanced, "DAREPO_DEV=%q must not reveal the "+
				"advanced group",
			v,
		)
	}
}

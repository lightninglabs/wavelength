package waveclicommands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestPromptConfirmationUsesStderr(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader("yes\n"))

	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	require.NoError(t, promptConfirmation(cmd, "Proceed? [y/N]: "))
	require.Empty(t, stdout.String())
	require.Equal(t, "Proceed? [y/N]: ", stderr.String())
}

func TestRecoveryPromptUsesStderr(t *testing.T) {
	t.Parallel()

	cmd := newRecoveryEscalateCmd()
	cmd.SetIn(strings.NewReader("yes\n"))

	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	require.NoError(t, confirmRecoveryEscalation(cmd, "recovery-1"))
	require.Empty(t, stdout.String())
	require.Contains(t, stderr.String(), "recovery-1")
	require.Contains(t, stderr.String(), "Start recovery? [y/N]: ")
}

func TestNoInputSuppressesPrompts(t *testing.T) {
	t.Parallel()

	root := newRootCmd(false)
	require.NoError(t, root.PersistentFlags().Set("no-input", "true"))

	cmd, _, err := root.Find([]string{"send"})
	require.NoError(t, err)
	cmd.SetIn(strings.NewReader("yes\n"))

	require.True(t, inputDisabled(cmd))
	require.False(t, canPrompt(cmd))
}

func TestCISuppressesPromptsButNotOutput(t *testing.T) {
	t.Setenv("CI", "true")

	root := newRootCmd(false)
	root.SetArgs([]string{"--help"})

	var stdout bytes.Buffer
	root.SetOut(&stdout)
	require.NoError(t, root.Execute())
	require.Contains(t, stdout.String(), "wavecli")
	require.Contains(t, stdout.String(), "Wallet:")

	cmd, _, err := root.Find([]string{"send"})
	require.NoError(t, err)
	cmd.SetIn(strings.NewReader("yes\n"))
	require.True(t, inputDisabled(cmd))
	require.False(t, canPrompt(cmd))
}

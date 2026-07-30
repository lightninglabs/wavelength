package waveclicommands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestConfirmMoneyMovementRequiresExplicitApproval verifies that automation
// receives the stable error and exit code before a fund-moving action runs.
func TestConfirmMoneyMovementRequiresExplicitApproval(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	cmd.Flags().Bool("yes", false, "")
	cmd.SetIn(strings.NewReader(""))

	err := confirmMoneyMovement(cmd, "broadcast the wallet sweep")
	require.Error(t, err)
	require.ErrorContains(t, err, "broadcast the wallet sweep")
	require.ErrorContains(t, err, "--yes")
	require.Equal(t, ExitConfirmationRequired, ExitCodeFor(err))
}

// TestConfirmMoneyMovementAcceptsYes verifies the explicit automation path.
func TestConfirmMoneyMovementAcceptsYes(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	cmd.Flags().Bool("yes", false, "")
	require.NoError(t, cmd.Flags().Set("yes", "true"))

	require.NoError(t, confirmMoneyMovement(cmd, "move funds"))
}

// TestConfirmMoneyMovementInteractivePrompt verifies the terminal path: on a
// TTY without --yes, a "y" answer approves and any other answer aborts.
func TestConfirmMoneyMovementInteractivePrompt(t *testing.T) {
	// NOT t.Parallel(): overrides the package-level stdinIsTTY indirection.
	prev := stdinIsTTY
	stdinIsTTY = func(*cobra.Command) bool { return true }
	t.Cleanup(func() { stdinIsTTY = prev })

	newCmd := func(answer string) *cobra.Command {
		cmd := &cobra.Command{}
		cmd.Flags().Bool("yes", false, "")
		cmd.SetIn(strings.NewReader(answer))
		cmd.SetErr(&bytes.Buffer{})

		return cmd
	}

	require.NoError(t, confirmMoneyMovement(newCmd("y\n"), "move funds"))

	err := confirmMoneyMovement(newCmd("n\n"), "move funds")
	require.ErrorContains(t, err, "aborted by user")
}

// TestMoneyMovingCommandsSkipGateOnPreview verifies preview paths (--dry-run
// for ark sends, no --broadcast for sweeps) never hit the confirmation gate:
// they flow past it toward the daemon dial rather than refusing with
// CONFIRMATION_REQUIRED.
func TestMoneyMovingCommandsSkipGateOnPreview(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cmd  func() *cobra.Command
		run  func(*cobra.Command, []string) error
		args map[string]string
	}{
		{
			name: "wallet sweep preview",
			cmd:  newWalletSweepCmd,
			run:  walletSweep,
			args: map[string]string{
				"destination": "bcrt1ptestdestination",
			},
		},
		{
			name: "boarding sweep preview",
			cmd:  newSweepCmd,
			run:  sweep,
			args: map[string]string{},
		},
		{
			name: "in-round send dry-run",
			cmd:  newArkSendInRoundCmd,
			run:  sendInRound,
			args: map[string]string{
				"to":      "bcrt1ptestdestination",
				"amount":  "1000",
				"dry-run": "true",
			},
		},
		{
			name: "out-of-round send dry-run",
			cmd:  newArkSendOORCmd,
			run:  sendOOR,
			args: map[string]string{
				"to":      "bcrt1ptestdestination",
				"amount":  "1000",
				"dry-run": "true",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cmd := test.cmd()
			cmd.SetIn(strings.NewReader(""))
			for name, value := range test.args {
				require.NoError(t, cmd.Flags().Set(name, value))
			}

			// The preview path has no daemon, so it errors on dial
			// — but it must NOT be the confirmation refusal.
			err := test.run(cmd, nil)
			require.NotEqual(
				t, ExitConfirmationRequired, ExitCodeFor(err),
			)
		})
	}
}

// TestSendOORRejectsEmptyRecipients guards the fix for the panic where the OOR
// gate indexed recipients[0] on a --request-json payload that skipped flag
// validation. It must now return the structured INVALID_ARGS error.
func TestSendOORRejectsEmptyRecipients(t *testing.T) {
	t.Parallel()

	cmd := newArkSendOORCmd()
	cmd.Flags().String("request-json", "", "")
	require.NoError(t, cmd.Flags().Set("request-json", "{}"))
	cmd.SetIn(strings.NewReader(""))

	err := sendOOR(cmd, nil)
	require.ErrorContains(t, err, "at least one recipient is required")
	require.Equal(t, ExitInvalidArgs, ExitCodeFor(err))
}

// TestMoneyMovingCommandsGateBeforeDial exercises each newly protected command
// with non-interactive stdin. A missing gate would continue into daemon setup
// and return a connection error instead of CONFIRMATION_REQUIRED.
func TestMoneyMovingCommandsGateBeforeDial(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cmd  func() *cobra.Command
		run  func(*cobra.Command, []string) error
		args map[string]string
	}{
		{
			name: "wallet sweep broadcast",
			cmd:  newWalletSweepCmd,
			run:  walletSweep,
			args: map[string]string{
				"destination": "bcrt1ptestdestination",
				"broadcast":   "true",
			},
		},
		{
			name: "boarding sweep broadcast",
			cmd:  newSweepCmd,
			run:  sweep,
			args: map[string]string{
				"broadcast": "true",
			},
		},
		{
			name: "in-round send",
			cmd:  newArkSendInRoundCmd,
			run:  sendInRound,
			args: map[string]string{
				"to":     "bcrt1ptestdestination",
				"amount": "1000",
			},
		},
		{
			name: "out-of-round send",
			cmd:  newArkSendOORCmd,
			run:  sendOOR,
			args: map[string]string{
				"to":     "bcrt1ptestdestination",
				"amount": "1000",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cmd := test.cmd()
			cmd.SetIn(strings.NewReader(""))
			for name, value := range test.args {
				require.NoError(t, cmd.Flags().Set(name, value))
			}

			err := test.run(cmd, nil)
			require.Error(t, err)
			require.Equal(
				t, ExitConfirmationRequired, ExitCodeFor(err),
			)
		})
	}
}

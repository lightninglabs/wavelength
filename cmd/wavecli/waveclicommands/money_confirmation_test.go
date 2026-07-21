package waveclicommands

import (
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

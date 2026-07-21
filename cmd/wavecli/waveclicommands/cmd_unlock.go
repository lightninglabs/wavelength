package waveclicommands

import (
	"fmt"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/spf13/cobra"
)

// newUnlockCmd builds the top-level `unlock` verb. It dials
// wavewalletrpc.WalletService.Unlock which proxies
// waverpc.UnlockWallet. The CLI reads the wallet password from an
// environment variable, file, explicit stdin, or a TTY prompt so the
// secret never enters argv.
func newUnlockCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unlock",
		Short: "Unlock an existing wallet",
		Long: "Opens the wallet database with its password and " +
			"starts the wallet subsystem. The password is " +
			"read from WAVED_WALLET_PASSWORD, " +
			"--wallet_password_file, or explicit " +
			"--password-stdin (never CLI args).\n\n" +
			"Example:\n" +
			"  printf '%s\\n' 'hunter2hunter2' | " +
			"wavecli unlock --password-stdin",
		Args: cobra.NoArgs,
		RunE: walletUnlock,
	}

	cmd.Flags().String("wallet_password_file", "",
		"path to file containing wallet password")
	cmd.Flags().Bool("password-stdin", false,
		"read one wallet password line from stdin")

	return cmd
}

// walletUnlock implements the top-level `unlock` verb.
func walletUnlock(cmd *cobra.Command, _ []string) error {
	password, err := readPassword(cmd)
	if err != nil {
		return err
	}
	defer zeroBytes(password)

	return withWalletClient(
		cmd, func(c wavewalletrpc.WalletServiceClient) error {
			resp, err := c.Unlock(
				cmd.Context(), &wavewalletrpc.UnlockRequest{
					WalletPassword: password,
				},
			)
			if err != nil {
				return fmt.Errorf("unlock wallet: %w", err)
			}

			return printWalletProto(resp)
		},
	)
}

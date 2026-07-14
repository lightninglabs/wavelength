package waveclicommands

import (
	"fmt"

	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
	"github.com/spf13/cobra"
)

// newUnlockCmd builds the top-level `unlock` verb. It dials
// walletdkrpc.WalletService.Unlock which proxies
// waverpc.UnlockWallet. The CLI reads the wallet password from
// stdin / WAVED_WALLET_PASSWORD / --wallet_password_file (never CLI
// args) so secrets never enter argv.
func newUnlockCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unlock",
		Short: "Unlock an existing wallet",
		Long: "Opens the wallet database with its password and " +
			"starts the wallet subsystem. The password is " +
			"read from stdin / WAVED_WALLET_PASSWORD env var / " +
			"--wallet_password_file (never CLI args).\n\n" +
			"Example:\n" +
			"  echo -n 'hunter2hunter2' | wavecli unlock",
		Args: cobra.NoArgs,
		RunE: walletUnlock,
	}

	cmd.Flags().String("wallet_password_file", "",
		"path to file containing wallet password")

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
		cmd, func(c walletdkrpc.WalletServiceClient) error {
			resp, err := c.Unlock(
				cmd.Context(), &walletdkrpc.UnlockRequest{
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

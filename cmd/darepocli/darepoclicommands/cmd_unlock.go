package darepoclicommands

import (
	"fmt"

	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"github.com/spf13/cobra"
)

// newUnlockCmd builds the top-level `unlock` verb. It dials
// walletrpc.WalletService.Unlock which proxies
// daemonrpc.UnlockWallet. The CLI reads the wallet password from
// stdin / DAREPOD_WALLET_PASSWORD / --wallet_password_file (never CLI
// args) so secrets never enter argv.
func newUnlockCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unlock",
		Short: "Unlock an existing wallet",
		Long: "Decrypts the on-disk wallet seed and starts the " +
			"wallet subsystem. The password is read from " +
			"stdin / DAREPOD_WALLET_PASSWORD env var / " +
			"--wallet_password_file (never CLI args).\n\n" +
			"Example:\n" +
			"  echo -n 'hunter2hunter2' | darepocli unlock",
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
		cmd, func(c walletrpc.WalletServiceClient) error {
			resp, err := c.Unlock(
				cmd.Context(), &walletrpc.UnlockRequest{
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

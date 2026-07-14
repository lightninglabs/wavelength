package waveclicommands

import (
	"fmt"

	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
	"github.com/spf13/cobra"
)

// newRecvCmd builds the top-level `recv` verb. Direction is chosen
// explicitly: --offchain (default) generates a BOLT-11 Lightning
// invoice via walletdkrpc.WalletService.Recv; --onchain returns a fresh
// boarding address via walletdkrpc.WalletService.Deposit.
func newRecvCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recv",
		Short: "Receive a payment (offchain invoice / onchain addr)",
		Long: "Asks the daemon to materialize an inbound payment " +
			"surface. With --offchain (default) the daemon opens " +
			"an out-swap and returns a BOLT-11 invoice signed " +
			"with a daemon-managed key. With --onchain the " +
			"daemon returns a fresh boarding address; fund it " +
			"and the daemon rolls the boarding output into the " +
			"next round.\n\n" +
			"Examples:\n" +
			"  wavecli recv --offchain --amt 5000 " +
			"--memo \"coffee\"\n" +
			"  wavecli recv --onchain",
		Args: cobra.NoArgs,
		// The retired `swap receive` verb is covered by `recv
		// --offchain`; steer stale invocations here.
		SuggestFor: []string{
			"swap",
		},
		RunE: walletRecv,
	}

	cmd.Flags().Bool("offchain", false,
		"force offchain (Lightning invoice) recv; default when "+
			"neither --offchain nor --onchain is set")
	cmd.Flags().Bool("onchain", false,
		"force onchain (boarding address) recv")
	cmd.Flags().Uint64("amt", 0,
		"amount in satoshis (required for --offchain)")
	cmd.Flags().String("memo", "",
		"optional human-readable memo embedded in the offchain "+
			"invoice")
	cmd.Flags().Uint64("amt_hint", 0,
		"optional expected amount for --onchain (accounting only)")

	return cmd
}

// walletRecv implements the top-level `recv` verb.
func walletRecv(cmd *cobra.Command, _ []string) error {
	offchain, err := resolveOffchainFlag(cmd)
	if err != nil {
		return invalidArgs(err)
	}

	amt, _ := cmd.Flags().GetUint64("amt")
	memo, _ := cmd.Flags().GetString("memo")
	amtHint, _ := cmd.Flags().GetUint64("amt_hint")

	if err := invalidArgs(validateFreeText("--memo", memo)); err != nil {
		return err
	}

	if offchain && amt == 0 {
		return PrintError(
			"INVALID_ARGS", "--amt is required for offchain "+
				"recv (use --onchain for a boarding "+
				"address without an amount)",
		)
	}

	return withWalletClient(
		cmd, func(c walletdkrpc.WalletServiceClient) error {
			if offchain {
				resp, err := c.Recv(
					cmd.Context(),
					&walletdkrpc.RecvRequest{
						AmtSat: amt,
						Memo:   memo,
					},
				)
				if err != nil {
					return fmt.Errorf("recv invoice: %w",
						err)
				}

				return printWalletProto(resp)
			}

			resp, err := c.Deposit(
				cmd.Context(),
				&walletdkrpc.DepositRequest{
					AmtSatHint: amtHint,
				},
			)
			if err != nil {
				return fmt.Errorf("recv deposit: %w", err)
			}

			return printWalletProto(resp)
		},
	)
}

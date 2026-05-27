package darepoclicommands

import (
	"fmt"

	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/spf13/cobra"
)

// newRecvCmd builds the top-level `recv` verb. Direction is chosen
// explicitly: --offchain (default) generates a BOLT-11 Lightning invoice via
// walletdkrpc.WalletService.Recv; --boarding returns a fresh boarding address
// via walletdkrpc.WalletService.Deposit; --onchain returns a fresh taproot
// address from the backing wallet.
func newRecvCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recv",
		Short: "Receive a payment (offchain / boarding / onchain)",
		Long: "Asks the daemon to materialize an inbound payment " +
			"surface. With --offchain (default) the daemon opens " +
			"a swap-in and returns a BOLT-11 invoice signed " +
			"with a daemon-managed key. With --boarding the " +
			"daemon returns a fresh boarding address; fund it " +
			"and the daemon rolls the boarding output into the " +
			"next round. With --onchain the daemon returns a " +
			"fresh taproot address from the backing Bitcoin " +
			"wallet.\n\n" +
			"Examples:\n" +
			"  darepocli recv --offchain --amt 5000 " +
			"--memo \"coffee\"\n" +
			"  darepocli recv --boarding\n" +
			"  darepocli recv --onchain",
		Args: cobra.NoArgs,
		RunE: walletRecv,
	}

	cmd.Flags().Bool("offchain", false,
		"force offchain (Lightning invoice) recv; default when "+
			"no receive mode is set")
	cmd.Flags().Bool("boarding", false,
		"force boarding-address recv; funds are rolled into Ark")
	cmd.Flags().Bool("onchain", false,
		"force backing-wallet taproot-address recv")
	cmd.Flags().Uint64("amt", 0,
		"amount in satoshis (required for --offchain)")
	cmd.Flags().String("memo", "",
		"optional human-readable memo embedded in the offchain "+
			"invoice")
	cmd.Flags().Uint64("amt_hint", 0,
		"optional expected amount for --boarding (accounting only)")

	return cmd
}

// walletRecv implements the top-level `recv` verb.
func walletRecv(cmd *cobra.Command, _ []string) error {
	mode, err := resolveRecvMode(cmd)
	if err != nil {
		return invalidArgs(err)
	}

	amt, _ := cmd.Flags().GetUint64("amt")
	memo, _ := cmd.Flags().GetString("memo")
	amtHint, _ := cmd.Flags().GetUint64("amt_hint")

	if err := invalidArgs(validateFreeText("--memo", memo)); err != nil {
		return err
	}

	if mode == recvModeOffchain && amt == 0 {
		return PrintError(
			"INVALID_ARGS", "--amt is required for offchain "+
				"recv (use --boarding for a boarding "+
				"address without an amount)",
		)
	}

	return withWalletClient(
		cmd, func(c walletdkrpc.WalletServiceClient) error {
			switch mode {
			case recvModeOffchain:
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

			case recvModeBoarding:
				resp, err := c.Deposit(
					cmd.Context(),
					&walletdkrpc.DepositRequest{
						AmtSatHint: amtHint,
					},
				)
				if err != nil {
					return fmt.Errorf("recv boarding: %w",
						err)
				}

				return printWalletProto(resp)

			case recvModeOnchain:
				req := &walletdkrpc.WalletOnchainAddressRequest{}
				resp, err := c.OnchainAddress(
					cmd.Context(), req,
				)
				if err != nil {
					return fmt.Errorf("recv onchain: %w",
						err)
				}

				return printWalletProto(resp)

			default:
				return fmt.Errorf("unknown recv mode %d", mode)
			}
		},
	)
}

package waveclicommands

import (
	"fmt"

	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
	"github.com/spf13/cobra"
)

// newWalletSweepCmd builds the top-level `wallet-sweep` verb. It dials
// walletdkrpc.WalletService.SweepWallet, which previews (or, with --broadcast,
// publishes) a sweep of every confirmed backing-wallet UTXO to a single
// destination address. Boarding outputs are excluded — those are handled by
// `ark sweep`.
//
// The daemon-side logic lives entirely in the wallet actor; this command is a
// thin client that validates the destination locally and prints the daemon's
// preview or broadcast response.
func newWalletSweepCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wallet-sweep",
		Short: "Sweep the backing wallet to a destination address",
		Long: "Previews, or with --broadcast publishes, a sweep " +
			"of every confirmed backing-wallet UTXO to a " +
			"single destination address. Boarding outputs " +
			"are excluded (use `ark sweep` for those). " +
			"Without --broadcast the command returns a " +
			"preview of the selected inputs, fee, and net " +
			"amount without leasing inputs or broadcasting." +
			"\n\n" +
			"Example:\n" +
			"  wavecli wallet-sweep --destination bcrt1...\n" +
			"  wavecli wallet-sweep --destination bcrt1... " +
			"--broadcast\n" +
			"  wavecli wallet-sweep --destination bcrt1... " +
			"--fee-rate 5 --broadcast",
		Args: cobra.NoArgs,
		RunE: walletSweep,
	}

	cmd.Flags().String("destination", "",
		"on-chain destination address for the swept funds")
	_ = cmd.MarkFlagRequired("destination")
	cmd.Flags().Bool("broadcast", false,
		"publish the sweep; omitted means preview only")
	cmd.Flags().Int64("fee-rate", 0,
		"explicit fee rate in sat/vByte; zero estimates from the "+
			"chain backend at --conf-target")
	cmd.Flags().Uint32("conf-target", 0,
		"confirmation target for fee estimation when --fee-rate is "+
			"zero")

	return cmd
}

// walletSweep implements the top-level `wallet-sweep` verb.
func walletSweep(cmd *cobra.Command, _ []string) error {
	destination, _ := cmd.Flags().GetString("destination")
	if err := invalidArgs(validateDestination(destination)); err != nil {
		return err
	}

	feeRate, _ := cmd.Flags().GetInt64("fee-rate")
	if feeRate < 0 {
		return invalidArgs(
			fmt.Errorf("--fee-rate must be non-negative"),
		)
	}

	broadcast, _ := cmd.Flags().GetBool("broadcast")
	confTarget, _ := cmd.Flags().GetUint32("conf-target")

	req := &walletdkrpc.SweepWalletRequest{
		DestinationAddress: destination,
		Broadcast:          broadcast,
		FeeRateSatPerVbyte: feeRate,
		ConfTarget:         confTarget,
	}

	return withWalletClient(
		cmd, func(c walletdkrpc.WalletServiceClient) error {
			resp, err := c.SweepWallet(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("wallet sweep: %w", err)
			}

			return printWalletProto(resp)
		},
	)
}

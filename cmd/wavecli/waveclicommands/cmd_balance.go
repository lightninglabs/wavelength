package waveclicommands

import (
	"fmt"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/spf13/cobra"
)

// newBalanceCmd builds the top-level `balance` verb. It dials
// wavewalletrpc.WalletService.Balance which returns the unified shape
// (confirmed_sat, pending_in_sat, pending_out_sat) the wallet layer
// projects onto every backend.
func newBalanceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "balance",
		Short: "Display wallet balance",
		Long: "Returns the unified wallet balance: confirmed " +
			"spendable VTXOs (confirmed_sat), in-flight " +
			"inbound (pending_in_sat: boarding + receive), " +
			"and in-flight outbound (pending_out_sat: send + " +
			"exit) — all in satoshis.\n\n" +
			"Example:\n" +
			"  wavecli balance",
		Args: cobra.NoArgs,
		RunE: walletBalance,
	}
}

// walletBalance implements the top-level `balance` verb.
func walletBalance(cmd *cobra.Command, _ []string) error {
	return withWalletClient(
		cmd, func(c wavewalletrpc.WalletServiceClient) error {
			resp, err := c.Balance(
				cmd.Context(), &wavewalletrpc.BalanceRequest{},
			)
			if err != nil {
				return fmt.Errorf("balance: %w", err)
			}

			return printWalletProto(resp)
		},
	)
}

package darepoclicommands

import (
	"fmt"

	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"github.com/spf13/cobra"
)

// newBalanceCmd builds the top-level `balance` verb. It dials
// walletrpc.WalletService.Balance which returns the unified shape
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
			"  darepocli balance",
		RunE: walletBalance,
	}
}

// walletBalance implements the top-level `balance` verb.
func walletBalance(cmd *cobra.Command, _ []string) error {
	return withWalletClient(
		cmd, func(c walletrpc.WalletServiceClient) error {
			resp, err := c.Balance(
				cmd.Context(), &walletrpc.BalanceRequest{},
			)
			if err != nil {
				return fmt.Errorf("balance: %w", err)
			}

			return printWalletProto(resp)
		},
	)
}

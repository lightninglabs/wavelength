package darepoclicommands

import (
	"fmt"

	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"github.com/spf13/cobra"
)

// newExitCmd builds the top-level `exit` verb. It dials
// walletrpc.WalletService.Exit which proxies daemonrpc.Unroll to spawn
// a durable unilateral-exit job. The `exit status` subcommand reads the
// job status via walletrpc.WalletService.ExitStatus.
//
// `exit` replaces the legacy `unroll` verb at the user surface; the
// underlying daemon actor/registry pathway is unchanged.
func newExitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exit",
		Short: "Trigger a unilateral exit for a VTXO",
		Long: "Starts the on-chain recovery process for the " +
			"specified VTXO outpoint. The daemon assembles a " +
			"recovery proof, spawns a durable exit (unroll) " +
			"job, and drives the on-chain recovery to " +
			"completion. The job survives daemon restarts; " +
			"this command only submits the request.\n\n" +
			"Example:\n" +
			"  darepocli exit --outpoint TXID:VOUT\n" +
			"  darepocli exit status --outpoint TXID:VOUT",
		RunE: walletExit,
	}

	cmd.Flags().String("outpoint", "",
		"VTXO outpoint to exit (txid:vout)")
	_ = cmd.MarkFlagRequired("outpoint")

	cmd.AddCommand(newExitStatusCmd())

	return cmd
}

// walletExit implements the top-level `exit` verb.
func walletExit(cmd *cobra.Command, _ []string) error {
	outpoint, _ := cmd.Flags().GetString("outpoint")
	if err := validateOutpoint(outpoint); err != nil {
		return err
	}

	return withWalletClient(
		cmd, func(c walletrpc.WalletServiceClient) error {
			resp, err := c.Exit(
				cmd.Context(), &walletrpc.ExitRequest{
					Outpoint: outpoint,
				},
			)
			if err != nil {
				return fmt.Errorf("exit: %w", err)
			}

			return printWalletProto(resp)
		},
	)
}

// newExitStatusCmd builds the `exit status` subcommand. It dials
// walletrpc.WalletService.ExitStatus which proxies
// daemonrpc.GetUnrollStatus.
func newExitStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Query the status of an exit (unroll) job",
		Long: "Returns the current status of a unilateral exit job " +
			"for the specified VTXO outpoint, including the " +
			"job phase, sweep txid, and any errors.\n\n" +
			"Example:\n" +
			"  darepocli exit status --outpoint TXID:VOUT",
		RunE: walletExitStatus,
	}

	cmd.Flags().String("outpoint", "",
		"VTXO outpoint to query (txid:vout)")
	_ = cmd.MarkFlagRequired("outpoint")

	return cmd
}

// walletExitStatus implements the `exit status` subcommand.
func walletExitStatus(cmd *cobra.Command, _ []string) error {
	outpoint, _ := cmd.Flags().GetString("outpoint")
	if err := validateOutpoint(outpoint); err != nil {
		return err
	}

	return withWalletClient(
		cmd, func(c walletrpc.WalletServiceClient) error {
			resp, err := c.ExitStatus(
				cmd.Context(),
				&walletrpc.ExitStatusRequest{
					Outpoint: outpoint,
				},
			)
			if err != nil {
				return fmt.Errorf("exit status: %w", err)
			}

			return printWalletProto(resp)
		},
	)
}

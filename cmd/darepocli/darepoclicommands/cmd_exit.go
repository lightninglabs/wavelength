package darepoclicommands

import (
	"fmt"

	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/spf13/cobra"
)

// newExitCmd builds the top-level `exit` verb. It dials
// walletdkrpc.WalletService.Exit which proxies daemonrpc.Unroll to spawn
// a durable unilateral-exit job. The `exit status` subcommand reads the
// job status via walletdkrpc.WalletService.ExitStatus.
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
		Args: cobra.NoArgs,
		RunE: walletExit,
	}

	cmd.Flags().String("outpoint", "",
		"VTXO outpoint to exit (txid:vout)")
	_ = cmd.MarkFlagRequired("outpoint")
	cmd.Flags().Bool("dry-run", false,
		"validate inputs locally and print the preview without "+
			"dispatching to the daemon; exits 10 on a valid "+
			"preview, non-zero on validation failure")

	cmd.AddCommand(newExitStatusCmd())

	return cmd
}

// walletExit implements the top-level `exit` verb.
func walletExit(cmd *cobra.Command, _ []string) error {
	outpoint, _ := cmd.Flags().GetString("outpoint")
	if err := invalidArgs(validateOutpoint(outpoint)); err != nil {
		return err
	}

	req := &walletdkrpc.ExitRequest{Outpoint: outpoint}

	if dryRun, _ := cmd.Flags().GetBool("dry-run"); dryRun {
		return walletDryRunPreview(
			"walletdkrpc.WalletService/Exit", req,
		)
	}

	return withWalletClient(
		cmd, func(c walletdkrpc.WalletServiceClient) error {
			resp, err := c.Exit(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("exit: %w", err)
			}

			return printWalletProto(resp)
		},
	)
}

// newExitStatusCmd builds the `exit status` subcommand. It dials
// walletdkrpc.WalletService.ExitStatus which proxies
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
		Args: cobra.NoArgs,
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
	if err := invalidArgs(validateOutpoint(outpoint)); err != nil {
		return err
	}

	return withWalletClient(
		cmd, func(c walletdkrpc.WalletServiceClient) error {
			resp, err := c.ExitStatus(
				cmd.Context(),
				&walletdkrpc.ExitStatusRequest{
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

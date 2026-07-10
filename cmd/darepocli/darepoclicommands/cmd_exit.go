package darepoclicommands

import (
	"fmt"

	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/spf13/cobra"
)

const forceUnrollAck = "I_KNOW_WHAT_I_AM_DOING"

// newExitCmd builds the top-level `exit` verb. It dials
// walletdkrpc.WalletService.Exit, which queues a cooperative leave by default
// and starts unilateral unroll only when the caller supplies the exact force
// acknowledgement. The `exit status` subcommand reads the forced-unroll job
// status via walletdkrpc.WalletService.ExitStatus.
//
// `exit` replaces the legacy `unroll` verb at the user surface; the
// underlying daemon actor/registry pathway is only used for acknowledged
// forced exits.
func newExitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exit",
		Short: "Cooperatively exit a VTXO",
		Long: "Queues the specified VTXO outpoint for cooperative " +
			"leave. If --onchain-address is omitted, the daemon " +
			"generates a fresh backing-wallet destination. " +
			"Unilateral unroll is only started when " +
			"--force-unroll-ack is exactly " +
			forceUnrollAck + ".\n\n" +
			"Example:\n" +
			"  darepocli exit --outpoint TXID:VOUT\n" +
			"  darepocli exit --outpoint TXID:VOUT " +
			"--onchain-address bcrt1...\n" +
			"  darepocli exit --outpoint TXID:VOUT " +
			"--force-unroll-ack " + forceUnrollAck + "\n" +
			"  darepocli exit status --outpoint TXID:VOUT",
		Args: cobra.NoArgs,
		RunE: walletExit,
	}

	cmd.Flags().String("outpoint", "",
		"VTXO outpoint to exit (txid:vout)")
	_ = cmd.MarkFlagRequired("outpoint")
	cmd.Flags().String("onchain-address", "",
		"cooperative leave destination; omitted means a fresh "+
			"wallet-owned address")
	cmd.Flags().String("force-unroll-ack", "",
		"exact acknowledgement required to force unilateral unroll")
	cmd.Flags().Bool("dry-run", false,
		"validate inputs locally and print the preview without "+
			"dispatching to the daemon; exits 10 on a valid "+
			"preview, non-zero on validation failure")

	cmd.AddCommand(newExitStatusCmd())
	cmd.AddCommand(newExitSummaryCmd())
	cmd.AddCommand(newExitPlanCmd())

	return cmd
}

// walletExit implements the top-level `exit` verb.
func walletExit(cmd *cobra.Command, _ []string) error {
	outpoint, _ := cmd.Flags().GetString("outpoint")
	if err := invalidArgs(validateOutpoint(outpoint)); err != nil {
		return err
	}

	onchainAddress, _ := cmd.Flags().GetString("onchain-address")
	if onchainAddress != "" {
		if err := invalidArgs(
			validateDestination(onchainAddress),
		); err != nil {
			return err
		}
	}

	forceAck, _ := cmd.Flags().GetString("force-unroll-ack")
	if forceAck != "" && forceAck != forceUnrollAck {
		return invalidArgs(
			fmt.Errorf("--force-unroll-ack must be exactly %q",
				forceUnrollAck),
		)
	}
	if forceAck != "" && onchainAddress != "" {
		return invalidArgs(
			fmt.Errorf("--onchain-address cannot be combined " +
				"with --force-unroll-ack"),
		)
	}

	req := &walletdkrpc.ExitRequest{
		Outpoint:       outpoint,
		OnchainAddress: onchainAddress,
		ForceUnrollAck: forceAck,
	}

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
			"for the specified VTXO outpoint. By default the " +
			"response includes recovery-tree progress (layer and " +
			"transaction counts), the CSV maturity countdown, a " +
			"best-case block estimate to full exit, and the " +
			"on-chain fee breakdown, alongside the job phase, " +
			"sweep txid, and any errors. The endpoint stays " +
			"queryable after the exit completes.\n\n" +
			"Example:\n" +
			"  darepocli exit status --outpoint TXID:VOUT\n" +
			"  darepocli exit status --outpoint TXID:VOUT " +
			"--detailed=false",
		Args: cobra.NoArgs,
		RunE: walletExitStatus,
	}

	cmd.Flags().String("outpoint", "",
		"VTXO outpoint to query (txid:vout)")
	_ = cmd.MarkFlagRequired("outpoint")
	cmd.Flags().Bool("detailed", true,
		"include tree/CSV progress, best-case block countdown, and "+
			"the fee breakdown; pass --detailed=false for a "+
			"coarse, cheaper phase-only status")

	return cmd
}

// walletExitStatus implements the `exit status` subcommand.
func walletExitStatus(cmd *cobra.Command, _ []string) error {
	outpoint, _ := cmd.Flags().GetString("outpoint")
	if err := invalidArgs(validateOutpoint(outpoint)); err != nil {
		return err
	}

	detailed, _ := cmd.Flags().GetBool("detailed")

	return withWalletClient(
		cmd, func(c walletdkrpc.WalletServiceClient) error {
			resp, err := c.ExitStatus(
				cmd.Context(),
				&walletdkrpc.ExitStatusRequest{
					Outpoint: outpoint,
					Detailed: detailed,
				},
			)
			if err != nil {
				return fmt.Errorf("exit status: %w", err)
			}

			return printWalletProto(resp)
		},
	)
}

// newExitSummaryCmd builds the `exit summary` subcommand. It dials
// walletdkrpc.WalletService.ExitSummary to report the wallet-wide portfolio of
// in-progress exits plus aggregate totals.
func newExitSummaryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "summary",
		Short: "Summarize all in-progress exits and their totals",
		Long: "Lists every in-progress unilateral exit and the " +
			"aggregate totals across them: the amount still " +
			"being recovered, the estimated on-chain fees, and " +
			"the estimated net recoverable. Completed and " +
			"failed exits are omitted; they have no amount " +
			"left to recover.\n\n" +
			"Example:\n" +
			"  darepocli exit summary",
		Args: cobra.NoArgs,
		RunE: walletExitSummary,
	}

	return cmd
}

// walletExitSummary implements the `exit summary` subcommand.
func walletExitSummary(cmd *cobra.Command, _ []string) error {
	return withWalletClient(
		cmd, func(c walletdkrpc.WalletServiceClient) error {
			resp, err := c.ExitSummary(
				cmd.Context(),
				&walletdkrpc.ExitSummaryRequest{},
			)
			if err != nil {
				return fmt.Errorf("exit summary: %w", err)
			}

			return printWalletProto(resp)
		},
	)
}

// newExitPlanCmd builds the `exit plan` subcommand. It dials
// walletdkrpc.WalletService.GetExitPlan to preview the backing-wallet funding
// readiness for one or more VTXO outpoints without dispatching an exit.
func newExitPlanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Preview backing-wallet funding readiness for an exit",
		Long: "Returns the combined backing-wallet unroll funding " +
			"plan for one or more VTXO outpoints, including the " +
			"required fee inputs, recommended funding amounts, " +
			"and aggregate shortfall. A funding address is only " +
			"allocated for outpoints with a shortfall.\n\n" +
			"Example:\n" +
			"  darepocli exit plan --outpoint TXID:VOUT\n" +
			"  darepocli exit plan --outpoint TXID:VOUT " +
			"--outpoint TXID:VOUT",
		Args: cobra.NoArgs,
		RunE: walletExitPlan,
	}

	cmd.Flags().StringArray("outpoint", nil,
		"VTXO outpoint to preview (txid:vout); repeatable")
	_ = cmd.MarkFlagRequired("outpoint")

	return cmd
}

// walletExitPlan implements the `exit plan` subcommand.
func walletExitPlan(cmd *cobra.Command, _ []string) error {
	outpoints, _ := cmd.Flags().GetStringArray("outpoint")
	if len(outpoints) == 0 {
		return invalidArgs(
			fmt.Errorf("at least one --outpoint is required"),
		)
	}
	for _, outpoint := range outpoints {
		if err := invalidArgs(validateOutpoint(outpoint)); err != nil {
			return err
		}
	}

	return withWalletClient(
		cmd, func(c walletdkrpc.WalletServiceClient) error {
			resp, err := c.GetExitPlan(
				cmd.Context(),
				&walletdkrpc.GetExitPlanRequest{
					Outpoints: outpoints,
				},
			)
			if err != nil {
				return fmt.Errorf("exit plan: %w", err)
			}

			return printWalletProto(resp)
		},
	)
}

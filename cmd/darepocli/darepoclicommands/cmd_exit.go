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

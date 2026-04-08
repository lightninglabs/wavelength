package darepoclicommands

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/spf13/cobra"
)

// newUnrollCmd creates the unroll subcommand that triggers a unilateral
// exit for a specified VTXO outpoint.
func newUnrollCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unroll",
		Short: "Trigger a unilateral exit for a VTXO",
		Long: "Starts the on-chain recovery process for the " +
			"specified VTXO outpoint. The daemon assembles " +
			"a recovery proof, spawns a durable unroll job, " +
			"and drives the exit to completion.",
		RunE: unroll,
	}

	cmd.Flags().String("outpoint", "",
		"VTXO outpoint to unroll (txid:index)")
	_ = cmd.MarkFlagRequired("outpoint")

	cmd.AddCommand(newUnrollStatusCmd())

	return cmd
}

// unroll executes the Unroll RPC and prints the result.
func unroll(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	outpoint, err := cmd.Flags().GetString("outpoint")
	if err != nil {
		return err
	}

	resp, err := client.Unroll(
		context.Background(), &daemonrpc.UnrollRequest{
			Outpoint: outpoint,
		},
	)
	if err != nil {
		return fmt.Errorf("unroll RPC failed: %w", err)
	}

	return printJSON(resp)
}

// newUnrollStatusCmd creates the "unroll status" subcommand that
// queries the status of an active unroll job.
func newUnrollStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Query the status of an unroll job",
		Long: "Returns the current status of a unilateral exit " +
			"job for the specified VTXO outpoint, including " +
			"the job phase, sweep txid, and any errors.",
		RunE: unrollStatus,
	}

	cmd.Flags().String("outpoint", "",
		"VTXO outpoint to query (txid:index)")
	_ = cmd.MarkFlagRequired("outpoint")

	return cmd
}

// unrollStatus executes the GetUnrollStatus RPC and prints the result.
func unrollStatus(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	outpoint, err := cmd.Flags().GetString("outpoint")
	if err != nil {
		return err
	}

	resp, err := client.GetUnrollStatus(
		context.Background(), &daemonrpc.GetUnrollStatusRequest{
			Outpoint: outpoint,
		},
	)
	if err != nil {
		return fmt.Errorf("unroll status RPC failed: %w", err)
	}

	return printJSON(resp)
}

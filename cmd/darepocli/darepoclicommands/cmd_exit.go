package darepoclicommands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/spf13/cobra"
)

// newWalletFundingAddressCmd creates the wallet funding-address
// subcommand for generating a plain on-chain P2TR address.
func newWalletFundingAddressCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "funding-address",
		Short: "Generate a plain on-chain P2TR address for fee funding",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, conn, err := getDaemonClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()

			resp, err := client.FundingAddress(
				context.Background(),
				&daemonrpc.FundingAddressRequest{},
			)
			if err != nil {
				return fmt.Errorf(
					"FundingAddress: %w", err,
				)
			}

			out, err := json.MarshalIndent(resp, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, string(out))

			return nil
		},
	}
}

// newWalletExitCmd creates the wallet exit subcommand for
// unilaterally exiting VTXOs back on-chain.
func newWalletExitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exit [outpoint...]",
		Short: "Unilaterally exit VTXOs on-chain",
		Long: "Initiates a unilateral exit for the " +
			"specified VTXOs, sweeping them back " +
			"on-chain. Outpoints are specified as " +
			"txid:index and can be passed as " +
			"positional arguments or via the " +
			"--outpoints flag (comma-separated).",
		RunE: walletExit,
	}

	cmd.Flags().StringSlice("outpoints", nil,
		"comma-separated outpoints (txid:index)")

	return cmd
}

// walletExit executes the ExitVTXO RPC and prints the result.
func walletExit(cmd *cobra.Command, args []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Collect outpoints from both --outpoints flag and
	// positional arguments.
	flagOutpoints, _ := cmd.Flags().GetStringSlice(
		"outpoints",
	)

	var outpoints []string
	outpoints = append(outpoints, flagOutpoints...)
	outpoints = append(outpoints, args...)

	if len(outpoints) == 0 {
		return fmt.Errorf(
			"at least one outpoint is required " +
				"(positional or --outpoints)")
	}

	// Validate outpoint format (txid:index).
	for _, op := range outpoints {
		parts := strings.SplitN(op, ":", 2)
		if len(parts) != 2 || parts[0] == "" ||
			parts[1] == "" {

			return fmt.Errorf(
				"invalid outpoint format "+
					"%q: expected txid:index",
				op)
		}
	}

	resp, err := client.ExitVTXO(
		context.Background(),
		&daemonrpc.ExitVTXORequest{
			Outpoints: outpoints,
		},
	)
	if err != nil {
		return fmt.Errorf("ExitVTXO RPC failed: %w", err)
	}

	out, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}

	fmt.Fprintln(os.Stdout, string(out))

	return nil
}

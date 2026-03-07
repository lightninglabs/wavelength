package main

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/spf13/cobra"
)

// newSendCmd creates the send parent command with subcommands for
// in-round and out-of-round transfers.
func newSendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "send",
		Short: "Send operations",
		Long: "Transfer VTXOs via in-round refresh or " +
			"out-of-round direct transfer.",
	}

	cmd.AddCommand(
		newSendInRoundCmd(),
		newSendOORCmd(),
	)

	return cmd
}

// newSendInRoundCmd creates the send inround subcommand.
func newSendInRoundCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inround",
		Short: "Send via in-round refresh",
		Long: "Initiates an in-round transfer by " +
			"submitting a refresh request to the " +
			"round coordinator. The transfer " +
			"completes when the next round commits.",
		RunE: sendInRound,
	}

	cmd.Flags().StringSlice("to", nil,
		"recipient address(es)")

	cmd.Flags().Int64Slice("amount", nil,
		"amount(s) in sats (one per --to)")

	cmd.Flags().Bool("dry-run", false,
		"validate without submitting")

	return cmd
}

// sendInRound executes the SendVTXO RPC.
func sendInRound(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &daemonrpc.SendVTXORequest{}
	if err := parseRequest(cmd, req, func() error {
		addresses, _ := cmd.Flags().GetStringSlice("to")
		amounts, _ := cmd.Flags().GetInt64Slice("amount")
		dryRun, _ := cmd.Flags().GetBool("dry-run")

		if len(addresses) == 0 {
			return fmt.Errorf(
				"at least one --to is required")
		}

		if len(addresses) != len(amounts) {
			return fmt.Errorf(
				"number of --to (%d) and "+
					"--amount (%d) flags "+
					"must match",
				len(addresses), len(amounts))
		}

		// Build the recipient list.
		recipients := make(
			[]*daemonrpc.Output, 0, len(addresses),
		)
		for i := range addresses {
			if amounts[i] <= 0 {
				return fmt.Errorf(
					"amount for recipient "+
						"%d must be positive",
					i)
			}

			recipients = append(
				recipients, &daemonrpc.Output{
					Address:   addresses[i],
					AmountSat: amounts[i],
				},
			)
		}

		req.Recipients = recipients
		req.DryRun = dryRun

		return nil
	}); err != nil {
		return err
	}

	resp, err := client.SendVTXO(
		context.Background(), req,
	)
	if err != nil {
		return fmt.Errorf("SendVTXO RPC failed: %w", err)
	}

	return printJSON(resp)
}

// newSendOORCmd creates the send oor subcommand.
func newSendOORCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "oor",
		Short: "Send via out-of-round transfer",
		Long: "Initiates an out-of-round transfer " +
			"directly between the client and " +
			"operator, without waiting for a round.",
		RunE: sendOOR,
	}

	cmd.Flags().String("to", "",
		"recipient address")

	cmd.Flags().Int64("amount", 0,
		"amount in sats")

	cmd.Flags().Bool("dry-run", false,
		"validate without initiating")

	return cmd
}

// sendOOR executes the SendOOR RPC.
func sendOOR(cmd *cobra.Command, _ []string) error {
	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := &daemonrpc.SendOORRequest{}
	if err := parseRequest(cmd, req, func() error {
		address, _ := cmd.Flags().GetString("to")
		amount, _ := cmd.Flags().GetInt64("amount")
		dryRun, _ := cmd.Flags().GetBool("dry-run")

		if address == "" {
			return fmt.Errorf("--to is required")
		}

		if amount <= 0 {
			return fmt.Errorf(
				"--amount must be positive")
		}

		req.Address = address
		req.AmountSat = amount
		req.DryRun = dryRun

		return nil
	}); err != nil {
		return err
	}

	resp, err := client.SendOOR(
		context.Background(), req,
	)
	if err != nil {
		return fmt.Errorf("SendOOR RPC failed: %w", err)
	}

	return printJSON(resp)
}

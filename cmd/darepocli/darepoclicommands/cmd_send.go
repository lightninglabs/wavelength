package darepoclicommands

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

	cmd.Flags().Bool("dry_run", false,
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
		dryRun, _ := cmd.Flags().GetBool("dry_run")

		if len(addresses) == 0 {
			return fmt.Errorf("at least one --to is required")
		}

		if len(addresses) != len(amounts) {
			return fmt.Errorf("number of --to (%d) and --amount "+
				"(%d) flags must match", len(addresses),
				len(amounts))
		}

		// Build the recipient list.
		recipients := make(
			[]*daemonrpc.Output, 0, len(addresses),
		)
		for i := range addresses {
			if amounts[i] <= 0 {
				return fmt.Errorf("amount for recipient %d "+
					"must be positive", i)
			}

			recipients = append(
				recipients, &daemonrpc.Output{
					Destination: &daemonrpc.Output_Address{
						Address: addresses[i],
					},
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
		// Map well-known server-side fee rejections to a
		// concise CLI message. Fall through to the generic
		// error wrap if the cause is not a fee rejection.
		if feeErr := mapFeeError(err); feeErr != nil {
			return feeErr
		}

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

	cmd.Flags().String("pubkey", "",
		"recipient 32-byte x-only pubkey hex")

	cmd.Flags().Int64("amount", 0,
		"amount in sats")

	cmd.Flags().Bool("dry_run", false,
		"validate without initiating")
	cmd.Flags().String("idempotency_key", "",
		"caller-provided key for retrying the same OOR send intent")

	cmd.MarkFlagsMutuallyExclusive(
		"to", "pubkey",
	)

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
		pubKeyHex, _ := cmd.Flags().GetString("pubkey")
		amount, _ := cmd.Flags().GetInt64("amount")
		dryRun, _ := cmd.Flags().GetBool("dry_run")
		idempotencyKey, _ := cmd.Flags().GetString("idempotency_key")

		recipient, err := buildOORRecipientOutput(
			address, pubKeyHex, amount,
		)
		if err != nil {
			return err
		}

		req.Recipient = recipient
		req.DryRun = dryRun
		req.IdempotencyKey = idempotencyKey

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

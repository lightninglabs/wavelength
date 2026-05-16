package darepoclicommands

import (
	"fmt"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/spf13/cobra"
)

// newArkSendCmd builds the `ark send` parent command that re-hosts the
// legacy in-round and out-of-round transfer subcommands. The everyday
// top-level `send` verb composes the same paths via walletrpc; this
// parent is the power-user knob for callers who want to drive the
// underlying daemonrpc methods directly.
func newArkSendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "send",
		Short: "Raw in-round / OOR send (advanced)",
		Long: "Power-user transfer subcommands. The everyday " +
			"`darepocli send` verb composes the same paths via " +
			"walletrpc; this parent surfaces the raw daemonrpc " +
			"methods for callers who need them.",
	}

	cmd.AddCommand(
		newArkSendInRoundCmd(),
		newArkSendOORCmd(),
	)

	return cmd
}

// newArkSendInRoundCmd creates the `ark send inround` subcommand. This
// is the raw daemonrpc.SendVTXO path; the top-level `send` verb
// composes the same logic via walletrpc.WalletService.Send.
func newArkSendInRoundCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inround",
		Short: "Send via in-round refresh",
		Long: "Initiates an in-round transfer by submitting a " +
			"refresh request to the round coordinator. The " +
			"transfer completes when the next round commits.",
		RunE: sendInRound,
	}

	cmd.Flags().StringSlice("to", nil, "recipient address(es)")
	cmd.Flags().Int64Slice("amount", nil,
		"amount(s) in sats (one per --to)")
	cmd.Flags().Bool("dry_run", false, "validate without submitting")

	return cmd
}

// sendInRound executes the raw SendVTXO RPC.
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

	resp, err := client.SendVTXO(cmd.Context(), req)
	if err != nil {
		if feeErr := mapFeeError(err); feeErr != nil {
			return feeErr
		}

		return fmt.Errorf("SendVTXO RPC failed: %w", err)
	}

	return printJSON(resp)
}

// newArkSendOORCmd creates the `ark send oor` subcommand. Raw
// daemonrpc.SendOOR path.
func newArkSendOORCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "oor",
		Short: "Send via out-of-round transfer",
		Long: "Initiates an out-of-round transfer directly between " +
			"the client and operator, without waiting for a round.",
		RunE: sendOOR,
	}

	cmd.Flags().String("to", "", "recipient address")
	cmd.Flags().String("pubkey", "",
		"recipient 32-byte x-only pubkey hex")
	cmd.Flags().Int64("amount", 0, "amount in sats")
	cmd.Flags().Bool("dry_run", false, "validate without initiating")
	cmd.Flags().String("idempotency_key", "",
		"caller-provided key for retrying the same OOR send intent")

	cmd.MarkFlagsMutuallyExclusive("to", "pubkey")

	return cmd
}

// sendOOR executes the raw SendOOR RPC.
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

	resp, err := client.SendOOR(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("SendOOR RPC failed: %w", err)
	}

	return printJSON(resp)
}

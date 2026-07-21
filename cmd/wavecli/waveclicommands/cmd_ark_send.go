package waveclicommands

import (
	"fmt"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
)

// newArkSendCmd builds the `ark send` parent command that re-hosts the
// legacy in-round and out-of-round transfer subcommands. The everyday
// top-level `send` verb composes the same paths via wavewalletrpc; this
// parent is the power-user knob for callers who want to drive the
// underlying waverpc methods directly.
func newArkSendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "send",
		Short: "Raw in-round / OOR send (advanced)",
		Long: "Power-user transfer subcommands. The everyday " +
			"`wavecli send` verb composes the same paths via " +
			"wavewalletrpc; this parent surfaces the raw waverpc " +
			"methods for callers who need them.",
	}

	cmd.AddCommand(
		newArkSendInRoundCmd(),
		newArkSendOORCmd(),
	)

	return cmd
}

// newArkSendInRoundCmd creates the `ark send inround` subcommand. This
// is the raw waverpc.SendVTXO path; the top-level `send` verb
// composes the same logic via wavewalletrpc.WalletService.Send.
//
// Supports both `--to <bech32m>` and `--pubkey <hex>` parallel
// destinations. The Output proto already has both `address` and
// `pubkey` oneof slots; without a CLI flag for `--pubkey`, an
// in-round send to a peer who hasn't published a bech32m VTXO
// address required hand-encoding the recipient's pkScript into
// bech32m yourself. Mirrors the shape of `ark send oor`, which
// has had `--pubkey` from the start.
func newArkSendInRoundCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inround",
		Short: "Send via in-round refresh",
		Long: "Initiates an in-round transfer by submitting a " +
			"refresh request to the round coordinator. The " +
			"transfer completes when the next round commits.\n\n" +
			"Pass --to for bech32m addresses, --pubkey for " +
			"recipient x-only pubkeys, or both. Recipients are " +
			"paired with --amount positionally in the order " +
			"they appear on the command line, with --to " +
			"entries before --pubkey entries.",
		RunE: sendInRound,
	}

	cmd.Flags().StringSlice("to", nil,
		"recipient bech32m taproot address(es)")
	cmd.Flags().StringSlice("pubkey", nil,
		"recipient 32-byte x-only pubkey hex (one per --amount, "+
			"paired after any --to entries)")
	cmd.Flags().Int64Slice("amount", nil,
		"amount(s) in sats (one per recipient, in --to + --pubkey "+
			"order)")
	cmd.Flags().Bool("dry_run", false, "validate without submitting")
	cmd.Flags().Bool("yes", false,
		"approve submitting the fund-moving transfer")

	return cmd
}

// sendInRound executes the raw SendVTXO RPC.
func sendInRound(cmd *cobra.Command, _ []string) error {
	req := &waverpc.SendVTXORequest{}
	if err := parseRequest(cmd, req, func() error {
		addresses, _ := cmd.Flags().GetStringSlice("to")
		pubkeyHexes, _ := cmd.Flags().GetStringSlice("pubkey")
		amounts, _ := cmd.Flags().GetInt64Slice("amount")
		dryRun, _ := cmd.Flags().GetBool("dry_run")

		if len(addresses)+len(pubkeyHexes) == 0 {
			return fmt.Errorf("at least one --to or --pubkey is " +
				"required")
		}

		total := len(addresses) + len(pubkeyHexes)
		if total != len(amounts) {
			return fmt.Errorf("number of recipients (%d via --to "+
				"+ %d via --pubkey = %d) and --amount (%d) "+
				"flags must match", len(addresses),
				len(pubkeyHexes), total, len(amounts))
		}

		recipients := make([]*waverpc.Output, 0, total)

		// --to entries come first, then --pubkey entries. The
		// composite list pairs index-by-index with --amount.
		for i, addr := range addresses {
			amount := amounts[i]
			if amount <= 0 {
				return fmt.Errorf("amount for recipient %d "+
					"(--to %q) must be positive", i, addr)
			}

			recipients = append(
				recipients, &waverpc.Output{
					Destination: &waverpc.Output_Address{
						Address: addr,
					},
					AmountSat: amount,
				},
			)
		}

		for i, hexStr := range pubkeyHexes {
			pos := len(addresses) + i
			amount := amounts[pos]
			if amount <= 0 {
				return fmt.Errorf("amount for recipient %d "+
					"(--pubkey %q) must be positive", pos,
					hexStr)
			}

			pubkey, err := parseOORPubKeyHex(hexStr)
			if err != nil {
				return fmt.Errorf("recipient %d --pubkey: %w",
					pos, err)
			}

			recipients = append(
				recipients, &waverpc.Output{
					Destination: &waverpc.Output_Pubkey{
						Pubkey: pubkey,
					},
					AmountSat: amount,
				},
			)
		}

		req.Recipients = recipients
		req.DryRun = dryRun

		return nil
	}); err != nil {
		return err
	}
	if !req.GetDryRun() {
		action := fmt.Sprintf("submit an in-round transfer to %d "+
			"recipient(s)", len(req.GetRecipients()))
		if err := confirmMoneyMovement(cmd, action); err != nil {
			return err
		}
	}

	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

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
// waverpc.SendOOR path.
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
	cmd.Flags().Bool("yes", false,
		"approve initiating the fund-moving transfer")
	cmd.Flags().String("idempotency_key", "",
		"caller-provided key for retrying the same OOR send intent")

	cmd.MarkFlagsMutuallyExclusive("to", "pubkey")

	return cmd
}

// sendOOR executes the raw SendOOR RPC.
func sendOOR(cmd *cobra.Command, _ []string) error {
	req := &waverpc.SendOORRequest{}
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

		req.Recipients = []*waverpc.Output{recipient}
		req.DryRun = dryRun
		req.IdempotencyKey = idempotencyKey

		return nil
	}); err != nil {
		return err
	}
	if !req.GetDryRun() {
		action := fmt.Sprintf("initiate an out-of-round transfer of "+
			"%d satoshis", req.GetRecipients()[0].GetAmountSat())
		if err := confirmMoneyMovement(cmd, action); err != nil {
			return err
		}
	}

	client, conn, err := getDaemonClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	resp, err := client.SendOOR(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("SendOOR RPC failed: %w", err)
	}

	return printJSON(resp)
}

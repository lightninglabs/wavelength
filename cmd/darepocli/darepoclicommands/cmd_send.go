package darepoclicommands

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/spf13/cobra"
)

// newSendCmd builds the top-level `send` verb. It dispatches an
// outbound payment via walletdkrpc.WalletService.Send. Direction is
// chosen explicitly with --offchain (default) or --onchain: the CLI
// does NOT sniff the destination string, so an agent cannot
// accidentally dispatch an onchain send by passing what it thinks is an
// invoice.
//
// The daemon does the authoritative destination parse and returns
// InvalidArgument on type mismatch.
func newSendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "send <invoice-or-onchain-address>",
		Short: "Send a payment (offchain Lightning invoice or onchain)",
		Long: "Dispatches an outbound payment. With --offchain " +
			"(default) the destination is treated as a BOLT-11 " +
			"Lightning invoice and routed through the swap " +
			"subsystem (which transparently picks same-Ark p2p " +
			"vs real Lightning). With --onchain the destination " +
			"is a bech32 onchain address and the daemon submits " +
			"a cooperative-leave (LeaveVTXOs) request.\n\n" +
			"Onchain v1 has whole-VTXO sweep semantics: the " +
			"actual outflow (echoed on stderr and in " +
			"actual_amount_sat) may exceed --amt because " +
			"selected VTXOs are swept in full. Set --sweep-all " +
			"with --amt=0 to drain every live VTXO.\n\n" +
			"Examples:\n" +
			"  darepocli send lnbcrt... --offchain\n" +
			"  darepocli send bcrt1... --onchain --amt 1000\n" +
			"  darepocli send bcrt1... --onchain --sweep-all",
		Args: cobra.ExactArgs(1),
		RunE: walletSend,
	}

	cmd.Flags().Bool("offchain", false,
		"force offchain (BOLT-11 invoice) dispatch; default when "+
			"neither --offchain nor --onchain is set")
	cmd.Flags().Bool("onchain", false,
		"force onchain (cooperative leave) dispatch")
	cmd.Flags().Uint64("amt", 0,
		"amount in satoshis (required for onchain unless "+
			"--sweep-all; ignored for amount-bearing invoices)")
	cmd.Flags().Uint64("max_fee", 0,
		"max fee in satoshis; 0 lets the daemon use defaults")
	cmd.Flags().String("note", "",
		"caller-supplied label to attach to the entry")
	cmd.Flags().Bool("sweep-all", false,
		"onchain only: drain the wallet to the destination. "+
			"--amt MUST be 0 when set.")
	cmd.Flags().Bool("dry-run", false,
		"prepare and print the preview without dispatching funds")
	cmd.Flags().Bool("force", false,
		"skip interactive confirmation after prepare")
	cmd.Flags().Bool("yes", false,
		"alias for --force")

	return cmd
}

// walletSend implements the top-level `send` verb.
func walletSend(cmd *cobra.Command, args []string) error {
	dest := args[0]
	if err := invalidArgs(validateDestination(dest)); err != nil {
		return err
	}

	offchain, err := resolveOffchainFlag(cmd)
	if err != nil {
		return invalidArgs(err)
	}

	amt, _ := cmd.Flags().GetUint64("amt")
	maxFee, _ := cmd.Flags().GetUint64("max_fee")
	note, _ := cmd.Flags().GetString("note")
	sweepAll, _ := cmd.Flags().GetBool("sweep-all")

	if err := invalidArgs(validateFreeText("--note", note)); err != nil {
		return err
	}

	// --sweep-all only makes sense on the onchain path. Reject it
	// up front on the offchain path so the daemon never sees a
	// silently-ignored flag and so a typo (forgot --onchain) gets
	// a clear error rather than a no-op invoice send.
	if offchain && sweepAll {
		return PrintError(
			"INVALID_ARGS", "--sweep-all is only valid with "+
				"--onchain (invoice sends drain no VTXO set)",
		)
	}

	// Onchain-only invariants: --sweep-all <=> amt==0. Enforce up
	// front so a typo'd zero never lands on the wallet RPC.
	if !offchain {
		switch {
		case sweepAll && amt != 0:
			return PrintError(
				"INVALID_ARGS", "--sweep-all requires "+
					"--amt=0 (amt is implied by "+
					"sweeping every live VTXO)",
			)

		case !sweepAll && amt == 0:
			return PrintError(
				"INVALID_ARGS", "--amt is required for "+
					"onchain sends (use --sweep-all to "+
					"drain the wallet)",
			)
		}
	}

	req := &walletdkrpc.PrepareSendRequest{
		AmtSat:    amt,
		MaxFeeSat: maxFee,
		Note:      note,
		SweepAll:  sweepAll,
	}
	if offchain {
		req.Destination = &walletdkrpc.PrepareSendRequest_Invoice{
			Invoice: dest,
		}
	} else {
		req.Destination = &walletdkrpc.
			PrepareSendRequest_OnchainAddress{
			OnchainAddress: dest,
		}
	}

	return withWalletClient(
		cmd, func(c walletdkrpc.WalletServiceClient) error {
			prepareResp, err := c.PrepareSend(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("prepare send: %w", err)
			}

			if dryRun, _ := cmd.Flags().GetBool("dry-run"); dryRun {
				if err := printWalletProto(
					prepareResp,
				); err != nil {
					return err
				}

				return PrintError(
					"DRY_RUN_OK", "dry-run validation "+
						"passed; no funds were "+
						"dispatched",
				)
			}

			err = confirmSendIfNeeded(cmd, prepareResp)
			if err != nil {
				return err
			}

			intentID := prepareResp.GetSendIntentId()
			resp, err := c.Send(
				cmd.Context(), &walletdkrpc.SendRequest{
					SendIntentId: intentID,
				},
			)
			if err != nil {
				return fmt.Errorf("send prepared intent: %w",
					err)
			}

			// For onchain sends actual_amount_sat may exceed
			// --amt under the v1 whole-VTXO sweep semantics.
			// Surface it on stderr so a human reading the
			// terminal sees the real outflow while shell
			// pipelines can still consume the JSON body.
			actual := resp.GetActualAmountSat()
			if !offchain && !sweepAll && actual != int64(amt) {
				fmt.Fprintf(
					cmd.ErrOrStderr(),
					"note: actual_amount_sat=%d exceeds "+
						"--amt=%d due to whole-VTXO "+
						"sweep semantics\n", actual,
					amt,
				)
			}

			return printWalletProto(resp)
		},
	)
}

func confirmSendIfNeeded(cmd *cobra.Command,
	resp *walletdkrpc.PrepareSendResponse) error {

	force, _ := cmd.Flags().GetBool("force")
	yes, _ := cmd.Flags().GetBool("yes")
	if force || yes {
		return nil
	}

	if !stdinIsTTY(cmd) {
		return PrintError(
			"INVALID_ARGS", "send requires --force or --yes on "+
				"non-interactive stdin; refusing to prompt "+
				"because an agent cannot respond to y/N",
		)
	}

	return promptSendConfirmation(cmd, resp)
}

func promptSendConfirmation(cmd *cobra.Command,
	resp *walletdkrpc.PrepareSendResponse) error {

	out := cmd.ErrOrStderr()

	fmt.Fprintf(out, "Send %d sats\n", sendPreviewHeadlineAmount(resp))
	fmt.Fprintf(out, "Rail: %s\n", sendRailLabel(resp.GetRail()))
	if resp.GetFeeKnown() {
		fmt.Fprintf(
			out, "Expected fee: %d sats\n",
			resp.GetExpectedFeeSat(),
		)
	} else {
		fmt.Fprintln(out, "Expected fee: unknown")
	}
	if resp.GetTotalOutflowKnown() {
		fmt.Fprintf(
			out, "Expected total outflow: %d sats\n",
			resp.GetExpectedTotalOutflowSat(),
		)
	} else {
		fmt.Fprintln(out, "Expected total outflow: unknown")
	}
	if dest := resp.GetDestinationSummary(); dest != "" {
		fmt.Fprintf(out, "Destination: %s\n", dest)
	}
	if desc := resp.GetInvoiceDescription(); desc != "" {
		fmt.Fprintf(out, "Invoice: %s\n", desc)
	}
	if hash := resp.GetPaymentHash(); hash != "" {
		fmt.Fprintf(out, "Payment hash: %s\n", hash)
	}
	if warning := resp.GetWarning(); warning != "" {
		fmt.Fprintf(out, "Warning: %s\n", warning)
	}
	fmt.Fprint(out, "\nProceed? [y/N]: ")

	reader := bufio.NewReader(cmd.InOrStdin())
	answer, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read confirmation: %w", err)
	}

	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" && answer != "yes" {
		return fmt.Errorf("aborted by user")
	}

	return nil
}

func sendPreviewHeadlineAmount(resp *walletdkrpc.PrepareSendResponse) int64 {
	if resp == nil {
		return 0
	}

	amountSat := resp.GetAmountSat()
	if amountSat != 0 {
		return amountSat
	}

	if resp.GetRail() != walletdkrpc.SendRail_SEND_RAIL_ONCHAIN {
		return amountSat
	}
	if !resp.GetTotalOutflowKnown() {
		return amountSat
	}

	return resp.GetExpectedTotalOutflowSat()
}

func sendRailLabel(rail walletdkrpc.SendRail) string {
	switch rail {
	case walletdkrpc.SendRail_SEND_RAIL_IN_ARK:
		return "in-Ark"

	case walletdkrpc.SendRail_SEND_RAIL_LIGHTNING:
		return "Lightning"

	case walletdkrpc.SendRail_SEND_RAIL_ONCHAIN:
		return "onchain"

	case walletdkrpc.SendRail_SEND_RAIL_OFFCHAIN_UNKNOWN:
		return "offchain"

	default:
		return "unknown"
	}
}

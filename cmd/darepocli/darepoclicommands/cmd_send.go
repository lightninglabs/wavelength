package darepoclicommands

import (
	"fmt"

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
			"a cooperative-leave (LeaveVTXOs) request. Add " +
			"--fromonchain to spend the backing Bitcoin wallet " +
			"instead of Ark VTXOs.\n\n" +
			"Ark onchain v1 has whole-VTXO sweep semantics: the " +
			"actual outflow (echoed on stderr and in " +
			"actual_amount_sat) may exceed --amt because " +
			"selected VTXOs are swept in full. Set --sweep-all " +
			"with --amt=0 to drain every live VTXO.\n\n" +
			"Examples:\n" +
			"  darepocli send lnbcrt... --offchain\n" +
			"  darepocli send bcrt1... --onchain --amt 1000\n" +
			"  darepocli send bcrt1... --onchain --sweep-all\n" +
			"  darepocli send bcrt1... --onchain --fromonchain " +
			"--amt 1000",
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
	cmd.Flags().Bool("fromonchain", false,
		"onchain only: spend the backing Bitcoin wallet instead "+
			"of Ark VTXOs")
	cmd.Flags().Bool("dry-run", false,
		"validate inputs locally and print the preview without "+
			"dispatching to the daemon; exits 10 on a valid "+
			"preview, non-zero on validation failure")

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
	fromOnchain, _ := cmd.Flags().GetBool("fromonchain")

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
	if offchain && fromOnchain {
		return PrintError(
			"INVALID_ARGS",
			"--fromonchain is only valid with --onchain",
		)
	}
	if fromOnchain && sweepAll {
		return PrintError(
			"INVALID_ARGS", "--fromonchain requires an "+
				"explicit --amt; --sweep-all drains Ark VTXOs",
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

	req := &walletdkrpc.SendRequest{
		AmtSat:      amt,
		MaxFeeSat:   maxFee,
		Note:        note,
		SweepAll:    sweepAll,
		FromOnchain: fromOnchain,
	}
	if offchain {
		req.Destination = &walletdkrpc.SendRequest_Invoice{
			Invoice: dest,
		}
	} else {
		req.Destination = &walletdkrpc.SendRequest_OnchainAddress{
			OnchainAddress: dest,
		}
	}

	// --dry-run validates every invariant we just enforced and prints
	// the proto-JSON preview without dispatching. Returning a code-10
	// printedError lets main.go signal "dry-run passed" distinctly
	// from a real send so an agent can stage a payment without
	// risking a duplicate dispatch.
	if dryRun, _ := cmd.Flags().GetBool("dry-run"); dryRun {
		return walletDryRunPreview(
			"walletdkrpc.WalletService/Send", req,
		)
	}

	return withWalletClient(
		cmd, func(c walletdkrpc.WalletServiceClient) error {
			resp, err := c.Send(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("send: %w", err)
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

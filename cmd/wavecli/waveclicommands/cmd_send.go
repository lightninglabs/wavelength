package waveclicommands

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/spf13/cobra"
)

const (
	// defaultSendWaitTimeout bounds how long `send` blocks on settlement
	// before returning the last observed status. A pay swap that has to
	// route over real Lightning can take a while, so the ceiling is
	// generous.
	defaultSendWaitTimeout = 5 * time.Minute

	// defaultSendWaitPollInterval is how often `send` re-inspects the
	// dispatched entry while it is still pending. It is kept tight so a
	// fast same-Ark p2p settlement is reported with minimal latency.
	defaultSendWaitPollInterval = 200 * time.Millisecond

	entryStatusUnspecified = wavewalletrpc.
				EntryStatus_ENTRY_STATUS_UNSPECIFIED
	entryStatusPending  = wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING
	entryStatusComplete = wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE
	entryStatusFailed   = wavewalletrpc.EntryStatus_ENTRY_STATUS_FAILED
)

// newSendCmd builds the top-level `send` verb. It dispatches an
// outbound payment via wavewalletrpc.WalletService.Send. Direction is
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
			"an atomic onchain-send (SendOnChain) request that " +
			"lands exactly --amt sats at the destination and " +
			"returns the residual to the wallet as a change " +
			"VTXO. Use --sweep-all with --amt=0 to drain every " +
			"live VTXO instead.\n\n" +
			"Examples:\n" +
			"  wavecli send lnbcrt... --offchain\n" +
			"  wavecli send bcrt1... --onchain --amt 1000\n" +
			"  wavecli send bcrt1... --onchain --sweep-all",
		Args: cobra.ExactArgs(1),
		// The retired `swap pay` verb is covered by `send
		// --offchain`; steer stale invocations here.
		SuggestFor: []string{
			"swap",
		},
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
		"max swap fee in satoshis for invoice (Lightning) sends; 0 "+
			"lets the daemon default the cap to ~1% of the amount "+
			"(with a small floor), so a normal payment routes "+
			"without setting this")
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
	cmd.Flags().Bool("no-wait", false,
		"return as soon as the send is dispatched instead of blocking "+
			"until it reaches a terminal state; by default send "+
			"waits and Lightning sends print the payment "+
			"preimage. Onchain sends never block: they settle in "+
			"a cooperative-leave round and always return a "+
			"pending receipt")
	cmd.Flags().Duration("wait-timeout", defaultSendWaitTimeout,
		"while waiting: give up after this long and return the last "+
			"observed status (0 waits indefinitely; ignored with "+
			"--no-wait)")
	cmd.Flags().Duration("wait-poll-interval", defaultSendWaitPollInterval,
		"while waiting: how often to poll the daemon for status "+
			"updates (ignored with --no-wait)")

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

	req := &wavewalletrpc.PrepareSendRequest{
		AmtSat:    amt,
		MaxFeeSat: maxFee,
		Note:      note,
		SweepAll:  sweepAll,
	}
	if offchain {
		req.Destination = &wavewalletrpc.PrepareSendRequest_Invoice{
			Invoice: dest,
		}
	} else {
		req.Destination = &wavewalletrpc.
			PrepareSendRequest_OnchainAddress{
			OnchainAddress: dest,
		}
	}

	return withWalletClient(
		cmd, func(c wavewalletrpc.WalletServiceClient) error {
			prepareCtx, cancelPrepare := rpcContext(cmd)
			prepareResp, err := c.PrepareSend(prepareCtx, req)
			cancelPrepare()
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
			sendCtx, cancelSend := rpcContext(cmd)
			resp, err := c.Send(
				sendCtx, &wavewalletrpc.SendRequest{
					SendIntentId: intentID,
				},
			)
			cancelSend()
			if err != nil {
				return fmt.Errorf("send prepared intent: %w",
					err)
			}

			// An onchain send settles by forfeiting its source
			// VTXO into a cooperative-leave round and waiting for
			// that round to confirm on chain, which can take many
			// minutes. Blocking the CLI on that terminal state is a
			// poor fit: there is no preimage to wait for the way a
			// Lightning send has, and the funds are already
			// committed the moment Send returns. So the onchain
			// rail always returns a pending receipt rather than
			// hanging, regardless of --no-wait.
			noWait, _ := cmd.Flags().GetBool("no-wait")
			onchain := prepareResp.GetRail() ==
				wavewalletrpc.SendRail_SEND_RAIL_ONCHAIN

			if noWait || onchain {
				if onchain {
					printOnchainPendingNotice(
						cmd.ErrOrStderr(),
						resp.GetEntry().GetId(),
					)
				}

				// Without waiting there is no terminal state to
				// summarize yet, so echo the dispatched entry
				// as a compact pending receipt rather than the
				// full proto dump.
				return printSendResult(
					cmd.OutOrStdout(),
					sendResultFromEntry(
						resp.GetEntry(),
					),
				)
			}

			return waitForSendTerminal(cmd, resp.GetEntry())
		},
	)
}

// printOnchainPendingNotice writes a short human-readable explanation to stderr
// after an onchain send is dispatched. The onchain rail never blocks to
// terminal, so this notice sets expectations the compact JSON receipt on stdout
// cannot: the send stays PENDING until its cooperative-leave round confirms,
// and the residual returns as a fresh change VTXO at round seal-time (so a
// mid-flight balance that looks drained by the whole source VTXO is expected,
// not a loss).
func printOnchainPendingNotice(out io.Writer, id string) {
	fmt.Fprintln(
		out, "Onchain send dispatched. It settles in the next "+
			"cooperative-leave round, so it stays PENDING "+
			"until that round confirms on chain; your change "+
			"returns as a new VTXO once the round seals.",
	)
	if id != "" {
		fmt.Fprintf(
			out, "Track it with: wavecli activity inspect %s\n", id,
		)
	}
}

// waitForSendTerminal blocks until the dispatched send entry reaches a terminal
// status (complete or failed), printing each lifecycle phase transition as it
// is observed and rendering the final inspection trace at the end. For a
// Lightning send the trace carries the payment preimage, the proof of payment.
//
// The poll runs over the always-available WalletInspectionService so the wait
// surface does not depend on the optional swapruntime build. A failed terminal
// state returns an error so an agent or shell sees a non-zero exit; a timeout
// emits the latest observed entry as a compact pending receipt on stdout, then
// returns the last observed status without claiming success or failure.
func waitForSendTerminal(cmd *cobra.Command,
	entry *wavewalletrpc.WalletEntry) error {

	id := entry.GetId()
	if id == "" {
		return PrintError(
			"WAIT_UNAVAILABLE", "send did not return an entry "+
				"id to wait on; poll `da activity inspect "+
				"<id>` manually",
		)
	}

	timeout, _ := cmd.Flags().GetDuration("wait-timeout")
	pollInterval, _ := cmd.Flags().GetDuration("wait-poll-interval")
	if pollInterval <= 0 {
		pollInterval = defaultSendWaitPollInterval
	}

	ctx := cmd.Context()
	if timeout > 0 {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	out := cmd.ErrOrStderr()
	lastPhase := ""
	failedMsg := "send reached a failed state"

	// The daemon has already accepted the send by the time we start
	// waiting, so any exit that is not a terminal verdict still owes the
	// caller a machine-readable receipt: emit the latest observed entry as
	// the compact pending summary on stdout before surfacing the error on
	// stderr, so a JSON pipeline keeps the id of a payment that may still
	// settle.
	lastEntry := entry
	emitReceipt := func() {
		_ = printSendResult(
			cmd.OutOrStdout(), sendResultFromEntry(lastEntry),
		)
	}
	timeoutErr := func() error {
		emitReceipt()

		return PrintError(
			"WAIT_TIMEOUT",
			fmt.Sprintf(
				"timed out after %s waiting for %s; last "+
					"phase: %s", timeout, id,
				emptyDash(lastPhase),
			),
		)
	}

	return withWalletInspectionClient(
		cmd, func(c wavewalletrpc.WalletInspectionServiceClient) error {
			ticker := time.NewTicker(pollInterval)
			defer ticker.Stop()

			for {
				req := &wavewalletrpc.InspectActivityRequest{
					Id: id,
				}
				resp, err := c.InspectActivity(ctx, req)
				if err != nil {
					// A deadline hit while polling is a
					// timeout, not a hard failure: report
					// the last phase the user saw.
					if ctx.Err() != nil {
						return timeoutErr()
					}

					// The send itself was accepted, so
					// leave a pending receipt behind even
					// though inspection broke.
					emitReceipt()

					return fmt.Errorf("inspect "+
						"activity: %w", err)
				}

				inspected := resp.GetEntry()
				if inspected != nil {
					lastEntry = inspected
				}
				phase := formatEntryPhase(
					inspected.GetProgress(),
				)
				if phase != lastPhase {
					fmt.Fprintf(out, "phase: %s\n", phase)
					lastPhase = phase
				}

				switch inspected.GetStatus() {
				case entryStatusComplete:
					// On success collapse the verbose trace
					// down to the compact summary; the full
					// breakdown stays available via `da
					// activity inspect <id>`.
					return printSendResult(
						cmd.OutOrStdout(),
						sendResultFromInspection(resp),
					)

				case entryStatusFailed:
					rerr := printWalletInspectionExpanded(
						resp,
					)
					if rerr != nil {
						return rerr
					}

					reason := inspected.GetFailureReason()

					return PrintError(
						"SEND_FAILED", emptyNonEmpty(
							reason, failedMsg,
						),
					)

				case entryStatusUnspecified, entryStatusPending:
				}

				select {
				case <-ctx.Done():
					return timeoutErr()

				case <-ticker.C:
				}
			}
		},
	)
}

// emptyNonEmpty returns value when it is non-empty after trimming, otherwise
// the fallback. It keeps the failure-reason rendering terse without importing a
// second nonEmpty helper into the CLI package.
func emptyNonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}

	return value
}

func confirmSendIfNeeded(cmd *cobra.Command,
	resp *wavewalletrpc.PrepareSendResponse) error {

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
	resp *wavewalletrpc.PrepareSendResponse) error {

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

func sendPreviewHeadlineAmount(resp *wavewalletrpc.PrepareSendResponse) int64 {
	if resp == nil {
		return 0
	}

	amountSat := resp.GetAmountSat()
	if amountSat != 0 {
		return amountSat
	}

	if resp.GetRail() != wavewalletrpc.SendRail_SEND_RAIL_ONCHAIN {
		return amountSat
	}
	if !resp.GetTotalOutflowKnown() {
		return amountSat
	}

	return resp.GetExpectedTotalOutflowSat()
}

func sendRailLabel(rail wavewalletrpc.SendRail) string {
	switch rail {
	case wavewalletrpc.SendRail_SEND_RAIL_IN_ARK:
		return "in-Ark"

	case wavewalletrpc.SendRail_SEND_RAIL_LIGHTNING:
		return "Lightning"

	case wavewalletrpc.SendRail_SEND_RAIL_ONCHAIN:
		return "onchain"

	case wavewalletrpc.SendRail_SEND_RAIL_OFFCHAIN_UNKNOWN:
		return "offchain"

	default:
		return "unknown"
	}
}

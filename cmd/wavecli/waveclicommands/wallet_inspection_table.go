package waveclicommands

import (
	"fmt"
	"io"
	"os"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
)

// printWalletInspectionExpanded writes the expanded inspection view to stdout.
func printWalletInspectionExpanded(
	resp *wavewalletrpc.InspectActivityResponse) error {

	return renderWalletInspectionExpanded(os.Stdout, resp)
}

// renderWalletInspectionExpanded renders one inspection response as a
// markdown-like diagnostic report.
func renderWalletInspectionExpanded(out io.Writer,
	resp *wavewalletrpc.InspectActivityResponse) error {

	if resp == nil {
		return nil
	}

	renderActivitySection(out, resp.GetEntry())
	if swap := resp.GetSwap(); swap != nil {
		renderSwapSection(out, swap)
	}
	renderVTXOTraceSection(out, resp.GetVtxos())
	renderLedgerTraceSection(out, resp.GetLedgerRows())
	renderNotesSection(out, resp.GetNotes())

	return nil
}

// renderActivitySection renders the inspected wallet activity entry.
func renderActivitySection(out io.Writer, entry *wavewalletrpc.WalletEntry) {
	renderActivitySectionWithTitle(out, "Activity", entry)
}

// renderActivitySectionWithTitle renders a wallet activity entry under the
// supplied section title.
func renderActivitySectionWithTitle(out io.Writer, title string,
	entry *wavewalletrpc.WalletEntry) {

	if entry == nil {
		return
	}

	fmt.Fprintln(out, title)
	printBullet(
		out, 0, "last_update",
		formatEntryTime(
			entry.GetUpdatedAtUnix(),
		),
	)
	printBullet(
		out, 0, "created",
		formatEntryTime(
			entry.GetCreatedAtUnix(),
		),
	)
	printBullet(out, 0, "kind", formatEntryKind(entry.GetKind()))
	printBullet(out, 0, "status", formatEntryStatus(entry.GetStatus()))
	printBullet(out, 0, "amount", formatSat(entry.GetAmountSat()))
	printBullet(out, 0, "fee", formatSat(entry.GetFeeSat()))
	printBullet(out, 0, "phase", formatEntryPhase(entry.GetProgress()))
	printBullet(out, 0, "id", emptyDash(entry.GetId()))

	req := entry.GetRequest()
	var requestPaymentHash string
	switch {
	case req.GetLightningInvoice() != nil:
		ln := req.GetLightningInvoice()
		requestPaymentHash = ln.GetPaymentHash()
		printBullet(out, 0, "invoice", emptyDash(ln.GetInvoice()))
		printBullet(
			out, 0, "payment_hash",
			emptyDash(
				ln.GetPaymentHash(),
			),
		)

	case req.GetOnchainAddress() != nil:
		printBullet(
			out, 0, "address",
			emptyDash(
				req.GetOnchainAddress().GetAddress(),
			),
		)

	case req.GetArkAddress() != nil:
		printBullet(
			out, 0, "ark_address",
			emptyDash(
				req.GetArkAddress().GetAddress(),
			),
		)
	}

	progress := entry.GetProgress()
	if progress != nil {
		progressPaymentHash := progress.GetPaymentHash()
		if activityUsesPaymentHash(entry) &&
			progressPaymentHash != requestPaymentHash {

			printOptionalBullet(
				out, "payment_hash", progressPaymentHash,
			)
		}
		printOptionalBullet(
			out, "progress_vtxo", progress.GetVtxoOutpoint(),
		)
		printOptionalBullet(out, "txid", progress.GetTxid())
		if progress.GetConfirmationHeight() != 0 {
			printBullet(
				out, 0, "confirmation_height",
				fmt.Sprintf(
					"%d", progress.GetConfirmationHeight(),
				),
			)
		}
	}

	printOptionalBullet(out, "counterparty", entry.GetCounterparty())
	printOptionalBullet(out, "note", entry.GetNote())
	printOptionalBullet(out, "failure", entry.GetFailureReason())
}

// activityUsesPaymentHash reports whether a payment hash is meaningful for the
// inspected activity kind.
func activityUsesPaymentHash(entry *wavewalletrpc.WalletEntry) bool {
	switch entry.GetKind() {
	case wavewalletrpc.EntryKind_ENTRY_KIND_SEND,
		wavewalletrpc.EntryKind_ENTRY_KIND_RECV:
		return true

	default:
		return false
	}
}

// renderSwapSection renders swap-specific trace details.
func renderSwapSection(out io.Writer, swap *wavewalletrpc.ActivitySwapTrace) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Swap")
	printBullet(out, 0, "payment_hash", emptyDash(swap.GetPaymentHash()))
	printOptionalBullet(out, "preimage", swap.GetPreimage())
	printBullet(out, 0, "direction", emptyDash(swap.GetDirection()))
	printBullet(out, 0, "state", emptyDash(swap.GetState()))
	printBullet(out, 0, "pending", fmt.Sprintf("%t", swap.GetPending()))
	printBullet(out, 0, "amount", formatSat(swap.GetAmountSat()))
	printBullet(out, 0, "fee", fmt.Sprintf("%d sat", swap.GetFeeSat()))
	printOptionalBullet(out, "settlement_type", swap.GetSettlementType())
	printOptionalBullet(out, "sender_pubkey", swap.GetSenderPubkey())
	printOptionalBullet(out, "invoice", swap.GetInvoice())
	printOptionalBullet(out, "vhtlc_outpoint", swap.GetVhtlcOutpoint())
	if swap.GetVhtlcAmountSat() != 0 {
		printBullet(
			out, 0, "vhtlc_amount",
			formatSat(
				swap.GetVhtlcAmountSat(),
			),
		)
	}
	printOptionalBullet(out, "funding_session", swap.GetFundingSessionId())
	printOptionalBullet(out, "claim_session", swap.GetClaimSessionId())
	printOptionalBullet(out, "refund_session", swap.GetRefundSessionId())
	printOptionalBullet(out, "terminal_reason", swap.GetTerminalReason())
}

// renderVTXOTraceSection renders VTXO movement rows correlated to the
// inspected activity.
func renderVTXOTraceSection(out io.Writer,
	rows []*wavewalletrpc.ActivityVTXOTrace) {

	if len(rows) == 0 {
		return
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "VTXOs")
	for _, row := range rows {
		fmt.Fprintf(out, "- %s\n", emptyDash(row.GetRole()))
		printBullet(out, 1, "id", emptyDash(row.GetId()))
		printBullet(out, 1, "amount", formatSat(row.GetAmountSat()))
		printBullet(out, 1, "ours", fmt.Sprintf("%t", row.GetOurs()))
		printBullet(out, 1, "source", emptyDash(row.GetSource()))
		printOptionalIndentedBullet(
			out, 1, "session_id", row.GetSessionId(),
		)
		if row.GetOutputIndex() != 0 {
			printBullet(
				out, 1, "output_index",
				fmt.Sprintf(
					"%d", row.GetOutputIndex(),
				),
			)
		}
	}
}

// renderLedgerTraceSection renders local ledger rows correlated to the
// inspected activity.
func renderLedgerTraceSection(out io.Writer,
	rows []*wavewalletrpc.ActivityLedgerTrace) {

	if len(rows) == 0 {
		return
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Ledger")
	for _, row := range rows {
		fmt.Fprintf(out, "- entry %d\n", row.GetEntryId())
		printBullet(out, 1, "type", emptyDash(row.GetType()))
		printBullet(out, 1, "subtype", emptyDash(row.GetSubtype()))
		printBullet(out, 1, "amount", formatSat(row.GetAmountSat()))
		if row.GetFeeSat() != 0 {
			printBullet(out, 1, "fee", formatSat(row.GetFeeSat()))
		}
		printOptionalIndentedBullet(out, 1, "role", row.GetRole())
		printOptionalIndentedBullet(
			out, 1, "status", row.GetConfirmationStatus(),
		)
		printOptionalIndentedBullet(out, 1, "txid", row.GetTxid())
		if row.GetTxid() != "" && row.GetOutputIndex() >= 0 {
			printBullet(
				out, 1, "output_index",
				fmt.Sprintf(
					"%d", row.GetOutputIndex(),
				),
			)
		}
		printOptionalIndentedBullet(
			out, 1, "session_id", row.GetSessionId(),
		)
		printOptionalIndentedBullet(
			out, 1, "round_id", row.GetRoundId(),
		)
		printOptionalIndentedBullet(
			out, 1, "debit", row.GetDebitAccount(),
		)
		printOptionalIndentedBullet(
			out, 1, "credit", row.GetCreditAccount(),
		)
		if row.GetConfirmationHeight() != 0 {
			printBullet(
				out, 1, "confirmation_height",
				fmt.Sprintf(
					"%d", row.GetConfirmationHeight(),
				),
			)
		}
	}
}

// renderNotesSection renders best-effort correlation caveats.
func renderNotesSection(out io.Writer, notes []string) {
	if len(notes) == 0 {
		return
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Notes")
	for _, note := range notes {
		fmt.Fprintf(out, "- %s\n", note)
	}
}

// printOptionalBullet renders a top-level bullet when the value is non-empty.
func printOptionalBullet(out io.Writer, name, value string) {
	printOptionalIndentedBullet(out, 0, name, value)
}

// printOptionalIndentedBullet renders an indented bullet when the value is
// non-empty.
func printOptionalIndentedBullet(out io.Writer, depth int, name string,
	value string) {

	if value == "" {
		return
	}

	printBullet(out, depth, name, value)
}

// printBullet renders one markdown-like bullet at the requested indentation
// depth.
func printBullet(out io.Writer, depth int, name, value string) {
	indent := ""
	for i := 0; i < depth; i++ {
		indent += "  "
	}

	fmt.Fprintf(out, "%s- %s: %s\n", indent, name, value)
}

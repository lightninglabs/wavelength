//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/lightningnetwork/lnd/zpay32"
)

// truncatedCounterpartyLen caps the rendered counterparty field length so
// the wallet UI never has to deal with a multi-hundred-character invoice
// string. Truncation keeps the start of the bech32 prefix plus a recognisable
// chunk of the body.
const (
	truncatedCounterpartyLen = 32

	// legacyReceiveInvoiceMemo is the historical SDK placeholder used
	// before recv memos were threaded into receive invoices. Do not surface
	// it as a wallet note; note should mean user-provided memo.
	legacyReceiveInvoiceMemo = "swap"
)

// swapEntryFromSummary normalizes a swapclientrpc.SwapSummary into the flat
// WalletEntry shape. The wallet layer collapses every internal swap state
// into PENDING / COMPLETE / FAILED and drops all internal correlators
// (session IDs, settlement type, vHTLC outpoints) so the user surface is
// uniform across SEND, RECV, DEPOSIT, and EXIT rows.
//
// counterparty carries the invoice (truncated) for SEND rows and the
// payment hash (truncated) for RECV rows so callers can correlate a
// generated invoice with the row it produced; note carries the caller's
// label as-is when present.
//
// callerKind lets the dispatcher pin the entry's user-facing direction
// (e.g. router.sendInvoice always produces SEND; recv.go always produces
// RECV) so the amount sign matches the operation the caller asked for
// even when the SDK's summary direction has not yet been populated
// (UNSPECIFIED is a real intermediate state on the first lazy summary).
// Pass UNSPECIFIED to fall back to deriving the kind from
// s.GetDirection(); the monitor loop, which fans every swap regardless
// of direction, uses that fallback.
func swapEntryFromSummary(s *swapclientrpc.SwapSummary, note string,
	counterparty string,
	callerKind walletdkrpc.EntryKind) *walletdkrpc.WalletEntry {

	if s == nil {
		return &walletdkrpc.WalletEntry{
			Kind:   walletdkrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
			Status: walletdkrpc.EntryStatus_ENTRY_STATUS_UNSPECIFIED,
		}
	}

	kind := callerKind
	if kind == walletdkrpc.EntryKind_ENTRY_KIND_UNSPECIFIED {
		kind = kindFromSwapDirection(s.GetDirection())
	}

	// Derive reason and code from one classification so a terminal failure
	// never carries a code without a human-readable reason.
	failureCode := failureCodeFromSwapState(s.GetState(), s.GetPending())

	entry := &walletdkrpc.WalletEntry{
		Id:            s.GetPaymentHash(),
		Kind:          kind,
		Status:        statusFromSwapState(s.GetState(), s.GetPending()),
		FeeSat:        int64(s.GetFeeSat()),
		Counterparty:  truncate(counterparty, truncatedCounterpartyLen),
		CreatedAtUnix: s.GetCreatedAtUnix(),
		UpdatedAtUnix: s.GetUpdatedAtUnix(),
		Note:          entryNote(note, s.GetInvoice()),
		FailureReason: failureReason(s.GetTerminalReason(), failureCode),
		Request:       requestFromSwapSummary(s, counterparty, kind),
		Progress:      progressFromSwapSummary(s),
		FailureCode:   failureCodePtr(failureCode),
	}

	// Render amount with the wallet's signed-amount convention: positive
	// for incoming RECV, negative for outgoing SEND. The sign is derived
	// from the FINAL kind (caller override or direction fallback), never
	// the raw direction, so an UNSPECIFIED direction during a lazy SDK
	// summary cannot flip a SEND amount to positive.
	amount := s.GetAmountSat()
	switch kind {
	case walletdkrpc.EntryKind_ENTRY_KIND_SEND:
		entry.AmountSat = -amount

	case walletdkrpc.EntryKind_ENTRY_KIND_RECV:
		entry.AmountSat = amount

	default:
		entry.AmountSat = amount
	}

	return entry
}

// requestFromSwapSummary surfaces the durable payment request associated with
// a Lightning-backed wallet entry. Historical swap rows carry the invoice on
// the summary; immediate SEND responses fall back to the caller-supplied
// invoice passed as counterparty until the persisted summary catches up.
// Payment hashes are tracked separately and must not be promoted into the
// invoice field.
func requestFromSwapSummary(s *swapclientrpc.SwapSummary, counterparty string,
	kind walletdkrpc.EntryKind) *walletdkrpc.WalletEntryRequest {

	if s == nil {
		return nil
	}

	invoice := s.GetInvoice()
	if invoice == "" &&
		kind == walletdkrpc.EntryKind_ENTRY_KIND_SEND &&
		counterparty != s.GetPaymentHash() {

		invoice = counterparty
	}
	if invoice == "" && s.GetPaymentHash() == "" {
		return nil
	}

	return &walletdkrpc.WalletEntryRequest{
		Request: &walletdkrpc.WalletEntryRequest_LightningInvoice{
			LightningInvoice: &walletdkrpc.LightningInvoiceRequest{
				Invoice:     invoice,
				PaymentHash: s.GetPaymentHash(),
			},
		},
	}
}

// progressFromSwapSummary maps detailed swap states onto a compact
// wallet-facing lifecycle hint. The exact SDK state remains available through
// swapclientrpc for power users; wallet callers get stable phases.
func progressFromSwapSummary(
	s *swapclientrpc.SwapSummary) *walletdkrpc.WalletEntryProgress {

	if s == nil {
		return nil
	}

	phase, label := phaseFromSwapState(s.GetState(), s.GetPending())
	if phase == walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_UNSPECIFIED &&
		s.GetPaymentHash() == "" {
		return nil
	}

	return &walletdkrpc.WalletEntryProgress{
		Phase:        phase,
		PhaseLabel:   label,
		PaymentHash:  s.GetPaymentHash(),
		VtxoOutpoint: s.GetVhtlcOutpoint(),
		Preimage:     s.GetPreimage(),
	}
}

// phaseFromSwapState maps a swap state and pending flag to the wallet
// lifecycle phase and display label.
func phaseFromSwapState(state swapclientrpc.SwapState,
	pending bool) (walletdkrpc.WalletEntryPhase, string) {

	switch state {
	case swapclientrpc.SwapState_SWAP_STATE_CREATED,
		swapclientrpc.SwapState_SWAP_STATE_SWAP_CREATED,
		swapclientrpc.SwapState_SWAP_STATE_INVOICE_CREATED:
		return walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_PAYMENT,
			"waiting_for_payment"

	case swapclientrpc.SwapState_SWAP_STATE_FUNDING_INITIATED,
		swapclientrpc.SwapState_SWAP_STATE_VHTLC_FUNDED,
		swapclientrpc.SwapState_SWAP_STATE_WAITING_FOR_CLAIM,
		swapclientrpc.SwapState_SWAP_STATE_CLAIM_INITIATED:
		return walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING,
			"settling"

	case swapclientrpc.SwapState_SWAP_STATE_COMPLETED:
		return walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_CONFIRMED,
			"confirmed"

	case swapclientrpc.SwapState_SWAP_STATE_REFUND_INITIATED:
		return walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_REFUNDING,
			"refunding"

	case swapclientrpc.SwapState_SWAP_STATE_REFUNDED:
		return walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_REFUNDED,
			"refunded"

	case swapclientrpc.SwapState_SWAP_STATE_FAILED,
		swapclientrpc.SwapState_SWAP_STATE_EXPIRED,
		swapclientrpc.SwapState_SWAP_STATE_NEEDS_INTERVENTION:
		return walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_FAILED,
			"failed"
	}

	if pending {
		return walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING,
			"settling"
	}

	return walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_UNSPECIFIED, ""
}

// kindFromSwapDirection maps the swap proto direction to a user-facing
// EntryKind.
func kindFromSwapDirection(dir swapclientrpc.SwapDirection,
) walletdkrpc.EntryKind {

	switch dir {
	case swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY:
		return walletdkrpc.EntryKind_ENTRY_KIND_SEND

	case swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE:
		return walletdkrpc.EntryKind_ENTRY_KIND_RECV

	default:
		return walletdkrpc.EntryKind_ENTRY_KIND_UNSPECIFIED
	}
}

// statusFromSwapState collapses every backing SwapState plus the pending
// flag into the three user-facing wallet states. Pending governs PENDING
// vs COMPLETE / FAILED; terminal states pick between COMPLETE and FAILED
// based on whether the run reached the happy path. REFUNDED is a terminal
// failure from the payment perspective; the phase carries the extra context
// that funds returned.
func statusFromSwapState(state swapclientrpc.SwapState,
	pending bool) walletdkrpc.EntryStatus {

	if pending {
		return walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING
	}

	switch state {
	case swapclientrpc.SwapState_SWAP_STATE_COMPLETED:
		return walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE

	case swapclientrpc.SwapState_SWAP_STATE_FAILED,
		swapclientrpc.SwapState_SWAP_STATE_EXPIRED,
		swapclientrpc.SwapState_SWAP_STATE_NEEDS_INTERVENTION,
		swapclientrpc.SwapState_SWAP_STATE_REFUNDED:
		return walletdkrpc.EntryStatus_ENTRY_STATUS_FAILED

	default:
		// Non-pending, non-terminal is an unexpected state. Surface
		// it as PENDING so the caller can poll for the next
		// transition rather than treating the row as terminal.
		return walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING
	}
}

// unspecCode is the unspecified EntryFailureCode zero value, shared by the
// classifier and the runtime overlay.
const unspecCode = walletdkrpc.EntryFailureCode_ENTRY_FAILURE_CODE_UNSPECIFIED

// failureCodeFromSwapState classifies a terminal swap failure into a stable,
// machine-readable code. It returns the unspecified code for pending or
// non-failure states; only the terminal states that statusFromSwapState
// collapses to FAILED carry a code.
func failureCodeFromSwapState(state swapclientrpc.SwapState,
	pending bool) walletdkrpc.EntryFailureCode {

	if pending {
		return unspecCode
	}

	switch state {
	case swapclientrpc.SwapState_SWAP_STATE_EXPIRED:
		return walletdkrpc.EntryFailureCode_ENTRY_FAILURE_CODE_EXPIRED

	case swapclientrpc.SwapState_SWAP_STATE_REFUNDED:
		return walletdkrpc.EntryFailureCode_ENTRY_FAILURE_CODE_REFUNDED

	case swapclientrpc.SwapState_SWAP_STATE_NEEDS_INTERVENTION:
		return walletdkrpc.EntryFailureCode_ENTRY_FAILURE_CODE_NEEDS_INTERVENTION //nolint:ll

	case swapclientrpc.SwapState_SWAP_STATE_FAILED:
		return walletdkrpc.EntryFailureCode_ENTRY_FAILURE_CODE_FAILED

	default:
		return unspecCode
	}
}

// failureCodePtr adapts a classifier result to the optional failure_code proto
// field. It returns nil for the unspecified (no-failure) sentinel so the field
// stays absent on the wire, and a pointer to a concrete code otherwise; only a
// genuinely failed entry carries a failure_code.
func failureCodePtr(
	code walletdkrpc.EntryFailureCode) *walletdkrpc.EntryFailureCode {

	if code == unspecCode {
		return nil
	}

	return code.Enum()
}

// failureReasonFromTerminal surfaces the SDK's terminal reason string only
// when it carries information. The wallet layer keeps the field empty for
// happy-path rows so callers can use !=" "" as a "did something go wrong"
// check.
func failureReasonFromTerminal(reason string) string {
	return strings.TrimSpace(reason)
}

// failureReason prefers the SDK terminal reason and falls back to a stable
// default for the classified code, so a failed row never renders empty. A
// non-failure row (unspecified code, empty terminal) stays blank.
func failureReason(terminal string, code walletdkrpc.EntryFailureCode) string {
	if r := failureReasonFromTerminal(terminal); r != "" {
		return r
	}

	return defaultFailureReason(code)
}

// defaultFailureReason maps a failure code to a short reason for when the swap
// FSM supplied no terminal string. The unspecified code stays blank.
func defaultFailureReason(code walletdkrpc.EntryFailureCode) string {
	switch code {
	case walletdkrpc.EntryFailureCode_ENTRY_FAILURE_CODE_TIMED_OUT:
		return "timed_out"

	case walletdkrpc.EntryFailureCode_ENTRY_FAILURE_CODE_EXPIRED:
		return "invoice expired"

	case walletdkrpc.EntryFailureCode_ENTRY_FAILURE_CODE_REFUNDED:
		return "refunded"

	case walletdkrpc.EntryFailureCode_ENTRY_FAILURE_CODE_NEEDS_INTERVENTION:
		return "needs intervention"

	case walletdkrpc.EntryFailureCode_ENTRY_FAILURE_CODE_FAILED:
		return "failed"

	default:
		return ""
	}
}

// entryNote prefers the explicit caller note when the submit path still has
// it in memory. Historical receive rows derive their note from the BOLT-11
// description because the swap DB persists the invoice but not a separate
// wallet memo field.
func entryNote(explicitNote, invoice string) string {
	if note := strings.TrimSpace(explicitNote); note != "" {
		return note
	}

	return invoiceMemo(invoice)
}

// invoiceMemo extracts the BOLT-11 description that represents the caller's
// receive memo.
func invoiceMemo(invoice string) string {
	invoice = strings.TrimSpace(invoice)
	if invoice == "" {
		return ""
	}

	for _, params := range invoiceDecodeNetworks() {
		decoded, err := zpay32.Decode(invoice, params)
		if err != nil || decoded.Description == nil {
			continue
		}

		memo := strings.TrimSpace(*decoded.Description)
		if memo == legacyReceiveInvoiceMemo {
			return ""
		}

		return memo
	}

	return ""
}

// invoiceDecodeNetworks returns every chain parameter set that may decode a
// persisted invoice.
func invoiceDecodeNetworks() []*chaincfg.Params {
	return []*chaincfg.Params{
		&chaincfg.MainNetParams,
		&chaincfg.TestNet3Params,
		&chaincfg.TestNet4Params,
		&chaincfg.RegressionNetParams,
		&chaincfg.SigNetParams,
		&chaincfg.SimNetParams,
	}
}

// truncate clips s to at most n characters. The function preserves the
// prefix so a Lightning bech32 invoice still shows its network and amount
// segments in the rendered counterparty.
func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}

	return s[:n]
}

// nowUnix returns the current unix-seconds timestamp. Hoisted so a future
// unit test can replace it via a build-tagged variable; the production path
// always uses time.Now.
func nowUnix() int64 {
	return time.Now().Unix()
}

// unixToTime converts an int64 unix-seconds timestamp into time.Time.
// Returns time.Now when the input is zero so callers always get a usable
// timestamp for pending tracking.
func unixToTime(ts int64) time.Time {
	if ts == 0 {
		return time.Now()
	}

	return time.Unix(ts, 0)
}

// walletVTXOFromDaemon projects a daemonrpc.VTXO onto the wallet-facing
// WalletVTXO shape. Returns (vtxo, true) when the underlying VTXO maps to a
// spendable wallet view; (nil, false) when the row should be hidden (terminal
// states the user has no agency over).
func walletVTXOFromDaemon(v *daemonrpc.VTXO) (*walletdkrpc.WalletVTXO, bool) {
	if v == nil {
		return nil, false
	}

	status, keep := walletVTXOStatusFromDaemon(v.GetStatus())
	if !keep {
		return nil, false
	}

	return &walletdkrpc.WalletVTXO{
		Outpoint:       v.GetOutpoint(),
		AmountSat:      v.GetAmountSat(),
		Status:         status,
		BatchExpiry:    v.GetBatchExpiry(),
		RelativeExpiry: v.GetRelativeExpiry(),
		CommitmentTxid: v.GetCommitmentTxid(),
	}, true
}

// walletVTXOStatusFromDaemon maps the daemon VTXO status enum onto a short
// lowercase wallet string. Terminal internal states (forfeited, spent,
// failed) return keep=false so the wallet view stays focused on the
// VTXOs a user can still act on.
func walletVTXOStatusFromDaemon(s daemonrpc.VTXOStatus) (string, bool) {
	switch s {
	case daemonrpc.VTXOStatus_VTXO_STATUS_LIVE:
		return "live", true

	case daemonrpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT:
		return "pending_forfeit", true

	case daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITING:
		return "forfeiting", true

	case daemonrpc.VTXOStatus_VTXO_STATUS_SPENDING:
		return "spending", true

	case daemonrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT:
		return "unilateral_exit", true

	default:
		return "", false
	}
}

// onchainTxFromLedgerRow flattens a daemonrpc.TransactionHistoryEntry onto
// the wallet-facing OnchainTx shape. Internal correlators (round_id,
// session_id, debit/credit accounts) are not surfaced; the wallet view
// keeps only what's useful at the top of an everyday wallet history.
func onchainTxFromLedgerRow(t *daemonrpc.TransactionHistoryEntry,
) *walletdkrpc.OnchainTx {

	if t == nil {
		return &walletdkrpc.OnchainTx{}
	}

	return &walletdkrpc.OnchainTx{
		Txid:               t.GetTxid(),
		Kind:               t.GetType(),
		AmountSat:          t.GetAmountSat(),
		FeeSat:             t.GetFeeSat(),
		Status:             t.GetConfirmationStatus(),
		ConfirmationHeight: t.GetConfirmationHeight(),
		CreatedAtUnix:      t.GetCreatedAtUnixS(),
		Description:        t.GetDescription(),
	}
}

// requestFromOnchainAddress wraps an onchain destination in the wallet request
// oneof.
func requestFromOnchainAddress(address string) *walletdkrpc.WalletEntryRequest {
	if address == "" {
		return nil
	}

	return &walletdkrpc.WalletEntryRequest{
		Request: &walletdkrpc.WalletEntryRequest_OnchainAddress{
			OnchainAddress: &walletdkrpc.OnchainAddressRequest{
				Address: address,
			},
		},
	}
}

// paginateVTXOs slices wallet VTXOs by offset and limit, returning a fresh
// slice so the caller cannot mutate the merger's internal buffer.
func paginateVTXOs(vtxos []*walletdkrpc.WalletVTXO, offset,
	limit uint32) []*walletdkrpc.WalletVTXO {

	if offset >= uint32(len(vtxos)) {
		return nil
	}
	end := offset + limit
	if end > uint32(len(vtxos)) {
		end = uint32(len(vtxos))
	}
	page := make([]*walletdkrpc.WalletVTXO, 0, end-offset)
	page = append(page, vtxos[offset:end]...)

	return page
}

// leaveEntryStub builds the initial WalletEntry returned by Send when the
// caller targets an onchain destination. The id is the daemon's stable
// leave-job id (a deterministic hash of the consumed outpoints and payload),
// so the row keeps one durable handle from initiation through confirmation,
// across restarts, and represents a multi-input sweep as a single row. When
// the daemon does not return one, it falls back to the first queued outpoint
// (the pre-#610 behavior). The first consumed outpoint is retained in
// vtxo_outpoint so the forfeit-driven completion can still correlate the row.
func leaveEntryStub(leaveJobID string, queuedOutpoints []string,
	destination string, amtSat int64,
	note string) *walletdkrpc.WalletEntry {

	var firstOutpoint string
	if len(queuedOutpoints) > 0 {
		firstOutpoint = queuedOutpoints[0]
	}

	id := leaveJobID
	if id == "" {
		id = firstOutpoint
	}
	createdAt := nowUnix()

	return &walletdkrpc.WalletEntry{
		Id:            id,
		Kind:          walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
		Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     -amtSat,
		Counterparty:  truncate(destination, truncatedCounterpartyLen),
		CreatedAtUnix: createdAt,
		UpdatedAtUnix: createdAt,
		Note:          note,
		Request:       requestFromOnchainAddress(destination),
		Progress: &walletdkrpc.WalletEntryProgress{
			Phase:        walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_REQUEST_CREATED,
			PhaseLabel:   "request_created",
			VtxoOutpoint: firstOutpoint,
		},
	}
}

// unilateralExitEntryStub builds the initial WalletEntry tracked after the
// user explicitly requests a unilateral exit.
func unilateralExitEntryStub(outpoint string) *walletdkrpc.WalletEntry {
	createdAt := nowUnix()

	return &walletdkrpc.WalletEntry{
		Id:            outpoint,
		Kind:          walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
		Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		Counterparty:  "unilateral",
		CreatedAtUnix: createdAt,
		UpdatedAtUnix: createdAt,
		Progress: &walletdkrpc.WalletEntryProgress{
			Phase: walletdkrpc.
				WalletEntryPhase_WALLET_ENTRY_PHASE_REQUEST_CREATED,
			PhaseLabel:   "request_created",
			VtxoOutpoint: outpoint,
		},
	}
}

// applyUnrollStatus projects daemon unroll status onto an EXIT activity row.
func applyUnrollStatus(entry *walletdkrpc.WalletEntry,
	resp *daemonrpc.GetUnrollStatusResponse) {

	if entry == nil || resp == nil || !resp.GetFound() {
		return
	}

	progress := entry.GetProgress()
	if progress == nil {
		progress = &walletdkrpc.WalletEntryProgress{}
		entry.Progress = progress
	}
	if progress.GetVtxoOutpoint() == "" {
		progress.VtxoOutpoint = entry.GetId()
	}
	progress.Txid = resp.GetSweepTxid()

	switch resp.GetStatus() {
	case daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED:
		entry.Status = walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE
		progress.Phase = walletdkrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_CONFIRMED
		progress.PhaseLabel = "confirmed"

	case daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_FAILED:
		entry.Status = walletdkrpc.EntryStatus_ENTRY_STATUS_FAILED
		entry.FailureReason = resp.GetLastError()
		entry.FailureCode = walletdkrpc.
			EntryFailureCode_ENTRY_FAILURE_CODE_FAILED.Enum()
		progress.Phase = walletdkrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_FAILED
		progress.PhaseLabel = "failed"

	case daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_CSV_PENDING:
		entry.Status = walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING
		progress.Phase = walletdkrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_CONFIRMATION
		progress.PhaseLabel = "csv_pending"

	case daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_SWEEPING:
		entry.Status = walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING
		progress.Phase = walletdkrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING
		progress.PhaseLabel = "sweeping"

	case daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_PENDING,
		daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_MATERIALIZING:

		entry.Status = walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING
		progress.Phase = walletdkrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING
		progress.PhaseLabel = "unilateral_exit"
	}
}

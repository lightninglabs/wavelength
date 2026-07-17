//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waverpc"
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
	callerKind wavewalletrpc.EntryKind) *wavewalletrpc.WalletEntry {

	if s == nil {
		return &wavewalletrpc.WalletEntry{
			Kind:   wavewalletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
			Status: wavewalletrpc.EntryStatus_ENTRY_STATUS_UNSPECIFIED,
		}
	}

	kind := callerKind
	if kind == wavewalletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED {
		kind = kindFromSwapDirection(s.GetDirection())
	}

	// Derive reason and code from one classification so a terminal failure
	// never carries a code without a human-readable reason.
	failureCode := failureCodeFromSwapState(s.GetState(), s.GetPending())

	entry := &wavewalletrpc.WalletEntry{
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
	case wavewalletrpc.EntryKind_ENTRY_KIND_SEND:
		entry.AmountSat = -amount

	case wavewalletrpc.EntryKind_ENTRY_KIND_RECV:
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
	kind wavewalletrpc.EntryKind) *wavewalletrpc.WalletEntryRequest {

	if s == nil {
		return nil
	}

	invoice := s.GetInvoice()
	if invoice == "" &&
		kind == wavewalletrpc.EntryKind_ENTRY_KIND_SEND &&
		counterparty != s.GetPaymentHash() {

		invoice = counterparty
	}
	if invoice == "" && s.GetPaymentHash() == "" {
		return nil
	}

	return &wavewalletrpc.WalletEntryRequest{
		Request: &wavewalletrpc.WalletEntryRequest_LightningInvoice{
			LightningInvoice: &wavewalletrpc.LightningInvoiceRequest{
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
	s *swapclientrpc.SwapSummary) *wavewalletrpc.WalletEntryProgress {

	if s == nil {
		return nil
	}

	phase, label := phaseFromSwapState(s.GetState(), s.GetPending())
	if phase == wavewalletrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_UNSPECIFIED &&
		s.GetPaymentHash() == "" {
		return nil
	}

	return &wavewalletrpc.WalletEntryProgress{
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
	pending bool) (wavewalletrpc.WalletEntryPhase, string) {

	switch state {
	case swapclientrpc.SwapState_SWAP_STATE_CREATED,
		swapclientrpc.SwapState_SWAP_STATE_SWAP_CREATED,
		swapclientrpc.SwapState_SWAP_STATE_INVOICE_CREATED:
		return wavewalletrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_PAYMENT,
			"waiting_for_payment"

	case swapclientrpc.SwapState_SWAP_STATE_FUNDING_INITIATED,
		swapclientrpc.SwapState_SWAP_STATE_VHTLC_FUNDED,
		swapclientrpc.SwapState_SWAP_STATE_WAITING_FOR_CLAIM,
		swapclientrpc.SwapState_SWAP_STATE_CLAIM_INITIATED:
		return wavewalletrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING,
			"settling"

	case swapclientrpc.SwapState_SWAP_STATE_COMPLETED:
		return wavewalletrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_CONFIRMED,
			"confirmed"

	case swapclientrpc.SwapState_SWAP_STATE_REFUND_INITIATED:
		return wavewalletrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_REFUNDING,
			"refunding"

	case swapclientrpc.SwapState_SWAP_STATE_REFUNDED:
		return wavewalletrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_REFUNDED,
			"refunded"

	case swapclientrpc.SwapState_SWAP_STATE_FAILED,
		swapclientrpc.SwapState_SWAP_STATE_EXPIRED,
		swapclientrpc.SwapState_SWAP_STATE_NEEDS_INTERVENTION:
		return wavewalletrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_FAILED,
			"failed"
	}

	if pending {
		return wavewalletrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING,
			"settling"
	}

	return wavewalletrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_UNSPECIFIED, ""
}

// kindFromSwapDirection maps the swap proto direction to a user-facing
// EntryKind.
func kindFromSwapDirection(dir swapclientrpc.SwapDirection,
) wavewalletrpc.EntryKind {

	switch dir {
	case swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY:
		return wavewalletrpc.EntryKind_ENTRY_KIND_SEND

	case swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE:
		return wavewalletrpc.EntryKind_ENTRY_KIND_RECV

	default:
		return wavewalletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED
	}
}

// statusFromSwapState collapses every backing SwapState plus the pending
// flag into the three user-facing wallet states. Pending governs PENDING
// vs COMPLETE / FAILED; terminal states pick between COMPLETE and FAILED
// based on whether the run reached the happy path. REFUNDED is a terminal
// failure from the payment perspective; the phase carries the extra context
// that funds returned.
func statusFromSwapState(state swapclientrpc.SwapState,
	pending bool) wavewalletrpc.EntryStatus {

	if pending {
		return wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING
	}

	switch state {
	case swapclientrpc.SwapState_SWAP_STATE_COMPLETED:
		return wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE

	case swapclientrpc.SwapState_SWAP_STATE_FAILED,
		swapclientrpc.SwapState_SWAP_STATE_EXPIRED,
		swapclientrpc.SwapState_SWAP_STATE_NEEDS_INTERVENTION,
		swapclientrpc.SwapState_SWAP_STATE_REFUNDED:
		return wavewalletrpc.EntryStatus_ENTRY_STATUS_FAILED

	default:
		// Non-pending, non-terminal is an unexpected state. Surface
		// it as PENDING so the caller can poll for the next
		// transition rather than treating the row as terminal.
		return wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING
	}
}

// unspecCode is the unspecified EntryFailureCode zero value, shared by the
// classifier and the runtime overlay.
const unspecCode = wavewalletrpc.EntryFailureCode_ENTRY_FAILURE_CODE_UNSPECIFIED

// failureCodeFromSwapState classifies a terminal swap failure into a stable,
// machine-readable code. It returns the unspecified code for pending or
// non-failure states; only the terminal states that statusFromSwapState
// collapses to FAILED carry a code.
func failureCodeFromSwapState(state swapclientrpc.SwapState,
	pending bool) wavewalletrpc.EntryFailureCode {

	if pending {
		return unspecCode
	}

	switch state {
	case swapclientrpc.SwapState_SWAP_STATE_EXPIRED:
		return wavewalletrpc.EntryFailureCode_ENTRY_FAILURE_CODE_EXPIRED

	case swapclientrpc.SwapState_SWAP_STATE_REFUNDED:
		return wavewalletrpc.EntryFailureCode_ENTRY_FAILURE_CODE_REFUNDED

	case swapclientrpc.SwapState_SWAP_STATE_NEEDS_INTERVENTION:
		return wavewalletrpc.EntryFailureCode_ENTRY_FAILURE_CODE_NEEDS_INTERVENTION //nolint:ll

	case swapclientrpc.SwapState_SWAP_STATE_FAILED:
		return wavewalletrpc.EntryFailureCode_ENTRY_FAILURE_CODE_FAILED

	default:
		return unspecCode
	}
}

// failureCodePtr adapts a classifier result to the optional failure_code proto
// field. It returns nil for the unspecified (no-failure) sentinel so the field
// stays absent on the wire, and a pointer to a concrete code otherwise; only a
// genuinely failed entry carries a failure_code.
func failureCodePtr(
	code wavewalletrpc.EntryFailureCode) *wavewalletrpc.EntryFailureCode {

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
func failureReason(terminal string,
	code wavewalletrpc.EntryFailureCode) string {

	if r := failureReasonFromTerminal(terminal); r != "" {
		return r
	}

	return defaultFailureReason(code)
}

// defaultFailureReason maps a failure code to a short reason for when the swap
// FSM supplied no terminal string. The unspecified code stays blank.
func defaultFailureReason(code wavewalletrpc.EntryFailureCode) string {
	switch code {
	case wavewalletrpc.EntryFailureCode_ENTRY_FAILURE_CODE_TIMED_OUT:
		return "timed_out"

	case wavewalletrpc.EntryFailureCode_ENTRY_FAILURE_CODE_EXPIRED:
		return "invoice expired"

	case wavewalletrpc.EntryFailureCode_ENTRY_FAILURE_CODE_REFUNDED:
		return "refunded"

	case wavewalletrpc.EntryFailureCode_ENTRY_FAILURE_CODE_NEEDS_INTERVENTION:
		return "needs intervention"

	case wavewalletrpc.EntryFailureCode_ENTRY_FAILURE_CODE_FAILED:
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

// walletVTXOFromDaemon projects a waverpc.VTXO onto the wallet-facing
// WalletVTXO shape. Returns (vtxo, true) when the underlying VTXO maps to a
// spendable wallet view; (nil, false) when the row should be hidden (terminal
// states the user has no agency over).
func walletVTXOFromDaemon(v *waverpc.VTXO) (*wavewalletrpc.WalletVTXO, bool) {
	if v == nil {
		return nil, false
	}

	status, keep := walletVTXOStatusFromDaemon(v.GetStatus())
	if !keep {
		return nil, false
	}

	return &wavewalletrpc.WalletVTXO{
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
func walletVTXOStatusFromDaemon(s waverpc.VTXOStatus) (string, bool) {
	switch s {
	case waverpc.VTXOStatus_VTXO_STATUS_LIVE:
		return "live", true

	case waverpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT:
		return "pending_forfeit", true

	case waverpc.VTXOStatus_VTXO_STATUS_FORFEITING:
		return "forfeiting", true

	case waverpc.VTXOStatus_VTXO_STATUS_SPENDING:
		return "spending", true

	case waverpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT:
		return "unilateral_exit", true

	default:
		return "", false
	}
}

// onchainTxFromLedgerRow flattens a waverpc.TransactionHistoryEntry onto
// the wallet-facing OnchainTx shape. Internal correlators (round_id,
// session_id, debit/credit accounts) are not surfaced; the wallet view
// keeps only what's useful at the top of an everyday wallet history.
func onchainTxFromLedgerRow(t *waverpc.TransactionHistoryEntry,
) *wavewalletrpc.OnchainTx {

	if t == nil {
		return &wavewalletrpc.OnchainTx{}
	}

	return &wavewalletrpc.OnchainTx{
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
func requestFromOnchainAddress(
	address string) *wavewalletrpc.WalletEntryRequest {

	if address == "" {
		return nil
	}

	return &wavewalletrpc.WalletEntryRequest{
		Request: &wavewalletrpc.WalletEntryRequest_OnchainAddress{
			OnchainAddress: &wavewalletrpc.OnchainAddressRequest{
				Address: address,
			},
		},
	}
}

// paginateVTXOs slices wallet VTXOs by offset and limit, returning a fresh
// slice so the caller cannot mutate the merger's internal buffer.
func paginateVTXOs(vtxos []*wavewalletrpc.WalletVTXO, offset,
	limit uint32) []*wavewalletrpc.WalletVTXO {

	if offset >= uint32(len(vtxos)) {
		return nil
	}
	end := offset + limit
	if end > uint32(len(vtxos)) {
		end = uint32(len(vtxos))
	}
	page := make([]*wavewalletrpc.WalletVTXO, 0, end-offset)
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
	destination string, amtSat int64, note string,
	sweepAll bool) *wavewalletrpc.WalletEntry {

	var firstOutpoint string
	if len(queuedOutpoints) > 0 {
		firstOutpoint = queuedOutpoints[0]
	}

	id := leaveJobID
	if id == "" {
		id = firstOutpoint
	}
	createdAt := nowUnix()

	// The sweep-all marker is persisted on the request so completion can
	// net the seal-time operator fee back out of the gross sweep amount
	// once the fee becomes known (see applyCooperativeLeaveForfeited).
	request := requestFromOnchainAddress(destination)
	if sweepAll && request.GetOnchainAddress() != nil {
		request.GetOnchainAddress().SweepAll = true
	}

	return &wavewalletrpc.WalletEntry{
		Id:            id,
		Kind:          wavewalletrpc.EntryKind_ENTRY_KIND_EXIT,
		Status:        wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     -amtSat,
		Counterparty:  truncate(destination, truncatedCounterpartyLen),
		CreatedAtUnix: createdAt,
		UpdatedAtUnix: createdAt,
		Note:          note,
		Request:       request,
		Progress: &wavewalletrpc.WalletEntryProgress{
			Phase:        wavewalletrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_REQUEST_CREATED,
			PhaseLabel:   "request_created",
			VtxoOutpoint: firstOutpoint,
		},
	}
}

// unilateralExitEntryStub builds the initial WalletEntry tracked after the
// user explicitly requests a unilateral exit.
func unilateralExitEntryStub(outpoint string) *wavewalletrpc.WalletEntry {
	createdAt := nowUnix()

	return &wavewalletrpc.WalletEntry{
		Id:            outpoint,
		Kind:          wavewalletrpc.EntryKind_ENTRY_KIND_EXIT,
		Status:        wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		Counterparty:  "unilateral",
		CreatedAtUnix: createdAt,
		UpdatedAtUnix: createdAt,
		Progress: &wavewalletrpc.WalletEntryProgress{
			Phase: wavewalletrpc.
				WalletEntryPhase_WALLET_ENTRY_PHASE_REQUEST_CREATED,
			PhaseLabel:   "request_created",
			VtxoOutpoint: outpoint,
		},
	}
}

// applyUnrollStatus projects daemon unroll status onto an EXIT activity row.
func applyUnrollStatus(entry *wavewalletrpc.WalletEntry,
	resp *waverpc.GetUnrollStatusResponse) {

	if entry == nil || resp == nil || !resp.GetFound() {
		return
	}

	progress := entry.GetProgress()
	if progress == nil {
		progress = &wavewalletrpc.WalletEntryProgress{}
		entry.Progress = progress
	}
	if progress.GetVtxoOutpoint() == "" {
		progress.VtxoOutpoint = entry.GetId()
	}
	progress.Txid = resp.GetSweepTxid()

	switch resp.GetStatus() {
	case waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED:
		entry.Status = wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE
		progress.Phase = wavewalletrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_CONFIRMED
		progress.PhaseLabel = "confirmed"

		// A completed exit reports the settled on-chain cost from the
		// ledger. The row's pending amount is the gross VTXO value, so
		// the cost is netted back out, leaving amount = value delivered
		// on chain and fee = cost on top — the same shape a completed
		// cooperative leave has. Zero cost (old daemon, or an exit
		// predating exit-cost accounting) leaves the row untouched.
		// The mutation operates on the per-derive clone from
		// pendingSnapshot / the per-VTXO derived row, so it applies
		// exactly once per derived row.
		if cost := resp.GetExitCostSat(); cost > 0 {
			entry.FeeSat = cost

			// Clamp at zero so a cost exceeding the gross amount
			// (impossible for a sane exit, but the figure crosses
			// an RPC boundary) can never flip the outflow row's
			// sign positive.
			if entry.AmountSat < 0 {
				entry.AmountSat += cost
				if entry.AmountSat > 0 {
					entry.AmountSat = 0
				}
			}
		}

	case waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_FAILED:
		entry.Status = wavewalletrpc.EntryStatus_ENTRY_STATUS_FAILED
		entry.FailureReason = resp.GetLastError()
		entry.FailureCode = wavewalletrpc.
			EntryFailureCode_ENTRY_FAILURE_CODE_FAILED.Enum()
		progress.Phase = wavewalletrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_FAILED
		progress.PhaseLabel = "failed"

	case waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_CSV_PENDING:
		entry.Status = wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING
		progress.Phase = wavewalletrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_CONFIRMATION
		progress.PhaseLabel = "csv_pending"

	case waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_SWEEPING:
		entry.Status = wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING
		progress.Phase = wavewalletrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING
		progress.PhaseLabel = "sweeping"

	case waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_PENDING,
		waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_MATERIALIZING:

		entry.Status = wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING
		progress.Phase = wavewalletrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING
		progress.PhaseLabel = "unilateral_exit"
	}
}

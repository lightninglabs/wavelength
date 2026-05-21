//go:build walletrpc && swapruntime

package swapwallet

import (
	"context"
	"fmt"
	"sort"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// history merges entries from the daemon's existing history surfaces
// (swap subserver ListSwaps + RPCServer.ListTransactions) into the single
// flat WalletEntry shape exposed by walletrpc.WalletService.List.
//
// The merge is deliberately consumer-side: every source already has its
// own canonical persistence (swap DB, ledger entries, boarding_sweeps),
// and swapwallet must not duplicate that state. The merger simply pulls
// the latest pages, normalizes each row to a WalletEntry, applies the
// caller's filters, sorts by updated_at descending, paginates, and
// overlays the runtime's deadline-driven FAILED projections.
type history struct {
	deps    *Deps
	runtime *Runtime
}

// newHistory constructs the history merger.
func newHistory(deps *Deps, runtime *Runtime) *history {
	return &history{deps: deps, runtime: runtime}
}

// List dispatches the request to the typed view selected by req.view. The
// default (LIST_VIEW_UNSPECIFIED) is treated as LIST_VIEW_ACTIVITY so older
// callers that omit the field keep getting the unified activity stream.
// Each view is implemented by a dedicated helper; the top-level response
// shape is a oneof so agents see a tagged union, not a polymorphic blob.
func (h *history) List(ctx context.Context, req *walletrpc.ListRequest) (
	*walletrpc.ListResponse, error) {

	if h == nil || h.deps == nil {
		return nil, ErrSwapBackendUnavailable
	}

	switch req.GetView() {
	case walletrpc.ListView_LIST_VIEW_VTXOS:
		body, err := h.listVTXOs(ctx, req)
		if err != nil {
			return nil, err
		}

		return &walletrpc.ListResponse{
			Body: &walletrpc.ListResponse_Vtxos{
				Vtxos: body,
			},
		}, nil

	case walletrpc.ListView_LIST_VIEW_ONCHAIN:
		body, err := h.listOnchain(ctx, req)
		if err != nil {
			return nil, err
		}

		return &walletrpc.ListResponse{
			Body: &walletrpc.ListResponse_Onchain{
				Onchain: body,
			},
		}, nil

	case walletrpc.ListView_LIST_VIEW_ACTIVITY,
		walletrpc.ListView_LIST_VIEW_UNSPECIFIED:

		body, err := h.listActivity(ctx, req)
		if err != nil {
			return nil, err
		}

		return &walletrpc.ListResponse{
			Body: &walletrpc.ListResponse_Activity{
				Activity: body,
			},
		}, nil

	default:
		return nil, fmt.Errorf("unknown list view: %v", req.GetView())
	}
}

// listActivity returns the merged WalletEntry stream — the v1 unified
// history. The page size is capped at the daemon-level maximum so a
// malformed request cannot fan out unbounded work; sources are queried
// with the request's own limit so per-source pagination remains the
// per-source contract.
func (h *history) listActivity(ctx context.Context,
	req *walletrpc.ListRequest) (*walletrpc.ActivityList, error) {

	limit := h.deps.resolveListLimit(req.GetLimit())
	kindFilter, err := buildKindFilter(req.GetKinds())
	if err != nil {
		return nil, err
	}

	var entries []*walletrpc.WalletEntry

	if h.shouldInclude(kindFilter, walletrpc.EntryKind_ENTRY_KIND_SEND) ||
		h.shouldInclude(kindFilter,
			walletrpc.EntryKind_ENTRY_KIND_RECV) {

		swapEntries, err := h.collectSwapEntries(
			ctx, req.GetPendingOnly(),
		)
		if err != nil {
			return nil, fmt.Errorf("collect swap entries: %w", err)
		}

		entries = append(entries, swapEntries...)
	}

	if h.shouldInclude(kindFilter, walletrpc.EntryKind_ENTRY_KIND_DEPOSIT) ||
		h.shouldInclude(kindFilter,
			walletrpc.EntryKind_ENTRY_KIND_EXIT) ||
		h.shouldInclude(kindFilter,
			walletrpc.EntryKind_ENTRY_KIND_SEND) {

		ledgerEntries, err := h.collectLedgerEntries(
			ctx, req.GetOffset(), limit,
		)
		if err != nil {
			return nil, fmt.Errorf("collect ledger entries: %w",
				err)
		}

		entries = append(entries, ledgerEntries...)
	}

	// Apply wallet-level overlay (deadline timeout) BEFORE filtering so
	// a stuck entry appears as FAILED in the wallet view even when the
	// caller asked for pending_only=false.
	h.applyOverlays(entries)

	// Dedupe by canonical id BEFORE filtering and sorting. An OOR-backed
	// SEND surfaces once from collectSwapEntries (swap subsystem) and
	// once from collectLedgerEntries (ledger projection); after the
	// canonical-id projection both rows resolve to the same id, but the
	// merger has them as two distinct rows. Keep the most-recent
	// updated_at (the ledger row may carry a confirmed txid the swap
	// summary does not).
	entries = dedupeByID(entries)

	filtered := filterEntries(entries, req.GetPendingOnly(), kindFilter)

	// Sort by updated_at descending — most recent first.
	sort.SliceStable(filtered, func(i, j int) bool {
		return filtered[i].GetUpdatedAtUnix() >
			filtered[j].GetUpdatedAtUnix()
	})

	total := uint32(len(filtered))
	paged := paginate(filtered, req.GetOffset(), limit)

	return &walletrpc.ActivityList{
		Entries: paged,
		Total:   total,
	}, nil
}

// listVTXOs returns the live VTXO inventory. The daemon's ListVTXOs RPC
// is filtered to live + spendable statuses so the wallet view never
// surfaces internal terminal states (forfeited, spent, failed) the user
// has no agency over.
func (h *history) listVTXOs(ctx context.Context, req *walletrpc.ListRequest) (
	*walletrpc.VTXOInventory, error) {

	if h.deps.RPCServer == nil {
		return nil, ErrSwapBackendUnavailable
	}

	limit := h.deps.resolveListLimit(req.GetLimit())

	resp, err := h.deps.RPCServer.ListVTXOs(
		ctx, &daemonrpc.ListVTXOsRequest{
			StatusFilter: daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("list vtxos: %w", err)
	}

	all := make([]*walletrpc.WalletVTXO, 0, len(resp.GetVtxos()))
	for _, v := range resp.GetVtxos() {
		w, keep := walletVTXOFromDaemon(v)
		if !keep {
			continue
		}
		all = append(all, w)
	}

	total := uint32(len(all))
	paged := paginateVTXOs(all, req.GetOffset(), limit)

	return &walletrpc.VTXOInventory{
		Vtxos: paged,
		Total: total,
	}, nil
}

// listOnchain returns the on-chain transaction history page. It composes
// the same daemonrpc.ListTransactions surface the legacy `listtransactions`
// CLI verb used, but flattens the ledger row shape onto the
// wallet-facing OnchainTx type so internal correlators don't leak into
// the user surface.
func (h *history) listOnchain(ctx context.Context, req *walletrpc.ListRequest) (
	*walletrpc.OnchainHistory, error) {

	if h.deps.RPCServer == nil {
		return nil, ErrSwapBackendUnavailable
	}

	limit := h.deps.resolveListLimit(req.GetLimit())

	resp, err := h.deps.RPCServer.ListTransactions(
		ctx, &daemonrpc.ListTransactionsRequest{
			Limit:  limit,
			Offset: req.GetOffset(),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("list onchain transactions: %w", err)
	}

	txs := make([]*walletrpc.OnchainTx, 0, len(resp.GetTransactions()))
	for _, t := range resp.GetTransactions() {
		txs = append(txs, onchainTxFromLedgerRow(t))
	}

	return &walletrpc.OnchainHistory{
		Txs:     txs,
		Total:   uint32(len(txs)),
		HasMore: resp.GetHasMore(),
	}, nil
}

// collectSwapEntries pulls the latest swap summaries from the swap
// subserver and normalizes them into WalletEntry rows. Pay rows become
// SEND, receive rows become RECV; the underlying SwapDirection enum drives
// the mapping.
func (h *history) collectSwapEntries(ctx context.Context, pendingOnly bool) (
	[]*walletrpc.WalletEntry, error) {

	if h.deps.SwapService == nil {
		return nil, nil
	}

	resp, err := h.deps.SwapService.ListSwaps(
		ctx, &swapclientrpc.ListSwapsRequest{
			PendingOnly: pendingOnly,
		},
	)
	if err != nil {
		return nil, err
	}

	out := make([]*walletrpc.WalletEntry, 0, len(resp.GetSwaps()))
	for _, s := range resp.GetSwaps() {
		// The wallet layer does not surface vHTLC outpoints or
		// session IDs; counterparty for swaps is the payment hash
		// (truncated). Note is empty here because the swap
		// subsystem does not persist the caller's note.
		// History lists swaps in both directions; let direction
		// drive the kind here (callers that own the SEND/RECV
		// intent pass an explicit override on submit).
		entry := swapEntryFromSummary(
			s, "", s.GetPaymentHash(),
			walletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
		)
		// entry.Id is the swap row's payment_hash — that is the
		// stable wallet-layer canonical id for SEND-invoice and
		// RECV across their entire lifecycle.
		out = append(out, entry)
	}

	return out, nil
}

// collectLedgerEntries reads the daemon's unified ledger+sweep page and
// projects each row onto a WalletEntry. Boarding rows become DEPOSIT,
// sweep rows become EXIT, OOR rows become SEND or RECV based on the
// debit/credit account convention. Rows the wallet layer cannot classify
// are dropped so the user surface stays clean.
//
// The (offset, limit) pair is the caller's unified-merger page. We pull
// `offset + limit` rows from the ledger source (capped at the daemon's
// internal max via the server-side clamp) so the in-memory paginate
// after the swap-and-ledger merge has enough rows to satisfy a page
// past the first window. Without this plumbing, page 2+ of wallet
// history returns no ledger rows because the daemon got Limit=limit
// and Offset=0 and only the first `limit` rows ever came back.
func (h *history) collectLedgerEntries(ctx context.Context, offset,
	limit uint32) ([]*walletrpc.WalletEntry, error) {

	if h.deps.RPCServer == nil {
		return nil, nil
	}

	// Compute the pull size, guarding against uint32 overflow on the
	// addition. The daemon clamps further to maxTransactionHistoryLimit
	// (1000) server-side.
	pullLimit := offset + limit
	if pullLimit < offset {
		pullLimit = limit
	}

	resp, err := h.deps.RPCServer.ListTransactions(
		ctx, &daemonrpc.ListTransactionsRequest{
			Limit: pullLimit,
		},
	)
	if err != nil {
		return nil, err
	}

	out := make([]*walletrpc.WalletEntry, 0, len(resp.GetTransactions()))
	for _, t := range resp.GetTransactions() {
		entry, ok := walletEntryFromLedgerRow(t)
		if !ok {
			continue
		}
		// v1 SCOPE: EXIT and DEPOSIT ledger rows carry txid but no
		// link back to the original pending intent, so they
		// surface under their synthetic id (the ledger txid or
		// the entry_id fallback). See swapwallet/doc.go.
		out = append(out, entry)
	}

	return out, nil
}

// applyOverlays elevates entries to FAILED in place when the runtime has
// flagged them past their wallet-level deadline. The underlying swap or
// ledger row is left alone; the elevation is a wallet-surface projection.
func (h *history) applyOverlays(entries []*walletrpc.WalletEntry) {
	for _, e := range entries {
		if e.GetStatus() != walletrpc.EntryStatus_ENTRY_STATUS_PENDING {
			continue
		}
		ov, ok := h.runtime.overlayFor(e.GetId())
		if !ok {
			continue
		}
		e.Status = ov.status
		if ov.failureReason != "" {
			e.FailureReason = ov.failureReason
		}
	}
}

// shouldInclude reports whether a kind should be queried based on the
// caller's kindFilter. An empty filter means include everything.
func (h *history) shouldInclude(filter map[walletrpc.EntryKind]struct{},
	kind walletrpc.EntryKind) bool {

	if len(filter) == 0 {
		return true
	}
	_, ok := filter[kind]

	return ok
}

// buildKindFilter materializes a set from the caller's repeated EntryKind
// filter. An empty input yields a nil set so the merger treats the call as
// "all kinds."
func buildKindFilter(kinds []walletrpc.EntryKind,
) (map[walletrpc.EntryKind]struct{}, error) {

	if len(kinds) == 0 {
		return nil, nil
	}

	out := make(map[walletrpc.EntryKind]struct{}, len(kinds))
	for _, k := range kinds {
		switch k {
		case walletrpc.EntryKind_ENTRY_KIND_SEND,
			walletrpc.EntryKind_ENTRY_KIND_RECV,
			walletrpc.EntryKind_ENTRY_KIND_DEPOSIT,
			walletrpc.EntryKind_ENTRY_KIND_EXIT:

		default:
			return nil, fmt.Errorf("%w: %v", ErrUnsupportedKind, k)
		}
		out[k] = struct{}{}
	}

	return out, nil
}

// filterEntries applies pending_only and kind filters in one pass.
func filterEntries(entries []*walletrpc.WalletEntry, pendingOnly bool,
	kindFilter map[walletrpc.EntryKind]struct{}) []*walletrpc.WalletEntry {

	out := entries[:0]
	for _, e := range entries {
		if pendingOnly && e.GetStatus() !=
			walletrpc.EntryStatus_ENTRY_STATUS_PENDING {

			continue
		}
		if len(kindFilter) > 0 {
			if _, ok := kindFilter[e.GetKind()]; !ok {
				continue
			}
		}
		out = append(out, e)
	}

	return out
}

// dedupeByID collapses entries that share a canonical id, keeping the
// most-recent UpdatedAtUnix. Rows without an id (e.g. ledger fallbacks
// that synthesize "ledger-N") are never deduped; collapsing them by ""
// would silently merge unrelated rows.
//
// Returns a fresh slice; the input slice is sorted in place but not
// otherwise mutated. The caller (history.List) re-sorts after filtering
// anyway, so the local sort here is not load-bearing for output order —
// it only governs which duplicate wins.
func dedupeByID(entries []*walletrpc.WalletEntry) []*walletrpc.WalletEntry {
	if len(entries) <= 1 {
		return entries
	}

	// Sort by updated_at desc so the first occurrence of each id is the
	// most-recent one; the set lookup then keeps it.
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].GetUpdatedAtUnix() >
			entries[j].GetUpdatedAtUnix()
	})

	seen := fn.NewSet[string]()
	out := make([]*walletrpc.WalletEntry, 0, len(entries))
	for _, e := range entries {
		id := e.GetId()
		if id == "" {
			out = append(out, e)

			continue
		}
		if seen.Contains(id) {
			continue
		}
		seen.Add(id)
		out = append(out, e)
	}

	return out
}

// paginate slices entries by offset and limit, returning a fresh slice so
// the caller cannot mutate the merger's internal buffer.
func paginate(entries []*walletrpc.WalletEntry, offset,
	limit uint32) []*walletrpc.WalletEntry {

	if offset >= uint32(len(entries)) {
		return nil
	}
	end := offset + limit
	if end > uint32(len(entries)) {
		end = uint32(len(entries))
	}
	page := make([]*walletrpc.WalletEntry, 0, end-offset)
	page = append(page, entries[offset:end]...)

	return page
}

// walletEntryFromLedgerRow projects one ledger/sweep row onto a WalletEntry.
// Returns (entry, true) when the row maps onto a user-facing wallet
// operation; (nil, false) when the row should be hidden from the wallet
// view (e.g. internal fee accounting rows we don't yet model).
func walletEntryFromLedgerRow(t *daemonrpc.TransactionHistoryEntry) (
	*walletrpc.WalletEntry, bool) {

	if t == nil {
		return nil, false
	}

	kind, direction, ok := classifyLedgerRow(t)
	if !ok {
		return nil, false
	}

	id := t.GetTxid()
	if id == "" {
		// Fall back to entry_id for ledger-backed rows so every
		// WalletEntry has a stable id.
		id = fmt.Sprintf("ledger-%d", t.GetEntryId())
	}

	amount := t.GetAmountSat() * direction

	return &walletrpc.WalletEntry{
		Id:            id,
		Kind:          kind,
		Status:        statusForLedgerRow(t, kind),
		AmountSat:     amount,
		FeeSat:        t.GetFeeSat(),
		Counterparty:  ledgerCounterparty(t, kind),
		CreatedAtUnix: t.GetCreatedAtUnixS(),
		UpdatedAtUnix: t.GetCreatedAtUnixS(),
	}, true
}

// statusForLedgerRow folds the ledger row's confirmation_status and the
// wallet-facing entry kind into the flat EntryStatus the API surfaces.
// Most rows are a straight projection of the chain confirmation
// signal, but DEPOSIT rows from the `wallet_utxo_created` event are a
// special case: chain confirmation of the boarding UTXO only means the
// funds landed on-chain, NOT that they have been boarded into a live
// VTXO. Reporting COMPLETE there is misleading — under
// eagerroundjoin=false the deposit can sit forever in that state
// while remaining unspendable through any in-Ark flow. Keep DEPOSIT
// rows backed by wallet_utxo_created at PENDING until a follow-up
// boarding_fee_paid (or equivalent "boarded" ledger entry) flips
// them to COMPLETE.
func statusForLedgerRow(t *daemonrpc.TransactionHistoryEntry,
	kind walletrpc.EntryKind) walletrpc.EntryStatus {

	confirmation := statusFromLedgerConfirmation(t.GetConfirmationStatus())

	if kind == walletrpc.EntryKind_ENTRY_KIND_DEPOSIT &&
		t.GetSubtype() == ledger.EventWalletUTXOCreated &&
		confirmation == walletrpc.EntryStatus_ENTRY_STATUS_COMPLETE {

		// TODO(#503 follow-up): flip back to COMPLETE once the
		// ledger emits a boarding_fee_paid (or equivalent
		// "boarded into a round" entry) for this deposit. Until
		// that signal exists, DEPOSIT rows backed by
		// wallet_utxo_created are stuck at PENDING here — which
		// is correct ("on-chain, not yet boarded") but loses the
		// eventual "boarded" transition.
		return walletrpc.EntryStatus_ENTRY_STATUS_PENDING
	}

	return confirmation
}

// classifyLedgerRow maps a ledger row's type+subtype+account triple onto
// a WalletEntry kind and an amount-sign direction (+1 incoming, -1
// outgoing). Returns ok=false for rows that don't map onto a user-facing
// wallet operation (internal fee bookkeeping, intermediate states).
func classifyLedgerRow(t *daemonrpc.TransactionHistoryEntry) (
	walletrpc.EntryKind, int64, bool) {

	switch t.GetType() {
	case "boarding":
		return walletrpc.EntryKind_ENTRY_KIND_DEPOSIT, +1, true

	case "sweep":
		return walletrpc.EntryKind_ENTRY_KIND_EXIT, -1, true

	case "oor":
		// The ledger books OOR receives as
		// `debit vtxo_balance, credit transfers_in` and OOR sends
		// as `debit transfers_out, credit vtxo_balance` (see
		// ledger/handlers.go handleVTXOReceived/handleVTXOSent).
		// Classify the wallet direction off the counterparty side
		// so we don't depend on which leg the daemon row carries.
		switch {
		case t.GetCreditAccount() == ledger.AccountTransfersIn:
			return walletrpc.EntryKind_ENTRY_KIND_RECV, +1, true

		case t.GetDebitAccount() == ledger.AccountTransfersOut:
			return walletrpc.EntryKind_ENTRY_KIND_SEND, -1, true
		}

		// OOR rows without a recognisable counterparty account are
		// internal bookkeeping — hide them from the wallet view.
		return walletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED, 0, false

	default:
		// Round-level and other rows are not yet modeled as wallet
		// operations in v1.
		return walletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED, 0, false
	}
}

// statusFromLedgerConfirmation maps the ledger row's confirmation_status
// string onto the flat wallet status.
func statusFromLedgerConfirmation(s string) walletrpc.EntryStatus {
	switch s {
	case "confirmed", "swept":
		return walletrpc.EntryStatus_ENTRY_STATUS_COMPLETE

	case "failed":
		return walletrpc.EntryStatus_ENTRY_STATUS_FAILED

	default:
		return walletrpc.EntryStatus_ENTRY_STATUS_PENDING
	}
}

// ledgerCounterparty renders a short, display-friendly counterparty for a
// ledger-derived WalletEntry. For DEPOSIT rows it returns the literal
// "boarding"; for EXIT rows it returns the txid (truncated); for SEND/RECV
// OOR rows it returns the txid or an empty string when the row carries
// none.
func ledgerCounterparty(t *daemonrpc.TransactionHistoryEntry,
	kind walletrpc.EntryKind) string {

	switch kind {
	case walletrpc.EntryKind_ENTRY_KIND_DEPOSIT:
		return "boarding"

	case walletrpc.EntryKind_ENTRY_KIND_EXIT:
		return truncate(t.GetTxid(), truncatedCounterpartyLen)

	default:
		return truncate(t.GetTxid(), truncatedCounterpartyLen)
	}
}

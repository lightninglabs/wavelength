//go:build walletrpc && swapruntime

package swapwallet

import (
	"context"
	"fmt"
	"sort"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
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

// List returns the unified history page. The page size is capped at the
// daemon-level maximum so a malformed request cannot fan out unbounded
// work; sources are queried with the request's own limit so per-source
// pagination remains the per-source contract.
func (h *history) List(ctx context.Context,
	req *walletrpc.ListRequest) (*walletrpc.ListResponse, error) {

	if h == nil || h.deps == nil {
		return nil, ErrSwapBackendUnavailable
	}

	limit := h.deps.resolveListLimit(req.GetLimit())
	kindFilter := buildKindFilter(req.GetKinds())

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

		ledgerEntries, err := h.collectLedgerEntries(ctx, limit)
		if err != nil {
			return nil, fmt.Errorf("collect ledger entries: %w", err)
		}

		entries = append(entries, ledgerEntries...)
	}

	// Apply wallet-level overlay (deadline timeout) BEFORE filtering so
	// a stuck entry appears as FAILED in the wallet view even when the
	// caller asked for pending_only=false.
	h.applyOverlays(entries)

	filtered := filterEntries(entries, req.GetPendingOnly(), kindFilter)

	// Sort by updated_at descending — most recent first.
	sort.SliceStable(filtered, func(i, j int) bool {
		return filtered[i].GetUpdatedAtUnix() >
			filtered[j].GetUpdatedAtUnix()
	})

	total := uint32(len(filtered))
	paged := paginate(filtered, req.GetOffset(), limit)

	return &walletrpc.ListResponse{
		Entries: paged,
		Total:   total,
	}, nil
}

// collectSwapEntries pulls the latest swap summaries from the swap
// subserver and normalizes them into WalletEntry rows. Pay rows become
// SEND, receive rows become RECV; the underlying SwapDirection enum drives
// the mapping.
func (h *history) collectSwapEntries(ctx context.Context,
	pendingOnly bool) ([]*walletrpc.WalletEntry, error) {

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
		entry := swapEntryFromSummary(s, "", s.GetPaymentHash())
		// Project onto the canonical intent id. For swap rows the
		// canonical id is the payment hash, which is already the
		// id returned by swapEntryFromSummary, so the lookup is a
		// no-op in the common case and a stable fallback when the
		// SDK ever changes id semantics.
		entry.Id = h.runtime.resolveCanonicalID(
			entry.GetId(), s.GetPaymentHash(), "", nil, "",
		)
		out = append(out, entry)
	}

	return out, nil
}

// collectLedgerEntries reads the daemon's unified ledger+sweep page and
// projects each row onto a WalletEntry. Boarding rows become DEPOSIT,
// sweep rows become EXIT, OOR rows become SEND or RECV based on the
// debit/credit account convention. Rows the wallet layer cannot classify
// are dropped so the user surface stays clean.
func (h *history) collectLedgerEntries(ctx context.Context,
	limit uint32) ([]*walletrpc.WalletEntry, error) {

	if h.deps.RPCServer == nil {
		return nil, nil
	}

	resp, err := h.deps.RPCServer.ListTransactions(
		ctx, &daemonrpc.ListTransactionsRequest{
			Limit: limit,
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
		// Project onto the canonical intent id when the runtime has
		// a registered intent that owns this ledger row. The merger
		// uses txid as the primary projection key for sweep and OOR
		// rows; outpoint and address keys are used by the monitor
		// loop, where the underlying source carries them directly.
		entry.Id = h.runtime.resolveCanonicalID(
			entry.GetId(), "", t.GetTxid(), nil, "",
		)
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
) map[walletrpc.EntryKind]struct{} {

	if len(kinds) == 0 {
		return nil
	}

	out := make(map[walletrpc.EntryKind]struct{}, len(kinds))
	for _, k := range kinds {
		out[k] = struct{}{}
	}

	return out
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
		Status:        statusFromLedgerConfirmation(t.GetConfirmationStatus()),
		AmountSat:     amount,
		FeeSat:        t.GetFeeSat(),
		Counterparty:  ledgerCounterparty(t, kind),
		CreatedAtUnix: t.GetCreatedAtUnixS(),
		UpdatedAtUnix: t.GetCreatedAtUnixS(),
	}, true
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
		// Use the credit/debit accounts to decide direction.
		// Wallet-credited rows are incoming; wallet-debited rows
		// are outgoing.
		if t.GetCreditAccount() == "wallet_in" {
			return walletrpc.EntryKind_ENTRY_KIND_RECV, +1, true
		}
		if t.GetDebitAccount() == "wallet_out" {
			return walletrpc.EntryKind_ENTRY_KIND_SEND, -1, true
		}

		// OOR rows without a recognisable wallet account are
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

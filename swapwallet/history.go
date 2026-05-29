//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// history merges entries from the daemon's existing history surfaces
// (swap subserver ListSwaps + RPCServer.ListTransactions) into the single
// flat WalletEntry shape exposed by walletdkrpc.WalletService.List.
//
// The merge is deliberately consumer-side: every source already has its
// own canonical persistence (swap DB, ledger entries, boarding_sweeps),
// and swapwallet must not duplicate that state. The merger simply pulls
// the latest pages, normalizes each row to a WalletEntry, applies the
// caller's filters, sorts by updated_at descending, paginates, and applies
// wallet-local deadline projections.
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
func (h *history) List(ctx context.Context, req *walletdkrpc.ListRequest) (
	*walletdkrpc.ListResponse, error) {

	if h == nil || h.deps == nil {
		return nil, ErrSwapBackendUnavailable
	}

	switch req.GetView() {
	case walletdkrpc.ListView_LIST_VIEW_VTXOS:
		body, err := h.listVTXOs(ctx, req)
		if err != nil {
			return nil, err
		}

		return &walletdkrpc.ListResponse{
			Body: &walletdkrpc.ListResponse_Vtxos{
				Vtxos: body,
			},
		}, nil

	case walletdkrpc.ListView_LIST_VIEW_ONCHAIN:
		body, err := h.listOnchain(ctx, req)
		if err != nil {
			return nil, err
		}

		return &walletdkrpc.ListResponse{
			Body: &walletdkrpc.ListResponse_Onchain{
				Onchain: body,
			},
		}, nil

	case walletdkrpc.ListView_LIST_VIEW_ACTIVITY,
		walletdkrpc.ListView_LIST_VIEW_UNSPECIFIED:

		body, err := h.listActivity(ctx, req)
		if err != nil {
			return nil, err
		}

		return &walletdkrpc.ListResponse{
			Body: &walletdkrpc.ListResponse_Activity{
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
	req *walletdkrpc.ListRequest) (*walletdkrpc.ActivityList, error) {

	limit := h.deps.resolveListLimit(req.GetLimit())
	kindFilter, err := buildKindFilter(req.GetKinds())
	if err != nil {
		return nil, err
	}

	var (
		entries          []*walletdkrpc.WalletEntry
		swapEntries      []*walletdkrpc.WalletEntry
		swapCorrelations swapOORCorrelations
	)
	swapEntryIDs := make(map[string]struct{})

	if h.shouldInclude(kindFilter, walletdkrpc.EntryKind_ENTRY_KIND_SEND) ||
		h.shouldInclude(
			kindFilter, walletdkrpc.EntryKind_ENTRY_KIND_RECV,
		) {

		var err error
		swapEntries, swapCorrelations, err = h.collectSwapEntries(ctx)
		if err != nil {
			return nil, fmt.Errorf("collect swap entries: %w", err)
		}

		for _, entry := range swapEntries {
			swapEntryIDs[entry.GetId()] = struct{}{}
		}
		entries = append(entries, swapEntries...)
	}

	if h.shouldInclude(kindFilter, walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT) ||
		h.shouldInclude(
			kindFilter, walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
		) ||
		h.shouldInclude(
			kindFilter, walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		) {

		ledgerEntries, err := h.collectLedgerEntries(
			ctx, req.GetOffset(), limit, swapCorrelations,
		)
		if err != nil {
			return nil, fmt.Errorf("collect ledger entries: %w",
				err)
		}

		entries = append(entries, ledgerEntries...)
	}

	if h.shouldInclude(kindFilter, walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT) {
		pendingBoarding, err := h.collectPendingBoardingEntries(ctx)
		if err != nil {
			return nil, fmt.Errorf("collect pending boarding "+
				"entries: %w", err)
		}

		entries = append(entries, pendingBoarding...)
	}

	if h.shouldInclude(kindFilter, walletdkrpc.EntryKind_ENTRY_KIND_EXIT) {
		exitEntries, err := h.collectUnilateralExitEntries(ctx)
		if err != nil {
			return nil, fmt.Errorf("collect unilateral exit "+
				"entries: %w", err)
		}
		entries = append(entries, exitEntries...)

		entries = append(
			entries, h.collectWalletLocalPendingEntries(ctx)...,
		)
	}

	// Apply wallet-level overlay (deadline timeout) BEFORE filtering so
	// a stuck entry appears as FAILED in the wallet view even when the
	// caller asked for pending_only=false.
	h.applyOverlays(entries, swapEntryIDs)

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

	return &walletdkrpc.ActivityList{
		Entries: paged,
		Total:   total,
	}, nil
}

// listVTXOs returns the live VTXO inventory. The daemon's ListVTXOs RPC
// is filtered to live + spendable statuses so the wallet view never
// surfaces internal terminal states (forfeited, spent, failed) the user
// has no agency over.
func (h *history) listVTXOs(ctx context.Context, req *walletdkrpc.ListRequest) (
	*walletdkrpc.VTXOInventory, error) {

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

	all := make([]*walletdkrpc.WalletVTXO, 0, len(resp.GetVtxos()))
	for _, v := range resp.GetVtxos() {
		w, keep := walletVTXOFromDaemon(v)
		if !keep {
			continue
		}
		all = append(all, w)
	}

	total := uint32(len(all))
	paged := paginateVTXOs(all, req.GetOffset(), limit)

	return &walletdkrpc.VTXOInventory{
		Vtxos: paged,
		Total: total,
	}, nil
}

// listOnchain returns the on-chain transaction history page. It composes
// the same daemonrpc.ListTransactions surface the legacy `listtransactions`
// CLI verb used, but flattens the ledger row shape onto the
// wallet-facing OnchainTx type so internal correlators don't leak into
// the user surface.
func (h *history) listOnchain(ctx context.Context,
	req *walletdkrpc.ListRequest) (*walletdkrpc.OnchainHistory, error) {

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

	txs := make([]*walletdkrpc.OnchainTx, 0, len(resp.GetTransactions()))
	for _, t := range resp.GetTransactions() {
		txs = append(txs, onchainTxFromLedgerRow(t))
	}

	return &walletdkrpc.OnchainHistory{
		Txs:     txs,
		Total:   uint32(len(txs)),
		HasMore: resp.GetHasMore(),
	}, nil
}

// collectSwapEntries pulls the latest swap summaries from the swap
// subserver and normalizes them into WalletEntry rows. Pay rows become
// SEND, receive rows become RECV; the underlying SwapDirection enum drives
// the mapping. The full swap set is always queried so terminal swaps can
// still hide their internal OOR ledger rows when callers request --pending.
func (h *history) collectSwapEntries(ctx context.Context) (
	[]*walletdkrpc.WalletEntry, swapOORCorrelations, error) {

	if h.deps.SwapService == nil {
		return nil, swapOORCorrelations{}, nil
	}

	resp, err := h.deps.SwapService.ListSwaps(
		ctx, &swapclientrpc.ListSwapsRequest{},
	)
	if err != nil {
		return nil, swapOORCorrelations{}, err
	}

	out := make([]*walletdkrpc.WalletEntry, 0, len(resp.GetSwaps()))
	for _, s := range resp.GetSwaps() {
		// The wallet layer does not surface vHTLC outpoints or
		// session IDs; counterparty for swaps is the payment hash
		// (truncated). History lists swaps in both directions; let
		// direction drive the kind here (callers that own the
		// SEND/RECV intent pass an explicit override on submit).
		entry := swapEntryFromSummary(
			s, "", "", walletdkrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
		)
		// entry.Id is the swap row's payment_hash — that is the
		// stable wallet-layer canonical id for SEND-invoice and
		// RECV across their entire lifecycle.
		out = append(out, entry)
	}

	return out, swapOORCorrelationsFromSwaps(resp.GetSwaps()), nil
}

// collectPendingBoardingEntries adds an aggregate row for funds seen by the
// on-chain wallet but not yet confirmed into a ledger-backed boarding deposit.
// Once confirmed, ListTransactions owns the durable row and this synthetic
// pending entry naturally disappears.
func (h *history) collectPendingBoardingEntries(ctx context.Context) (
	[]*walletdkrpc.WalletEntry, error) {

	if h.deps.RPCServer == nil {
		return nil, nil
	}

	resp, err := h.deps.RPCServer.GetBalance(
		ctx, &daemonrpc.GetBalanceRequest{},
	)
	if err != nil {
		return nil, err
	}
	if resp.GetBoardingUnconfirmedSat() <= 0 {
		return nil, nil
	}

	now := nowUnix()

	return []*walletdkrpc.WalletEntry{
		{
			Id:            "boarding-unconfirmed",
			Kind:          walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
			Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
			AmountSat:     resp.GetBoardingUnconfirmedSat(),
			Counterparty:  "boarding",
			CreatedAtUnix: now,
			UpdatedAtUnix: now,
			Progress: &walletdkrpc.WalletEntryProgress{
				Phase: walletdkrpc.
					WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_CONFIRMATION,
				PhaseLabel: "waiting_for_confirmation",
			},
		},
	}, nil
}

// collectUnilateralExitEntries synthesizes activity rows for VTXOs already
// handed to the unroll subsystem. The unroll job store is the durable source
// of truth for unilateral exits; activity consumes it through the existing
// VTXO inventory plus per-outpoint ExitStatus surface.
func (h *history) collectUnilateralExitEntries(ctx context.Context) (
	[]*walletdkrpc.WalletEntry, error) {

	if h.deps.RPCServer == nil {
		return nil, nil
	}

	resp, err := h.deps.RPCServer.ListVTXOs(
		ctx, &daemonrpc.ListVTXOsRequest{
			StatusFilter: daemonrpc.
				VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("list unilateral-exit vtxos: %w", err)
	}

	pendingByID := make(map[string]*walletdkrpc.WalletEntry)
	if h.runtime != nil {
		for _, entry := range h.runtime.pendingSnapshot() {
			pendingByID[entry.GetId()] = entry
		}
	}

	out := make([]*walletdkrpc.WalletEntry, 0, len(resp.GetVtxos()))
	for _, vtxo := range resp.GetVtxos() {
		entry := unilateralExitEntryFromVTXO(vtxo)
		if entry.GetId() == "" {
			continue
		}
		if pending := pendingByID[entry.GetId()]; pending != nil {
			if pending.GetCreatedAtUnix() != 0 {
				entry.CreatedAtUnix = pending.GetCreatedAtUnix()
			}
			if pending.GetUpdatedAtUnix() != 0 {
				entry.UpdatedAtUnix = pending.GetUpdatedAtUnix()
			}
		}

		if err := h.decorateExitEntry(ctx, entry); err != nil {
			return nil, err
		}

		out = append(out, entry)
	}

	return out, nil
}

// collectWalletLocalPendingEntries returns pending rows created by wallet RPC
// calls before their durable terminal source is available. Cooperative leave
// uses this path immediately after Send returns; unilateral exit uses it until
// the unroll status/VTXO projection catches up.
func (h *history) collectWalletLocalPendingEntries(
	ctx context.Context) []*walletdkrpc.WalletEntry {

	if h.runtime == nil {
		return nil
	}

	entries := h.runtime.pendingSnapshot()
	for _, entry := range entries {
		if entry.GetKind() != walletdkrpc.EntryKind_ENTRY_KIND_EXIT {
			continue
		}

		// Status lookup is best-effort for wallet-local entries. A
		// missing unroll job usually means this is a cooperative
		// leave pending row, not a unilateral exit.
		_ = h.decorateExitEntry(ctx, entry)
	}

	return entries
}

// unilateralExitEntryFromVTXO projects a VTXO in UNILATERAL_EXIT into a
// wallet-facing EXIT activity row. The amount is negative because value is
// leaving Ark custody.
func unilateralExitEntryFromVTXO(v *daemonrpc.VTXO) *walletdkrpc.WalletEntry {
	if v == nil {
		return nil
	}

	now := nowUnix()

	return &walletdkrpc.WalletEntry{
		Id:            v.GetOutpoint(),
		Kind:          walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
		Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     -v.GetAmountSat(),
		Counterparty:  "unilateral",
		CreatedAtUnix: now,
		UpdatedAtUnix: now,
		Progress: &walletdkrpc.WalletEntryProgress{
			Phase: walletdkrpc.
				WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING,
			PhaseLabel:   "unilateral_exit",
			VtxoOutpoint: v.GetOutpoint(),
		},
	}
}

// decorateExitEntry applies unroll status to an EXIT entry when the daemon has
// a unilateral-exit job for the entry id. Cooperative leave entries do not have
// unroll jobs, so Found=false leaves the original pending row untouched.
func (h *history) decorateExitEntry(ctx context.Context,
	entry *walletdkrpc.WalletEntry) error {

	if h.deps.RPCServer == nil || entry == nil || entry.GetId() == "" {
		return nil
	}

	resp, err := h.deps.RPCServer.GetUnrollStatus(
		ctx, &daemonrpc.GetUnrollStatusRequest{
			Outpoint: entry.GetId(),
		},
	)
	if err != nil {
		return fmt.Errorf("get unroll status %s: %w", entry.GetId(),
			err)
	}
	if !resp.GetFound() {
		return nil
	}

	applyUnrollStatus(entry, resp)

	return nil
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
	limit uint32, correlations swapOORCorrelations) (
	[]*walletdkrpc.WalletEntry, error) {

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

	oorProjection := projectOORLedgerActivity(
		resp.GetTransactions(), correlations,
	)
	out := make([]*walletdkrpc.WalletEntry, 0, len(resp.GetTransactions()))
	for _, t := range resp.GetTransactions() {
		if _, ok := oorProjection.hidden[t.GetEntryId()]; ok {
			continue
		}

		entry, ok := walletEntryFromLedgerRow(t)
		if !ok {
			continue
		}
		entryID := t.GetEntryId()
		if amount, ok := oorProjection.displayAmountByEntryID[entryID]; ok {
			entry.AmountSat = amount
		}

		// v1 SCOPE: EXIT and DEPOSIT ledger rows carry txid but no
		// link back to the original pending intent, so they
		// surface under their synthetic id (the ledger txid or
		// the entry_id fallback). See swapwallet/doc.go.
		out = append(out, entry)
	}

	return out, nil
}

// swapOORCorrelations keeps the swap-side session metadata needed to hide
// internal OOR ledger legs without matching by amount alone.
type swapOORCorrelations struct {
	payAmountsByFundingSession map[string]map[int64]int
	claimSessions              map[string]struct{}
	refundSessions             map[string]struct{}
}

// swapOORCorrelationsFromSwaps indexes swap session ids by the internal OOR leg
// they explain. Pay swaps hide their funding input only when the same funding
// session returns change and the delta equals the swap amount. Receive swaps
// hide their claim input only when the same claim session materializes a wallet
// VTXO.
func swapOORCorrelationsFromSwaps(
	swaps []*swapclientrpc.SwapSummary) swapOORCorrelations {

	out := swapOORCorrelations{
		payAmountsByFundingSession: make(map[string]map[int64]int),
		claimSessions:              make(map[string]struct{}),
		refundSessions:             make(map[string]struct{}),
	}

	for _, swap := range swaps {
		switch swap.GetDirection() {
		case swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY:
			session := strings.ToLower(swap.GetFundingSessionId())
			amount := swap.GetAmountSat()
			if session != "" && amount > 0 {
				if out.payAmountsByFundingSession[session] == nil {
					out.payAmountsByFundingSession[session] =
						make(map[int64]int)
				}
				out.payAmountsByFundingSession[session][amount]++
			}

			refundSession := strings.ToLower(
				swap.GetRefundSessionId(),
			)
			if refundSession != "" {
				out.refundSessions[refundSession] = struct{}{}
			}

		case swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE:
			session := strings.ToLower(swap.GetClaimSessionId())
			if session == "" {
				continue
			}
			out.claimSessions[session] = struct{}{}
		}
	}

	return out
}

type oorLedgerActivityProjection struct {
	hidden                 map[int64]struct{}
	displayAmountByEntryID map[int64]int64
}

// internalOORLedgerEntries returns ledger entry IDs for OOR legs that are
// internal to swap execution or represent wallet-local OOR change. Inspection
// uses this compact view because it only needs to mark hidden rows.
func internalOORLedgerEntries(rows []*daemonrpc.TransactionHistoryEntry,
	correlations swapOORCorrelations) map[int64]struct{} {

	return projectOORLedgerActivity(rows, correlations).hidden
}

// projectOORLedgerActivity returns the wallet-facing projection for OOR ledger
// rows. Accounting rows stay gross; this helper only hides internal rows and
// computes display amounts for activity.
func projectOORLedgerActivity(rows []*daemonrpc.TransactionHistoryEntry,
	correlations swapOORCorrelations) oorLedgerActivityProjection {

	receivedBySession := make(map[string]int64)
	receiveRowsBySession := make(
		map[string][]*daemonrpc.TransactionHistoryEntry,
	)
	var claimOutputSessions map[string]struct{}
	for _, row := range rows {
		session, ok := oorReceiveSessionID(row)
		if !ok {
			continue
		}

		receivedBySession[session] += row.GetAmountSat()
		receiveRowsBySession[session] = append(
			receiveRowsBySession[session], row,
		)

		if receiveMatchesClaimOutput(row, correlations.claimSessions) {
			if claimOutputSessions == nil {
				claimOutputSessions = make(map[string]struct{})
			}
			claimOutputSessions[session] = struct{}{}
		}
	}

	projection := oorLedgerActivityProjection{
		hidden:                 make(map[int64]struct{}),
		displayAmountByEntryID: make(map[int64]int64),
	}
	for _, row := range rows {
		session, ok := oorSendSessionID(row)
		if !ok {
			continue
		}

		received := receivedBySession[session]
		if received == 0 {
			continue
		}

		for _, receiveRow := range receiveRowsBySession[session] {
			projection.hidden[receiveRow.GetEntryId()] = struct{}{}
		}

		switch delta := row.GetAmountSat() - received; {
		case delta == 0 && internalZeroDeltaSession(
			correlations, claimOutputSessions, session,
		):

			projection.hidden[row.GetEntryId()] = struct{}{}

		case delta > 0:
			amounts := correlations.payAmountsByFundingSession[session]
			if amounts == nil || amounts[delta] == 0 {
				projection.displayAmountByEntryID[row.GetEntryId()] =
					-delta

				continue
			}
			projection.hidden[row.GetEntryId()] = struct{}{}
			amounts[delta]--
		}
	}

	return projection
}

// internalZeroDeltaSession reports whether a balanced same-session OOR
// send+receive pair belongs to a swap-internal claim or refund. Those rows
// are already represented by the swap entry itself.
func internalZeroDeltaSession(correlations swapOORCorrelations,
	claimOutputSessions map[string]struct{}, session string) bool {

	return sessionInSet(correlations.claimSessions, session) ||
		sessionInSet(correlations.refundSessions, session) ||
		sessionInSet(claimOutputSessions, session)
}

// receiveMatchesClaimOutput reports whether an OOR receive row materialized
// the output produced by a receive-swap claim. Some ledgers record the OOR
// session id separately from the materialized output txid, while the swap
// summary stores the claim id as the output txid. Matching both keeps the
// internal send leg hidden in either shape.
func receiveMatchesClaimOutput(row *daemonrpc.TransactionHistoryEntry,
	claimSessions map[string]struct{}) bool {

	if len(claimSessions) == 0 {
		return false
	}

	if row == nil {
		return false
	}

	txid := strings.ToLower(row.GetTxid())
	if txid == "" {
		return false
	}

	return sessionInSet(claimSessions, txid)
}

// sessionInSet reports whether a normalized OOR session id appears in set.
func sessionInSet(set map[string]struct{}, session string) bool {
	_, ok := set[session]

	return ok
}

// oorSendSessionID extracts the session id from a ledger row that spends a VTXO
// through OOR.
func oorSendSessionID(row *daemonrpc.TransactionHistoryEntry) (string, bool) {
	if row == nil || row.GetType() != "oor" ||
		row.GetSubtype() != ledger.EventVTXOSent ||
		row.GetDebitAccount() != ledger.AccountTransfersOut ||
		row.GetCreditAccount() != ledger.AccountVTXOBalance ||
		row.GetAmountSat() <= 0 ||
		len(row.GetSessionId()) != chainhash.HashSize {
		return "", false
	}

	hash, err := chainhash.NewHash(row.GetSessionId())
	if err != nil {
		return "", false
	}

	return hash.String(), true
}

// oorReceiveSessionID extracts the session id from a ledger row that receives a
// VTXO through OOR.
func oorReceiveSessionID(row *daemonrpc.TransactionHistoryEntry) (string,
	bool) {

	session, _, ok := oorReceiveRef(row)

	return session, ok
}

// oorReceiveRef extracts the OOR output reference from structured ledger
// fields.
func oorReceiveRef(row *daemonrpc.TransactionHistoryEntry) (string, uint32,
	bool) {

	if row == nil || row.GetType() != "oor" ||
		row.GetSubtype() != ledger.EventVTXOReceived ||
		row.GetDebitAccount() != ledger.AccountVTXOBalance ||
		row.GetCreditAccount() != ledger.AccountTransfersIn ||
		row.GetAmountSat() <= 0 {
		return "", 0, false
	}

	var session string
	if len(row.GetSessionId()) > 0 {
		if len(row.GetSessionId()) != chainhash.HashSize {
			return "", 0, false
		}

		hash, err := chainhash.NewHash(row.GetSessionId())
		if err != nil {
			return "", 0, false
		}

		session = hash.String()
	} else {
		session = strings.ToLower(row.GetTxid())
		if len(session) != 64 {
			return "", 0, false
		}
		if _, err := hex.DecodeString(session); err != nil {
			return "", 0, false
		}
	}

	if row.GetOutputIndex() < 0 {
		return "", 0, false
	}

	return session, uint32(row.GetOutputIndex()), true
}

// applyOverlays elevates entries to FAILED in place when the runtime has
// flagged them past their wallet-level deadline. The underlying swap or
// ledger row is left alone; the elevation is a wallet-surface projection.
func (h *history) applyOverlays(entries []*walletdkrpc.WalletEntry,
	swapEntryIDs map[string]struct{}) {

	for _, e := range entries {
		if e.GetStatus() != walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING {
			continue
		}
		if _, ok := swapEntryIDs[e.GetId()]; ok {
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
func (h *history) shouldInclude(filter map[walletdkrpc.EntryKind]struct{},
	kind walletdkrpc.EntryKind) bool {

	if len(filter) == 0 {
		return true
	}
	_, ok := filter[kind]

	return ok
}

// buildKindFilter materializes a set from the caller's repeated EntryKind
// filter. An empty input yields a nil set so the merger treats the call as
// "all kinds."
func buildKindFilter(kinds []walletdkrpc.EntryKind,
) (map[walletdkrpc.EntryKind]struct{}, error) {

	if len(kinds) == 0 {
		return nil, nil
	}

	out := make(map[walletdkrpc.EntryKind]struct{}, len(kinds))
	for _, k := range kinds {
		switch k {
		case walletdkrpc.EntryKind_ENTRY_KIND_SEND,
			walletdkrpc.EntryKind_ENTRY_KIND_RECV,
			walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
			walletdkrpc.EntryKind_ENTRY_KIND_EXIT:

		default:
			return nil, fmt.Errorf("%w: %v", ErrUnsupportedKind, k)
		}
		out[k] = struct{}{}
	}

	return out, nil
}

// filterEntries applies pending_only and kind filters in one pass.
func filterEntries(entries []*walletdkrpc.WalletEntry, pendingOnly bool,
	kindFilter map[walletdkrpc.EntryKind]struct{}) []*walletdkrpc.WalletEntry {

	out := entries[:0]
	for _, e := range entries {
		if pendingOnly && e.GetStatus() !=
			walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING {

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
func dedupeByID(entries []*walletdkrpc.WalletEntry) []*walletdkrpc.WalletEntry {
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
	out := make([]*walletdkrpc.WalletEntry, 0, len(entries))
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
func paginate(entries []*walletdkrpc.WalletEntry, offset,
	limit uint32) []*walletdkrpc.WalletEntry {

	if offset >= uint32(len(entries)) {
		return nil
	}
	end := offset + limit
	if end > uint32(len(entries)) {
		end = uint32(len(entries))
	}
	page := make([]*walletdkrpc.WalletEntry, 0, end-offset)
	page = append(page, entries[offset:end]...)

	return page
}

// walletEntryFromLedgerRow projects one ledger/sweep row onto a WalletEntry.
// Returns (entry, true) when the row maps onto a user-facing wallet
// operation; (nil, false) when the row should be hidden from the wallet
// view (e.g. internal fee accounting rows we don't yet model).
func walletEntryFromLedgerRow(t *daemonrpc.TransactionHistoryEntry) (
	*walletdkrpc.WalletEntry, bool) {

	if t == nil {
		return nil, false
	}

	kind, direction, ok := classifyLedgerRow(t)
	if !ok {
		return nil, false
	}

	id := ledgerActivityID(t, kind)
	if id == "" {
		// Fall back to entry_id for ledger-backed rows so every
		// WalletEntry has a stable id.
		id = fmt.Sprintf("ledger-%d", t.GetEntryId())
	}

	amount := t.GetAmountSat() * direction

	return &walletdkrpc.WalletEntry{
		Id:            id,
		Kind:          kind,
		Status:        statusForLedgerRow(t),
		AmountSat:     amount,
		FeeSat:        t.GetFeeSat(),
		Counterparty:  ledgerCounterparty(t, kind),
		CreatedAtUnix: t.GetCreatedAtUnixS(),
		UpdatedAtUnix: t.GetCreatedAtUnixS(),
		Progress:      progressFromLedgerRow(t),
	}, true
}

// ledgerActivityID returns the stable activity id for one ledger-backed row.
// Boarding UTXO creation rows are identified by outpoint, not bare txid, so a
// single Bitcoin transaction that pays multiple wallet-owned outputs surfaces
// every deposit instead of collapsing them during activity de-duplication.
func ledgerActivityID(t *daemonrpc.TransactionHistoryEntry,
	kind walletdkrpc.EntryKind) string {

	if t == nil || t.GetTxid() == "" {
		return ""
	}

	if kind == walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT &&
		t.GetSubtype() == ledger.EventWalletUTXOCreated &&
		t.GetOutputIndex() >= 0 {
		return fmt.Sprintf("%s:%d", t.GetTxid(), t.GetOutputIndex())
	}

	return t.GetTxid()
}

// statusForLedgerRow folds the ledger row's confirmation_status into the
// flat EntryStatus the API surfaces. Detailed lifecycle context stays in
// WalletEntry.progress and InspectActivity; confirmed on-chain deposits stay
// pending while they are still waiting to board into a confirmed round.
func statusForLedgerRow(
	t *daemonrpc.TransactionHistoryEntry) walletdkrpc.EntryStatus {

	if t.GetType() == "oor" && t.GetConfirmationStatus() == "recorded" {
		return walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE
	}

	return statusFromLedgerConfirmation(t.GetConfirmationStatus())
}

// progressFromLedgerRow projects ledger confirmation metadata onto
// WalletEntryProgress.
func progressFromLedgerRow(
	t *daemonrpc.TransactionHistoryEntry) *walletdkrpc.WalletEntryProgress {

	if t == nil {
		return nil
	}

	phase, label := phaseFromLedgerConfirmation(t.GetConfirmationStatus())
	if t.GetType() == "oor" && t.GetConfirmationStatus() == "recorded" {
		phase = walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_CONFIRMED
		label = "confirmed"
	}

	return &walletdkrpc.WalletEntryProgress{
		Phase:              phase,
		PhaseLabel:         label,
		Txid:               t.GetTxid(),
		ConfirmationHeight: t.GetConfirmationHeight(),
	}
}

// classifyLedgerRow maps a ledger row's type+subtype+account triple onto
// a WalletEntry kind and an amount-sign direction (+1 incoming, -1
// outgoing). Returns ok=false for rows that don't map onto a user-facing
// wallet operation (internal fee bookkeeping, intermediate states).
func classifyLedgerRow(t *daemonrpc.TransactionHistoryEntry) (
	walletdkrpc.EntryKind, int64, bool) {

	switch t.GetType() {
	case "boarding":
		return walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT, +1, true

	case "sweep":
		return walletdkrpc.EntryKind_ENTRY_KIND_EXIT, -1, true

	case "oor":
		// The ledger books OOR receives as
		// `debit vtxo_balance, credit transfers_in` and OOR sends
		// as `debit transfers_out, credit vtxo_balance` (see
		// ledger/handlers.go handleVTXOReceived/handleVTXOSent).
		// Classify the wallet direction off the counterparty side
		// so we don't depend on which leg the daemon row carries.
		switch {
		case t.GetCreditAccount() == ledger.AccountTransfersIn:
			return walletdkrpc.EntryKind_ENTRY_KIND_RECV, +1, true

		case t.GetDebitAccount() == ledger.AccountTransfersOut:
			return walletdkrpc.EntryKind_ENTRY_KIND_SEND, -1, true
		}

		// OOR rows without a recognisable counterparty account are
		// internal bookkeeping — hide them from the wallet view.
		return walletdkrpc.EntryKind_ENTRY_KIND_UNSPECIFIED, 0, false

	default:
		// Round-level and other rows are not yet modeled as wallet
		// operations in v1.
		return walletdkrpc.EntryKind_ENTRY_KIND_UNSPECIFIED, 0, false
	}
}

// statusFromLedgerConfirmation maps the ledger row's confirmation_status
// string onto the flat wallet status.
func statusFromLedgerConfirmation(s string) walletdkrpc.EntryStatus {
	switch s {
	case "confirmed", "swept":
		return walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE

	case "failed":
		return walletdkrpc.EntryStatus_ENTRY_STATUS_FAILED

	default:
		return walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING
	}
}

// phaseFromLedgerConfirmation maps the ledger confirmation string to the
// normalized wallet phase and label.
func phaseFromLedgerConfirmation(s string) (walletdkrpc.WalletEntryPhase,
	string) {

	switch s {
	case "boarding":
		return walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING,
			"boarding"

	case "confirmed", "swept":
		return walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_CONFIRMED,
			"confirmed"

	case "failed":
		return walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_FAILED,
			"failed"

	default:
		return walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_PAYMENT_DETECTED,
			"payment_detected"
	}
}

// ledgerCounterparty renders a short, display-friendly counterparty for a
// ledger-derived WalletEntry. For DEPOSIT rows it returns the literal
// "boarding"; for EXIT rows it returns the txid (truncated); for SEND/RECV
// OOR rows it returns the txid or an empty string when the row carries
// none.
func ledgerCounterparty(t *daemonrpc.TransactionHistoryEntry,
	kind walletdkrpc.EntryKind) string {

	switch kind {
	case walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT:
		return "boarding"

	case walletdkrpc.EntryKind_ENTRY_KIND_EXIT:
		return truncate(t.GetTxid(), truncatedCounterpartyLen)

	default:
		return truncate(t.GetTxid(), truncatedCounterpartyLen)
	}
}

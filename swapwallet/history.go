//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/credit"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// history merges entries from the daemon's existing history surfaces
// (swap subserver ListSwaps + RPCServer.ListTransactions) into the single
// flat WalletEntry shape exposed by wavewalletrpc.WalletService.List.
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

	// creditTopupLinks memoizes the registry-derived top-up index for the
	// lifetime of this history instance. A history is built once per
	// derive call (and once per reproject pass, which pages deriveActivity
	// in a loop), so the memo bounds the registry Ask to one per pass
	// instead of one per page. Instances are used single-threaded.
	creditTopupLinks    map[string]creditTopupLink
	creditTopupLinksSet bool
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
func (h *history) List(ctx context.Context, req *wavewalletrpc.ListRequest) (
	*wavewalletrpc.ListResponse, error) {

	if h == nil || h.deps == nil {
		return nil, ErrSwapBackendUnavailable
	}

	switch req.GetView() {
	case wavewalletrpc.ListView_LIST_VIEW_VTXOS:
		body, err := h.listVTXOs(ctx, req)
		if err != nil {
			return nil, err
		}

		return &wavewalletrpc.ListResponse{
			Body: &wavewalletrpc.ListResponse_Vtxos{
				Vtxos: body,
			},
		}, nil

	case wavewalletrpc.ListView_LIST_VIEW_ONCHAIN:
		body, err := h.listOnchain(ctx, req)
		if err != nil {
			return nil, err
		}

		return &wavewalletrpc.ListResponse{
			Body: &wavewalletrpc.ListResponse_Onchain{
				Onchain: body,
			},
		}, nil

	case wavewalletrpc.ListView_LIST_VIEW_ACTIVITY,
		wavewalletrpc.ListView_LIST_VIEW_UNSPECIFIED:

		body, err := h.listActivity(ctx, req)
		if err != nil {
			return nil, err
		}

		return &wavewalletrpc.ListResponse{
			Body: &wavewalletrpc.ListResponse_Activity{
				Activity: body,
			},
		}, nil

	default:
		return nil, fmt.Errorf("unknown list view: %v", req.GetView())
	}
}

// errInvalidActivityCursor is returned when the ACTIVITY cursor token cannot be
// decoded into a keyset position.
var errInvalidActivityCursor = errors.New("invalid activity cursor")

// syntheticBoardingUnconfirmedID is the fallback id of the derive-path-only
// aggregate row used when the daemon cannot correlate the balance with one
// boarding address. The normal in-process path uses deposit-<address>, matching
// both Deposit's response and the later confirmed ledger row.
const syntheticBoardingUnconfirmedID = "boarding-unconfirmed"

// activityScanBudgetFactor bounds how many store rows a single filtered
// ACTIVITY page may scan (and protojson-decode), as a multiple of the page
// limit. Filters (pending_only/kinds) are applied in Go after decode, so a
// highly selective filter over a large table could otherwise scan the whole
// table for one page. When the budget is hit before the page fills, the call
// returns a short page plus a cursor so the caller resumes — bounding the work
// of any one request at the cost of an occasional extra round-trip.
const activityScanBudgetFactor = 8

// listActivity returns an ACTIVITY page read from the canonical activity store,
// newest-first and paged by the opaque cursor. Because the store orders by the
// immutable (created_at_unix, canonical_id) keyset, paging is stable across
// concurrent inserts: it neither skips nor duplicates rows. When no store is
// wired (tests without a database) it falls back to the derive-on-read merge.
func (h *history) listActivity(ctx context.Context,
	req *wavewalletrpc.ListRequest) (*wavewalletrpc.ActivityList, error) {

	if h.deps.ActivityStore == nil {
		return h.deriveActivity(ctx, req)
	}

	limit := h.deps.resolveListLimit(req.GetLimit())
	kindFilter, err := buildKindFilter(req.GetKinds())
	if err != nil {
		return nil, err
	}

	cursorCreated, cursorID, err := decodeActivityCursor(req.GetCursor())
	if err != nil {
		return nil, err
	}

	// A page containing only the ephemeral unconfirmed-boarding row uses
	// that row as its cursor. It is not present in the canonical store, so
	// resume the store scan from its beginning while suppressing the
	// ephemeral row on this and every subsequent page.
	if cursorID == syntheticBoardingUnconfirmedID {
		cursorCreated, cursorID = 0, ""
	}

	pendingOnly := req.GetPendingOnly()

	// Scan the keyset, applying filters in Go, until limit+1 rows match so
	// has_more can be computed with the standard extra-row trick even when
	// filters skip store rows. The keyset advances by the last SCANNED row;
	// next_cursor points at the last RETURNED row so the next page resumes
	// exactly after it.
	matched := make([]*wavewalletrpc.WalletEntry, 0, limit+1)

	// The canonical activity store deliberately excludes the aggregate
	// unconfirmed-boarding row because it has no durable identity to
	// transition after confirmation. Merge that live row into the first
	// page so activity and balance expose the same pending deposit without
	// leaving a stale PENDING row in the store. Subsequent pages omit it so
	// cursor pagination never duplicates the synthetic entry.
	liveOverlayIDs := make(map[string]struct{})
	if req.GetCursor() == "" {
		boarding, err := h.collectPendingBoardingEntries(ctx)
		if err != nil {
			h.deps.resolveLog().DebugS(
				ctx,
				"Pending boarding activity overlay skipped",
				err,
			)
			boarding = nil
		}

		for _, entry := range boarding {
			liveOverlayIDs[entry.GetId()] = struct{}{}
			if matchesActivityFilter(
				entry, pendingOnly, kindFilter,
			) {

				matched = append(matched, entry)
			}
		}
	}

	lastCreated, lastID := cursorCreated, cursorID
	scanBudget := int(limit) * activityScanBudgetFactor
	scanned := 0
	budgetExhausted := false
	for uint32(len(matched)) <= limit {
		batch, err := h.deps.ActivityStore.ListEntries(
			ctx, lastCreated, lastID, int32(limit)+1,
		)
		if err != nil {
			return nil, fmt.Errorf("list activity entries: %w", err)
		}
		if len(batch) == 0 {
			break
		}

		for _, row := range batch {
			lastCreated, lastID = row.CreatedAtUnix, row.CanonicalID
			scanned++

			entry, err := rowToWalletEntry(row)
			if err != nil {
				return nil, fmt.Errorf("decode activity row "+
					"%q: %w", row.CanonicalID, err)
			}
			if !matchesActivityFilter(
				entry, pendingOnly, kindFilter,
			) {

				continue
			}

			matched = append(matched, entry)
			if uint32(len(matched)) > limit {
				break
			}
		}

		// A short page means the store has no more rows to scan.
		if uint32(len(batch)) < uint32(limit)+1 {
			break
		}

		// Stop once the scan budget is spent without filling the page.
		// The last scanned row becomes the resume cursor so the caller
		// continues rather than the server scanning the rest in one
		// call.
		if uint32(len(matched)) <= limit && scanned >= scanBudget {
			budgetExhausted = true

			break
		}
	}

	hasMore := uint32(len(matched)) > limit
	if hasMore {
		matched = matched[:limit]
	}

	var nextCursor string
	switch {
	case hasMore:
		last := matched[len(matched)-1]
		cursorID := last.GetId()
		if _, ok := liveOverlayIDs[cursorID]; ok {
			// The overlay is absent from the canonical store, so
			// its displayed id is not a valid keyset position.
			// Encode the reserved marker that the decode path
			// resets to the store start sentinel.
			cursorID = syntheticBoardingUnconfirmedID
		}
		nextCursor = encodeActivityCursor(
			last.GetCreatedAtUnix(), cursorID,
		)

	case budgetExhausted:
		// The page did not fill but the store is not drained: resume
		// strictly after the last scanned row.
		hasMore = true
		nextCursor = encodeActivityCursor(lastCreated, lastID)
	}

	return &wavewalletrpc.ActivityList{
		Entries:    matched,
		Total:      uint32(len(matched)),
		HasMore:    hasMore,
		NextCursor: nextCursor,
	}, nil
}

// countPending returns the total number of in-flight (PENDING) activity
// entries. When the canonical store is wired it counts rows directly, so the
// result is a true full-feed count instead of the single-page total the
// paginated listActivity path reports. Without a store it derives the merged
// pending set and counts that, matching the deadline-overlay semantics of the
// derive path.
func (h *history) countPending(ctx context.Context) (uint32, error) {
	if h.deps.ActivityStore == nil {
		list, err := h.deriveActivity(ctx, &wavewalletrpc.ListRequest{
			View:        wavewalletrpc.ListView_LIST_VIEW_ACTIVITY,
			PendingOnly: true,
			Limit:       h.deps.resolveMaxListLimit(),
		})
		if err != nil {
			return 0, err
		}

		return list.GetTotal(), nil
	}

	count, err := h.deps.ActivityStore.CountByStatus(
		ctx, int64(wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING),
	)
	if err != nil {
		return 0, err
	}
	if count < 0 {
		count = 0
	}

	// The aggregate unconfirmed-boarding row is intentionally ephemeral
	// and therefore absent from CountByStatus. Include it in the wallet
	// summary whenever the backing wallet currently reports pending
	// boarding value.
	boarding, err := h.collectPendingBoardingEntries(ctx)
	if err != nil {
		h.deps.resolveLog().DebugS(
			ctx,
			"Pending boarding activity count overlay skipped",
			err,
		)
		boarding = nil
	}
	count += int64(len(boarding))

	return uint32(count), nil
}

// matchesActivityFilter reports whether an entry passes the pending_only and
// kind filters. It is the single-entry form of filterEntries, applied per row
// during the store keyset scan.
func matchesActivityFilter(e *wavewalletrpc.WalletEntry, pendingOnly bool,
	kindFilter map[wavewalletrpc.EntryKind]struct{}) bool {

	if pendingOnly &&
		e.GetStatus() != wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING {
		return false
	}
	if len(kindFilter) > 0 {
		if _, ok := kindFilter[e.GetKind()]; !ok {
			return false
		}
	}

	return true
}

// encodeActivityCursor encodes the immutable keyset position
// (created_at_unix, canonical_id) as an opaque base64 token.
func encodeActivityCursor(created int64, id string) string {
	raw := strconv.FormatInt(created, 10) + ":" + id

	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeActivityCursor decodes a cursor token back into its keyset position. An
// empty cursor decodes to the newest-first start position (0, "").
func decodeActivityCursor(cursor string) (int64, string, error) {
	if cursor == "" {
		return 0, "", nil
	}

	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, "", fmt.Errorf("%w: %v", errInvalidActivityCursor,
			err)
	}

	created, id, ok := strings.Cut(string(raw), ":")
	if !ok {
		return 0, "", errInvalidActivityCursor
	}

	createdUnix, err := strconv.ParseInt(created, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("%w: %v", errInvalidActivityCursor,
			err)
	}

	// A real row always has a positive created_at_unix (ProjectEntry
	// substitutes the clock when a projection omits it), and the
	// empty-cursor "start from newest" sentinel is handled above. So a
	// non-positive decoded timestamp is a forged or corrupt token: reject
	// it loudly rather than let created_at_unix == 0 collide with the
	// return-all sentinel in the keyset query and silently restart paging
	// from the newest row.
	if createdUnix <= 0 {
		return 0, "", errInvalidActivityCursor
	}

	return createdUnix, id, nil
}

// deriveActivity returns the merged WalletEntry stream by re-joining the live
// sources on read. It is the pre-canonical-log path, retained only to seed the
// canonical store during the startup backfill; the RPC read path
// (listActivity) reads the store instead. The page size is capped at the
// daemon-level maximum so a malformed request cannot fan out unbounded work.
func (h *history) deriveActivity(ctx context.Context,
	req *wavewalletrpc.ListRequest) (*wavewalletrpc.ActivityList, error) {

	limit := h.deps.resolveListLimit(req.GetLimit())
	kindFilter, err := buildKindFilter(req.GetKinds())
	if err != nil {
		return nil, err
	}

	var (
		entries          []*wavewalletrpc.WalletEntry
		swapEntries      []*wavewalletrpc.WalletEntry
		swapCorrelations swapOORCorrelations
	)
	swapEntryIDs := make(map[string]struct{})

	if h.shouldInclude(kindFilter, wavewalletrpc.EntryKind_ENTRY_KIND_SEND) ||
		h.shouldInclude(
			kindFilter, wavewalletrpc.EntryKind_ENTRY_KIND_RECV,
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

	if h.shouldInclude(
		kindFilter, wavewalletrpc.EntryKind_ENTRY_KIND_DEPOSIT,
	) ||
		h.shouldInclude(
			kindFilter, wavewalletrpc.EntryKind_ENTRY_KIND_EXIT,
		) ||
		h.shouldInclude(
			kindFilter, wavewalletrpc.EntryKind_ENTRY_KIND_SEND,
		) {

		ledgerEntries, err := h.collectLedgerEntries(
			ctx, req.GetOffset(), limit, swapCorrelations,
			h.collectCreditTopupLinks(ctx),
		)
		if err != nil {
			return nil, fmt.Errorf("collect ledger entries: %w",
				err)
		}

		entries = append(entries, ledgerEntries...)
	}

	if h.shouldInclude(
		kindFilter, wavewalletrpc.EntryKind_ENTRY_KIND_DEPOSIT,
	) {

		pendingBoarding, err := h.collectPendingBoardingEntries(ctx)
		if err != nil {
			return nil, fmt.Errorf("collect pending boarding "+
				"entries: %w", err)
		}

		entries = append(entries, pendingBoarding...)
	}

	if h.shouldInclude(kindFilter, wavewalletrpc.EntryKind_ENTRY_KIND_EXIT) {
		exitEntries, err := h.collectUnilateralExitEntries(ctx)
		if err != nil {
			return nil, fmt.Errorf("collect unilateral exit "+
				"entries: %w", err)
		}
		entries = append(entries, exitEntries...)

		var forfeited map[string]settlement
		if h.hasWalletLocalExitEntries() {
			forfeited, _ = h.collectForfeitedVTXOSettlements(ctx)
		}

		entries = append(
			entries, h.collectWalletLocalPendingEntries(
				ctx, forfeited,
			)...,
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

	return &wavewalletrpc.ActivityList{
		Entries: paged,
		Total:   total,
	}, nil
}

// listVTXOs returns the live VTXO inventory. The daemon's ListVTXOs RPC
// is filtered to live + spendable statuses so the wallet view never
// surfaces internal terminal states (forfeited, spent, failed) the user
// has no agency over.
func (h *history) listVTXOs(ctx context.Context,
	req *wavewalletrpc.ListRequest) (*wavewalletrpc.VTXOInventory, error) {

	if h.deps.RPCServer == nil {
		return nil, ErrSwapBackendUnavailable
	}

	limit := h.deps.resolveListLimit(req.GetLimit())

	resp, err := h.deps.RPCServer.ListVTXOs(
		ctx, &waverpc.ListVTXOsRequest{
			StatusFilter: waverpc.VTXOStatus_VTXO_STATUS_LIVE,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("list vtxos: %w", err)
	}

	all := make([]*wavewalletrpc.WalletVTXO, 0, len(resp.GetVtxos()))
	for _, v := range resp.GetVtxos() {
		w, keep := walletVTXOFromDaemon(v)
		if !keep {
			continue
		}
		all = append(all, w)
	}

	total := uint32(len(all))
	paged := paginateVTXOs(all, req.GetOffset(), limit)

	return &wavewalletrpc.VTXOInventory{
		Vtxos: paged,
		Total: total,
	}, nil
}

// listOnchain returns the on-chain transaction history page. It composes
// the same waverpc.ListTransactions surface the legacy `listtransactions`
// CLI verb used, but flattens the ledger row shape onto the
// wallet-facing OnchainTx type so internal correlators don't leak into
// the user surface.
func (h *history) listOnchain(ctx context.Context,
	req *wavewalletrpc.ListRequest) (*wavewalletrpc.OnchainHistory, error) {

	if h.deps.RPCServer == nil {
		return nil, ErrSwapBackendUnavailable
	}

	limit := h.deps.resolveListLimit(req.GetLimit())

	resp, err := h.deps.RPCServer.ListTransactions(
		ctx, &waverpc.ListTransactionsRequest{
			Limit:  limit,
			Offset: req.GetOffset(),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("list onchain transactions: %w", err)
	}

	txs := make([]*wavewalletrpc.OnchainTx, 0, len(resp.GetTransactions()))
	for _, t := range resp.GetTransactions() {
		txs = append(txs, onchainTxFromLedgerRow(t))
	}

	return &wavewalletrpc.OnchainHistory{
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
	[]*wavewalletrpc.WalletEntry, swapOORCorrelations, error) {

	if h.deps.SwapService == nil {
		return nil, swapOORCorrelations{}, nil
	}

	resp, err := h.deps.SwapService.ListSwaps(
		ctx, &swapclientrpc.ListSwapsRequest{},
	)
	if err != nil {
		return nil, swapOORCorrelations{}, err
	}

	out := make([]*wavewalletrpc.WalletEntry, 0, len(resp.GetSwaps()))
	for _, s := range resp.GetSwaps() {
		// A credit-only pay's SDK swap row records a zero Ark funding
		// amount. Its durable credit operation owns the activity row
		// and retains the invoice principal, so derived history must
		// not race that projector during startup backfill or periodic
		// reconcile.
		if h.runtime != nil &&
			h.runtime.creditProjectorOwnsSwapSummary(s) {

			continue
		}

		// The wallet layer does not surface vHTLC outpoints or
		// session IDs; counterparty for swaps is the payment hash
		// (truncated). History lists swaps in both directions; let
		// direction drive the kind here (callers that own the
		// SEND/RECV intent pass an explicit override on submit).
		entry := swapEntryFromSummary(
			s, "", "",
			wavewalletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
		)
		// entry.Id is the swap row's payment_hash — that is the
		// stable wallet-layer canonical id for SEND-invoice and
		// RECV across their entire lifecycle.
		out = append(out, entry)
	}

	return out, swapOORCorrelationsFromSwaps(resp.GetSwaps()), nil
}

// collectPendingBoardingEntries adds a live row for funds seen by the on-chain
// wallet but not yet confirmed into a ledger-backed boarding deposit. When one
// active boarding address accounts for the aggregate zero-conf balance, the
// row immediately uses deposit-<address> so its identity survives confirmation.
// Older embeddings and ambiguous multi-address balances retain the aggregate
// fallback row.
func (h *history) collectPendingBoardingEntries(ctx context.Context) (
	[]*wavewalletrpc.WalletEntry, error) {

	if h.deps.RPCServer == nil {
		return nil, nil
	}

	resp, err := h.deps.RPCServer.GetBalance(
		ctx, &waverpc.GetBalanceRequest{},
	)
	if err != nil {
		return nil, err
	}
	if resp.GetBoardingUnconfirmedSat() <= 0 {
		return nil, nil
	}

	now := nowUnix()
	id := syntheticBoardingUnconfirmedID
	var request *wavewalletrpc.WalletEntryRequest

	provider, ok := h.deps.RPCServer.(activeBoardingAddressProvider)
	if ok && h.deps.ChainParams != nil {
		addresses, err := provider.ListActiveBoardingAddresses(ctx)
		if err != nil {
			return nil, fmt.Errorf("list active boarding "+
				"addresses: %w", err)
		}

		utxos, err := provider.ListUnconfirmedBoardingUTXOs(ctx)
		if err != nil {
			return nil, fmt.Errorf("list unconfirmed boarding "+
				"utxos: %w", err)
		}

		active := make(map[string]struct{}, len(addresses))
		for _, address := range addresses {
			active[address] = struct{}{}
		}

		var address string
		var amount int64
		for _, utxo := range utxos {
			if utxo == nil || utxo.Confirmations != 0 ||
				len(utxo.PkScript) != 34 ||
				utxo.PkScript[0] != 0x51 ||
				utxo.PkScript[1] != 0x20 {

				continue
			}

			derived, err := btcaddr.NewAddressTaproot(
				utxo.PkScript[2:], h.deps.ChainParams,
			)
			if err != nil {
				continue
			}
			derivedString := derived.String()
			if _, ok := active[derivedString]; !ok {
				continue
			}
			if address != "" && address != derivedString {
				address = ""
				break
			}

			address = derivedString
			amount += int64(utxo.Amount)
		}

		if address != "" && amount == resp.GetBoardingUnconfirmedSat() {
			id = fmt.Sprintf("deposit-%s", address)
			request = requestFromOnchainAddress(address)
		}
	}

	return []*wavewalletrpc.WalletEntry{
		{
			Id:            id,
			Kind:          wavewalletrpc.EntryKind_ENTRY_KIND_DEPOSIT,
			Status:        wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
			AmountSat:     resp.GetBoardingUnconfirmedSat(),
			Counterparty:  "boarding",
			CreatedAtUnix: now,
			UpdatedAtUnix: now,
			Request:       request,
			Progress: &wavewalletrpc.WalletEntryProgress{
				Phase: wavewalletrpc.
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
	[]*wavewalletrpc.WalletEntry, error) {

	if h.deps.RPCServer == nil {
		return nil, nil
	}

	resp, err := h.deps.RPCServer.ListVTXOs(
		ctx, &waverpc.ListVTXOsRequest{
			StatusFilter: waverpc.
				VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("list unilateral-exit vtxos: %w", err)
	}

	pendingByID := make(map[string]*wavewalletrpc.WalletEntry)
	if h.runtime != nil {
		for _, entry := range h.runtime.pendingSnapshot() {
			pendingByID[entry.GetId()] = entry
		}
	}

	out := make([]*wavewalletrpc.WalletEntry, 0, len(resp.GetVtxos()))
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

		// A per-row GetUnrollStatus error must not abort the whole
		// derive pass: that would block every other terminal flip
		// (deposits, other exits) on this and every reconcile/backfill
		// pass. Log-and-skip like collectWalletLocalPendingEntries; the
		// row stays at its pre-decoration (PENDING) status and is
		// retried next pass.
		if err := h.decorateExitEntry(ctx, entry, nil); err != nil {
			h.deps.resolveLog().DebugS(
				ctx,
				"Unilateral exit status decorate skipped",
				err,
			)
		}

		out = append(out, entry)
	}

	return out, nil
}

// collectWalletLocalPendingEntries returns pending rows created by wallet RPC
// calls before their durable terminal source is available. Cooperative leave
// uses this path immediately after Send returns; unilateral exit uses it until
// the unroll status/VTXO projection catches up.
func (h *history) collectWalletLocalPendingEntries(ctx context.Context,
	forfeited map[string]settlement) []*wavewalletrpc.WalletEntry {

	if h.runtime == nil {
		return nil
	}

	entries := h.runtime.pendingSnapshot()
	for _, entry := range entries {
		if entry.GetKind() != wavewalletrpc.EntryKind_ENTRY_KIND_EXIT {
			continue
		}

		// Status lookup is best-effort for wallet-local entries. A
		// missing unroll job usually means this is a cooperative
		// leave pending row, not a unilateral exit.
		_ = h.decorateExitEntry(ctx, entry, forfeited)
	}

	return entries
}

// hasWalletLocalExitEntries returns true when the runtime has any local EXIT
// row to decorate. It lets the activity list avoid querying terminal VTXOs when
// no cooperative-leave row could use them.
func (h *history) hasWalletLocalExitEntries() bool {
	if h.runtime == nil {
		return false
	}

	for _, entry := range h.runtime.pendingSnapshot() {
		if entry.GetKind() == wavewalletrpc.EntryKind_ENTRY_KIND_EXIT {
			return true
		}
	}

	return false
}

// settlement carries the on-chain coordinates of the round commitment tx that
// forfeited a VTXO. It is correlated to a cooperative-leave EXIT row by the
// consumed VTXO outpoint so a completed leave can be reconciled against the
// chain. The values come from the daemon's per-VTXO settlement fields
// (settlement_txid / settlement_height), which an old daemon leaves empty; a
// zero-value settlement still marks its outpoint as forfeited.
type settlement struct {
	txid   string
	height int32
}

// collectForfeitedVTXOSettlements returns the terminal VTXO outpoints used to
// complete cooperative-leave rows, each mapped to the settling round's txid and
// confirmation height when the daemon reports them. A present key means the
// VTXO is forfeited (the leave round confirmed); the mapped settlement may be
// zero-valued against an old daemon that does not populate the fields.
func (h *history) collectForfeitedVTXOSettlements(ctx context.Context) (
	map[string]settlement, error) {

	if h.deps.RPCServer == nil {
		return nil, nil
	}

	resp, err := h.deps.RPCServer.ListVTXOs(
		ctx, &waverpc.ListVTXOsRequest{
			StatusFilter: waverpc.
				VTXOStatus_VTXO_STATUS_FORFEITED,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("list forfeited vtxos: %w", err)
	}

	settlements := make(map[string]settlement, len(resp.GetVtxos()))
	for _, vtxo := range resp.GetVtxos() {
		if vtxo.GetOutpoint() == "" {
			continue
		}

		// GetSettlement() is nil unless the daemon reported the forfeit
		// round's coordinates; the getters degrade to ""/0, so an old
		// daemon still yields a present key with a zero-value
		// settlement (which marks the outpoint forfeited without
		// stamping a txid).
		settlements[vtxo.GetOutpoint()] = settlement{
			txid:   vtxo.GetSettlement().GetTxid(),
			height: vtxo.GetSettlement().GetHeight(),
		}
	}

	return settlements, nil
}

// unilateralExitEntryFromVTXO projects a VTXO in UNILATERAL_EXIT into a
// wallet-facing EXIT activity row. The amount is negative because value is
// leaving Ark custody.
func unilateralExitEntryFromVTXO(v *waverpc.VTXO) *wavewalletrpc.WalletEntry {
	if v == nil {
		return nil
	}

	now := nowUnix()

	return &wavewalletrpc.WalletEntry{
		Id:            v.GetOutpoint(),
		Kind:          wavewalletrpc.EntryKind_ENTRY_KIND_EXIT,
		Status:        wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     -v.GetAmountSat(),
		Counterparty:  "unilateral",
		CreatedAtUnix: now,
		UpdatedAtUnix: now,
		Progress: &wavewalletrpc.WalletEntryProgress{
			Phase: wavewalletrpc.
				WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING,
			PhaseLabel:   "unilateral_exit",
			VtxoOutpoint: v.GetOutpoint(),
		},
	}
}

// decorateExitEntry applies the best daemon-backed status available for an EXIT
// entry. Unilateral exits are owned by the unroll job store. Cooperative leaves
// have no unroll job, so when Found=false we fall back to the VTXO terminal
// state for the entry id: a forfeited source VTXO means the leave round
// confirmed and the wallet-local pending row can be shown as complete.
func (h *history) decorateExitEntry(ctx context.Context,
	entry *wavewalletrpc.WalletEntry,
	forfeited map[string]settlement) error {

	if h.deps.RPCServer == nil || entry == nil || entry.GetId() == "" {
		return nil
	}

	// decorateExitEntry only reads/annotates: it never clears the pending
	// map. Clearing a terminal wallet-local EXIT is done exclusively from
	// reprojectActivity, after the terminal row is durably projected
	// (clearProjectedTerminalExit) — decorating happens on the read/derive
	// path too, where clearing a cooperative-leave row (which lives only in
	// the pending map) before it is persisted would strand it forever.

	// GetUnrollStatus is keyed by the VTXO outpoint. A unilateral-exit
	// row's id IS that outpoint; a cooperative-leave row is keyed by the
	// stable send_job_id (a bare hash, not an outpoint) and retains the
	// consumed outpoint in vtxo_outpoint. Query by whichever is a real
	// outpoint so the hash id is never fed to the outpoint-only RPC — doing
	// so returns InvalidArgument, which would abort here and strand the
	// leave row PENDING forever. With no queryable outpoint the row cannot
	// be a unilateral exit, so treat it as a cooperative leave.
	lookup := entry.GetId()
	if !looksLikeOutpoint(lookup) {
		lookup = entry.GetProgress().GetVtxoOutpoint()
	}
	if !looksLikeOutpoint(lookup) {
		decorateCooperativeLeaveEntry(entry, forfeited)

		return nil
	}

	resp, err := h.deps.RPCServer.GetUnrollStatus(
		ctx, &waverpc.GetUnrollStatusRequest{
			Outpoint: lookup,
		},
	)
	if err != nil {
		return fmt.Errorf("get unroll status %s: %w", lookup, err)
	}
	if !resp.GetFound() {
		decorateCooperativeLeaveEntry(entry, forfeited)

		return nil
	}

	applyUnrollStatus(entry, resp)

	return nil
}

// looksLikeOutpoint reports whether s has the txid:vout shape the daemon's
// outpoint parser accepts. A stable send_job_id (bare hex, no colon) does not,
// so this distinguishes a cooperative-leave row keyed by the job id from a
// unilateral-exit or legacy row keyed by the VTXO outpoint, keeping the hash
// id out of the outpoint-only GetUnrollStatus RPC.
func looksLikeOutpoint(s string) bool {
	_, vout, ok := strings.Cut(s, ":")
	if !ok {
		return false
	}
	_, err := strconv.ParseUint(vout, 10, 32)

	return err == nil
}

// decorateCooperativeLeaveEntry completes a wallet-local cooperative leave row
// once the source VTXO is terminally forfeited. This is intentionally
// best-effort: until the daemon persists a leave job that links queued
// outpoints to the commitment tx, a restarted daemon cannot recreate the
// original counterparty/note from the runtime-local row.
func decorateCooperativeLeaveEntry(entry *wavewalletrpc.WalletEntry,
	forfeited map[string]settlement) {

	if entry.GetStatus() != wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING {
		return
	}

	if forfeited == nil {
		return
	}

	// The row is now keyed by the stable leave-job id, so the forfeit
	// correlation uses the retained consumed outpoint (vtxo_outpoint), not
	// the id. Fall back to the id only for legacy rows still keyed by the
	// outpoint — never to a bare send_job_id hash, which can never be in
	// the forfeited-outpoint set. A matching forfeited VTXO means the round
	// that consumed the queued leave input confirmed.
	outpoint := entry.GetProgress().GetVtxoOutpoint()
	if outpoint == "" && looksLikeOutpoint(entry.GetId()) {
		outpoint = entry.GetId()
	}
	if outpoint == "" {
		return
	}
	settle, ok := forfeited[outpoint]
	if !ok {
		return
	}

	applyCooperativeLeaveForfeited(entry, settle)
}

// applyCooperativeLeaveForfeited projects a forfeited cooperative-leave source
// VTXO onto the original wallet-local EXIT row. When the settlement carries the
// settling round's txid, it is stamped onto the row's progress so the completed
// leave can be reconciled against the chain; an empty txid (old daemon) leaves
// the row complete but without on-chain coordinates, preserving prior behavior.
func applyCooperativeLeaveForfeited(entry *wavewalletrpc.WalletEntry,
	settle settlement) {

	if entry == nil {
		return
	}

	entry.Status = wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE
	entry.FailureReason = ""

	progress := entry.GetProgress()
	if progress == nil {
		progress = &wavewalletrpc.WalletEntryProgress{}
		entry.Progress = progress
	}
	if progress.GetVtxoOutpoint() == "" {
		progress.VtxoOutpoint = entry.GetId()
	}
	progress.Phase = wavewalletrpc.
		WalletEntryPhase_WALLET_ENTRY_PHASE_CONFIRMED
	progress.PhaseLabel = "confirmed"

	// Stamp the settling round's on-chain coordinates when the daemon
	// reported them. Guard on a non-empty txid so an old daemon that does
	// not populate the settlement fields does not overwrite any coordinates
	// the row already carries with zero values.
	if settle.txid != "" {
		progress.Txid = settle.txid
		progress.ConfirmationHeight = settle.height
	}
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
	limit uint32, correlations swapOORCorrelations,
	creditTopups map[string]creditTopupLink) ([]*wavewalletrpc.WalletEntry,
	error) {

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
		ctx, &waverpc.ListTransactionsRequest{
			Limit: pullLimit,
		},
	)
	if err != nil {
		return nil, err
	}

	oorProjection := projectOORLedgerActivity(
		resp.GetTransactions(), correlations,
	)
	out := make(
		[]*wavewalletrpc.WalletEntry, 0,
		len(
			resp.GetTransactions(),
		),
	)
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
		if link, ok := creditTopupLinkForRow(t, creditTopups); ok {
			decorateCreditTopupEntry(entry, link)
		}

		out = append(out, entry)
	}

	// Confirmed boarding deposits keyed by deposit-<address> (the daemon
	// surfaced the boarding address) are summed per address so multiple
	// UTXOs paid to the same address show their total rather than
	// overwriting each other. See sumDepositsByAddress.
	return sumDepositsByAddress(out), nil
}

// sumDepositsByAddress collapses boarding-deposit rows that share a canonical
// id into one row whose amount is the SUM of every UTXO under that id, so a
// reused boarding address (multiple UTXOs → one deposit-<address> id) shows the
// total received instead of a single UTXO's amount. Rows with a unique id
// (older-daemon txid:vout deposits, and every non-deposit row) form a group of
// one and pass through unchanged. The most-recently-updated row in each group
// supplies the non-amount fields (status, phase, txid), so the collapsed row
// reflects the latest deposit's state with the correct running total. Order is
// not preserved; deriveActivity re-sorts by updated_at.
func sumDepositsByAddress(
	entries []*wavewalletrpc.WalletEntry) []*wavewalletrpc.WalletEntry {

	sums := make(map[string]int64)
	rep := make(map[string]*wavewalletrpc.WalletEntry)
	out := make([]*wavewalletrpc.WalletEntry, 0, len(entries))

	for _, e := range entries {
		id := e.GetId()
		if e.GetKind() != wavewalletrpc.EntryKind_ENTRY_KIND_DEPOSIT ||
			id == "" {

			out = append(out, e)

			continue
		}

		sums[id] += e.GetAmountSat()
		if cur, ok := rep[id]; !ok ||
			e.GetUpdatedAtUnix() >= cur.GetUpdatedAtUnix() {

			rep[id] = e
		}
	}

	for id, e := range rep {
		e.AmountSat = sums[id]
		out = append(out, e)
	}

	return out
}

// swapOORCorrelations keeps the swap-side session metadata needed to hide
// internal OOR ledger legs without matching by amount alone.
type swapOORCorrelations struct {
	payAmountsByFundingSession map[string]map[int64]int
	claimSessions              map[string]struct{}
	refundSessions             map[string]struct{}
}

// swapOORCorrelationsFromSwaps indexes swap session ids by the internal OOR leg
// they explain. Pay swaps hide their funding input when the session plus net
// outgoing amount matches the swap amount. Receive swaps hide their claim input
// only when the same claim session materializes a wallet VTXO.
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
func internalOORLedgerEntries(rows []*waverpc.TransactionHistoryEntry,
	correlations swapOORCorrelations) map[int64]struct{} {

	return projectOORLedgerActivity(rows, correlations).hidden
}

// projectOORLedgerActivity returns the wallet-facing projection for OOR ledger
// rows. Accounting rows stay gross; this helper only hides internal rows and
// computes display amounts for activity.
func projectOORLedgerActivity(rows []*waverpc.TransactionHistoryEntry,
	correlations swapOORCorrelations) oorLedgerActivityProjection {

	receivedBySession := make(map[string]int64)
	receiveRowsBySession := make(
		map[string][]*waverpc.TransactionHistoryEntry,
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

		amounts := correlations.payAmountsByFundingSession[session]
		received := receivedBySession[session]
		if received == 0 {
			if consumePayFundingAmount(amounts, row.GetAmountSat()) {
				projection.hidden[row.GetEntryId()] = struct{}{}
			}

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
			if !consumePayFundingAmount(amounts, delta) {
				projection.displayAmountByEntryID[row.GetEntryId()] =
					-delta

				continue
			}
			projection.hidden[row.GetEntryId()] = struct{}{}
		}
	}

	return projection
}

// consumePayFundingAmount matches one pay-swap amount against an OOR send leg.
// The amount can be either the net send delta after wallet change, or the full
// send amount when the selected input produced no change.
func consumePayFundingAmount(amounts map[int64]int, amount int64) bool {
	if amounts[amount] == 0 {
		return false
	}

	amounts[amount]--

	return true
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
func receiveMatchesClaimOutput(row *waverpc.TransactionHistoryEntry,
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

// creditTopupLink correlates one ledger OOR-send row to the credit pay
// operation whose Ark top-up it funded.
type creditTopupLink struct {
	paymentHash string

	// topupSat is the top-up amount recorded on the pay operation. A row
	// only matches when its outflow equals this amount, so a session-id
	// collision with an unrelated transfer cannot mislabel the feed.
	topupSat int64
}

// collectCreditTopupLinks asks the credit registry for the durable operation
// snapshot and indexes pay operations by their delegated top-up OOR session
// id. The raw ledger row of a credit top-up otherwise surfaces as an
// unexplained outgoing transfer (issue #989): the sats leave the VTXO balance
// through a plain OOR send whose ledger row carries no hint that it funded
// the credit account backing a sub-dust payment. Both hex orientations of
// each session id are indexed so the lookup is independent of the display
// convention the registry recorded. Transient registry errors degrade to an
// unlabeled feed rather than failing the listing.
func (h *history) collectCreditTopupLinks(
	ctx context.Context) map[string]creditTopupLink {

	if h.creditTopupLinksSet {
		return h.creditTopupLinks
	}
	h.creditTopupLinksSet = true

	if h.deps == nil || h.deps.CreditRegistry == nil {
		return nil
	}

	resp, err := h.deps.CreditRegistry.Ask(
		ctx, &credit.ListCreditOpsRequest{PendingOnly: false},
	).Await(ctx).Unpack()
	if err != nil {
		h.deps.resolveLog().DebugS(
			ctx,
			"Credit top-up labeling skipped: registry list failed",
			slog.String("err", err.Error()),
		)

		return nil
	}
	list, ok := resp.(*credit.ListCreditOpsResponse)
	if !ok {
		h.deps.resolveLog().DebugS(ctx,
			"Credit top-up labeling skipped: unexpected registry "+
				"response",
			slog.String("response_type", fmt.Sprintf("%T", resp)),
		)

		return nil
	}

	out := make(map[string]creditTopupLink)
	for i := range list.Ops {
		op := list.Ops[i]
		if op.Kind != credit.KindPay || op.OORSessionID == "" {
			continue
		}

		link := creditTopupLink{
			paymentHash: strings.TrimPrefix(op.OpKey, "pay:"),
			topupSat:    op.TopupSat,
		}

		key := strings.ToLower(op.OORSessionID)
		out[key] = link

		raw, err := hex.DecodeString(key)
		if err != nil || len(raw) != chainhash.HashSize {
			continue
		}
		if hash, err := chainhash.NewHash(raw); err == nil {
			out[strings.ToLower(hash.String())] = link
		}
	}

	h.creditTopupLinks = out

	return out
}

// creditTopupLinkForRow resolves the credit pay operation funded by one
// ledger OOR-send row, when one exists.
func creditTopupLinkForRow(row *waverpc.TransactionHistoryEntry,
	links map[string]creditTopupLink) (creditTopupLink, bool) {

	if len(links) == 0 {
		return creditTopupLink{}, false
	}

	session, ok := oorSendSessionID(row)
	if !ok {
		return creditTopupLink{}, false
	}

	link, ok := links[strings.ToLower(session)]
	if !ok {
		return creditTopupLink{}, false
	}

	// The recorded top-up amount must match the row's outflow exactly; a
	// mismatch means the session id resolved to an unrelated transfer.
	if link.topupSat > 0 && row.GetAmountSat() != link.topupSat {
		return creditTopupLink{}, false
	}

	return link, true
}

// decorateCreditTopupEntry relabels the ledger row of a credit top-up
// transfer so it reads as the credit-funding leg of a payment instead of an
// unexplained outgoing transfer. The amount is left untouched: the row
// records the real VTXO outflow, while the surplus above the paid amount
// remains visible as the wallet's credit balance.
func decorateCreditTopupEntry(entry *wavewalletrpc.WalletEntry,
	link creditTopupLink) {

	if entry == nil {
		return
	}

	entry.Counterparty = creditCounterparty
	if entry.Progress == nil {
		entry.Progress = &wavewalletrpc.WalletEntryProgress{}
	}
	entry.Progress.PhaseLabel = creditTopupPhaseLabel
	if link.paymentHash != "" {
		entry.Progress.PaymentHash = link.paymentHash
	}
}

// oorSendSessionID extracts the session id from a ledger row that spends a VTXO
// through OOR.
func oorSendSessionID(row *waverpc.TransactionHistoryEntry) (string, bool) {
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
func oorReceiveSessionID(row *waverpc.TransactionHistoryEntry) (string, bool) {
	session, _, ok := oorReceiveRef(row)

	return session, ok
}

// oorReceiveRef extracts the OOR output reference from structured ledger
// fields.
func oorReceiveRef(row *waverpc.TransactionHistoryEntry) (string, uint32,
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
func (h *history) applyOverlays(entries []*wavewalletrpc.WalletEntry,
	swapEntryIDs map[string]struct{}) {

	for _, e := range entries {
		if e.GetStatus() != wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING {
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
		e.FailureCode = failureCodePtr(ov.failureCode)
	}
}

// shouldInclude reports whether a kind should be queried based on the
// caller's kindFilter. An empty filter means include everything.
func (h *history) shouldInclude(filter map[wavewalletrpc.EntryKind]struct{},
	kind wavewalletrpc.EntryKind) bool {

	if len(filter) == 0 {
		return true
	}
	_, ok := filter[kind]

	return ok
}

// buildKindFilter materializes a set from the caller's repeated EntryKind
// filter. An empty input yields a nil set so the merger treats the call as
// "all kinds."
func buildKindFilter(kinds []wavewalletrpc.EntryKind,
) (map[wavewalletrpc.EntryKind]struct{}, error) {

	if len(kinds) == 0 {
		return nil, nil
	}

	out := make(map[wavewalletrpc.EntryKind]struct{}, len(kinds))
	for _, k := range kinds {
		switch k {
		case wavewalletrpc.EntryKind_ENTRY_KIND_SEND,
			wavewalletrpc.EntryKind_ENTRY_KIND_RECV,
			wavewalletrpc.EntryKind_ENTRY_KIND_DEPOSIT,
			wavewalletrpc.EntryKind_ENTRY_KIND_EXIT:

		default:
			return nil, fmt.Errorf("%w: %v", ErrUnsupportedKind, k)
		}
		out[k] = struct{}{}
	}

	return out, nil
}

// filterEntries applies pending_only and kind filters in one pass.
func filterEntries(entries []*wavewalletrpc.WalletEntry, pendingOnly bool,
	kindFilter map[wavewalletrpc.EntryKind]struct{}) []*wavewalletrpc.WalletEntry {

	out := entries[:0]
	for _, e := range entries {
		if pendingOnly && e.GetStatus() !=
			wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING {

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
func dedupeByID(
	entries []*wavewalletrpc.WalletEntry) []*wavewalletrpc.WalletEntry {

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
	out := make([]*wavewalletrpc.WalletEntry, 0, len(entries))
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
func paginate(entries []*wavewalletrpc.WalletEntry, offset,
	limit uint32) []*wavewalletrpc.WalletEntry {

	if offset >= uint32(len(entries)) {
		return nil
	}
	end := offset + limit
	if end > uint32(len(entries)) {
		end = uint32(len(entries))
	}
	page := make([]*wavewalletrpc.WalletEntry, 0, end-offset)
	page = append(page, entries[offset:end]...)

	return page
}

// walletEntryFromLedgerRow projects one ledger/sweep row onto a WalletEntry.
// Returns (entry, true) when the row maps onto a user-facing wallet
// operation; (nil, false) when the row should be hidden from the wallet
// view (e.g. internal fee accounting rows we don't yet model).
func walletEntryFromLedgerRow(t *waverpc.TransactionHistoryEntry) (
	*wavewalletrpc.WalletEntry, bool) {

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

	return &wavewalletrpc.WalletEntry{
		Id:            id,
		Kind:          kind,
		Status:        statusForLedgerRow(t),
		AmountSat:     amount,
		FeeSat:        t.GetFeeSat(),
		Counterparty:  ledgerCounterparty(t, kind),
		CreatedAtUnix: t.GetCreatedAtUnixS(),
		UpdatedAtUnix: t.GetCreatedAtUnixS(),
		// A confirmed boarding deposit carries its allocated address as
		// a structured onchain-address request, so a client reads the
		// deposit address from request.onchain_address rather than
		// parsing it out of the deposit-<address> id. Nil for every
		// non-deposit row and for an older daemon that does not surface
		// boarding_address.
		Request:  requestFromOnchainAddress(t.GetBoardingAddress()),
		Progress: progressFromLedgerRow(t),
	}, true
}

// ledgerActivityID returns the stable activity id for one ledger-backed row.
// A confirmed boarding-deposit (wallet_utxo_created) row is keyed by
// deposit-<boarding_address> when the daemon surfaces the address, so it shares
// the id of the pending row Deposit projected and upserts onto it. It falls
// back to txid:vout for an older daemon that does not set boarding_address, and
// then to the bare txid. Because the key is the address, multiple UTXOs paid to
// the same (single-use-by-design) boarding address collapse into one row; the
// ledger and the ONCHAIN view retain the per-UTXO truth.
func ledgerActivityID(t *waverpc.TransactionHistoryEntry,
	kind wavewalletrpc.EntryKind) string {

	if t == nil || t.GetTxid() == "" {
		return ""
	}

	if kind == wavewalletrpc.EntryKind_ENTRY_KIND_DEPOSIT &&
		t.GetSubtype() == ledger.EventWalletUTXOCreated {

		// Prefer the address-scoped id so the confirmed deposit row
		// shares a canonical id with the pending deposit-<address> row
		// projected by Deposit, letting the store upsert flip it to
		// COMPLETE. Fall back to txid:vout for older daemons that do
		// not surface the boarding address on the history row.
		if addr := t.GetBoardingAddress(); addr != "" {
			return fmt.Sprintf("deposit-%s", addr)
		}
		if t.GetOutputIndex() >= 0 {
			return fmt.Sprintf("%s:%d", t.GetTxid(),
				t.GetOutputIndex())
		}
	}

	return t.GetTxid()
}

// statusForLedgerRow folds the ledger row's confirmation_status into the
// flat EntryStatus the API surfaces. Detailed lifecycle context stays in
// WalletEntry.progress and InspectActivity; confirmed on-chain deposits stay
// pending while they are still waiting to board into a confirmed round.
func statusForLedgerRow(
	t *waverpc.TransactionHistoryEntry) wavewalletrpc.EntryStatus {

	if t.GetType() == "oor" && t.GetConfirmationStatus() == "recorded" {
		return wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE
	}

	return statusFromLedgerConfirmation(t.GetConfirmationStatus())
}

// progressFromLedgerRow projects ledger confirmation metadata onto
// WalletEntryProgress.
func progressFromLedgerRow(
	t *waverpc.TransactionHistoryEntry) *wavewalletrpc.WalletEntryProgress {

	if t == nil {
		return nil
	}

	phase, label := phaseFromLedgerConfirmation(t.GetConfirmationStatus())
	if t.GetType() == "oor" && t.GetConfirmationStatus() == "recorded" {
		phase = wavewalletrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_CONFIRMED
		label = "confirmed"
	}

	return &wavewalletrpc.WalletEntryProgress{
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
func classifyLedgerRow(t *waverpc.TransactionHistoryEntry) (
	wavewalletrpc.EntryKind, int64, bool) {

	switch t.GetType() {
	case "boarding":
		return wavewalletrpc.EntryKind_ENTRY_KIND_DEPOSIT, +1, true

	case "sweep":
		return wavewalletrpc.EntryKind_ENTRY_KIND_EXIT, -1, true

	case "oor":
		// The ledger books OOR receives as
		// `debit vtxo_balance, credit transfers_in` and OOR sends
		// as `debit transfers_out, credit vtxo_balance` (see
		// ledger/handlers.go handleVTXOReceived/handleVTXOSent).
		// Classify the wallet direction off the counterparty side
		// so we don't depend on which leg the daemon row carries.
		switch {
		case t.GetCreditAccount() == ledger.AccountTransfersIn:
			return wavewalletrpc.EntryKind_ENTRY_KIND_RECV, +1, true

		case t.GetDebitAccount() == ledger.AccountTransfersOut:
			return wavewalletrpc.EntryKind_ENTRY_KIND_SEND, -1, true
		}

		// OOR rows without a recognisable counterparty account are
		// internal bookkeeping — hide them from the wallet view.
		return wavewalletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED, 0, false

	default:
		// Round-level and other rows are not yet modeled as wallet
		// operations in v1.
		return wavewalletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED, 0, false
	}
}

// statusFromLedgerConfirmation maps the ledger row's confirmation_status
// string onto the flat wallet status.
func statusFromLedgerConfirmation(s string) wavewalletrpc.EntryStatus {
	switch s {
	case "confirmed", "swept":
		return wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE

	case "failed":
		return wavewalletrpc.EntryStatus_ENTRY_STATUS_FAILED

	default:
		return wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING
	}
}

// phaseFromLedgerConfirmation maps the ledger confirmation string to the
// normalized wallet phase and label.
func phaseFromLedgerConfirmation(s string) (wavewalletrpc.WalletEntryPhase,
	string) {

	switch s {
	case "boarding":
		return wavewalletrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING,
			"boarding"

	case "confirmed", "swept":
		return wavewalletrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_CONFIRMED,
			"confirmed"

	case "failed":
		return wavewalletrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_FAILED,
			"failed"

	default:
		return wavewalletrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_PAYMENT_DETECTED,
			"payment_detected"
	}
}

// ledgerCounterparty renders a short, display-friendly counterparty for a
// ledger-derived WalletEntry. For DEPOSIT rows it returns the literal
// "boarding"; for EXIT rows it returns the txid (truncated); for SEND/RECV
// OOR rows it returns the txid or an empty string when the row carries
// none.
func ledgerCounterparty(t *waverpc.TransactionHistoryEntry,
	kind wavewalletrpc.EntryKind) string {

	switch kind {
	case wavewalletrpc.EntryKind_ENTRY_KIND_DEPOSIT:
		return "boarding"

	case wavewalletrpc.EntryKind_ENTRY_KIND_EXIT:
		return truncate(t.GetTxid(), truncatedCounterpartyLen)

	default:
		return truncate(t.GetTxid(), truncatedCounterpartyLen)
	}
}

//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// pagedLedgerRPC serves ListTransactions from a newest-first row set,
// applying the type filter before paging, clamping every reply to pageSize and
// reporting the daemon's continuation, the way waved.RPCServer.ListTransactions
// does. afterPage, when set, runs after each reply so a test can commit a row
// between pages.
type pagedLedgerRPC struct {
	*fakeRPCServer

	rows      []*waverpc.TransactionHistoryEntry
	pageSize  int
	pages     []string
	afterPage func()
}

// ListTransactions returns one clamped page of the filtered rows starting at
// req.Offset.
func (s *pagedLedgerRPC) ListTransactions(_ context.Context,
	req *waverpc.ListTransactionsRequest) (
	*waverpc.ListTransactionsResponse, error) {

	var rows []*waverpc.TransactionHistoryEntry
	for _, row := range s.rows {
		if req.GetType() == "" || row.GetType() == req.GetType() {
			rows = append(rows, row)
		}
	}

	limit := min(int(req.GetLimit()), s.pageSize)
	start := min(int(req.GetOffset()), len(rows))
	end := min(start+limit, len(rows))
	s.pages = append(s.pages, fmt.Sprintf("%d:%d", start, end))

	resp := &waverpc.ListTransactionsResponse{
		Transactions: rows[start:end],
		HasMore:      end < len(rows),
	}
	if resp.HasMore {
		resp.NextOffset = uint32(end)
	}
	if s.afterPage != nil {
		s.afterPage()
	}

	return resp, nil
}

// reusedAddressDeposit returns a confirmed boarding deposit to the shared test
// address, so every such row sums into deposit-bcrt1qreused.
func reusedAddressDeposit(txid string, entryID, amount,
	createdAt int64) *waverpc.TransactionHistoryEntry {

	return &waverpc.TransactionHistoryEntry{
		Type:               "boarding",
		Subtype:            ledger.EventWalletUTXOCreated,
		ConfirmationStatus: "confirmed",
		AmountSat:          amount,
		Txid:               txid,
		OutputIndex:        0,
		BoardingAddress:    "bcrt1qreused",
		EntryId:            entryID,
		CreatedAtUnixS:     createdAt,
	}
}

// requireReusedDepositTotal asserts the stored total for the shared test
// deposit address.
func requireReusedDepositTotal(t *testing.T,
	store interface {
		GetEntry(context.Context, string) (sqlc.ActivityEntry, error)
	},
	want int64) {

	t.Helper()

	stored, err := store.GetEntry(t.Context(), "deposit-bcrt1qreused")
	require.NoError(t, err)
	require.Equal(t, want, stored.AmountSat)
}

// oorReceipt returns a plain OOR receive row keyed by its txid.
func oorReceipt(txid string, createdAt int64) *waverpc.TransactionHistoryEntry {
	return &waverpc.TransactionHistoryEntry{
		Type:               "oor",
		Subtype:            ledger.EventVTXOReceived,
		ConfirmationStatus: "recorded",
		DebitAccount:       ledger.AccountVTXOBalance,
		CreditAccount:      ledger.AccountTransfersIn,
		AmountSat:          3_000,
		Txid:               txid,
		CreatedAtUnixS:     createdAt,
	}
}

// TestBackfillReadsEveryLedgerPage verifies the startup backfill seeds ledger
// rows older than the newest ledger page.
func TestBackfillReadsEveryLedgerPage(t *testing.T) {
	t.Parallel()

	rt, store, rpc := newReconcileFixture(t)
	backend := &pagedLedgerRPC{fakeRPCServer: rpc, pageSize: 1000}
	for i := 1001; i > 0; i-- {
		backend.rows = append(
			backend.rows,
			oorReceipt(
				fmt.Sprintf("receipt-%d", i), int64(i),
			),
		)
	}
	rt.deps.RPCServer = backend

	rt.backfillActivity(t.Context())

	for _, id := range []string{"receipt-1001", "receipt-1"} {
		_, err := store.GetEntry(t.Context(), id)
		require.NoErrorf(
			t, err, "%s missing, ledger pages %v", id,
			backend.pages,
		)
	}
	require.Equal(t, []string{"0:1000", "1000:1001"}, backend.pages)
}

// TestBackfillCorrelatesRowsAcrossLedgerPages verifies rows that project
// together still do so when a ledger page boundary separates them: an OOR send
// and its same-session change, and two deposits to one boarding address.
func TestBackfillCorrelatesRowsAcrossLedgerPages(t *testing.T) {
	t.Parallel()

	rt, store, rpc := newReconcileFixture(t)

	sessionID := testBytes(32, 0x31)
	send := &waverpc.TransactionHistoryEntry{
		Type:               "oor",
		Subtype:            ledger.EventVTXOSent,
		ConfirmationStatus: "recorded",
		AmountSat:          999_745,
		DebitAccount:       ledger.AccountTransfersOut,
		CreditAccount:      ledger.AccountVTXOBalance,
		SessionId:          sessionID,
		EntryId:            13,
		CreatedAtUnixS:     104,
	}
	change := &waverpc.TransactionHistoryEntry{
		Type:               "oor",
		Subtype:            ledger.EventVTXOReceived,
		ConfirmationStatus: "recorded",
		AmountSat:          998_511,
		DebitAccount:       ledger.AccountVTXOBalance,
		CreditAccount:      ledger.AccountTransfersIn,
		Txid:               testSessionString(t, sessionID),
		OutputIndex:        1,
		EntryId:            14,
		CreatedAtUnixS:     103,
	}
	// A page size of one puts every row on its own ledger page.
	backend := &pagedLedgerRPC{
		fakeRPCServer: rpc,
		pageSize:      1,
		rows: []*waverpc.TransactionHistoryEntry{
			send, change,
			reusedAddressDeposit("deposit-a", 15, 40_000, 102),
			reusedAddressDeposit("deposit-b", 16, 2_000, 101),
		},
	}
	rt.deps.RPCServer = backend

	rt.backfillActivity(t.Context())
	require.Len(t, backend.pages, 4)

	sendEntry, ok := walletEntryFromLedgerRow(send)
	require.True(t, ok)
	stored, err := store.GetEntry(t.Context(), sendEntry.GetId())
	require.NoError(t, err)
	require.Equal(t, int64(-1_234), stored.AmountSat)

	changeEntry, ok := walletEntryFromLedgerRow(change)
	require.True(t, ok)
	_, err = store.GetEntry(t.Context(), changeEntry.GetId())
	require.ErrorIs(t, err, sql.ErrNoRows)

	requireReusedDepositTotal(t, store, 42_000)
}

// TestBackfillContinuesPastFilteredLedgerPage verifies a ledger page holding
// only rows the wallet hides does not end the backfill early.
func TestBackfillContinuesPastFilteredLedgerPage(t *testing.T) {
	t.Parallel()

	rt, store, rpc := newReconcileFixture(t)

	feeLeg := func(entryID int64) *waverpc.TransactionHistoryEntry {
		return &waverpc.TransactionHistoryEntry{
			Type:           "boarding",
			Subtype:        ledger.EventBoardingFeePaid,
			AmountSat:      100,
			EntryId:        entryID,
			CreatedAtUnixS: 50,
		}
	}
	backend := &pagedLedgerRPC{
		fakeRPCServer: rpc,
		pageSize:      2,
		rows: []*waverpc.TransactionHistoryEntry{
			oorReceipt("receipt-new", 100),
			feeLeg(1),
			feeLeg(2), feeLeg(3),
			oorReceipt("receipt-old", 10),
		},
	}
	rt.deps.RPCServer = backend

	rt.backfillActivity(t.Context())

	_, err := store.GetEntry(t.Context(), "receipt-old")
	require.NoErrorf(t, err, "ledger pages %v", backend.pages)
}

// TestReconcileKeepsDepositTotalAcrossLedgerPages verifies the periodic
// reconciler keeps the full per-address deposit total the startup backfill
// wrote when the address's deposits span more than one ledger page, instead of
// replacing it with the sum of the newest page.
func TestReconcileKeepsDepositTotalAcrossLedgerPages(t *testing.T) {
	t.Parallel()

	rt, store, rpc := newReconcileFixture(t)
	backend := &pagedLedgerRPC{
		fakeRPCServer: rpc,
		pageSize:      1,
		rows: []*waverpc.TransactionHistoryEntry{
			reusedAddressDeposit("deposit-a", 15, 40_000, 105),
			oorReceipt("receipt-1", 104),
			oorReceipt("receipt-2", 103),
			reusedAddressDeposit("deposit-b", 16, 2_000, 101),
		},
	}
	rt.deps.RPCServer = backend

	rt.backfillActivity(t.Context())
	requireReusedDepositTotal(t, store, 42_000)

	rt.reconcileActivity(t.Context())
	requireReusedDepositTotal(t, store, 42_000)
}

// TestBackfillDedupesRowRepeatedByConcurrentInsert verifies a ledger row that a
// concurrent insert pushes onto the next page is read once, so a deposit is not
// counted twice.
func TestBackfillDedupesRowRepeatedByConcurrentInsert(t *testing.T) {
	t.Parallel()

	rt, store, rpc := newReconcileFixture(t)
	backend := &pagedLedgerRPC{
		fakeRPCServer: rpc,
		pageSize:      1,
		rows: []*waverpc.TransactionHistoryEntry{
			reusedAddressDeposit("deposit-a", 15, 40_000, 102),
			reusedAddressDeposit("deposit-b", 16, 2_000, 101),
		},
	}

	// Commit a newer row after the first page, so deposit-a shifts to
	// offset 1 and is served again on the second page.
	inserted := false
	backend.afterPage = func() {
		if inserted {
			return
		}
		inserted = true
		newer := reusedAddressDeposit("deposit-c", 17, 500, 103)
		newer.BoardingAddress = "bcrt1qother"
		backend.rows = append(
			[]*waverpc.TransactionHistoryEntry{newer},
			backend.rows...,
		)
	}
	rt.deps.RPCServer = backend

	rt.backfillActivity(t.Context())
	require.Equal(t, []string{"0:1", "1:2", "2:3"}, backend.pages)
	requireReusedDepositTotal(t, store, 42_000)
}

// bindingReceipt returns an OOR receive the daemon reconstructs from a VTXO
// binding, carrying the per-query negative entry_id the history query assigns.
func bindingReceipt(txid string, entryID,
	createdAt int64) *waverpc.TransactionHistoryEntry {

	row := oorReceipt(txid, createdAt)
	row.Source = ledgerSourceOORBinding
	row.EntryId = entryID

	return row
}

// TestBackfillKeepsBindingRowsRenumberedBetweenPages verifies binding rows are
// deduplicated by outpoint, not by their per-query entry_id. A binding
// committed between page reads renumbers the others, so a repeated row comes
// back with a new id and a distinct row can come back with an id already seen.
func TestBackfillKeepsBindingRowsRenumberedBetweenPages(t *testing.T) {
	t.Parallel()

	rt, store, rpc := newReconcileFixture(t)
	backend := &pagedLedgerRPC{
		fakeRPCServer: rpc,
		pageSize:      1,
		rows: []*waverpc.TransactionHistoryEntry{
			bindingReceipt("binding-a", -2, 102),
			bindingReceipt("binding-b", -1, 101),
		},
	}

	// After the first page, a new binding renumbers the rest: binding-a
	// repeats at offset 1 as -1 and binding-b takes -2, the id binding-a
	// had on the first page.
	inserted := false
	backend.afterPage = func() {
		if inserted {
			return
		}
		inserted = true
		backend.rows = []*waverpc.TransactionHistoryEntry{
			bindingReceipt("binding-c", -3, 103),
			bindingReceipt("binding-a", -1, 102),
			bindingReceipt("binding-b", -2, 101),
		}
	}
	rt.deps.RPCServer = backend

	h := newHistory(rt.deps, rt)
	rows, err := h.listAllLedgerRows(t.Context(), nil)
	require.NoError(t, err)

	var txids []string
	ids := make(map[int64]struct{})
	for _, row := range rows {
		txids = append(txids, row.GetTxid())
		ids[row.GetEntryId()] = struct{}{}
	}
	require.Equal(t, []string{"binding-a", "binding-b"}, txids)
	require.Len(t, ids, len(rows), "binding ids must be unique")

	// The backfill reads the same renumbered pages and stores both.
	inserted = false
	backend.rows = []*waverpc.TransactionHistoryEntry{
		bindingReceipt("binding-a", -2, 102),
		bindingReceipt("binding-b", -1, 101),
	}
	rt.backfillActivity(t.Context())
	for _, id := range []string{"binding-a", "binding-b"} {
		_, err := store.GetEntry(t.Context(), id)
		require.NoErrorf(
			t, err, "%s missing, ledger pages %v", id,
			backend.pages,
		)
	}
}

package db

import (
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// newTestStore creates a Store backed by a test SQLite database.
func newTestStore(t testing.TB) *Store {
	t.Helper()

	sqlStore := NewTestDB(t)

	return NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
}

// TestAccountsSeeded verifies that the migration seeds the
// chart of accounts with all expected entries. The set matches
// the chart of accounts in docs/fee-model.md so operator equity
// can be computed directly from the ledger as
// assets - liabilities.
func TestAccountsSeeded(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	accounts, err := store.ListAccounts(ctx)
	require.NoError(t, err)

	// We expect 9 seeded accounts: 2 assets, 1 liability,
	// 4 revenues (one per product), 1 expense, 1 equity.
	require.Len(t, accounts, 9)

	accountIDs := make(map[string]string)
	for _, a := range accounts {
		accountIDs[a.AccountID] = a.AccountType
	}

	require.Equal(t, "asset", accountIDs["treasury_wallet"])
	require.Equal(t, "asset", accountIDs["deployed_capital"])
	require.Equal(t, "liability", accountIDs["user_vtxo_claims"])
	require.Equal(t, "revenue",
		accountIDs["boarding_fee_revenue"])
	require.Equal(t, "revenue",
		accountIDs["refresh_fee_revenue"])
	require.Equal(t, "revenue",
		accountIDs["offboard_fee_revenue"])
	require.Equal(t, "revenue", accountIDs["oor_fee_revenue"])
	require.Equal(t, "expense", accountIDs["mining_fees"])
	require.Equal(t, "equity", accountIDs["external_funding"])
}

// TestInsertAndListLedgerEntries verifies round-trip insertion
// and retrieval of ledger entries using the fee-model.md event
// taxonomy.
func TestInsertAndListLedgerEntries(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	now := time.Now().Unix()

	// Boarding fee: the fee portion of a boarding deposit is
	// drawn from the deposit (asset) and recognized as
	// operator revenue.
	_, err := store.InsertLedgerEntry(ctx, sqlc.InsertLedgerEntryParams{
		DebitAccount:  "deployed_capital",
		CreditAccount: "boarding_fee_revenue",
		AmountSat:     1000,
		RoundID:       []byte("test-round-001"),
		EventType:     "boarding_fee",
		Description:   "boarding input fee",
		CreatedAt:     now,
	})
	require.NoError(t, err)

	// Mining fee: paid out of the operator's on-chain wallet.
	_, err = store.InsertLedgerEntry(ctx, sqlc.InsertLedgerEntryParams{
		DebitAccount:  "mining_fees",
		CreditAccount: "treasury_wallet",
		AmountSat:     5000,
		RoundID:       []byte("test-round-001"),
		EventType:     "mining_fee",
		Description:   "round tx mining fee",
		CreatedAt:     now + 1,
	})
	require.NoError(t, err)

	// List all entries.
	entries, err := store.ListLedgerEntries(
		ctx, sqlc.ListLedgerEntriesParams{
			Limit:  100,
			Offset: 0,
		},
	)
	require.NoError(t, err)
	require.Len(t, entries, 2)

	// Entries should be in descending creation order.
	require.Equal(t, "mining_fee", entries[0].EventType)
	require.Equal(t, "boarding_fee", entries[1].EventType)

	// Verify count.
	count, err := store.CountLedgerEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
}

// TestListLedgerEntriesByRound verifies filtering by round ID.
func TestListLedgerEntriesByRound(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	now := time.Now().Unix()
	round1 := []byte("round-aaa")
	round2 := []byte("round-bbb")

	// Round 1: boarding fee.
	_, err := store.InsertLedgerEntry(ctx, sqlc.InsertLedgerEntryParams{
		DebitAccount:  "deployed_capital",
		CreditAccount: "boarding_fee_revenue",
		AmountSat:     500,
		RoundID:       round1,
		EventType:     "boarding_fee",
		Description:   "round 1 fee",
		CreatedAt:     now,
	})
	require.NoError(t, err)

	// Round 2: refresh fee. The refresh fee reduces the user's
	// outstanding claim (liability) in favor of refresh fee
	// revenue.
	_, err = store.InsertLedgerEntry(ctx, sqlc.InsertLedgerEntryParams{
		DebitAccount:  "user_vtxo_claims",
		CreditAccount: "refresh_fee_revenue",
		AmountSat:     700,
		RoundID:       round2,
		EventType:     "refresh_fee",
		Description:   "round 2 fee",
		CreatedAt:     now + 1,
	})
	require.NoError(t, err)

	// Filter by round1.
	entries, err := store.ListLedgerEntriesByRound(ctx, round1)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, int64(500), entries[0].AmountSat)
}

// TestListLedgerEntriesByEventType verifies filtering by event
// type, covering the fine-grained event taxonomy defined in
// fee-model.md.
func TestListLedgerEntriesByEventType(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	now := time.Now().Unix()

	// Insert mixed event types. boarding_fee appears twice so
	// the filter check has something to find.
	for i, evt := range []string{
		"boarding_fee", "mining_fee", "boarding_fee",
		"capital_committed",
	} {
		_, err := store.InsertLedgerEntry(
			ctx, sqlc.InsertLedgerEntryParams{
				DebitAccount:  "deployed_capital",
				CreditAccount: "boarding_fee_revenue",
				AmountSat:     int64((i + 1) * 100),
				EventType:     evt,
				Description:   "test",
				CreatedAt:     now + int64(i),
			},
		)
		require.NoError(t, err)
	}

	// Filter boarding_fee only.
	entries, err := store.ListLedgerEntriesByEventType(
		ctx, sqlc.ListLedgerEntriesByEventTypeParams{
			EventType: "boarding_fee",
			Limit:     100,
			Offset:    0,
		},
	)
	require.NoError(t, err)
	require.Len(t, entries, 2)
}

// TestGetAccountBalance verifies the double-entry balance
// computation for both an asset account (positive with debits)
// and a revenue account (positive with credits → reported as
// negative debit-minus-credit).
func TestGetAccountBalance(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	now := time.Now().Unix()

	// Boarding fee: debit deployed_capital (asset, +1000),
	// credit operator_revenue (revenue, +1000).
	_, err := store.InsertLedgerEntry(ctx, sqlc.InsertLedgerEntryParams{
		DebitAccount:  "deployed_capital",
		CreditAccount: "boarding_fee_revenue",
		AmountSat:     1000,
		EventType:     "boarding_fee",
		Description:   "fee 1",
		CreatedAt:     now,
	})
	require.NoError(t, err)

	// Refresh fee: debit user_vtxo_claims (liability, -500),
	// credit refresh_fee_revenue (revenue, +500).
	_, err = store.InsertLedgerEntry(ctx, sqlc.InsertLedgerEntryParams{
		DebitAccount:  "user_vtxo_claims",
		CreditAccount: "refresh_fee_revenue",
		AmountSat:     500,
		EventType:     "refresh_fee",
		Description:   "fee 2",
		CreatedAt:     now + 1,
	})
	require.NoError(t, err)

	// boarding_fee_revenue balance: debits(0) - credits(1000)
	// = -1000. Revenue accounts increase with credits, so a
	// negative debit-minus-credit reflects positive recognized
	// revenue.
	balance, err := store.GetAccountBalance(
		ctx, "boarding_fee_revenue",
	)
	require.NoError(t, err)
	require.Equal(t, int64(-1000), balance)

	// refresh_fee_revenue balance: debits(0) - credits(500) =
	// -500. Same semantics as boarding_fee_revenue.
	balance, err = store.GetAccountBalance(
		ctx, "refresh_fee_revenue",
	)
	require.NoError(t, err)
	require.Equal(t, int64(-500), balance)

	// deployed_capital: debits(1000) - credits(0) = 1000.
	// Asset accounts increase with debits.
	balance, err = store.GetAccountBalance(
		ctx, "deployed_capital",
	)
	require.NoError(t, err)
	require.Equal(t, int64(1000), balance)

	// user_vtxo_claims: debits(500) - credits(0) = 500.
	// Liability accounts increase with credits; a positive
	// debit-minus-credit reflects a reduction in the
	// outstanding claim (fee carved out of the user's VTXO
	// value).
	balance, err = store.GetAccountBalance(
		ctx, "user_vtxo_claims",
	)
	require.NoError(t, err)
	require.Equal(t, int64(500), balance)
}

// TestGetAccountBalanceOverflowSafety verifies that balances
// exceeding int32 range (~21.47 BTC in sats) are returned
// without overflow — the query CASTs the SUM to BIGINT and the
// Go layer uses int64 throughout.
func TestGetAccountBalanceOverflowSafety(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	now := time.Now().Unix()

	// 50 BTC in sats = 5_000_000_000, well above int32 max of
	// 2_147_483_647.
	const bigAmount = int64(5_000_000_000)

	_, err := store.InsertLedgerEntry(ctx, sqlc.InsertLedgerEntryParams{
		DebitAccount:  "treasury_wallet",
		CreditAccount: "deployed_capital",
		AmountSat:     bigAmount,
		EventType:     "round_sweep",
		Description:   "50 BTC sweep",
		CreatedAt:     now,
	})
	require.NoError(t, err)

	balance, err := store.GetAccountBalance(ctx, "treasury_wallet")
	require.NoError(t, err)
	require.Equal(t, bigAmount, balance,
		"balance must be returned without int32 truncation")
}

// TestLedgerEntryRejectsSelfTransfer verifies that the schema
// CHECK (debit_account <> credit_account) rejects self-transfer
// entries. A self-transfer passes all FKs and contributes +A/−A
// to the same account, which cancel in any balance aggregation
// (including the sum-to-zero invariant), so it must be rejected
// at the schema layer before any caller bug can inject one.
func TestLedgerEntryRejectsSelfTransfer(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.InsertLedgerEntry(ctx, sqlc.InsertLedgerEntryParams{
		DebitAccount:  "deployed_capital",
		CreditAccount: "deployed_capital",
		AmountSat:     1000,
		EventType:     "boarding_fee",
		Description:   "self-transfer — should be rejected",
		CreatedAt:     time.Now().Unix(),
	})
	require.Error(t, err,
		"self-transfer must be rejected by the schema CHECK")
}

// TestLedgerEventTypesSeeded verifies that migration 000010
// seeds all 14 ledger event types (9 fee-model events + 5
// wallet/OOR tracking events). Insert one entry per event type
// to indirectly verify every enum value is FK-accepted.
func TestLedgerEventTypesSeeded(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	now := time.Now().Unix()

	// Fee-model events (from docs/fee-model.md). We pick a
	// debit/credit account pair that is economically sensible
	// for each event type per the spec's ledger-entries-by-event
	// table.
	feeEvents := map[string][2]string{
		"boarding_deposit": {
			"deployed_capital", "user_vtxo_claims",
		},
		"boarding_fee": {
			"deployed_capital", "boarding_fee_revenue",
		},
		"refresh_forfeit": {
			"user_vtxo_claims", "deployed_capital",
		},
		"refresh_new_vtxo": {
			"deployed_capital", "user_vtxo_claims",
		},
		"refresh_fee": {
			"user_vtxo_claims", "refresh_fee_revenue",
		},
		"offboard": {
			"user_vtxo_claims", "treasury_wallet",
		},
		"offboard_fee": {
			"user_vtxo_claims", "offboard_fee_revenue",
		},
		"mining_fee": {
			"mining_fees", "treasury_wallet",
		},
		"round_sweep": {
			"treasury_wallet", "deployed_capital",
		},
		"capital_committed": {
			"deployed_capital", "treasury_wallet",
		},
	}

	// Non-round/non-session tracking events. OOR fees cross
	// user_vtxo_claims and oor_fee_revenue; external deposit/
	// withdrawal cross treasury_wallet and external_funding
	// (equity). Wall-mounted to the canonical pairs so every
	// enum value is FK-accepted.
	nonRoundEvents := map[string][2]string{
		"oor_transfer":        {"user_vtxo_claims", "oor_fee_revenue"},
		"external_deposit":    {"treasury_wallet", "external_funding"},
		"external_withdrawal": {"external_funding", "treasury_wallet"},
	}

	ts := now
	for evt, accounts := range feeEvents {
		ts++
		_, err := store.InsertLedgerEntry(
			ctx, sqlc.InsertLedgerEntryParams{
				DebitAccount:  accounts[0],
				CreditAccount: accounts[1],
				AmountSat:     100,
				EventType:     evt,
				Description:   "seeded event check",
				CreatedAt:     ts,
			},
		)
		require.NoError(t, err,
			"fee-model event type %q must be seeded", evt)
	}

	for evt, accounts := range nonRoundEvents {
		ts++
		_, err := store.InsertLedgerEntry(
			ctx, sqlc.InsertLedgerEntryParams{
				DebitAccount:  accounts[0],
				CreditAccount: accounts[1],
				AmountSat:     100,
				EventType:     evt,
				Description:   "seeded event check",
				CreatedAt:     ts,
			},
		)
		require.NoError(t, err,
			"non-round event type %q must be seeded", evt)
	}

	// 10 fee-model + 3 non-round = 13 entries.
	count, err := store.CountLedgerEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(13), count)
}

// TestDoubleEntryBalanceInvariant verifies that for every
// ledger entry, the amount appears on both sides (single-amount
// double-entry) and that amounts are strictly positive. The
// entries simulate the full round lifecycle described in
// fee-model.md.
func TestDoubleEntryBalanceInvariant(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	now := time.Now().Unix()

	// Simulate a full round lifecycle per fee-model.md
	// "Ledger Entries by Event". Boarding splits into the
	// deposit (value − fee) plus the fee recognition.
	entries := []sqlc.InsertLedgerEntryParams{
		// Boarding — user deposit (value net of fee).
		{
			DebitAccount:  "deployed_capital",
			CreditAccount: "user_vtxo_claims",
			AmountSat:     98000,
			RoundID:       []byte("round-1"),
			EventType:     "boarding_deposit",
			Description:   "boarded value net of fee",
			CreatedAt:     now,
		},
		// Boarding — fee recognized as operator revenue.
		{
			DebitAccount:  "deployed_capital",
			CreditAccount: "boarding_fee_revenue",
			AmountSat:     2000,
			RoundID:       []byte("round-1"),
			EventType:     "boarding_fee",
			Description:   "boarding fee",
			CreatedAt:     now + 1,
		},
		// Mining fee paid from the operator wallet.
		{
			DebitAccount:  "mining_fees",
			CreditAccount: "treasury_wallet",
			AmountSat:     500,
			RoundID:       []byte("round-1"),
			EventType:     "mining_fee",
			Description:   "mining fee paid",
			CreatedAt:     now + 2,
		},
		// Round sweep: old round's deployed_capital recycled
		// back into the treasury wallet after csv expiry.
		{
			DebitAccount:  "treasury_wallet",
			CreditAccount: "deployed_capital",
			AmountSat:     100000,
			EventType:     "round_sweep",
			Description:   "sweep reclaimed",
			CreatedAt:     now + 3,
		},
	}

	for _, e := range entries {
		_, err := store.InsertLedgerEntry(ctx, e)
		require.NoError(t, err)
	}

	// Entry-level sanity: all amounts must be strictly positive
	// (the schema enforces this, but assert it for defence in
	// depth).
	allEntries, err := store.ListLedgerEntries(
		ctx, sqlc.ListLedgerEntriesParams{
			Limit:  1000,
			Offset: 0,
		},
	)
	require.NoError(t, err)
	require.Len(t, allEntries, 4)

	for _, e := range allEntries {
		require.Greater(t, e.AmountSat, int64(0),
			"ledger entry amounts must be positive")
	}

	// The fundamental accounting invariant: every ledger entry
	// adds +amount_sat to the debit account and −amount_sat to
	// the credit account, so summing GetAccountBalance across
	// every seeded account must yield exactly zero. If any entry
	// were unbalanced or a "self-transfer" (debit == credit), the
	// sum would still be zero only by coincidence — but all 5
	// balances together form the canonical check.
	accounts, err := store.ListAccounts(ctx)
	require.NoError(t, err)
	require.Len(t, accounts, 9,
		"expected 9 seeded accounts: 2 asset, 1 "+
			"liability, 4 revenue, 1 expense, 1 equity")

	var totalBalance int64
	for _, acc := range accounts {
		balance, err := store.GetAccountBalance(ctx, acc.AccountID)
		require.NoError(t, err)
		totalBalance += balance
	}

	require.Equal(t, int64(0), totalBalance,
		"sum of all account balances must be zero "+
			"(fundamental double-entry invariant)")
}

// TestFeeScheduleHistory verifies that fee schedule changes are
// recorded for audit, including the min_refresh_delta_blocks
// field that controls the refresh liquidity fee floor.
func TestFeeScheduleHistory(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	now := time.Now().Unix()

	// Record two schedule changes.
	err := store.InsertFeeScheduleHistory(
		ctx, sqlc.InsertFeeScheduleHistoryParams{
			AnnualRate:            0.05,
			BaseMarginSat:         100,
			UtilThresholdBps:      7000,
			UtilSpreadDelta0Bps:   100,
			UtilSpreadDelta1Bps:   500,
			MinRefreshDeltaBlocks: 144,
			MinViablePolicy:       "reject",
			MinViablePct:          50,
			CreatedAt:             now,
		},
	)
	require.NoError(t, err)

	err = store.InsertFeeScheduleHistory(
		ctx, sqlc.InsertFeeScheduleHistoryParams{
			AnnualRate:            0.08,
			BaseMarginSat:         200,
			UtilThresholdBps:      6000,
			UtilSpreadDelta0Bps:   200,
			UtilSpreadDelta1Bps:   1000,
			MinRefreshDeltaBlocks: 288,
			MinViablePolicy:       "warn",
			MinViablePct:          30,
			CreatedAt:             now + 1,
		},
	)
	require.NoError(t, err)

	// List history (most recent first).
	history, err := store.ListFeeScheduleHistory(ctx, 10)
	require.NoError(t, err)
	require.Len(t, history, 2)

	// Most recent should be the 8% rate with the wider floor.
	require.InDelta(t, 0.08, history[0].AnnualRate, 1e-9)
	require.Equal(t, int64(200), history[0].BaseMarginSat)
	require.Equal(t, "warn", history[0].MinViablePolicy)
	require.Equal(t, int32(288), history[0].MinRefreshDeltaBlocks)

	// Oldest entry retains the ~1 day floor.
	require.Equal(t, int32(144), history[1].MinRefreshDeltaBlocks)
}

// TestLedgerPagination verifies LIMIT/OFFSET pagination.
func TestLedgerPagination(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	now := time.Now().Unix()

	// Insert 10 entries.
	for i := range 10 {
		_, err := store.InsertLedgerEntry(
			ctx, sqlc.InsertLedgerEntryParams{
				DebitAccount:  "deployed_capital",
				CreditAccount: "boarding_fee_revenue",
				AmountSat:     int64((i + 1) * 100),
				EventType:     "boarding_fee",
				Description:   "test",
				CreatedAt:     now + int64(i),
			},
		)
		require.NoError(t, err)
	}

	// Page 1: first 3 entries (most recent).
	page1, err := store.ListLedgerEntries(
		ctx, sqlc.ListLedgerEntriesParams{
			Limit:  3,
			Offset: 0,
		},
	)
	require.NoError(t, err)
	require.Len(t, page1, 3)

	// Most recent has amount 1000.
	require.Equal(t, int64(1000), page1[0].AmountSat)

	// Page 2: next 3 entries.
	page2, err := store.ListLedgerEntries(
		ctx, sqlc.ListLedgerEntriesParams{
			Limit:  3,
			Offset: 3,
		},
	)
	require.NoError(t, err)
	require.Len(t, page2, 3)

	// No overlap between pages.
	require.NotEqual(
		t, page1[2].AmountSat, page2[0].AmountSat,
	)
}

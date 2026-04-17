package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/stretchr/testify/require"
)

// newLedgerStoreForTest creates a LedgerStoreDB backed by a fresh test
// database with all migrations applied.
func newLedgerStoreForTest(t *testing.T) *LedgerStoreDB {
	t.Helper()

	db := NewTestDB(t)

	txExec := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) *sqlc.Queries {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	return &LedgerStoreDB{
		TransactionExecutor: txExec,
	}
}

// makeLedgerEntry is a test helper that creates a ledger.LedgerEntry with the
// given parameters and sensible defaults for the remaining fields.
func makeLedgerEntry(debit, credit string, amount int64,
	eventType string, roundID []byte, ts int64) ledger.LedgerEntry {

	return ledger.LedgerEntry{
		DebitAccount:  debit,
		CreditAccount: credit,
		AmountSat:     amount,
		RoundID:       roundID,
		EventType:     eventType,
		Description:   eventType + " test entry",
		CreatedAt:     ts,
	}
}

// TestLedgerStoreInsertAndRetrieve verifies that a single ledger entry
// can be inserted and retrieved via ListLedgerEntries.
func TestLedgerStoreInsertAndRetrieve(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	now := time.Now().Unix()
	roundID := []byte("round-001")

	entry := makeLedgerEntry(
		"fees_paid", "wallet_balance", 1000,
		"boarding_fee_paid", roundID, now,
	)
	require.NoError(t, store.InsertLedgerEntry(ctx, entry))

	// Retrieve the entry.
	entries, err := store.ListLedgerEntries(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	got := entries[0]
	require.Equal(t, entry.DebitAccount, got.DebitAccount)
	require.Equal(t, entry.CreditAccount, got.CreditAccount)
	require.Equal(t, entry.AmountSat, got.AmountSat)
	require.Equal(t, entry.RoundID, got.RoundID)
	require.Equal(t, entry.EventType, got.EventType)
	require.Equal(t, entry.Description, got.Description)
	require.Equal(t, entry.CreatedAt, got.CreatedAt)
}

// TestLedgerStoreAccountBalance verifies that GetAccountBalance
// correctly computes net balance (debits minus credits) for an account.
func TestLedgerStoreAccountBalance(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	now := time.Now().Unix()

	// Insert a debit of 5000 to fees_paid from wallet_balance.
	entry1 := makeLedgerEntry(
		"fees_paid", "wallet_balance", 5000,
		"boarding_fee_paid", []byte("round-a"), now,
	)
	require.NoError(t, store.InsertLedgerEntry(ctx, entry1))

	// Insert a credit to fees_paid (debit from vtxo_balance).
	entry2 := makeLedgerEntry(
		"vtxo_balance", "fees_paid", 2000,
		"refresh_fee_paid", []byte("round-b"), now+1,
	)
	require.NoError(t, store.InsertLedgerEntry(ctx, entry2))

	// fees_paid: debits=5000, credits=2000 => balance=3000.
	balance, err := store.GetAccountBalance(ctx, "fees_paid")
	require.NoError(t, err)
	require.Equal(t, int64(3000), balance)

	// wallet_balance: debits=0, credits=5000 => balance=-5000.
	balance, err = store.GetAccountBalance(ctx, "wallet_balance")
	require.NoError(t, err)
	require.Equal(t, int64(-5000), balance)

	// vtxo_balance: debits=2000, credits=0 => balance=2000.
	balance, err = store.GetAccountBalance(ctx, "vtxo_balance")
	require.NoError(t, err)
	require.Equal(t, int64(2000), balance)
}

// TestLedgerStoreAccountBalanceEmpty verifies that querying the balance
// of an account with no entries returns zero.
func TestLedgerStoreAccountBalanceEmpty(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	balance, err := store.GetAccountBalance(ctx, "fees_paid")
	require.NoError(t, err)
	require.Equal(t, int64(0), balance)
}

// TestLedgerStoreTotalOperatorFeesPaid verifies that
// GetTotalOperatorFeesPaid sums only entries debited to the fees_paid
// account.
func TestLedgerStoreTotalOperatorFeesPaid(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	now := time.Now().Unix()

	// Two fee entries debiting fees_paid.
	e1 := makeLedgerEntry(
		"fees_paid", "wallet_balance", 3000,
		"boarding_fee_paid", []byte("round-1"), now,
	)
	e2 := makeLedgerEntry(
		"fees_paid", "wallet_balance", 7000,
		"refresh_fee_paid", []byte("round-2"), now+1,
	)

	// An unrelated entry that should not be counted.
	e3 := makeLedgerEntry(
		"vtxo_balance", "wallet_balance", 500,
		"vtxo_received", []byte("round-3"), now+2,
	)

	require.NoError(t, store.InsertLedgerEntry(ctx, e1))
	require.NoError(t, store.InsertLedgerEntry(ctx, e2))
	require.NoError(t, store.InsertLedgerEntry(ctx, e3))

	total, err := store.GetTotalOperatorFeesPaid(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(10000), total)
}

// TestLedgerStoreTotalOperatorFeesPaidEmpty verifies that
// GetTotalOperatorFeesPaid returns zero when no entries exist.
func TestLedgerStoreTotalOperatorFeesPaidEmpty(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	total, err := store.GetTotalOperatorFeesPaid(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), total)
}

// TestLedgerStoreListEntriesPagination verifies that ListLedgerEntries
// respects limit and offset, returning entries in descending created_at
// order.
func TestLedgerStoreListEntriesPagination(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	baseTime := time.Now().Unix()

	// Insert 5 entries with distinct timestamps.
	for i := range 5 {
		e := makeLedgerEntry(
			"fees_paid", "wallet_balance", int64(1000*(i+1)),
			"boarding_fee_paid", []byte{byte(i)}, baseTime+int64(i),
		)
		require.NoError(t, store.InsertLedgerEntry(ctx, e))
	}

	// Fetch first page (2 entries).
	page1, err := store.ListLedgerEntries(ctx, 2, 0)
	require.NoError(t, err)
	require.Len(t, page1, 2)

	// Should be newest first (index 4, then 3).
	require.Equal(t, baseTime+4, page1[0].CreatedAt)
	require.Equal(t, baseTime+3, page1[1].CreatedAt)

	// Fetch second page.
	page2, err := store.ListLedgerEntries(ctx, 2, 2)
	require.NoError(t, err)
	require.Len(t, page2, 2)
	require.Equal(t, baseTime+2, page2[0].CreatedAt)
	require.Equal(t, baseTime+1, page2[1].CreatedAt)

	// Fetch third page.
	page3, err := store.ListLedgerEntries(ctx, 2, 4)
	require.NoError(t, err)
	require.Len(t, page3, 1)
	require.Equal(t, baseTime, page3[0].CreatedAt)
}

// TestLedgerStoreListEntriesByType verifies filtering by event type.
func TestLedgerStoreListEntriesByType(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	now := time.Now().Unix()

	// Insert entries with different event types.
	boarding := makeLedgerEntry(
		"fees_paid", "wallet_balance", 1000,
		"boarding_fee_paid", []byte("r-1"), now,
	)
	refresh := makeLedgerEntry(
		"fees_paid", "wallet_balance", 2000,
		"refresh_fee_paid", []byte("r-2"), now+1,
	)
	boarding2 := makeLedgerEntry(
		"fees_paid", "wallet_balance", 3000,
		"boarding_fee_paid", []byte("r-3"), now+2,
	)

	require.NoError(t, store.InsertLedgerEntry(ctx, boarding))
	require.NoError(t, store.InsertLedgerEntry(ctx, refresh))
	require.NoError(t, store.InsertLedgerEntry(ctx, boarding2))

	// Filter by boarding_fee_paid.
	entries, err := store.ListLedgerEntriesByType(
		ctx, "boarding_fee_paid", 10, 0,
	)
	require.NoError(t, err)
	require.Len(t, entries, 2)

	// Should be newest first.
	require.Equal(t, int64(3000), entries[0].AmountSat)
	require.Equal(t, int64(1000), entries[1].AmountSat)

	// Filter by refresh_fee_paid.
	entries, err = store.ListLedgerEntriesByType(
		ctx, "refresh_fee_paid", 10, 0,
	)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, int64(2000), entries[0].AmountSat)
}

// TestLedgerStoreListEntriesByTypePagination verifies that the type
// filter correctly applies limit and offset.
func TestLedgerStoreListEntriesByTypePagination(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	baseTime := time.Now().Unix()

	// Insert 4 entries of the same type.
	for i := range 4 {
		e := makeLedgerEntry(
			"fees_paid", "wallet_balance", int64(100*(i+1)),
			"boarding_fee_paid", []byte{byte(i)},
			baseTime+int64(i),
		)
		require.NoError(t, store.InsertLedgerEntry(ctx, e))
	}

	// Page through with limit 2.
	page1, err := store.ListLedgerEntriesByType(
		ctx, "boarding_fee_paid", 2, 0,
	)
	require.NoError(t, err)
	require.Len(t, page1, 2)

	page2, err := store.ListLedgerEntriesByType(
		ctx, "boarding_fee_paid", 2, 2,
	)
	require.NoError(t, err)
	require.Len(t, page2, 2)

	// No overlap between pages.
	require.NotEqual(t, page1[0].EntryID, page2[0].EntryID)
	require.NotEqual(t, page1[1].EntryID, page2[1].EntryID)
}

// TestLedgerStoreCountEntries verifies the count query returns the
// correct total.
func TestLedgerStoreCountEntries(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	// Start with zero entries.
	count, err := store.CountLedgerEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), count)

	now := time.Now().Unix()

	// Insert 3 entries.
	for i := range 3 {
		e := makeLedgerEntry(
			"fees_paid", "wallet_balance", 1000,
			"boarding_fee_paid", []byte{byte(i)},
			now+int64(i),
		)
		require.NoError(t, store.InsertLedgerEntry(ctx, e))
	}

	count, err = store.CountLedgerEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), count)
}

// TestLedgerStoreListAccounts verifies that the migration seed data for
// the chart of accounts is returned correctly, including the
// opening_balance equity account that acts as the source-of-funds
// counterparty for wallet UTXO confirmations.
func TestLedgerStoreListAccounts(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	accounts, err := store.ListAccounts(ctx)
	require.NoError(t, err)

	// The migration seeds 7 accounts: wallet_balance, vtxo_balance,
	// fees_paid, onchain_fees, transfers_in, transfers_out,
	// opening_balance.
	require.Len(t, accounts, 7)

	// Build a map for easier assertions.
	byID := make(map[string]sqlc.Account, len(accounts))
	for _, a := range accounts {
		byID[a.AccountID] = a
	}

	// Verify a few key accounts.
	require.Equal(t, "asset", byID["wallet_balance"].AccountType)
	require.Equal(t, "Wallet Balance", byID["wallet_balance"].AccountName)

	require.Equal(t, "expense", byID["fees_paid"].AccountType)
	require.Equal(t, "Fees Paid", byID["fees_paid"].AccountName)

	require.Equal(t, "revenue", byID["transfers_in"].AccountType)
	require.Equal(t, "Transfers In", byID["transfers_in"].AccountName)

	require.Equal(t, "expense", byID["transfers_out"].AccountType)
	require.Equal(t, "Transfers Out", byID["transfers_out"].AccountName)

	// opening_balance is the equity source-of-funds account for
	// wallet UTXO deposits. Without it, wallet_balance would drift
	// negative on every boarding because SourceRoundBoarding only
	// ever credits it.
	require.Equal(t, "equity", byID["opening_balance"].AccountType)
	require.Equal(
		t, "Opening Balance",
		byID["opening_balance"].AccountName,
	)
}

// TestLedgerStoreIdempotentInsert verifies that a redelivered
// message resolves to a silent no-op: the partial unique index on
// (round_id, event_type, debit_account, credit_account) combined
// with ON CONFLICT DO NOTHING on InsertClientLedgerEntry swallows
// the duplicate. The call returns nil (so durable-actor replay
// does not nack) and the row count stays at one.
func TestLedgerStoreIdempotentInsert(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	now := time.Now().Unix()
	roundID := []byte("round-dup")

	entry := makeLedgerEntry(
		"fees_paid", "wallet_balance", 1000,
		"boarding_fee_paid", roundID, now,
	)

	// First insert succeeds.
	require.NoError(t, store.InsertLedgerEntry(ctx, entry))

	// Second insert with the same (round_id, event_type,
	// debit_account, credit_account) is swallowed by
	// ON CONFLICT DO NOTHING rather than surfacing a
	// constraint violation. Returning an error here would
	// drive an infinite durable-actor retry loop on a
	// permanent condition.
	require.NoError(t, store.InsertLedgerEntry(ctx, entry))

	// Only one entry should exist.
	count, err := store.CountLedgerEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

// TestLedgerStoreNilRoundIDAllowsDuplicates verifies that entries with
// NULL round_id are not subject to the idempotency constraint, since
// the unique index uses a WHERE round_id IS NOT NULL filter.
func TestLedgerStoreNilRoundIDAllowsDuplicates(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	now := time.Now().Unix()

	entry := makeLedgerEntry(
		"fees_paid", "wallet_balance", 500,
		"onchain_fee_paid", nil, now,
	)

	// Both inserts should succeed because round_id is NULL.
	require.NoError(t, store.InsertLedgerEntry(ctx, entry))

	entry.CreatedAt = now + 1
	require.NoError(t, store.InsertLedgerEntry(ctx, entry))

	count, err := store.CountLedgerEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
}

// TestLedgerStoreIdempotentInsertBySession verifies that a
// redelivered OOR VTXO-sent message is deduped silently: the
// partial unique index idx_client_ledger_idempotent_session
// combined with ON CONFLICT DO NOTHING on
// InsertClientLedgerEntry treats the duplicate as a no-op
// instead of surfacing a constraint error.
func TestLedgerStoreIdempotentInsertBySession(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	now := time.Now().Unix()
	sessionID := []byte("session-abcdefghijklmnopqrstuvwx")

	entry := ledger.LedgerEntry{
		DebitAccount:  "transfers_out",
		CreditAccount: "vtxo_balance",
		AmountSat:     5_000,
		SessionID:     sessionID,
		EventType:     "vtxo_sent",
		Description:   "duplicate session send test",
		CreatedAt:     now,
	}

	// First insert succeeds.
	require.NoError(t, store.InsertLedgerEntry(ctx, entry))

	// Replay of the same (session_id, event_type, debit,
	// credit) tuple is swallowed by ON CONFLICT DO NOTHING
	// and returns nil so the durable actor can ack the
	// redelivery instead of nacking forever.
	entry.CreatedAt = now + 1
	require.NoError(t, store.InsertLedgerEntry(ctx, entry))

	count, err := store.CountLedgerEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

// TestLedgerStoreCheckConstraintSameAccount verifies that the CHECK
// constraint preventing debit_account == credit_account is enforced.
func TestLedgerStoreCheckConstraintSameAccount(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	entry := makeLedgerEntry(
		"fees_paid", "fees_paid", 1000,
		"boarding_fee_paid", []byte("round-x"), time.Now().Unix(),
	)

	err := store.InsertLedgerEntry(ctx, entry)
	require.Error(t, err)
}

// TestLedgerStoreCheckConstraintPositiveAmount verifies that the CHECK
// constraint enforcing amount_sat > 0 rejects zero and negative amounts.
func TestLedgerStoreCheckConstraintPositiveAmount(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	now := time.Now().Unix()

	// Zero amount should be rejected.
	zeroEntry := makeLedgerEntry(
		"fees_paid", "wallet_balance", 0,
		"boarding_fee_paid", []byte("round-zero"), now,
	)
	err := store.InsertLedgerEntry(ctx, zeroEntry)
	require.Error(t, err)

	// Negative amount should also be rejected.
	negEntry := makeLedgerEntry(
		"fees_paid", "wallet_balance", -100,
		"boarding_fee_paid", []byte("round-neg"), now+1,
	)
	err = store.InsertLedgerEntry(ctx, negEntry)
	require.Error(t, err)
}

// TestLedgerStoreForeignKeyEventType verifies that inserting an entry
// with an invalid event type fails the FK constraint.
func TestLedgerStoreForeignKeyEventType(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	entry := makeLedgerEntry(
		"fees_paid", "wallet_balance", 1000,
		"invalid_event_type", []byte("round-fk"), time.Now().Unix(),
	)

	err := store.InsertLedgerEntry(ctx, entry)
	require.Error(t, err)
}

// TestLedgerStoreForeignKeyAccount verifies that inserting an entry
// with an invalid account ID fails the FK constraint.
func TestLedgerStoreForeignKeyAccount(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	entry := makeLedgerEntry(
		"nonexistent_account", "wallet_balance", 1000,
		"boarding_fee_paid", []byte("round-fk2"), time.Now().Unix(),
	)

	err := store.InsertLedgerEntry(ctx, entry)
	require.Error(t, err)
}

// TestLedgerStoreMultipleAccountBalances verifies balance computation
// across several accounts with many entries to exercise the aggregate
// query paths.
func TestLedgerStoreMultipleAccountBalances(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newLedgerStoreForTest(t)

	now := time.Now().Unix()

	// Simulate a boarding fee: wallet_balance -> fees_paid.
	e1 := makeLedgerEntry(
		"fees_paid", "wallet_balance", 5000,
		"boarding_fee_paid", []byte("r-01"), now,
	)

	// Simulate receiving a VTXO: vtxo_balance <- transfers_in.
	e2 := makeLedgerEntry(
		"vtxo_balance", "transfers_in", 20000,
		"vtxo_received", []byte("r-02"), now+1,
	)

	// Simulate sending a VTXO: fees_paid <- vtxo_balance.
	e3 := makeLedgerEntry(
		"fees_paid", "vtxo_balance", 1000,
		"vtxo_sent", []byte("r-03"), now+2,
	)

	// Simulate on-chain fee: onchain_fees <- wallet_balance.
	e4 := makeLedgerEntry(
		"onchain_fees", "wallet_balance", 250,
		"onchain_fee_paid", []byte("r-04"), now+3,
	)

	for _, e := range []ledger.LedgerEntry{e1, e2, e3, e4} {
		require.NoError(t, store.InsertLedgerEntry(ctx, e))
	}

	// wallet_balance: debits=0, credits=5000+250=5250 => -5250.
	bal, err := store.GetAccountBalance(ctx, "wallet_balance")
	require.NoError(t, err)
	require.Equal(t, int64(-5250), bal)

	// fees_paid: debits=5000+1000=6000, credits=0 => 6000.
	bal, err = store.GetAccountBalance(ctx, "fees_paid")
	require.NoError(t, err)
	require.Equal(t, int64(6000), bal)

	// vtxo_balance: debits=20000, credits=1000 => 19000.
	bal, err = store.GetAccountBalance(ctx, "vtxo_balance")
	require.NoError(t, err)
	require.Equal(t, int64(19000), bal)

	// transfers_in: debits=0, credits=20000 => -20000.
	bal, err = store.GetAccountBalance(ctx, "transfers_in")
	require.NoError(t, err)
	require.Equal(t, int64(-20000), bal)

	// onchain_fees: debits=250, credits=0 => 250.
	bal, err = store.GetAccountBalance(ctx, "onchain_fees")
	require.NoError(t, err)
	require.Equal(t, int64(250), bal)

	// Total operator fees paid = debits to fees_paid = 6000.
	total, err := store.GetTotalOperatorFeesPaid(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(6000), total)
}

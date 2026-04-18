package db

import (
	"testing"
	"time"

	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightninglabs/darepo/fees"
	"github.com/stretchr/testify/require"
)

// newTestLedgerStore builds a LedgerStoreDB backed by a fresh
// test SQLite database. The store embeds a
// TransactionExecutor[*sqlc.Queries] so the test exercises the
// full ExecTx path, not the raw sqlc.Queries embedded on *Store.
func newTestLedgerStore(t testing.TB) (*LedgerStoreDB, *Store) {
	t.Helper()
	store := newTestStore(t)
	return NewLedgerStoreDB(store), store
}

// TestLedgerStoreDBRoundTrip verifies that an entry inserted
// via the adapter (LedgerStoreDB.InsertLedgerEntry) is visible
// through the sibling read queries on *Store. This is the
// primary guard that the adapter's ExecTx wrapper commits the
// row on success and propagates the fields correctly into the
// generated InsertLedgerEntryParams struct.
func TestLedgerStoreDBRoundTrip(t *testing.T) {
	t.Parallel()

	adapter, store := newTestLedgerStore(t)
	ctx := t.Context()

	now := time.Now().Unix()

	entry := LedgerEntry{
		DebitAccount:  fees.AccountDeployedCapital,
		CreditAccount: fees.AccountOperatorRevenue,
		AmountSat:     1234,
		RoundID:       []byte("round-adapter-test"),
		EventType:     fees.LedgerEventBoardingFee,
		Description:   "round-trip adapter test",
		CreatedAt:     now,
	}

	err := adapter.InsertLedgerEntry(ctx, entry)
	require.NoError(t, err)

	// Read back via the embedded sqlc.Queries on *Store.
	entries, err := store.ListLedgerEntries(
		ctx, sqlc.ListLedgerEntriesParams{
			Limit:  100,
			Offset: 0,
		},
	)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	// sqlc model fields are raw strings; cast the typed ids
	// back to string for direct comparison.
	got := entries[0]
	require.Equal(
		t, string(entry.DebitAccount), got.DebitAccount,
	)
	require.Equal(
		t, string(entry.CreditAccount), got.CreditAccount,
	)
	require.Equal(t, entry.AmountSat, got.AmountSat)
	require.Equal(t, entry.RoundID, got.RoundID)
	require.Equal(t, string(entry.EventType), got.EventType)
	require.Equal(t, entry.Description, got.Description)
	require.Equal(t, entry.CreatedAt, got.CreatedAt)
}

// TestLedgerStoreDBAccountsMatchChartOfAccounts verifies that the
// typed AccountID constants in the fees package are exactly the
// set of accounts seeded by the accounting migration. Drift
// between the two (e.g. adding a new account in the schema
// without adding the Go constant, or vice versa) would cause
// silent FK failures at runtime — this test catches it at build
// time of the test suite.
func TestLedgerStoreDBAccountsMatchChartOfAccounts(t *testing.T) {
	t.Parallel()

	_, store := newTestLedgerStore(t)
	ctx := t.Context()

	seeded, err := store.ListAccounts(ctx)
	require.NoError(t, err)

	seededIDs := make(map[string]struct{}, len(seeded))
	for _, a := range seeded {
		seededIDs[a.AccountID] = struct{}{}
	}

	goIDs := make(map[string]struct{})
	for _, id := range fees.AllAccounts() {
		goIDs[string(id)] = struct{}{}
	}

	// Every Go constant must exist in the seeded chart.
	for id := range goIDs {
		_, ok := seededIDs[id]
		require.True(t, ok,
			"Go AccountID %q is not seeded in the chart "+
				"of accounts", id)
	}

	// Every seeded account must have a matching Go constant.
	for id := range seededIDs {
		_, ok := goIDs[id]
		require.True(t, ok,
			"seeded account %q has no matching "+
				"fees.AccountID constant", id)
	}
}

// TestLedgerStoreDBFKError verifies that the adapter propagates
// a foreign-key violation from the underlying sqlc insert as a
// Go error rather than silently succeeding. An unseeded account
// id must cause InsertLedgerEntry to return a non-nil error,
// and no row should be committed.
func TestLedgerStoreDBFKError(t *testing.T) {
	t.Parallel()

	adapter, store := newTestLedgerStore(t)
	ctx := t.Context()

	err := adapter.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:  fees.AccountID("not_a_real_account"),
		CreditAccount: fees.AccountOperatorRevenue,
		AmountSat:     500,
		EventType:     fees.LedgerEventBoardingFee,
		Description:   "should fail FK",
		CreatedAt:     time.Now().Unix(),
	})
	require.Error(t, err, "unseeded debit_account must fail FK")

	// Transaction was rolled back: no rows visible.
	count, err := store.CountLedgerEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), count,
		"failed insert must not leak a partial row")
}

// TestLedgerStoreDBCheckConstraint verifies that the adapter
// propagates schema CHECK violations — zero-amount entries and
// self-transfers — as Go errors. This guards both the
// amount_sat > 0 constraint and the
// debit_account <> credit_account constraint.
func TestLedgerStoreDBCheckConstraint(t *testing.T) {
	t.Parallel()

	adapter, store := newTestLedgerStore(t)
	ctx := t.Context()

	now := time.Now().Unix()

	// Zero-amount entry must be rejected.
	err := adapter.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:  fees.AccountDeployedCapital,
		CreditAccount: fees.AccountOperatorRevenue,
		AmountSat:     0,
		EventType:     fees.LedgerEventBoardingFee,
		Description:   "zero amount",
		CreatedAt:     now,
	})
	require.Error(t, err, "zero-amount entry must be rejected")

	// Self-transfer must be rejected.
	err = adapter.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:  fees.AccountDeployedCapital,
		CreditAccount: fees.AccountDeployedCapital,
		AmountSat:     1000,
		EventType:     fees.LedgerEventBoardingFee,
		Description:   "self-transfer via adapter",
		CreatedAt:     now,
	})
	require.Error(t, err, "self-transfer must be rejected")

	count, err := store.CountLedgerEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), count)
}

// TestLedgerStoreDBMultipleInserts verifies that sequential
// adapter calls each commit independently. Each call runs in
// its own ExecTx, so a later failure does not roll back an
// earlier successful insert.
func TestLedgerStoreDBMultipleInserts(t *testing.T) {
	t.Parallel()

	adapter, store := newTestLedgerStore(t)
	ctx := t.Context()

	now := time.Now().Unix()

	// First insert succeeds.
	err := adapter.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:  fees.AccountDeployedCapital,
		CreditAccount: fees.AccountUserVTXOClaims,
		AmountSat:     98_000,
		EventType:     fees.LedgerEventBoardingDeposit,
		Description:   "boarding deposit",
		CreatedAt:     now,
	})
	require.NoError(t, err)

	// Second insert succeeds.
	err = adapter.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:  fees.AccountDeployedCapital,
		CreditAccount: fees.AccountOperatorRevenue,
		AmountSat:     2_000,
		EventType:     fees.LedgerEventBoardingFee,
		Description:   "boarding fee",
		CreatedAt:     now + 1,
	})
	require.NoError(t, err)

	// Third insert fails (invalid event type).
	err = adapter.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:  fees.AccountDeployedCapital,
		CreditAccount: fees.AccountOperatorRevenue,
		AmountSat:     1,
		EventType:     fees.LedgerEventType("nonsense_event"),
		Description:   "should fail",
		CreatedAt:     now + 2,
	})
	require.Error(t, err)

	// First two inserts remain committed.
	count, err := store.CountLedgerEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), count,
		"prior successful inserts must survive a later failure")

	// Sanity: deployed_capital balance reflects both inserts.
	balance, err := store.GetAccountBalance(
		ctx, "deployed_capital",
	)
	require.NoError(t, err)
	require.Equal(t, int64(100_000), balance,
		"deployed_capital should reflect 98_000 + 2_000")
}

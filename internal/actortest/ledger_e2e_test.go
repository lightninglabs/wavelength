package actortest

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/actordelivery"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// newLedgerActorForTest wires a real ledger.LedgerActor on the durable
// Read/Commit execution path. The tx-aware delivery store and the ledger store
// are backed by the SAME in-memory SQLite database, so a handler's
// InsertLedgerEntry joins the actor's Commit transaction (via
// actor.TxFromContext) exactly as it does in production. This exercises the
// full stack -- durable mailbox lease/ack + fenced Commit + double-entry
// writes -- rather than the mocks used in the ledger package's unit tests.
func newLedgerActorForTest(t *testing.T) (*ledger.LedgerActor,
	*db.LedgerStoreDB) {

	t.Helper()

	sqlDB := db.NewTestDB(t)

	// Use a real-time clock so the delivery store's available_at/lease
	// timestamps agree with the durable mailbox's default clock. Injecting
	// a frozen test clock here would desync the two (the mailbox stamps
	// available_at from its own default clock) and the message would never
	// be claim-eligible.
	clk := clock.NewDefaultClock()

	txStore, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		sqlDB.DB, sqlDB.Backend(), clk, btclog.Disabled,
	)
	require.NoError(t, err)

	ledgerTxExec := db.NewTransactionExecutor(
		sqlDB.BaseDB,
		func(tx *sql.Tx) *sqlc.Queries {
			return sqlDB.WithTx(tx)
		},
		btclog.Disabled,
	)
	ledgerStore := &db.LedgerStoreDB{TransactionExecutor: ledgerTxExec}

	ledgerActor := ledger.NewLedgerActor(ledger.ActorConfig{
		Log:           fn.None[btclog.Logger](),
		DeliveryStore: txStore,
		LedgerStore:   ledgerStore,
		Clock:         fn.Some[clock.Clock](clk),
	})
	require.NoError(t, ledgerActor.Start(t.Context()))

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()

		_ = ledgerActor.OnStop(ctx)
	})

	return ledgerActor, ledgerStore
}

// requireBalance asserts the net (debits minus credits) balance of an account.
func requireBalance(t *testing.T, store *db.LedgerStoreDB, account string,
	want int64) {

	t.Helper()

	got, err := store.GetAccountBalance(t.Context(), account)
	require.NoError(t, err)
	require.Equalf(t, want, got, "account %s balance", account)
}

// tryCount reads the total ledger row count, tolerating transient read errors
// (e.g. SQLITE_BUSY while the actor holds the writer during a retry). Returns
// ok=false when the count could not be read, so polling closures treat a
// transient failure as "not yet / nothing observed" rather than a hard error.
func tryCount(store *db.LedgerStoreDB) (int64, bool) {
	n, err := store.CountLedgerEntries(context.Background())
	if err != nil {
		return 0, false
	}

	return n, true
}

// TestLedgerActorExitCostCommitsBothLegs drives a real ExitCostMsg through the
// durable mailbox and asserts the two legs (send + fee) are booked atomically
// in one fenced Commit, and that a replay is idempotent (the shared
// outpoint-keyed legs dedup, so no double-booking through the real stack).
func TestLedgerActorExitCostCommitsBothLegs(t *testing.T) {
	t.Parallel()

	ledgerActor, store := newLedgerActorForTest(t)

	msg := &ledger.ExitCostMsg{
		OutpointHash: [32]byte{
			0xaa,
			0x01,
			0x02,
		},
		OutpointIndex: 3,
		AmountSat:     10_000,
		ExitCostSat:   1_500,
		BlockHeight:   880_000,
	}
	require.NoError(t, ledgerActor.Ref().Tell(t.Context(), msg))

	// Both legs commit: the message drains and exactly two rows land.
	require.Eventually(t, func() bool {
		n, ok := tryCount(store)

		return ok && n == 2
	}, 3*time.Second, 20*time.Millisecond)

	// Send leg debits transfers_out by the net amount; fee leg debits
	// onchain_fees by the exit cost; vtxo_balance is credited the gross.
	net := msg.AmountSat - msg.ExitCostSat
	requireBalance(t, store, ledger.AccountTransfersOut, net)
	requireBalance(t, store, ledger.AccountOnchainFees, msg.ExitCostSat)
	requireBalance(t, store, ledger.AccountVTXOBalance, -msg.AmountSat)

	// Replaying the same event is a no-op: both legs share the
	// outpoint-derived idempotency key and dedup via ON CONFLICT DO
	// NOTHING, so the row count never grows past two.
	require.NoError(t, ledgerActor.Ref().Tell(t.Context(), msg))
	require.Never(t, func() bool {
		n, ok := tryCount(store)

		return ok && n != 2
	}, 500*time.Millisecond, 50*time.Millisecond)
}

// TestLedgerActorRejectsInvalidExitCost confirms a handler validation failure
// (exit cost >= VTXO amount) commits nothing: because the Commit closure
// returns an error, the fenced transaction rolls back and no partial ledger
// leg is left behind.
func TestLedgerActorRejectsInvalidExitCost(t *testing.T) {
	t.Parallel()

	ledgerActor, store := newLedgerActorForTest(t)

	// Fee consumes the whole VTXO: handleExitCost rejects with
	// ErrInvalidMessage before any leg can be persisted.
	msg := &ledger.ExitCostMsg{
		OutpointHash: [32]byte{
			0xbb,
			0x09,
		},
		OutpointIndex: 1,
		AmountSat:     2_000,
		ExitCostSat:   2_000,
		BlockHeight:   880_001,
	}
	require.NoError(t, ledgerActor.Ref().Tell(t.Context(), msg))

	// No ledger rows are ever written, even as the message is retried.
	require.Never(t, func() bool {
		n, ok := tryCount(store)

		return ok && n != 0
	}, 500*time.Millisecond, 50*time.Millisecond)
}

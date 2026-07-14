package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/stretchr/testify/require"
)

// testQueryCreator wraps sqlc.Queries and records the underlying *sql.Tx so
// tests can verify which transaction the executor chose. The backend field
// tracks the database type so helpers can emit the correct SQL dialect.
type testQueryCreator struct {
	*sqlc.Queries

	// tx is the underlying *sql.Tx that was passed to the query creator.
	tx *sql.Tx

	// backend is the database backend type used by the test database.
	backend sqlc.BackendType
}

// newTxExecutorForTest creates a TransactionExecutor backed by a test database
// and a helper table for verifying transaction semantics. The DDL adapts to the
// active backend (SQLite vs Postgres). The returned TransactionExecutor uses a
// testQueryCreator that records the underlying transaction for inspection.
func newTxExecutorForTest(t *testing.T) (
	*TransactionExecutor[*testQueryCreator], *BaseDB) {

	db := NewTestDB(t)
	backend := db.BaseDB.Backend()

	// Pick the correct DDL for the active backend. SQLite uses INTEGER
	// PRIMARY KEY while Postgres uses SERIAL PRIMARY KEY.
	var ddl string
	switch backend {
	case sqlc.BackendTypePostgres:
		ddl = `CREATE TABLE IF NOT EXISTS tx_test (
			id SERIAL PRIMARY KEY,
			val TEXT NOT NULL
		)`

	default:
		ddl = `CREATE TABLE IF NOT EXISTS tx_test (
			id INTEGER PRIMARY KEY,
			val TEXT NOT NULL
		)`
	}

	// Create a simple table used to observe writes within transactions.
	_, err := db.BaseDB.Exec(ddl)
	require.NoError(t, err)

	txExec := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) *testQueryCreator {
			return &testQueryCreator{
				Queries: db.BaseDB.WithTx(tx),
				tx:      tx,
				backend: backend,
			}
		},
		btclog.Disabled,
	)

	return txExec, db.BaseDB
}

// countRows returns the number of rows in the tx_test table visible to the
// provided querier (either *sql.DB or *sql.Tx).
func countRows(t *testing.T, q actor.TxQuerier) int {
	t.Helper()

	row := q.QueryRowContext(
		t.Context(), "SELECT count(*) FROM tx_test",
	)

	var count int
	require.NoError(t, row.Scan(&count))

	return count
}

// insertRow inserts a row into the tx_test table using the transaction
// captured by the testQueryCreator. The placeholder syntax adapts to the
// active backend (? for SQLite, $N for Postgres).
func insertRow(q *testQueryCreator, id int, val string) error {
	var query string
	switch q.backend {
	case sqlc.BackendTypePostgres:
		query = "INSERT INTO tx_test (id, val) VALUES ($1, $2)"

	default:
		query = "INSERT INTO tx_test (id, val) VALUES (?, ?)"
	}

	_, err := q.tx.ExecContext(
		context.Background(), query, id, val,
	)

	return err
}

// TestExecTxJoinOuterActorTx verifies that when the context carries an outer
// database transaction from the actor framework, ExecTx joins it instead of
// creating a new one. The outer transaction controls commit/rollback.
func TestExecTxJoinOuterActorTx(t *testing.T) {
	t.Parallel()

	txExec, baseDB := newTxExecutorForTest(t)

	// Start an outer transaction that simulates what the
	// TxAwareActorDeliveryStore would create for durable actor message
	// processing.
	outerTx, err := baseDB.DB.BeginTx(
		t.Context(), &sql.TxOptions{
			Isolation: sql.LevelSerializable,
		},
	)
	require.NoError(t, err)

	defer func() {
		_ = outerTx.Rollback()
	}()

	// Inject the outer transaction into the context.
	ctx := actor.WithTx(t.Context(), outerTx)

	// ExecTx should join the outer transaction and write within it.
	err = txExec.ExecTx(
		ctx, WriteTxOption(), func(q *testQueryCreator) error {
			return insertRow(q, 1, "hello")
		},
	)
	require.NoError(t, err)

	// The row should be visible within the outer transaction but NOT yet
	// visible from a separate connection (since outerTx hasn't
	// committed).
	require.Equal(t, 1, countRows(t, outerTx))
	require.Equal(t, 0, countRows(t, baseDB.DB))

	// Commit the outer transaction.
	require.NoError(t, outerTx.Commit())

	// Now the row is visible globally.
	require.Equal(t, 1, countRows(t, baseDB.DB))
}

// TestExecTxStandaloneNoTx verifies that when no outer transaction is present
// in the context, ExecTx creates its own transaction and commits it. This is
// the existing behavior and serves as a regression test.
func TestExecTxStandaloneNoTx(t *testing.T) {
	t.Parallel()

	txExec, baseDB := newTxExecutorForTest(t)

	err := txExec.ExecTx(
		t.Context(), WriteTxOption(),
		func(q *testQueryCreator) error {
			return insertRow(q, 1, "standalone")
		},
	)
	require.NoError(t, err)

	// The row should be visible immediately since ExecTx committed its
	// own transaction.
	require.Equal(t, 1, countRows(t, baseDB.DB))
}

// TestExecTxOuterTxRollbackAtomicity verifies that when two ExecTx calls
// join the same outer transaction, rolling back the outer transaction
// atomically discards both writes. This is the key atomicity guarantee that
// the durable actor framework relies on.
func TestExecTxOuterTxRollbackAtomicity(t *testing.T) {
	t.Parallel()

	txExec, baseDB := newTxExecutorForTest(t)

	outerTx, err := baseDB.DB.BeginTx(
		t.Context(), &sql.TxOptions{
			Isolation: sql.LevelSerializable,
		},
	)
	require.NoError(t, err)

	defer func() {
		_ = outerTx.Rollback()
	}()

	ctx := actor.WithTx(t.Context(), outerTx)

	// First ExecTx call writes row 1.
	err = txExec.ExecTx(
		ctx, WriteTxOption(), func(q *testQueryCreator) error {
			return insertRow(q, 1, "first-store")
		},
	)
	require.NoError(t, err)

	// Second ExecTx call writes row 2 (simulating a second store
	// operation within the same actor message).
	err = txExec.ExecTx(
		ctx, WriteTxOption(), func(q *testQueryCreator) error {
			return insertRow(q, 2, "second-store")
		},
	)
	require.NoError(t, err)

	// Both rows are visible within the transaction.
	require.Equal(t, 2, countRows(t, outerTx))

	// Rolling back the outer transaction discards BOTH writes
	// atomically.
	require.NoError(t, outerTx.Rollback())
	require.Equal(t, 0, countRows(t, baseDB.DB))
}

// TestExecTxOuterTxErrorPropagation verifies that when the txBody returns an
// error while joined to an outer transaction, the error propagates without
// ExecTx committing or rolling back the outer transaction. The outer
// transaction owner decides what to do.
func TestExecTxOuterTxErrorPropagation(t *testing.T) {
	t.Parallel()

	txExec, baseDB := newTxExecutorForTest(t)

	outerTx, err := baseDB.DB.BeginTx(
		t.Context(), &sql.TxOptions{
			Isolation: sql.LevelSerializable,
		},
	)
	require.NoError(t, err)

	defer func() {
		_ = outerTx.Rollback()
	}()

	ctx := actor.WithTx(t.Context(), outerTx)

	// First call succeeds and writes a row.
	err = txExec.ExecTx(
		ctx, WriteTxOption(), func(q *testQueryCreator) error {
			return insertRow(q, 1, "success")
		},
	)
	require.NoError(t, err)

	// Second call fails with an error.
	testErr := sql.ErrNoRows
	err = txExec.ExecTx(
		ctx, WriteTxOption(), func(q *testQueryCreator) error {
			return testErr
		},
	)
	require.ErrorIs(t, err, testErr)

	// The outer transaction should still be usable: the first write is
	// visible because ExecTx didn't roll back the outer tx.
	require.Equal(t, 1, countRows(t, outerTx))
}

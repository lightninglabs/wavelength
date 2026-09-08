package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/stretchr/testify/require"
)

// TestExpectedTxShutdownErr verifies that shutdown classification follows the
// transaction error rather than an independently canceled caller context.
func TestExpectedTxShutdownErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "canceled",
			err:      context.Canceled,
			expected: true,
		},
		{
			name:     "wrapped canceled",
			err:      fmt.Errorf("query: %w", context.Canceled),
			expected: true,
		},
		{
			name:     "closed connection",
			err:      sql.ErrConnDone,
			expected: true,
		},
		{
			name:     "deadline",
			err:      context.DeadlineExceeded,
			expected: false,
		},
		{
			name:     "unrelated database error",
			err:      errors.New("disk full"),
			expected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, test.expected,
				isExpectedTxShutdownErr(test.err),
			)
		})
	}
}

// TestTransactionExecutorUsesContextTx verifies that ExecTx participates in an
// existing actor transaction when present in context.
func TestTransactionExecutorUsesContextTx(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db := NewTestDB(t)

	txExec := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) *sqlc.Queries {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	outerTx, err := db.BeginTx(ctx, WriteTxOption())
	require.NoError(t, err)

	txCtx := actor.WithTx(ctx, outerTx)
	chainName := "ctx-tx-chain"

	err = txExec.ExecTx(
		txCtx, WriteTxOption(), func(q *sqlc.Queries) error {
			params := sqlc.UpsertChainInfoParams{
				ID:        1,
				ChainName: chainName,
				GenesisHash: []byte{
					0x01,
				},
			}

			return q.UpsertChainInfo(ctx, params)
		},
	)
	require.NoError(t, err)

	// Rolling back the outer transaction should remove all writes done via
	// ExecTx, proving that the executor joined the actor transaction.
	require.NoError(t, outerTx.Rollback())

	_, err = db.GetChainInfo(ctx, chainName)
	require.Error(t, err)
	require.True(t, errors.Is(err, sql.ErrNoRows))
}

// TestTxIsolationLevel asserts that the isolation level is only relaxed for
// read-only transactions on Postgres. Every other combination has to stay
// fully serializable, in particular SQLite, which the same BaseDB backs.
func TestTxIsolationLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		backend  sqlc.BackendType
		readOnly bool
		expected sql.IsolationLevel
	}{
		{
			name:     "postgres read-only",
			backend:  sqlc.BackendTypePostgres,
			readOnly: true,
			expected: sql.LevelRepeatableRead,
		},
		{
			name:     "postgres read-write",
			backend:  sqlc.BackendTypePostgres,
			readOnly: false,
			expected: sql.LevelSerializable,
		},
		{
			name:     "sqlite read-only",
			backend:  sqlc.BackendTypeSqlite,
			readOnly: true,
			expected: sql.LevelSerializable,
		},
		{
			name:     "sqlite read-write",
			backend:  sqlc.BackendTypeSqlite,
			readOnly: false,
			expected: sql.LevelSerializable,
		},
		{
			name:     "unknown read-only",
			backend:  sqlc.BackendTypeUnknown,
			readOnly: true,
			expected: sql.LevelSerializable,
		},
		{
			name:     "unknown read-write",
			backend:  sqlc.BackendTypeUnknown,
			readOnly: false,
			expected: sql.LevelSerializable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, test.expected,
				txIsolationLevel(test.backend, test.readOnly),
			)
		})
	}
}

// TestBeginTxReadOnlyUsable asserts that a read-only transaction against the
// active test backend can still be opened and read from. On Postgres this
// exercises the relaxed read-only REPEATABLE READ path, and on SQLite it
// guards against a regression from the backend gate.
func TestBeginTxReadOnlyUsable(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := NewTestDB(t)

	tx, err := store.BeginTx(ctx, ReadTxOption())
	require.NoError(t, err)
	defer func() {
		require.NoError(t, tx.Rollback())
	}()

	var one int
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT 1").Scan(&one))
	require.Equal(t, 1, one)
}

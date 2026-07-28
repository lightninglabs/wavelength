//go:build test_postgres

package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// txSessionState reads back the isolation level and read-only access mode that
// Postgres actually applied to the given transaction. Asserting on the server's
// own view is the only way to prove that the requested sql.TxOptions survived
// the trip through the pgx stdlib driver and into the BEGIN statement.
func txSessionState(t *testing.T, tx *sql.Tx) (string, string) {
	t.Helper()

	var isolation string
	err := tx.QueryRow("SHOW transaction_isolation").Scan(&isolation)
	require.NoError(t, err)

	var readOnly string
	err = tx.QueryRow("SHOW transaction_read_only").Scan(&readOnly)
	require.NoError(t, err)

	return isolation, readOnly
}

// TestPostgresBeginTxIsolation asserts that read-only transactions against
// Postgres are opened as a read-only REPEATABLE READ tx, while writers stay
// SERIALIZABLE. The read-only flag matters as much as the level, because
// Postgres only skips SIRead predicate lock acquisition for a transaction that
// is genuinely declared read only.
//
// The subtests deliberately share one store. Each store costs a docker
// container, and the fixture derives its port from a docker port binding that
// is not always populated under load.
func TestPostgresBeginTxIsolation(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := NewTestPostgresDB(t)

	t.Run("read-only", func(t *testing.T) {
		tx, err := store.BeginTx(ctx, ReadTxOption())
		require.NoError(t, err)
		defer func() {
			require.NoError(t, tx.Rollback())
		}()

		isolation, readOnly := txSessionState(t, tx)
		require.Equal(t, "repeatable read", isolation)
		require.Equal(t, "on", readOnly)
	})

	t.Run("read-write", func(t *testing.T) {
		tx, err := store.BeginTx(ctx, WriteTxOption())
		require.NoError(t, err)
		defer func() {
			require.NoError(t, tx.Rollback())
		}()

		isolation, readOnly := txSessionState(t, tx)
		require.Equal(t, "serializable", isolation)
		require.Equal(t, "off", readOnly)
	})

	// The READ ONLY access mode is enforced by the server rather than being
	// advisory. This is the property that justifies the audit assumption
	// that every ReadTxOption call site is truly read-only: if one of them
	// ever starts writing, it fails loudly instead of silently relying on
	// snapshot isolation.
	t.Run("read-only rejects writes", func(t *testing.T) {
		tx, err := store.BeginTx(ctx, ReadTxOption())
		require.NoError(t, err)
		defer func() {
			require.NoError(t, tx.Rollback())
		}()

		_, err = tx.ExecContext(
			ctx, "INSERT INTO chain_info (id, chain_name, "+
				"genesis_hash) VALUES (2, 'nope', '\\x01')",
		)
		require.Error(t, err)
		require.ErrorContains(t, err, "read-only transaction")
	})
}

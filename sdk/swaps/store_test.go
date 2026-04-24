package swaps

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// newTestSwapStore opens one isolated swap SQLite database in a temp
// directory and closes it automatically when the test ends.
func newTestSwapStore(t *testing.T) *Store {
	t.Helper()

	store, err := NewSqliteStore(&SqliteStoreConfig{
		DatabaseFileName: filepath.Join(
			t.TempDir(), DefaultSqliteDatabaseFileName,
		),
	}, btclog.Disabled)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	return store
}

// sqliteTableExists reports whether one sqlite table exists in the test store.
func sqliteTableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()

	var count int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master "+
			"WHERE type = 'table' AND name = ?",
		table,
	).Scan(&count)
	require.NoError(t, err)

	return count == 1
}

// TestSessionMutateAndPersistRollsBackMutateError verifies failed transition
// closures do not leave partially-applied in-memory state behind.
func TestSessionMutateAndPersistRollsBackMutateError(t *testing.T) {
	t.Parallel()

	failErr := errors.New("transition failed")

	receive := &ReceiveSession{
		state:       ReceiveStateCreated,
		vhtlcAmount: 1,
	}
	err := receive.mutateAndPersist(t.Context(), func() error {
		receive.state = ReceiveStateInvoiceCreated
		receive.vhtlcAmount = 2

		return failErr
	})
	require.ErrorIs(t, err, failErr)
	require.Equal(t, ReceiveStateCreated, receive.state)
	require.EqualValues(t, 1, receive.vhtlcAmount)

	pay := &paySession{
		state:       PayStateCreated,
		vhtlcAmount: 1,
	}
	err = pay.mutateAndPersist(t.Context(), func() error {
		pay.state = PayStateSwapCreated
		pay.vhtlcAmount = 2

		return failErr
	})
	require.ErrorIs(t, err, failErr)
	require.Equal(t, PayStateCreated, pay.state)
	require.EqualValues(t, 1, pay.vhtlcAmount)
}

// TestSwapSqliteStoreRunsMigrations verifies that the isolated swap store
// creates its own schema and migration bookkeeping table.
func TestSwapSqliteStoreRunsMigrations(t *testing.T) {
	t.Parallel()

	store := newTestSwapStore(t)

	require.True(t, sqliteTableExists(
		t, store.DB(), "receive_swaps",
	))
	require.True(t, sqliteTableExists(
		t, store.DB(), "pay_swaps",
	))
	require.True(t, sqliteTableExists(
		t, store.DB(), DefaultMigrationsTable,
	))
}

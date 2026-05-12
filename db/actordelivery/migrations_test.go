package actordelivery

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func newSQLiteDB(t *testing.T) *sql.DB {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "actor_delivery_test.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, rawDB.Close())
	})

	return rawDB
}

func assertTableExists(t *testing.T, db *sql.DB, name string) {
	t.Helper()

	const tableExistsQuery = "SELECT COUNT(*) FROM sqlite_master " +
		"WHERE type='table' AND name=?"

	var cnt int
	err := db.QueryRow(
		tableExistsQuery,
		name,
	).Scan(&cnt)
	require.NoError(t, err)
	require.Equal(t, 1, cnt, "expected table %s to exist", name)
}

func TestRunMigrationsSQLite(t *testing.T) {
	t.Parallel()

	rawDB := newSQLiteDB(t)

	const tableName = "actor_delivery_test_migrations"
	err := RunMigrations(
		rawDB, sqlc.BackendTypeSqlite, WithMigrationsTable(tableName),
	)
	require.NoError(t, err)

	assertTableExists(t, rawDB, tableName)
	assertTableExists(t, rawDB, "mailbox_messages")
	assertTableExists(t, rawDB, "outbox_messages")

	// Re-running should be a no-op.
	err = RunMigrations(
		rawDB, sqlc.BackendTypeSqlite, WithMigrationsTable(tableName),
	)
	require.NoError(t, err)
}

func TestRunMigrationsUnsupportedBackend(t *testing.T) {
	t.Parallel()

	rawDB := newSQLiteDB(t)

	err := RunMigrations(rawDB, sqlc.BackendTypeUnknown)
	require.ErrorContains(t, err, "unsupported backend")
}

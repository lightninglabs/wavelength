package actordelivery

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/lightninglabs/wavelength/db/sqlc"
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

// newConcurrentSQLiteDB opens a sqlite test database configured exactly like
// the production store (see db/sqlite.go): WAL journaling, a 30s busy_timeout,
// synchronous=normal, and _txlock=immediate, with the same multi-connection
// pool.
// Tests that drive concurrent writers (e.g. a multi-worker durable actor) must
// use this rather than the bare newSQLiteDB: _txlock=immediate plus
// busy_timeout is what lets concurrent write transactions serialize by waiting
// instead of colliding with SQLITE_BUSY, which is precisely how the daemon
// runs. Modeling that here keeps the test faithful to production concurrency
// instead of hiding it behind a single connection.
func newConcurrentSQLiteDB(t testing.TB) *sql.DB {
	t.Helper()

	pragmas := []string{
		"foreign_keys=on",
		"journal_mode=WAL",
		"busy_timeout=30000",
		"synchronous=normal",
		"fullfsync=true",
	}
	opts := make(url.Values)
	for _, p := range pragmas {
		opts.Add("_pragma", p)
	}

	dbPath := filepath.Join(t.TempDir(), "actor_delivery_concurrent.db")
	dsn := fmt.Sprintf("%s?%s&_txlock=immediate", dbPath, opts.Encode())

	rawDB, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)

	rawDB.SetMaxOpenConns(25)
	rawDB.SetMaxIdleConns(25)

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

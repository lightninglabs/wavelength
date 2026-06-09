//go:build test_postgres

package db

import (
	"sync"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// testPgFixtureMtx guards testPgFixturesByPath. Tests may restart a client
// concurrently with others, so the path-keyed cache must be safe to read and
// write from multiple goroutines.
var testPgFixtureMtx sync.Mutex

// testPgFixturesByPath memoizes the Postgres fixture (the docker container and
// its underlying database) created for a given dbPath. The systest restart
// harness reopens the SAME dbPath to model a daemon restart and expects the
// previously persisted state to survive. The sqlite backend gets this for free
// because it reopens the file at dbPath; Postgres has no such file, so we
// retain the same underlying database across calls and hand out a fresh store
// (connection pool) over it each time.
var testPgFixturesByPath = make(map[string]*TestPgFixture)

// NewTestDB is a helper function that creates a Postgres database for testing.
func NewTestDB(t testing.TB) *PostgresStore {
	return NewTestPostgresDB(t)
}

// NewTestDBHandleFromPath is a helper function that creates a new handle to an
// existing Postgres database for testing, keyed by dbPath.
//
// On sqlite, reopening dbPath reattaches to the same on-disk file, so state
// survives a simulated daemon restart. Postgres has no such file: a naive
// implementation would spin up a brand-new empty database on every call,
// silently dropping all persisted state (OOR sessions, wallet, boarding) the
// moment a test restarts a client. The restart path also closes the previous
// store's connection pool on shutdown, so we cannot simply hand back the same
// store either; the reopened handle must be a fresh, open pool.
//
// To match the sqlite semantics we memoize the fixture (the docker container
// and its database) per dbPath, and open a NEW store over it on every call:
// the first call for a path creates the database, runs migrations, and
// registers teardown; later calls with the same path reuse that same database
// (so the state carries across the restart) but open a fresh connection pool
// and skip migrations, since the schema already exists. dbPaths are unique per
// client per test (temp dirs), so reuse only ever happens within a single test
// (the original handle plus its restart).
func NewTestDBHandleFromPath(t testing.TB, dbPath string) *PostgresStore {
	t.Helper()

	// For tests, use a simple logger that outputs to the test log.
	log := btclog.Disabled

	testPgFixtureMtx.Lock()
	defer testPgFixtureMtx.Unlock()

	sqlFixture, ok := testPgFixturesByPath[dbPath]
	if !ok {
		// First handle for this path: stand up the database, run
		// migrations via the default store path, and register teardown.
		sqlFixture = NewTestPgFixture(
			t, DefaultPostgresFixtureLifetime, true,
		)

		store, err := NewPostgresStore(sqlFixture.GetConfig(), log)
		require.NoError(t, err)

		t.Cleanup(func() {
			sqlFixture.TearDown(t)

			testPgFixtureMtx.Lock()
			delete(testPgFixturesByPath, dbPath)
			testPgFixtureMtx.Unlock()
		})

		testPgFixturesByPath[dbPath] = sqlFixture

		return store
	}

	// Subsequent handle for this path (a restart): reuse the existing
	// database but open a fresh connection pool over it. The schema and
	// data already exist, so migrations are skipped.
	storeCfg := sqlFixture.GetConfig()
	storeCfg.SkipMigrations = true

	store, err := NewPostgresStore(storeCfg, log)
	require.NoError(t, err)

	return store
}

// NewTestDBWithVersion is a helper function that creates a Postgres database
// for testing and migrates it to the given version.
func NewTestDBWithVersion(t testing.TB, version uint) *PostgresStore {
	return NewTestPostgresDBWithVersion(t, version)
}

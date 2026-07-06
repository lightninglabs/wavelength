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
//
// Invariant: the fixture teardown and cache eviction are registered against the
// testing.TB of the FIRST call for a path, so a given dbPath must only ever be
// reused within the lifetime of the test that first opened it. The unique
// per-client temp-dir paths guarantee this; callers must not share a dbPath
// across independent tests.
func NewTestDBHandleFromPath(t testing.TB, dbPath string) *PostgresStore {
	t.Helper()

	// For tests, use a simple logger that outputs to the test log.
	log := btclog.Disabled

	testPgFixtureMtx.Lock()
	sqlFixture, ok := testPgFixturesByPath[dbPath]
	testPgFixtureMtx.Unlock()

	if !ok {
		// First handle for this path: stand up the database, run
		// migrations via the default store path, and register teardown.
		// This must happen outside testPgFixtureMtx: creating the
		// fixture can block on the global Postgres fixture semaphore,
		// and cleanup needs the same mutex to evict finished fixtures.
		sqlFixture = NewTestPgFixture(
			t, DefaultPostgresFixtureLifetime, true,
		)

		store, err := NewPostgresStore(sqlFixture.GetConfig(), log)
		if err != nil {
			sqlFixture.TearDown(t)
			require.NoError(t, err)
		}

		testPgFixtureMtx.Lock()
		existingFixture, alreadyCreated := testPgFixturesByPath[dbPath]
		if alreadyCreated {
			testPgFixtureMtx.Unlock()

			// Defensive guard: callers should only reopen dbPath
			// sequentially within one test, but avoid leaking an
			// extra fixture if that invariant is violated.
			_ = store.DB.Close()
			sqlFixture.TearDown(t)

			storeCfg := existingFixture.GetConfig()
			storeCfg.SkipMigrations = true
			store, err := NewPostgresStore(storeCfg, log)
			require.NoError(t, err)

			t.Cleanup(func() {
				_ = store.DB.Close()
			})

			return store
		}

		testPgFixturesByPath[dbPath] = sqlFixture
		testPgFixtureMtx.Unlock()

		t.Cleanup(func() {
			testPgFixtureMtx.Lock()
			if testPgFixturesByPath[dbPath] == sqlFixture {
				delete(testPgFixturesByPath, dbPath)
			}
			testPgFixtureMtx.Unlock()

			sqlFixture.TearDown(t)
		})

		// Close this pool at test end even if the caller's restart
		// shutdown already closed it (sql.DB.Close is idempotent), so a
		// test that never restarts does not leak the pool. Registered
		// after TearDown so it runs first (cleanups are LIFO).
		t.Cleanup(func() {
			_ = store.DB.Close()
		})

		return store
	}

	// Subsequent handle for this path (a restart): reuse the existing
	// database but open a fresh connection pool over it. The schema and
	// data already exist, so migrations are skipped.
	storeCfg := sqlFixture.GetConfig()
	storeCfg.SkipMigrations = true

	store, err := NewPostgresStore(storeCfg, log)
	require.NoError(t, err)

	// The caller closes this pool on its restart-shutdown, but register a
	// cleanup too (sql.DB.Close is idempotent) so the per-restart pool is
	// released even if the test ends without a further restart.
	t.Cleanup(func() {
		_ = store.DB.Close()
	})

	return store
}

// NewTestDBWithVersion is a helper function that creates a Postgres database
// for testing and migrates it to the given version.
func NewTestDBWithVersion(t testing.TB, version uint) *PostgresStore {
	return NewTestPostgresDBWithVersion(t, version)
}

//go:build !js || !wasm

package db

import (
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

var (
	// DefaultPostgresFixtureLifetime is the default maximum time a Postgres
	// test fixture is being kept alive. After that time the docker
	// container will be terminated forcefully, even if the tests aren't
	// fully executed yet. So this time needs to be chosen correctly to be
	// longer than the longest expected individual test run time.
	DefaultPostgresFixtureLifetime = 60 * time.Minute
)

// NewTestPostgresDB is a helper function that creates a Postgres database for
// testing.
func NewTestPostgresDB(t testing.TB) *PostgresStore {
	t.Helper()

	t.Logf("Creating new Postgres DB for testing")

	// For tests, use a simple logger that outputs to the test log.
	log := btclog.Disabled

	sqlFixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime, true)

	// Cleanups run in reverse order. Register the fixture first so the
	// store pool registered below is closed before its container is
	// removed.
	t.Cleanup(func() {
		sqlFixture.TearDown(t)
	})

	store, err := NewPostgresStore(sqlFixture.GetConfig(), log)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, store.DB.Close())
	})

	return store
}

// NewTestPostgresDBWithVersion is a helper function that creates a Postgres
// database for testing and migrates it to the given version.
func NewTestPostgresDBWithVersion(t testing.TB, version uint) *PostgresStore {
	t.Helper()

	t.Logf(
		"Creating new Postgres DB for testing, migrating to version %d",
		version,
	)

	// For tests, use a simple logger that outputs to the test log.
	log := btclog.Disabled

	sqlFixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime, true)

	// Cleanups run in reverse order. Register the fixture first so the
	// store pool registered below is closed before its container is
	// removed.
	t.Cleanup(func() {
		sqlFixture.TearDown(t)
	})

	storeCfg := sqlFixture.GetConfig()
	storeCfg.SkipMigrations = true

	store, err := NewPostgresStore(storeCfg, log)
	require.NoError(t, err)

	err = store.ExecuteMigrations(TargetVersion(version))
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, store.DB.Close())
	})

	return store
}

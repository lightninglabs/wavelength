//go:build test_postgres

package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewTestPostgresDBLifecycle verifies that the common fixture helper
// releases its initialization slot before returning and closes its connection
// pool before tearing down the Postgres container.
func TestNewTestPostgresDBLifecycle(t *testing.T) {
	var store *PostgresStore

	t.Run("fixture lifetime", func(t *testing.T) {
		store = NewTestPostgresDB(t)
		require.NoError(t, store.DB.Ping())
		require.Empty(t, testPgFixtureSem)
	})

	require.NotNil(t, store)
	require.EqualError(t, store.DB.Ping(), "sql: database is closed")
}

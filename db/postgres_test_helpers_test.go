//go:build test_postgres

package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewTestPostgresDBClosesStore verifies that the common fixture helper
// closes its connection pool before tearing down the Postgres container.
func TestNewTestPostgresDBClosesStore(t *testing.T) {
	var store *PostgresStore

	t.Run("fixture lifetime", func(t *testing.T) {
		store = NewTestPostgresDB(t)
		require.NoError(t, store.DB.Ping())
	})

	require.NotNil(t, store)
	require.EqualError(t, store.DB.Ping(), "sql: database is closed")
}

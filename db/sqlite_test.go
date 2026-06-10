package db

import (
	"path/filepath"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// queryFullfsync reads back the effective fullfsync pragma on a store.
func queryFullfsync(t *testing.T, store *SqliteStore) int {
	t.Helper()

	var value int
	err := store.DB.QueryRow("PRAGMA fullfsync;").Scan(&value)
	require.NoError(t, err)

	return value
}

// TestSqliteFullfsyncKnob verifies that the NoFullfsync config field controls
// the fullfsync pragma on new connections, and that the default keeps it
// enabled.
func TestSqliteFullfsyncKnob(t *testing.T) {
	t.Parallel()

	newStore := func(t *testing.T, noFullfsync bool) *SqliteStore {
		store, err := NewSqliteStore(&SqliteConfig{
			DatabaseFileName: filepath.Join(
				t.TempDir(),
				"test.db",
			),
			SkipMigrations: true,
			NoFullfsync:    noFullfsync,
		}, btclog.Disabled)
		require.NoError(t, err)
		t.Cleanup(func() {
			require.NoError(t, store.DB.Close())
		})

		return store
	}

	defaultStore := newStore(t, false)
	require.Equal(t, 1, queryFullfsync(t, defaultStore))

	disabledStore := newStore(t, true)
	require.Equal(t, 0, queryFullfsync(t, disabledStore))
}

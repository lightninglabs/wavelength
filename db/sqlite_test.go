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

// TestResolveSqliteSynchronous verifies that the configured synchronous level
// is normalized and validated: an empty value resolves to the safe default,
// each valid level passes through unchanged, and an unknown value is rejected
// so a typo surfaces at startup rather than silently weakening durability.
func TestResolveSqliteSynchronous(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{
			name:  "empty resolves to default normal",
			value: "",
			want:  defaultSqliteSynchronous,
		},
		{
			name:  "full passes through",
			value: SqliteSynchronousFull,
			want:  SqliteSynchronousFull,
		},
		{
			name:  "normal passes through",
			value: SqliteSynchronousNormal,
			want:  SqliteSynchronousNormal,
		},
		{
			name:  "off passes through",
			value: SqliteSynchronousOff,
			want:  SqliteSynchronousOff,
		},
		{
			name:  "uppercase normalizes to lowercase",
			value: "NORMAL",
			want:  SqliteSynchronousNormal,
		},
		{
			name:  "mixed case normalizes to lowercase",
			value: "Full",
			want:  SqliteSynchronousFull,
		},
		{
			name:    "unknown value is rejected",
			value:   "fsync",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveSqliteSynchronous(tc.value)
			if tc.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

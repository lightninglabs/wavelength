//go:build !test_postgres

package relaydb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// testOpener creates a relay-owned SQLite file, retained across pool restarts.
func testOpener(t *testing.T) func() *Database {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.sqlite")

	return func() *Database {
		store, err := OpenSQLite(t.Context(), path)
		require.NoError(t, err)

		return store
	}
}

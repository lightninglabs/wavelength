//go:build !test_postgres

package notifyrelay

import (
	"path/filepath"
	"testing"

	"github.com/lightninglabs/wavelength/notifyrelay/relaydb"
	"github.com/stretchr/testify/require"
)

// sqlTestOpener creates a relay-owned SQLite file, retained across pool
// restarts.
func sqlTestOpener(t *testing.T) func() *relaydb.Database {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.sqlite")

	return func() *relaydb.Database {
		store, err := relaydb.OpenSQLite(t.Context(), path)
		require.NoError(t, err)

		return store
	}
}

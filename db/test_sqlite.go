//go:build !test_postgres

package db

import (
	"testing"

	"github.com/btcsuite/btclog/v2"
)

// NewTestDB is a helper function that creates an SQLite database for testing.
func NewTestDB(t testing.TB) *SqliteStore {
	return NewTestSqliteDB(t)
}

// NewTestDBHandleFromPath is a helper function that creates a new handle to an
// existing SQLite database for testing.
func NewTestDBHandleFromPath(t testing.TB, dbPath string) *SqliteStore {
	// For tests, use a simple logger that outputs to the test log.
	log := btclog.Disabled

	return NewTestSqliteDBHandleFromPath(t, dbPath, log)
}

// NewTestDBWithVersion is a helper function that creates an SQLite database for
// testing and migrates it to the given version.
func NewTestDBWithVersion(t testing.TB, version uint) *SqliteStore {
	return NewTestSqliteDBWithVersion(t, version)
}

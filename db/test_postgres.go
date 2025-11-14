//go:build test_db_postgres

package db

import (
	"testing"
)

// NewTestDB is a helper function that creates a Postgres database for testing.
func NewTestDB(t testing.TB) *PostgresStore {
	return NewTestPostgresDB(t)
}

// NewTestDBHandleFromPath is a helper function that creates a new handle to an
// existing SQLite database for testing.
func NewTestDBHandleFromPath(t testing.TB, dbPath string) *PostgresStore {
	return NewTestPostgresDB(t)
}

// NewTestDBWithVersion is a helper function that creates a Postgres database
// for testing and migrates it to the given version.
func NewTestDBWithVersion(t testing.TB, version uint) *PostgresStore {
	return NewTestPostgresDBWithVersion(t, version)
}

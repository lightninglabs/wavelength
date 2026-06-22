package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOpenReadOnlySQLiteRequiresExistingFile verifies that the report command
// does not create a new SQLite database when the operator passes a bad path.
func TestOpenReadOnlySQLiteRequiresExistingFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing.db")

	db, err := openReadOnlySQLite(t.Context(), dbPath)
	require.Error(t, err)
	require.Nil(t, db)
	require.NoFileExists(t, dbPath)
}

// TestOpenReadOnlySQLiteRejectsWrites verifies that the accounting command's
// database handle cannot mutate the target database.
func TestOpenReadOnlySQLiteRejectsWrites(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	writeDB, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = writeDB.ExecContext(
		t.Context(),
		"CREATE TABLE ledger_entries(id INTEGER)",
	)
	require.NoError(t, err)
	require.NoError(t, writeDB.Close())

	readDB, err := openReadOnlySQLite(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, readDB.Close())
	})

	_, err = readDB.ExecContext(
		t.Context(),
		"INSERT INTO ledger_entries(id) VALUES (1)",
	)
	require.Error(t, err)
}

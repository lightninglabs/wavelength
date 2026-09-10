//go:build !js || !wasm

package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/stretchr/testify/require"
)

// newSqliteStoreWithoutMigrations opens a SQLite store without applying its
// schema migrations.
func newSqliteStoreWithoutMigrations(t *testing.T,
	dbFileName string) *SqliteStore {

	t.Helper()

	store, err := NewSqliteStore(&SqliteConfig{
		DatabaseFileName: dbFileName,
		SkipMigrations:   true,
	}, btclog.Disabled)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, store.DB.Close())
	})

	return store
}

// queryChainName opens a SQLite database and returns the selected chain name.
func queryChainName(t *testing.T, dbFileName string, id int) string {
	t.Helper()

	result, err := OpenSQLiteDatabase(SQLiteOpenConfig{
		DatabaseFileName: dbFileName,
		MaxOpenConns:     1,
		MaxIdleConns:     1,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, result.DB.Close())
	})

	var chainName string
	err = result.DB.QueryRowContext(
		t.Context(),
		`SELECT chain_name FROM chain_info WHERE id = ?`, id,
	).Scan(&chainName)
	require.NoError(t, err)

	return chainName
}

// TestSqliteMigrationBackupReuse verifies that an interrupted migration reuses
// the complete backup for its source schema version instead of copying the
// database again.
func TestSqliteMigrationBackupReuse(t *testing.T) {
	dbFileName := filepath.Join(t.TempDir(), "test.db")
	store := newSqliteStoreWithoutMigrations(t, dbFileName)
	require.NoError(t, store.ExecuteMigrations(TargetVersion(1)))

	_, err := store.ExecContext(t.Context(), `
		INSERT INTO chain_info (id, chain_name, genesis_hash)
		VALUES (1, 'before', X'00')
	`)
	require.NoError(t, err)

	backupPath := sqliteMigrationBackupPath(dbFileName, 1)
	stagingPath := backupPath + ".tmp"
	require.NoError(t, os.WriteFile(stagingPath, []byte("partial"), 0o600))

	err = prepareSqliteMigrationBackup(
		store.DB, dbFileName, 1, btclog.Disabled,
	)
	require.NoError(t, err)
	require.NoFileExists(t, stagingPath)
	require.Equal(t, "before", queryChainName(t, backupPath, 1))

	_, err = store.ExecContext(
		t.Context(),
		`UPDATE chain_info SET chain_name = 'after' WHERE id = 1`,
	)
	require.NoError(t, err)

	err = prepareSqliteMigrationBackup(
		store.DB, dbFileName, 1, btclog.Disabled,
	)
	require.NoError(t, err)
	require.Equal(t, "before", queryChainName(t, backupPath, 1))
}

// TestSqliteMigrationBackupPrunedAfterSuccess verifies that a completed
// migration removes version-keyed backups, interrupted staging files, and
// timestamped backups created by older versions.
func TestSqliteMigrationBackupPrunedAfterSuccess(t *testing.T) {
	dbFileName := filepath.Join(t.TempDir(), "test.db")
	store := newSqliteStoreWithoutMigrations(t, dbFileName)
	require.NoError(t, store.ExecuteMigrations(TargetVersion(17)))
	require.NoError(t, store.DB.Close())

	legacyPath := fmt.Sprintf("%s.%d.backup", dbFileName,
		int64(1763079012000000000))
	stagingPath := sqliteMigrationBackupPath(dbFileName, 16) + ".tmp"
	manualPath := dbFileName + ".manual.backup"
	preservedPaths := []string{
		manualPath,
		dbFileName + ".1234.backup",
		dbFileName + ".20260904.backup",
		dbFileName + ".+1234.backup",
		dbFileName + ".001234.backup",
		dbFileName + ".1234.backup.tmp",
	}
	for _, path := range append(
		[]string{legacyPath, stagingPath}, preservedPaths...,
	) {
		require.NoError(t, os.WriteFile(path, []byte("test"), 0o600))
	}

	store, err := NewSqliteStore(&SqliteConfig{
		DatabaseFileName: dbFileName,
	}, btclog.Disabled)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, store.DB.Close())
	})

	var version int
	err = store.QueryRow(
		`SELECT version FROM schema_migrations`,
	).Scan(&version)
	require.NoError(t, err)
	require.Equal(t, int(LatestMigrationVersion), version)

	require.NoFileExists(t, legacyPath)
	require.NoFileExists(t, stagingPath)
	require.NoFileExists(t, sqliteMigrationBackupPath(dbFileName, 17))
	for _, path := range preservedPaths {
		require.FileExists(t, path)
	}
}

// TestSqliteMigrationBackupRetainedAfterActorMigrationFailure verifies that a
// main-schema backup remains available until actor-delivery migrations also
// complete.
func TestSqliteMigrationBackupRetainedAfterActorMigrationFailure(t *testing.T) {
	dbFileName := filepath.Join(t.TempDir(), "test.db")
	store := newSqliteStoreWithoutMigrations(t, dbFileName)
	require.NoError(t, store.ExecuteMigrations(TargetVersion(17)))

	_, err := store.ExecContext(t.Context(), `
		CREATE TABLE actor_delivery_schema_migrations (
			version INTEGER NOT NULL,
			dirty BOOLEAN NOT NULL
		);
		INSERT INTO actor_delivery_schema_migrations (version, dirty)
		VALUES (1, TRUE);
	`)
	require.NoError(t, err)
	require.NoError(t, store.DB.Close())

	backupPath := sqliteMigrationBackupPath(dbFileName, 17)
	for range 2 {
		_, err = NewSqliteStore(&SqliteConfig{
			DatabaseFileName: dbFileName,
		}, btclog.Disabled)
		require.ErrorContains(t, err, "database is in a dirty state")
		require.FileExists(t, backupPath)
	}

	info, err := os.Stat(backupPath)
	require.NoError(t, err)
	require.Positive(t, info.Size())
}

// TestSqliteMigrationBackupRetainedAfterFailure verifies that a failed
// migration keeps its pre-migration backup available for recovery.
func TestSqliteMigrationBackupRetainedAfterFailure(t *testing.T) {
	dbFileName := filepath.Join(t.TempDir(), "test.db")
	store := newSqliteStoreWithoutMigrations(t, dbFileName)
	require.NoError(t, store.ExecuteMigrations(TargetVersion(17)))

	testErr := errors.New("post-migration check failed")
	checks := map[uint]postMigrationCheck{
		18: func(_ context.Context, _ sqlc.Querier) error {
			return testErr
		},
	}

	err := store.ExecuteMigrations(
		store.backupAndMigrate,
		WithPostStepCallbacks(
			makePostStepCallbacks(store, btclog.Disabled, checks),
		),
	)
	require.ErrorIs(t, err, testErr)

	backupPath := sqliteMigrationBackupPath(dbFileName, 17)
	require.FileExists(t, backupPath)
	info, err := os.Stat(backupPath)
	require.NoError(t, err)
	require.Positive(t, info.Size())
}

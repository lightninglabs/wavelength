//go:build !js || !wasm

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/btcsuite/btclog/v2"
)

// sqliteMigrationBackupPath returns the stable backup path for a source schema
// version.
func sqliteMigrationBackupPath(dbFullFilePath string,
	currentDBVersion int) string {

	return fmt.Sprintf("%s.v%d.backup", dbFullFilePath, currentDBVersion)
}

// prepareSqliteMigrationBackup creates one reusable backup for the current
// source schema version.
func prepareSqliteMigrationBackup(srcDB *sql.DB, dbFullFilePath string,
	currentDBVersion int, backupLog btclog.Logger) error {

	backupPath := sqliteMigrationBackupPath(
		dbFullFilePath, currentDBVersion,
	)
	info, err := os.Stat(backupPath)
	switch {
	case err == nil && info.Mode().IsRegular():
		backupLog.InfoS(
			context.Background(),
			"Reusing database backup for pending migration",
			slog.String("backup", backupPath),
			slog.Int("current_db_version", currentDBVersion),
		)

		return nil

	case err == nil:
		return fmt.Errorf("migration backup path is not a regular "+
			"file: %s", backupPath)

	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("inspect migration backup %s: %w", backupPath,
			err)
	}

	// VACUUM INTO leaves its destination behind when it fails. A stable
	// staging path makes an interrupted copy replaceable on the next start
	// without accumulating partial files.
	stagingPath := backupPath + ".tmp"
	err = os.Remove(stagingPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove incomplete migration backup %s: %w",
			stagingPath, err)
	}

	err = vacuumSqliteDatabase(
		srcDB, dbFullFilePath, stagingPath, backupLog,
	)
	if err != nil {
		_ = os.Remove(stagingPath)

		return fmt.Errorf("create migration backup: %w", err)
	}

	err = os.Rename(stagingPath, backupPath)
	if err != nil {
		_ = os.Remove(stagingPath)

		return fmt.Errorf("publish migration backup: %w", err)
	}

	return nil
}

// pruneSqliteMigrationBackups removes completed and interrupted migration
// backups owned by the database package.
func pruneSqliteMigrationBackups(dbFullFilePath string) error {
	dir := filepath.Dir(dbFullFilePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("list migration backups: %w", err)
	}

	dbFileName := filepath.Base(dbFullFilePath)
	var pruneErr error
	for _, entry := range entries {
		if entry.IsDir() ||
			!isSqliteMigrationBackup(dbFileName, entry.Name()) {

			continue
		}

		backupPath := filepath.Join(dir, entry.Name())
		err := os.Remove(backupPath)
		if err != nil {
			pruneErr = errors.Join(
				pruneErr, fmt.Errorf("remove migration "+
					"backup %s: %w", backupPath, err),
			)
		}
	}

	return pruneErr
}

// isSqliteMigrationBackup reports whether a file name matches a timestamped
// legacy backup or a version-keyed backup created by this package.
func isSqliteMigrationBackup(dbFileName, candidate string) bool {
	prefix := dbFileName + "."
	if !strings.HasPrefix(candidate, prefix) {
		return false
	}

	identifier := strings.TrimPrefix(candidate, prefix)
	isStaging := strings.HasSuffix(identifier, ".backup.tmp")
	if isStaging {
		identifier = strings.TrimSuffix(identifier, ".tmp")
	}

	identifier, found := strings.CutSuffix(identifier, ".backup")
	if !found {
		return false
	}

	isVersioned := strings.HasPrefix(identifier, "v")
	if isStaging && !isVersioned {
		return false
	}
	if isVersioned {
		identifier = strings.TrimPrefix(identifier, "v")
	}

	value, err := strconv.ParseInt(identifier, 10, 64)

	return err == nil && strconv.FormatInt(value, 10) == identifier
}

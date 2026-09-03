//go:build js && wasm

package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/btcsuite/btclog/v2"
)

// prepareSqliteMigrationBackup creates a migration backup in the active SQLite
// virtual file system.
func prepareSqliteMigrationBackup(srcDB *sql.DB, dbFullFilePath string, _ int,
	backupLog btclog.Logger) error {

	backupPath := fmt.Sprintf("%s.%d.backup", dbFullFilePath,
		time.Now().UnixNano())

	return vacuumSqliteDatabase(
		srcDB, dbFullFilePath, backupPath, backupLog,
	)
}

// pruneSqliteMigrationBackups is a no-op because the browser SQLite virtual
// file system does not expose file removal through database/sql.
func pruneSqliteMigrationBackups(string) error {
	return nil
}

//go:build (!js || !wasm) && sqlite_cgo

package migrate

import (
	"database/sql"

	"github.com/golang-migrate/migrate/v4/database"
	sqlitemigrate "github.com/golang-migrate/migrate/v4/database/sqlite3"
)

// newSQLiteMigrationDriver pairs mattn with its migration adapter, avoiding
// the modernc dependency imported by the default SQLite migration driver.
func newSQLiteMigrationDriver(db *sql.DB,
	migrationsTable string) (database.Driver, error) {

	return sqlitemigrate.WithInstance(db, &sqlitemigrate.Config{
		MigrationsTable: migrationsTable,
	})
}

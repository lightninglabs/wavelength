//go:build (!js || !wasm) && !sqlite_cgo

package migrate

import (
	"database/sql"

	"github.com/golang-migrate/migrate/v4/database"
	sqlitemigrate "github.com/golang-migrate/migrate/v4/database/sqlite"
)

// newSQLiteMigrationDriver pairs modernc with its migration adapter.
func newSQLiteMigrationDriver(db *sql.DB,
	migrationsTable string) (database.Driver, error) {

	return sqlitemigrate.WithInstance(db, &sqlitemigrate.Config{
		MigrationsTable: migrationsTable,
	})
}

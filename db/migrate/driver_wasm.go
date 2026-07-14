//go:build js && wasm

package migrate

import (
	"database/sql"
	"fmt"

	"github.com/golang-migrate/migrate/v4/database"
	"github.com/lightninglabs/wavelength/db/sqlc"
)

// newMigrationDriver creates a browser-compatible migration driver.
func newMigrationDriver(db *sql.DB, backend sqlc.BackendType,
	migrationsTable string) (database.Driver, error) {

	switch backend {
	case sqlc.BackendTypeSqlite:
		return newWASMSQLiteMigrationDriver(db, migrationsTable)

	default:
		return nil, fmt.Errorf("unsupported wasm migration backend: %v",
			backend)
	}
}

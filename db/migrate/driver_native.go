//go:build !js || !wasm

package migrate

import (
	"database/sql"
	"fmt"

	"github.com/golang-migrate/migrate/v4/database"
	postgresmigrate "github.com/golang-migrate/migrate/v4/database/postgres"
	sqlitemigrate "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/lightninglabs/wavelength/db/sqlc"
)

// newMigrationDriver creates a native migration driver for the given backend.
func newMigrationDriver(db *sql.DB, backend sqlc.BackendType,
	migrationsTable string) (database.Driver, error) {

	switch backend {
	case sqlc.BackendTypeSqlite:
		cfg := &sqlitemigrate.Config{
			MigrationsTable: migrationsTable,
		}

		return sqlitemigrate.WithInstance(db, cfg)

	case sqlc.BackendTypePostgres:
		cfg := &postgresmigrate.Config{
			MigrationsTable: migrationsTable,
		}

		return postgresmigrate.WithInstance(db, cfg)

	default:
		return nil, fmt.Errorf("unsupported backend: %v", backend)
	}
}

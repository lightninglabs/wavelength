package actordelivery

import (
	"database/sql"
	"fmt"

	admigration "github.com/lightninglabs/wavelength/db/actordelivery/migrations"
	"github.com/lightninglabs/wavelength/db/sqlc"
)

const (
	// DefaultMigrationsTable is the default migration bookkeeping table
	// used by the isolated actor-delivery migration runner.
	DefaultMigrationsTable = admigration.DefaultMigrationsTable

	// defaultDatabaseName is the migration instance label used by
	// golang-migrate.
	defaultDatabaseName = admigration.DefaultDatabaseName
)

// MigrationOption configures RunMigrations.
type MigrationOption func(*migrationOptions)

type migrationOptions struct {
	migrationsTable string
	databaseName    string
}

func defaultMigrationOptions() migrationOptions {
	return migrationOptions{
		migrationsTable: DefaultMigrationsTable,
		databaseName:    defaultDatabaseName,
	}
}

// WithMigrationsTable overrides the migration bookkeeping table name.
func WithMigrationsTable(table string) MigrationOption {
	return func(o *migrationOptions) {
		if table != "" {
			o.migrationsTable = table
		}
	}
}

// WithDatabaseName overrides the migration instance name.
func WithDatabaseName(name string) MigrationOption {
	return func(o *migrationOptions) {
		if name != "" {
			o.databaseName = name
		}
	}
}

// RunMigrations applies isolated actor-delivery migrations on the provided
// database using a dedicated migration table.
func RunMigrations(db *sql.DB, backend sqlc.BackendType,
	opts ...MigrationOption) error {

	if db == nil {
		return fmt.Errorf("db is nil")
	}

	cfg := defaultMigrationOptions()
	for _, opt := range opts {
		opt(&cfg)
	}

	err := admigration.RunMigrations(
		db,
		backend,
		admigration.Config{
			MigrationsTable: cfg.migrationsTable,
			DatabaseName:    cfg.databaseName,
		},
	)
	if err != nil {
		return fmt.Errorf("apply actor-delivery migrations: %w", err)
	}

	return nil
}

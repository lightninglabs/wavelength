package migrations

import (
	"database/sql"
	"embed"
	"fmt"

	"github.com/btcsuite/btclog/v2"
	dbmigrate "github.com/lightninglabs/wavelength/db/migrate"
	"github.com/lightninglabs/wavelength/db/sqlc"
)

const (
	// LatestMigrationVersion is the latest actor-delivery migration
	// version.
	//
	// NOTE: This MUST be updated when a new actor-delivery migration is
	// added.
	LatestMigrationVersion uint = 1

	// DefaultMigrationsTable is the default migration bookkeeping table.
	DefaultMigrationsTable = "actor_delivery_schema_migrations"

	// DefaultDatabaseName is the default migration instance label.
	DefaultDatabaseName = "actor_delivery"
)

//go:embed *.sql
var migrationFiles embed.FS

// Config configures actor-delivery migration execution.
type Config struct {
	// MigrationsTable is the migration bookkeeping table.
	MigrationsTable string

	// DatabaseName is the migration instance label.
	DatabaseName string

	// LatestVersion enables downgrade protection when set.
	LatestVersion *uint

	// Log enables migration progress logging when non-nil.
	Log btclog.Logger
}

// RunMigrations applies actor-delivery migrations against the provided DB.
func RunMigrations(db *sql.DB, backend sqlc.BackendType, cfg Config) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	if cfg.MigrationsTable == "" {
		cfg.MigrationsTable = DefaultMigrationsTable
	}
	if cfg.DatabaseName == "" {
		cfg.DatabaseName = DefaultDatabaseName
	}
	if cfg.LatestVersion == nil {
		latestVersion := LatestMigrationVersion
		cfg.LatestVersion = &latestVersion
	}

	err := dbmigrate.RunMigrations(
		db,
		backend,
		migrationFiles,
		".",
		dbmigrate.TargetLatest,
		dbmigrate.Config{
			MigrationsTable: cfg.MigrationsTable,
			DatabaseName:    cfg.DatabaseName,
			LatestVersion:   cfg.LatestVersion,
			PostgresReplacements: dbmigrate.
				PostgresSchemaReplacements(),
			Log: cfg.Log,
		},
	)
	if err != nil {
		return fmt.Errorf("apply actor-delivery migrations: %w", err)
	}

	return nil
}

package db

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"regexp"

	"github.com/btcsuite/btclog/v2"
	"github.com/golang-migrate/migrate/v4/database"
	postgres_migrate "github.com/golang-migrate/migrate/v4/database/postgres"
	sqlite_migrate "github.com/golang-migrate/migrate/v4/database/sqlite"
)

// ExtraMigration declares an additional migration set that should run after
// darepo's core migrations against the same database connection. Each set is
// version-tracked independently using its own schema_migrations_<Name> table,
// so a downstream consumer (e.g. lightninglabs/swapdk-server) can layer its
// own schema onto darepo's without colliding with darepo's version counter.
//
// Use case: swap-server-specific tables (out_swaps, in_swaps,
// in_ark_receive_intents) live in the swapdk-server repo's own embed.FS but
// apply against the same DB that darepo opened, so swap queries can run
// side-by-side with darepo-managed tables (chain_info, mailbox_envelopes, …)
// within a single connection pool.
type ExtraMigration struct {
	// Name is a stable identifier used both as the
	// schema_migrations_<Name> suffix and for diagnostic logging. It
	// must match `^[a-zA-Z][a-zA-Z0-9_]*$` so the table name is SQL-safe
	// across both SQLite and Postgres without quoting.
	Name string

	// FS is the embed.FS (or any fs.FS) holding the .up.sql / .down.sql
	// files. Typically `//go:embed migrations/*.sql` in the downstream
	// package.
	FS fs.FS

	// Path is the directory inside FS that holds the migration files.
	// Files must follow golang-migrate's NNNN_name.up.sql /
	// NNNN_name.down.sql convention.
	Path string

	// LatestVersion is the highest migration number known to the
	// downstream consumer. Used for downgrade protection — opening a
	// database whose recorded version exceeds this is rejected. Must be
	// >= 1.
	LatestVersion uint
}

// extraMigrationName matches identifiers safe to interpolate into a
// schema_migrations_<Name> table name without quoting.
var extraMigrationName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

// validate sanity-checks the ExtraMigration descriptor before any DDL is
// applied. It runs before the migration loop so misconfigured downstream
// consumers fail at construction time rather than mid-rollout.
func (e ExtraMigration) validate() error {
	if !extraMigrationName.MatchString(e.Name) {
		return fmt.Errorf("extra migration name %q must match "+
			"[a-zA-Z][a-zA-Z0-9_]*", e.Name)
	}
	if e.FS == nil {
		return fmt.Errorf("extra migration %q: FS is required", e.Name)
	}
	if e.Path == "" {
		return fmt.Errorf("extra migration %q: Path is required",
			e.Name)
	}
	if e.LatestVersion == 0 {
		return fmt.Errorf("extra migration %q: LatestVersion must be "+
			">= 1", e.Name)
	}

	return nil
}

// migrationsTable returns the schema_migrations_<Name> table used by this
// extension to track its own version independently of darepo's core counter.
func (e ExtraMigration) migrationsTable() string {
	return "schema_migrations_" + e.Name
}

// StoreOption configures NewStoreFromConfig and the underlying SqliteStore /
// PostgresStore constructors.
type StoreOption func(*storeOpts)

// WithExtraMigrations registers one or more additional migration sets that run
// after darepo's core migrations against the same database connection. Each
// set is independently version-tracked using its own schema_migrations_<Name>
// table.
//
// Calling WithExtraMigrations multiple times is additive — each call's
// arguments are appended to any prior registration.
//
// Note: extra migrations are gated on the store's SkipMigrations flag in the
// same way as darepo's core migrations. If a SqliteConfig / PostgresConfig is
// constructed with SkipMigrations=true, the registrations here are silently
// ignored. Use WithSkipCoreMigrations instead when you want to suppress only
// darepo's core schema while still applying the extension sets.
func WithExtraMigrations(extras ...ExtraMigration) StoreOption {
	return func(o *storeOpts) {
		o.extras = append(o.extras, extras...)
	}
}

// storeOpts is the internal accumulator the StoreOption functions write into.
type storeOpts struct {
	extras []ExtraMigration
}

// collectStoreOpts folds the variadic StoreOption list into a single struct.
func collectStoreOpts(opts []StoreOption) *storeOpts {
	so := &storeOpts{}
	for _, opt := range opts {
		opt(so)
	}

	return so
}

// extraMigrationDriver constructs a golang-migrate database.Driver for one
// extension migration set. The dialect-specific entry points pass the
// appropriate constructor so the shared apply loop stays generic.
type extraMigrationDriver func(*sql.DB, string) (database.Driver, error)

// applyExtraMigrations is the dialect-agnostic core of the extra-migration
// runner. It pre-validates the entire slice before creating any driver or
// emitting any DDL so a malformed late entry can't leave the database in a
// partially-migrated state. The dialect-specific glue (driver constructor,
// schema replacements, dbName) is passed in by the SQLite / Postgres
// wrappers.
func applyExtraMigrations(db *sql.DB, log btclog.Logger,
	extras []ExtraMigration, dialect, dbName string,
	schemaReplacements map[string]string,
	newDriver extraMigrationDriver) error {

	// Preflight every descriptor before touching the database so a
	// malformed entry at index N can't strand entries 0..N-1 in a
	// half-applied state.
	for _, ex := range extras {
		if err := ex.validate(); err != nil {
			return err
		}
	}

	for _, ex := range extras {
		driver, err := newDriver(db, ex.migrationsTable())
		if err != nil {
			return fmt.Errorf("create %q %s migration driver: %w",
				ex.Name, dialect, err)
		}

		migrationFS := newReplacerFS(ex.FS, schemaReplacements)

		opts := defaultMigrateOptions()
		opts.latestVersion = ex.LatestVersion

		log.InfoS(context.Background(),
			"Applying downstream "+dialect+" migrations",
			"name", ex.Name,
			"latest_version", ex.LatestVersion,
			"migrations_table", ex.migrationsTable(),
		)

		err = applyMigrations(
			migrationFS, driver, ex.Path, dbName, TargetLatest,
			opts, log,
		)
		if err != nil {
			return fmt.Errorf("apply %q migrations: %w",
				ex.Name, err)
		}
	}

	return nil
}

// applyExtraMigrationsSQLite runs each registered ExtraMigration set against
// db using SQLite's golang-migrate driver and the SQLite schema replacements
// (currently a no-op map). Each set uses its own MigrationsTable so its
// version counter is independent of darepo's core schema_migrations table.
func applyExtraMigrationsSQLite(s *SqliteStore,
	extras []ExtraMigration) error {

	newDriver := func(db *sql.DB,
		table string) (database.Driver, error) {

		return sqlite_migrate.WithInstance(db, &sqlite_migrate.Config{
			MigrationsTable: table,
		})
	}

	return applyExtraMigrations(
		s.DB, s.log, extras, "sqlite", "sqlite",
		sqliteSchemaReplacements, newDriver,
	)
}

// applyExtraMigrationsPostgres mirrors applyExtraMigrationsSQLite for the
// Postgres backend. It uses the postgres-flavored schema replacements (BLOB
// -> BYTEA, INTEGER PRIMARY KEY -> BIGSERIAL PRIMARY KEY, …) shared with
// darepo's core migration runner.
func applyExtraMigrationsPostgres(s *PostgresStore,
	extras []ExtraMigration) error {

	newDriver := func(db *sql.DB,
		table string) (database.Driver, error) {

		return postgres_migrate.WithInstance(
			db, &postgres_migrate.Config{
				MigrationsTable: table,
			},
		)
	}

	return applyExtraMigrations(
		s.DB, s.log, extras, "postgres", s.cfg.DBName,
		postgresSchemaReplacements, newDriver,
	)
}

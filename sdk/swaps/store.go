package swaps

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"time"

	"github.com/btcsuite/btclog/v2"
	dbmigrate "github.com/lightninglabs/darepo-client/db/migrate"
	dbsqlc "github.com/lightninglabs/darepo-client/db/sqlc"
	swapsqlc "github.com/lightninglabs/darepo-client/sdk/swaps/sqlc"
	_ "modernc.org/sqlite"
)

const (
	// DefaultSqliteDatabaseFileName is the default filename for the
	// isolated swap-client SQLite database.
	DefaultSqliteDatabaseFileName = "swaps.db"

	// DefaultMigrationsTable is the isolated migration bookkeeping
	// table for the swap-client schema.
	DefaultMigrationsTable = "swap_client_schema_migrations"

	// defaultMigrationDatabaseName is the golang-migrate instance label
	// for the swap-client schema.
	defaultMigrationDatabaseName = "swap_client"

	// sqliteOptionPrefix is the modernc SQLite DSN prefix for pragma
	// settings.
	sqliteOptionPrefix = "_pragma"

	// sqliteTxLockImmediate starts write transactions immediately so state
	// persistence fails fast under contention instead of stalling halfway
	// through a swap step.
	sqliteTxLockImmediate = "_txlock=immediate"

	// defaultMaxConns bounds the swap store connection pool just like
	// the main client database.
	defaultMaxConns = 25

	// defaultConnMaxLifetime limits how long one SQLite connection is
	// reused.
	defaultConnMaxLifetime = 10 * time.Minute

	// LatestMigrationVersion is the latest swap-client schema migration.
	LatestMigrationVersion uint = 1
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// SqliteStoreConfig holds the configuration for the isolated swap SQLite
// database.
type SqliteStoreConfig struct {
	// DatabaseFileName is the full path to the isolated swap SQLite file.
	DatabaseFileName string

	// SkipMigrations skips schema setup when true.
	SkipMigrations bool
}

// DefaultSqliteStoreConfig returns the default isolated swap SQLite store
// configuration rooted under dataDir.
func DefaultSqliteStoreConfig(dataDir string) *SqliteStoreConfig {
	return &SqliteStoreConfig{
		DatabaseFileName: filepath.Join(
			dataDir, DefaultSqliteDatabaseFileName,
		),
	}
}

// Store provides isolated SQL-backed persistence for swap SDK sessions.
type Store struct {
	db      *sql.DB
	queries *swapsqlc.Queries
	log     btclog.Logger
}

// NewSqliteStore opens the isolated swap SQLite database and applies the
// swap-specific migrations.
func NewSqliteStore(cfg *SqliteStoreConfig,
	log btclog.Logger) (*Store, error) {

	if cfg == nil {
		return nil, fmt.Errorf("sqlite config must be provided")
	}
	if cfg.DatabaseFileName == "" {
		return nil, fmt.Errorf(
			"swap sqlite database file must be provided",
		)
	}
	if log == nil {
		log = btclog.Disabled
	}

	pragmaOptions := []struct {
		name  string
		value string
	}{
		{name: "foreign_keys", value: "on"},
		{name: "journal_mode", value: "WAL"},
		{name: "busy_timeout", value: "5000"},
		{name: "synchronous", value: "full"},
		{name: "fullfsync", value: "true"},
	}

	sqliteOptions := make(url.Values)
	for _, option := range pragmaOptions {
		sqliteOptions.Add(
			sqliteOptionPrefix,
			fmt.Sprintf("%s=%s", option.name, option.value),
		)
	}

	dsn := fmt.Sprintf(
		"%s?%s&%s", cfg.DatabaseFileName, sqliteOptions.Encode(),
		sqliteTxLockImmediate,
	)

	ctx := context.Background()
	log.InfoS(ctx, "Opening swap SQLite database",
		slog.String("db_file", cfg.DatabaseFileName))

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open swap sqlite db: %w", err)
	}

	db.SetMaxOpenConns(defaultMaxConns)
	db.SetMaxIdleConns(defaultMaxConns)
	db.SetConnMaxLifetime(defaultConnMaxLifetime)

	if !cfg.SkipMigrations {
		err = RunMigrations(db, log)
		if err != nil {
			_ = db.Close()

			return nil, err
		}
	}

	return &Store{
		db:      db,
		queries: swapsqlc.New(db),
		log:     log,
	}, nil
}

// RunMigrations applies the isolated swap-client schema migrations to the
// provided database handle.
func RunMigrations(db *sql.DB, log btclog.Logger) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	latestVersion := LatestMigrationVersion

	err := dbmigrate.RunMigrations(
		db,
		dbsqlc.BackendTypeSqlite,
		migrationFiles,
		"migrations",
		dbmigrate.TargetLatest,
		dbmigrate.Config{
			MigrationsTable: DefaultMigrationsTable,
			DatabaseName:    defaultMigrationDatabaseName,
			LatestVersion:   &latestVersion,
			Log:             log,
		},
	)
	if err != nil {
		return fmt.Errorf("apply swap-client migrations: %w", err)
	}

	return nil
}

// Queries exposes the generated query adapter for direct store tests and
// low-level callers.
func (s *Store) Queries() *swapsqlc.Queries {
	if s == nil {
		return nil
	}

	return s.queries
}

// DB returns the underlying SQL handle.
func (s *Store) DB() *sql.DB {
	if s == nil {
		return nil
	}

	return s.db
}

// Close closes the underlying swap SQLite handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}

	return s.db.Close()
}

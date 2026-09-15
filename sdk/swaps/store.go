package swaps

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/btcsuite/btclog/v2"
	clientdb "github.com/lightninglabs/wavelength/db"
	dbmigrate "github.com/lightninglabs/wavelength/db/migrate"
	dbsqlc "github.com/lightninglabs/wavelength/db/sqlc"
	swapsqlc "github.com/lightninglabs/wavelength/sdk/swaps/sqlc"
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

	// defaultMaxConns bounds the swap store connection pool just like
	// the main client database.
	defaultMaxConns = 25

	// defaultConnMaxLifetime limits how long one SQLite connection is
	// reused.
	defaultConnMaxLifetime = 10 * time.Minute

	// LatestMigrationVersion is the latest swap-client schema migration.
	LatestMigrationVersion uint = 2
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
func NewSqliteStore(cfg *SqliteStoreConfig, log btclog.Logger) (*Store, error) {
	if cfg == nil {
		return nil, fmt.Errorf("sqlite config must be provided")
	}
	if cfg.DatabaseFileName == "" {
		return nil, fmt.Errorf("swap sqlite database file must be " +
			"provided")
	}
	if log == nil {
		log = btclog.Disabled
	}

	pragmaOptions := []clientdb.SQLitePragma{
		{
			Name:  "foreign_keys",
			Value: "on",
		},
		{
			Name:  "journal_mode",
			Value: "WAL",
		},
		{
			Name:  "busy_timeout",
			Value: "5000",
		},
		{
			Name:  "synchronous",
			Value: "full",
		},
		{
			Name:  "fullfsync",
			Value: "true",
		},
	}

	ctx := context.Background()
	log.InfoS(ctx, "Opening swap SQLite database",
		slog.String("db_file", cfg.DatabaseFileName),
	)

	openResult, err := clientdb.OpenSQLiteDatabase(
		clientdb.SQLiteOpenConfig{
			DatabaseFileName: cfg.DatabaseFileName,
			Pragmas:          pragmaOptions,
			TxLockImmediate:  true,
			MaxOpenConns:     defaultMaxConns,
			MaxIdleConns:     defaultMaxConns,
			ConnMaxLifetime:  defaultConnMaxLifetime,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("open swap sqlite db: %w", err)
	}

	db := openResult.DB

	log.DebugS(ctx, "Swap SQLite connection pool configured",
		slog.String("driver", openResult.DriverName),
	)

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

package db

import (
	"database/sql"
	"fmt"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/db/sqlc"
)

// Store is the unified SQL-based storage implementation that wraps all
// repository types (events, rounds, vtxos, offchain txs).
type Store struct {
	queries *sqlc.Queries
	db      *sql.DB
	log     btclog.Logger

	// Backend type (sqlite or postgres)
	backend sqlc.BackendType
}

// NewStore creates a new Store from either a SqliteStore or PostgresStore.
func NewStore(db *sql.DB, queries *sqlc.Queries, backend sqlc.BackendType,
	log btclog.Logger) *Store {

	return &Store{
		queries: queries,
		db:      db,
		log:     log,
		backend: backend,
	}
}

// Queries returns the underlying sqlc queries.
func (s *Store) Queries() *sqlc.Queries {
	return s.queries
}

// DB returns the underlying database connection.
func (s *Store) DB() *sql.DB {
	return s.db
}

// Backend returns the type of database backend.
func (s *Store) Backend() sqlc.BackendType {
	return s.backend
}

// Close closes the database connection.
func (s *Store) Close() error {
	if s.db != nil {
		return s.db.Close()
	}

	return nil
}

// Config holds the configuration for the database store. It allows selecting
// between SQLite and Postgres backends.
type Config struct {
	// Backend specifies which database backend to use: "sqlite" or
	// "postgres".
	//nolint:ll
	Backend string `long:"backend" description:"Database backend to use (sqlite or postgres)" choice:"sqlite" choice:"postgres"`

	// Sqlite contains SQLite-specific configuration
	Sqlite *SqliteConfig `group:"sqlite" namespace:"sqlite"`

	// Postgres contains Postgres-specific configuration
	Postgres *PostgresConfig `group:"postgres" namespace:"postgres"`
}

// DefaultConfig returns the default database configuration (SQLite).
func DefaultConfig(dataDir string) *Config {
	return &Config{
		Backend:  "sqlite",
		Sqlite:   DefaultSqliteConfig(dataDir),
		Postgres: DefaultPostgresConfig(),
	}
}

// DefaultSqliteConfig returns default SQLite configuration.
func DefaultSqliteConfig(dataDir string) *SqliteConfig {
	return &SqliteConfig{
		DatabaseFileName: fmt.Sprintf("%s/arkd.db", dataDir),
	}
}

// DefaultPostgresConfig returns default Postgres configuration.
func DefaultPostgresConfig() *PostgresConfig {
	return &PostgresConfig{
		Host:               "localhost",
		Port:               5432,
		User:               "postgres",
		Password:           "",
		DBName:             "arkd",
		MaxOpenConnections: 0, // Use default
		MaxIdleConnections: 0, // Use default
		RequireSSL:         false,
	}
}

// NewStoreFromConfig creates a new Store based on the configuration.
func NewStoreFromConfig(cfg *Config, log btclog.Logger) (*Store, error) {
	switch cfg.Backend {
	case "sqlite":
		sqliteStore, err := NewSqliteStore(cfg.Sqlite, log)
		if err != nil {
			return nil, fmt.Errorf("failed to create sqlite "+
				"store: %w", err)
		}

		return NewStore(
			sqliteStore.DB, sqliteStore.Queries,
			sqliteStore.Backend(), log,
		), nil

	case "postgres":
		pgStore, err := NewPostgresStore(cfg.Postgres, log)
		if err != nil {
			return nil, fmt.Errorf("failed to create postgres "+
				"store: %w", err)
		}

		return NewStore(
			pgStore.DB, pgStore.Queries, pgStore.Backend(), log,
		), nil

	default:
		return nil, fmt.Errorf("unsupported database backend: %s",
			cfg.Backend)
	}
}

package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
)

// Store is the unified SQL-based storage implementation that wraps all
// repository types (events, rounds, vtxos, offchain txs).
type Store struct {
	*sqlc.Queries

	db    *sql.DB
	log   btclog.Logger
	clock clock.Clock

	// Backend type (sqlite or postgres)
	backend sqlc.BackendType
}

// NewStore creates a new Store from either a SqliteStore or PostgresStore.
func NewStore(db *sql.DB, queries *sqlc.Queries, backend sqlc.BackendType,
	log btclog.Logger, clk clock.Clock) *Store {

	if log == nil {
		log = btclog.Disabled
	}
	if clk == nil {
		clk = clock.NewDefaultClock()
	}

	return &Store{
		Queries: queries,
		db:      db,
		log:     log,
		clock:   clk,
		backend: backend,
	}
}

// DB returns the underlying database connection.
func (s *Store) DB() *sql.DB {
	return s.db
}

// SQLDB returns the underlying SQL handle.
func (s *Store) SQLDB() *sql.DB {
	return s.db
}

// Backend returns the type of database backend.
func (s *Store) Backend() sqlc.BackendType {
	return s.backend
}

// BeginTx creates a new database transaction given the set of transaction
// options.
func (s *Store) BeginTx(ctx context.Context, opts TxOptions) (*sql.Tx, error) {
	sqlOptions := sql.TxOptions{
		ReadOnly:  opts.ReadOnly(),
		Isolation: sql.LevelSerializable,
	}

	return s.db.BeginTx(ctx, &sqlOptions)
}

// WithTx returns a new Queries instance that uses the provided transaction.
func (s *Store) WithTx(tx *sql.Tx) *sqlc.Queries {
	return s.Queries.WithTx(tx)
}

// Close closes the database connection.
func (s *Store) Close() error {
	if s.db != nil {
		return s.db.Close()
	}

	return nil
}

// NewRoundStore creates a RoundStoreDB from this Store.
func (s *Store) NewRoundStore() *RoundStoreDB {
	return NewRoundStoreDB(s, s.clock)
}

// NewVTXOStore creates a VTXOStoreDB from this Store.
func (s *Store) NewVTXOStore() *VTXOStoreDB {
	return NewVTXOStoreDB(s)
}

// NewVTXORecordStore creates a VTXORecordStoreDB from this Store.
func (s *Store) NewVTXORecordStore() *VTXORecordStoreDB {
	return NewVTXORecordStoreDB(s)
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
		Host:     "localhost",
		Port:     5432,
		User:     "postgres",
		Password: "",
		DBName:   "arkd",
		// Use the default value for max open connections.
		MaxOpenConnections: 0,

		// Use the default value for max idle connections.
		MaxIdleConnections: 0,
		RequireSSL:         false,
	}
}

// NewStoreFromConfig creates a new Store based on the configuration.
func NewStoreFromConfig(cfg *Config, log btclog.Logger,
	clk clock.Clock) (*Store, error) {

	switch cfg.Backend {
	case "sqlite":
		sqliteStore, err := NewSqliteStore(cfg.Sqlite, log)
		if err != nil {
			return nil, fmt.Errorf("failed to create sqlite "+
				"store: %w", err)
		}

		return NewStore(
			sqliteStore.DB, sqliteStore.Queries,
			sqliteStore.Backend(), log, clk,
		), nil

	case "postgres":
		pgStore, err := NewPostgresStore(cfg.Postgres, log)
		if err != nil {
			return nil, fmt.Errorf("failed to create postgres "+
				"store: %w", err)
		}

		return NewStore(
			pgStore.DB, pgStore.Queries, pgStore.Backend(), log,
			clk,
		), nil

	default:
		return nil, fmt.Errorf("unsupported database backend: %s",
			cfg.Backend)
	}
}

package db

import (
	"database/sql"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
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

// NewStore constructs the unified SQL store wrapper from initialized DB
// primitives.
//
// Callers are expected to provide backend-specific connections and sqlc query
// adapters that already completed migration and connectivity setup.
func NewStore(db *sql.DB, queries *sqlc.Queries, backend sqlc.BackendType,
	log btclog.Logger) *Store {

	return &Store{
		queries: queries,
		db:      db,
		log:     log,
		backend: backend,
	}
}

// Queries returns the underlying sqlc query adapter.
//
// This is mainly used when a caller needs direct access to generated query
// methods outside repository wrappers.
func (s *Store) Queries() *sqlc.Queries {
	return s.queries
}

// DB returns the underlying shared SQL handle.
//
// The returned handle can be used for low-level integration points that are
// not yet wrapped by repository helpers.
func (s *Store) DB() *sql.DB {
	return s.db
}

// Backend reports which storage backend is active for this store instance.
func (s *Store) Backend() sqlc.BackendType {
	return s.backend
}

// Close releases the underlying SQL connection pool.
//
// Close is idempotent: calling it on a store with no DB handle is a no-op.
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
	Backend string `long:"backend" choice:"sqlite" choice:"postgres"`

	// Sqlite contains SQLite-specific configuration
	Sqlite *SqliteConfig `group:"sqlite" namespace:"sqlite"`

	// Postgres contains Postgres-specific configuration
	Postgres *PostgresConfig `group:"postgres" namespace:"postgres"`
}

// DefaultConfig returns a complete default database configuration.
//
// The default backend is SQLite, while Postgres defaults are still populated
// so callers can switch backend by toggling the Backend field.
func DefaultConfig(dataDir string) *Config {
	return &Config{
		Backend:  "sqlite",
		Sqlite:   DefaultSqliteConfig(dataDir),
		Postgres: DefaultPostgresConfig(),
	}
}

// DefaultSqliteConfig returns the default SQLite configuration values.
//
// The default database file is placed under the provided data directory.
func DefaultSqliteConfig(dataDir string) *SqliteConfig {
	return &SqliteConfig{
		DatabaseFileName: fmt.Sprintf("%s/arkd.db", dataDir),
	}
}

// DefaultPostgresConfig returns default Postgres connection settings.
//
// Connection pool limits are left at zero to keep Go SQL defaults unless the
// caller explicitly configures them.
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

// NewStoreFromConfig builds and initializes a store from backend config.
//
// The method dispatches to backend-specific constructors and returns a unified
// Store wrapper over the resulting SQL handle and query adapter.
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

// BaseDB returns the shared BaseDB used by transaction executors.
//
// Repository constructors use this helper to create typed transactional stores
// without duplicating base wiring.
func (s *Store) BaseDB() *BaseDB {
	return &BaseDB{
		DB:      s.db,
		Queries: s.queries,
	}
}

// NewRoundStore builds a round persistence store with transactional execution.
//
// The returned store wraps sqlc round queries in the generic transaction
// executor so multi-query round updates can run atomically.
func (s *Store) NewRoundStore(chainParams *chaincfg.Params,
	clk clock.Clock) *RoundPersistenceStore {

	baseDB := s.BaseDB()

	roundDB := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) RoundStore {
			return s.queries.WithTx(tx)
		},
		s.log,
	)

	return NewRoundPersistenceStore(roundDB, chainParams, clk)
}

// NewBoardingStore builds a boarding wallet store with transactional
// execution semantics.
//
// This keeps boarding persistence behavior consistent with other typed stores
// that use the shared transaction executor pattern.
func (s *Store) NewBoardingStore(chainParams *chaincfg.Params,
	clk clock.Clock) *BoardingWalletStore {

	baseDB := s.BaseDB()

	boardingDB := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) BoardingStore {
			return s.queries.WithTx(tx)
		},
		s.log,
	)

	return NewBoardingWalletStore(boardingDB, chainParams, clk)
}

// NewOORArtifactStore builds the OOR artifact persistence store with
// transactional query execution.
//
// The artifact store provides package/binding/cursor APIs used by OOR receive
// persistence and unroll package resolution paths.
func (s *Store) NewOORArtifactStore(
	clk clock.Clock) *OORArtifactPersistenceStore {

	baseDB := s.BaseDB()

	artifactDB := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) OORArtifactStore {
			return s.queries.WithTx(tx)
		},
		s.log,
	)

	return NewOORArtifactPersistenceStore(artifactDB, clk)
}

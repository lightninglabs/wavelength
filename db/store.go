package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
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
// adapters that already completed migration and connectivity setup. The
// explicit logger parameter is kept for backward compatibility; callers that
// prefer the fn.Option pattern should set Config.Log instead.
func NewStore(db *sql.DB, queries *sqlc.Queries, backend sqlc.BackendType,
	explicitLog btclog.Logger) *Store {

	return &Store{
		queries: queries,
		db:      db,
		log:     explicitLog,
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

	// Sqlite contains SQLite-specific configuration.
	Sqlite *SqliteConfig `group:"sqlite" namespace:"sqlite"`

	// Postgres contains Postgres-specific configuration.
	Postgres *PostgresConfig `group:"postgres" namespace:"postgres"`

	// Log is an optional logger for the database store. When None, the
	// store falls back to btclog.Disabled unless the caller threads a
	// logger through explicit constructor parameters.
	Log fn.Option[btclog.Logger]
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
		DatabaseFileName: fmt.Sprintf("%s/waved.db", dataDir),
		Synchronous:      defaultSqliteSynchronous,
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
		DBName:   "waved",
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
// Store wrapper over the resulting SQL handle and query adapter. The explicit
// logger parameter is kept for backward compatibility; when cfg.Log is set it
// takes precedence.
func NewStoreFromConfig(cfg *Config,
	explicitLog btclog.Logger) (*Store, error) {

	// Resolve the effective logger: prefer the config option, then fall
	// back to the explicitly provided logger parameter.
	storeLog := cfg.Log.UnwrapOr(explicitLog)

	ctx := context.Background()

	storeLog.InfoS(ctx, "Initializing database store",
		slog.String("backend", cfg.Backend),
	)

	// Propagate the resolved logger into backend-specific configs so the
	// sub-store constructors can pick it up through their own Log option.
	logOpt := fn.Some(storeLog)

	switch cfg.Backend {
	case "sqlite":
		cfg.Sqlite.Log = logOpt

		sqliteStore, err := NewSqliteStore(cfg.Sqlite, storeLog)
		if err != nil {
			return nil, fmt.Errorf("failed to create sqlite "+
				"store: %w", err)
		}

		storeLog.InfoS(ctx, "SQLite store created successfully")

		return NewStore(
			sqliteStore.DB, sqliteStore.Queries,
			sqliteStore.Backend(), storeLog,
		), nil

	case "postgres":
		cfg.Postgres.Log = logOpt

		pgStore, err := NewPostgresStore(cfg.Postgres, storeLog)
		if err != nil {
			return nil, fmt.Errorf("failed to create postgres "+
				"store: %w", err)
		}

		storeLog.InfoS(ctx, "Postgres store created successfully")

		return NewStore(
			pgStore.DB, pgStore.Queries, pgStore.Backend(),
			storeLog,
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

	// The pending-intent outbox shares the same database; a second
	// executor is needed only because the generic transaction executor
	// is typed per query-interface.
	intentDB := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) PendingIntentStore {
			return s.queries.WithTx(tx)
		},
		s.log,
	)

	return NewBoardingWalletStore(boardingDB, intentDB, chainParams, clk)
}

// NewVTXOStore builds a VTXO persistence store with transactional query
// execution.
//
// The returned store wraps sqlc VTXO queries in the generic transaction
// executor so multi-query VTXO updates can run atomically.
func (s *Store) NewVTXOStore(clk clock.Clock) *VTXOPersistenceStore {
	baseDB := s.BaseDB()

	roundDB := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) RoundStore {
			return s.queries.WithTx(tx)
		},
		s.log,
	)

	// Wire the daemon's subsystem logger through so rehydrate-path
	// diagnostics (expiry drift warnings etc.) land in the daemon's
	// log stream rather than being silently dropped.
	return NewVTXOPersistenceStoreWithLogger(
		roundDB, clk, fn.Some(s.log),
	)
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

// NewSpendingReservationStore builds the spending-reservation persistence
// store with transactional query execution.
//
// The reservation store maintains a durable index of VTXO outpoints reserved
// by an active spend owner so a startup sweep can release orphaned Spending
// VTXOs that have no live reservation.
func (s *Store) NewSpendingReservationStore(
	clk clock.Clock) *SpendingReservationPersistenceStore {

	baseDB := s.BaseDB()

	reservationDB := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) SpendingReservationStore {
			return s.queries.WithTx(tx)
		},
		s.log,
	)

	return NewSpendingReservationPersistenceStore(reservationDB, clk)
}

// NewActivityStore builds the canonical activity-log persistence store with
// transactional query execution.
//
// The activity store is the source of truth for the wallet activity feed: a
// current-state activity_entries projection read by List and an append-only
// activity_events transition log read by a resumable SubscribeWallet.
func (s *Store) NewActivityStore(clk clock.Clock) *ActivityPersistenceStore {
	baseDB := s.BaseDB()

	activityDB := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) ActivityStore {
			return s.queries.WithTx(tx)
		},
		s.log,
	)

	return NewActivityPersistenceStore(activityDB, clk)
}

// NewUnilateralExitStore builds the unilateral-exit persistence store with
// transactional query execution.
func (s *Store) NewUnilateralExitStore(
	clk clock.Clock) *UnilateralExitPersistenceStore {

	baseDB := s.BaseDB()

	exitDB := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) UnilateralExitStore {
			return s.queries.WithTx(tx)
		},
		s.log,
	)

	return NewUnilateralExitPersistenceStore(exitDB, clk)
}

// NewVHTLCRecoveryStore builds the vHTLC recovery persistence store with
// transactional execution.
func (s *Store) NewVHTLCRecoveryStore(clk clock.Clock) *VHTLCRecoveryStoreDB {
	return NewVHTLCRecoveryStore(s, clk)
}

// NewOORSessionRegistryStore builds the OOR session registry control-plane
// store with transactional query execution.
func (s *Store) NewOORSessionRegistryStore(
	clk clock.Clock) *OORSessionRegistryStoreDB {

	return NewOORSessionRegistryStore(s, clk)
}

// NewVirtualChannelStore builds the virtual channel registration store.
func (s *Store) NewVirtualChannelStore() *VirtualChannelStoreDB {
	return NewVirtualChannelStoreDB(s)
}

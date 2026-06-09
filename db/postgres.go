package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"
	admigration "github.com/lightninglabs/darepo-client/db/actordelivery/migrations"
	dbmigrate "github.com/lightninglabs/darepo-client/db/migrate"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

const (
	dsnTemplate = "postgres://%v:%v@%v:%d/%v?sslmode=%v"

	// defaultMaxIdleConns is the number of permitted idle connections.
	defaultMaxIdleConns = 6

	// defaultConnMaxIdleTime is the amount of time a connection can be
	// idle before it is closed.
	defaultConnMaxIdleTime = 5 * time.Minute

	// defaultPostgresSynchronous is the default durability level for the
	// Postgres backend, expressed in the shared SQLite vocabulary
	// ("full"/"normal"/"off"). It maps to the synchronous_commit session
	// setting. We default to "normal" to match the SQLite default; on a
	// single-node deployment this still flushes the WAL locally on commit
	// while avoiding the strictest sync, mirroring SQLite's WAL "normal".
	defaultPostgresSynchronous = SqliteSynchronousNormal
)

var (
	// DefaultPostgresFixtureLifetime is the default maximum time a Postgres
	// test fixture is being kept alive. After that time the docker
	// container will be terminated forcefully, even if the tests aren't
	// fully executed yet. So this time needs to be chosen correctly to be
	// longer than the longest expected individual test run time.
	DefaultPostgresFixtureLifetime = 60 * time.Minute

	// postgresSchemaReplacements is a map of schema strings that need to be
	// replaced for postgres. This is needed because we write the schemas
	// to work with sqlite primarily, and postgres has some differences.
	postgresSchemaReplacements = dbmigrate.PostgresSchemaReplacements()
)

// PostgresConfig holds the postgres database configuration.
//
//nolint:ll
type PostgresConfig struct {
	SkipMigrations     bool          `long:"skipmigrations" description:"Skip applying migrations on startup."`
	Host               string        `long:"host" description:"Database server hostname."`
	Port               int           `long:"port" description:"Database server port."`
	User               string        `long:"user" description:"Database user."`
	Password           string        `long:"password" description:"Database user's password."` //nolint:gosec // G117: DB config field; name required for flag binding.
	DBName             string        `long:"dbname" description:"Database name to use."`
	MaxOpenConnections int           `long:"maxconnections" description:"Max open connections to keep alive to the database server."`
	MaxIdleConnections int           `long:"maxidleconnections" description:"Max number of idle connections to keep in the connection pool."`
	ConnMaxLifetime    time.Duration `long:"connmaxlifetime" description:"Max amount of time a connection can be reused for before it is closed. Valid time units are {s, m, h}."`
	ConnMaxIdleTime    time.Duration `long:"connmaxidletime" description:"Max amount of time a connection can be idle for before it is closed. Valid time units are {s, m, h}."`
	RequireSSL         bool          `long:"requiressl" description:"Whether to require using SSL (mode: require) when connecting to the server."`

	// Synchronous controls commit durability via the Postgres
	// synchronous_commit session setting. To keep one knob across
	// backends it accepts the same vocabulary as the SQLite synchronous
	// pragma ("full", "normal", "off") and maps it onto the equivalent
	// Postgres value. When empty it defaults to "normal".
	Synchronous string `long:"synchronous" description:"The commit durability level. One of: full, normal, off. Mapped onto the Postgres synchronous_commit setting."`

	// Log is an optional logger for the Postgres store. When None, the
	// store falls back to the explicit constructor logger.
	Log fn.Option[btclog.Logger]
}

// resolvePostgresSynchronousCommit normalizes and validates the configured
// durability level (in the shared SQLite vocabulary) and maps it onto the
// Postgres synchronous_commit session value. The mapping is: "full" -> "on"
// (wait for local flush; on a primary with no sync standbys this is the
// strongest setting), "normal" -> "local" (flush the local WAL but never wait
// on replicas, the closest analog to SQLite's WAL "normal"), and "off" ->
// "off" (the most aggressive level). An empty value resolves to the default,
// and unknown values are rejected so a typo surfaces at startup.
func resolvePostgresSynchronousCommit(value string) (string, error) {
	level := value
	if level == "" {
		level = defaultPostgresSynchronous
	}

	switch level {
	case SqliteSynchronousFull:
		return "on", nil

	case SqliteSynchronousNormal:
		return "local", nil

	case SqliteSynchronousOff:
		return "off", nil

	default:
		return "", fmt.Errorf("invalid postgres synchronous level %q: "+
			"must be one of %q, %q, or %q", value,
			SqliteSynchronousFull, SqliteSynchronousNormal,
			SqliteSynchronousOff)
	}
}

// DSN returns the dns to connect to the database.
func (s *PostgresConfig) DSN(hidePassword bool) string {
	var sslMode = "disable"
	if s.RequireSSL {
		sslMode = "require"
	}

	password := s.Password
	if hidePassword {
		// Placeholder used for logging the DSN safely.
		password = "****"
	}

	return fmt.Sprintf(dsnTemplate, s.User, password, s.Host, s.Port,
		s.DBName, sslMode)
}

// PostgresStore is a database store implementation that uses a Postgres
// backend.
type PostgresStore struct {
	cfg *PostgresConfig

	log btclog.Logger

	*BaseDB
}

// NewPostgresStore creates a new store that is backed by a Postgres database
// backend. The explicit logger parameter is kept for backward compatibility
// with existing callers; when cfg.Log is set it takes precedence.
func NewPostgresStore(cfg *PostgresConfig,
	explicitLog btclog.Logger) (*PostgresStore, error) {

	// Resolve the effective logger: prefer the config option, then fall
	// back to the explicitly provided logger parameter.
	storeLog := cfg.Log.UnwrapOr(explicitLog)

	ctx := context.Background()

	// Resolve and validate the configured durability level, mapping it onto
	// the Postgres synchronous_commit session value before we build the
	// DSN. We pass it as a libpq "options" startup parameter so the setting
	// applies to every connection in the pool without a post-connect SET.
	syncCommit, err := resolvePostgresSynchronousCommit(cfg.Synchronous)
	if err != nil {
		return nil, err
	}

	syncOption := fmt.Sprintf("-c synchronous_commit=%s", syncCommit)

	storeLog.InfoS(ctx, "Opening Postgres database",
		slog.String("dsn", cfg.DSN(true)),
		slog.String("synchronous_commit", syncCommit),
	)

	connStr := fmt.Sprintf("%s&options=%s", cfg.DSN(false),
		url.QueryEscape(syncOption))

	rawDB, err := sql.Open("pgx", connStr)
	if err != nil {
		return nil, MapSQLError(err)
	}

	maxConns := defaultMaxConns
	if cfg.MaxOpenConnections > 0 {
		maxConns = cfg.MaxOpenConnections
	}

	maxIdleConns := defaultMaxIdleConns
	if cfg.MaxIdleConnections > 0 {
		maxIdleConns = cfg.MaxIdleConnections
	}

	connMaxLifetime := defaultConnMaxLifetime
	if cfg.ConnMaxLifetime > 0 {
		connMaxLifetime = cfg.ConnMaxLifetime
	}

	connMaxIdleTime := defaultConnMaxIdleTime
	if cfg.ConnMaxIdleTime > 0 {
		connMaxIdleTime = cfg.ConnMaxIdleTime
	}

	rawDB.SetMaxOpenConns(maxConns)
	rawDB.SetMaxIdleConns(maxIdleConns)
	rawDB.SetConnMaxLifetime(connMaxLifetime)
	rawDB.SetConnMaxIdleTime(connMaxIdleTime)

	storeLog.DebugS(ctx, "Postgres connection pool configured",
		slog.Int("max_open_conns", maxConns),
		slog.Int("max_idle_conns", maxIdleConns),
		slog.Duration("conn_max_lifetime", connMaxLifetime),
		slog.Duration("conn_max_idle_time", connMaxIdleTime),
	)

	// Persist the resolved logger into the config option so the
	// logger(ctx) helper can retrieve it without keeping a separate
	// field.
	cfg.Log = fn.Some(storeLog)

	queries := sqlc.NewPostgres(rawDB)
	s := &PostgresStore{
		cfg: cfg,
		log: storeLog,
		BaseDB: &BaseDB{
			DB:      rawDB,
			Queries: queries,
		},
	}

	// Now that the database is open, populate the database with our set of
	// schemas based on our embedded in-memory file system.
	if !cfg.SkipMigrations {
		storeLog.InfoS(ctx, "Starting Postgres schema migrations")

		err := s.ExecuteMigrations(TargetLatest)
		if err != nil {
			return nil, fmt.Errorf("error executing migrations: %w",
				err)
		}

		storeLog.InfoS(ctx, "Starting actor-delivery migrations")

		err = admigration.RunMigrations(
			s.DB, s.Backend(), admigration.Config{
				Log: s.log,
			},
		)
		if err != nil {
			return nil, fmt.Errorf("error executing "+
				"actor-delivery migrations: %w", err)
		}

		storeLog.InfoS(
			ctx, "All Postgres migrations completed successfully",
		)
	} else {
		storeLog.InfoS(
			ctx, "Skipping Postgres migrations as configured",
		)
	}

	return s, nil
}

// ExecuteMigrations runs migrations for the Postgres database, depending on the
// target given, either all migrations or up to a given version.
func (s *PostgresStore) ExecuteMigrations(target MigrationTarget,
	optFuncs ...MigrateOpt) error {

	opts := defaultMigrateOptions()
	for _, optFunc := range optFuncs {
		optFunc(opts)
	}

	err := dbmigrate.RunMigrations(
		s.DB,
		s.Backend(),
		sqlSchemas,
		"sqlc/migrations",
		dbmigrate.Target(target),
		dbmigrate.Config{
			DatabaseName:         s.cfg.DBName,
			LatestVersion:        &opts.latestVersion,
			PostStepCallbacks:    opts.postStepCallbacks,
			PostgresReplacements: postgresSchemaReplacements,
			Log:                  s.log,
		},
	)
	if err != nil {
		return fmt.Errorf("apply postgres migrations: %w", err)
	}

	return nil
}

// NewTestPostgresDB is a helper function that creates a Postgres database for
// testing.
func NewTestPostgresDB(t testing.TB) *PostgresStore {
	t.Helper()

	t.Logf("Creating new Postgres DB for testing")

	// For tests, use a simple logger that outputs to the test log.
	log := btclog.Disabled

	sqlFixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime, true)
	store, err := NewPostgresStore(sqlFixture.GetConfig(), log)
	require.NoError(t, err)

	t.Cleanup(func() {
		sqlFixture.TearDown(t)
	})

	return store
}

// NewTestPostgresDBWithVersion is a helper function that creates a Postgres
// database for testing and migrates it to the given version.
func NewTestPostgresDBWithVersion(t testing.TB, version uint) *PostgresStore {
	t.Helper()

	t.Logf(
		"Creating new Postgres DB for testing, migrating to version %d",
		version,
	)

	// For tests, use a simple logger that outputs to the test log.
	log := btclog.Disabled

	sqlFixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime, true)
	storeCfg := sqlFixture.GetConfig()
	storeCfg.SkipMigrations = true

	store, err := NewPostgresStore(storeCfg, log)
	require.NoError(t, err)

	err = store.ExecuteMigrations(TargetVersion(version))
	require.NoError(t, err)

	t.Cleanup(func() {
		sqlFixture.TearDown(t)
	})

	return store
}

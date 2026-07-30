package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/golang-migrate/migrate/v4"
	admigration "github.com/lightninglabs/wavelength/db/actordelivery/migrations"
	dbmigrate "github.com/lightninglabs/wavelength/db/migrate"
	"github.com/lightninglabs/wavelength/db/sqlc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

const (
	// defaultMaxConns is the number of permitted active and idle
	// connections. We want to limit this so it isn't unlimited. We use the
	// same value for the number of idle connections as, this can speed up
	// queries given a new connection doesn't need to be established each
	// time.
	defaultMaxConns = 25

	// defaultConnMaxLifetime is the maximum amount of time a connection can
	// be reused for before it is closed.
	defaultConnMaxLifetime = 10 * time.Minute

	// defaultSqliteSynchronous is the default value for the SQLite
	// synchronous pragma. We default to "normal" rather than "full"
	// because, under WAL mode, "normal" omits the per-commit WAL fsync
	// entirely (the WAL is synced only before a checkpoint), which is
	// exactly what makes it faster than "full". A committed transaction is
	// still durable across an application/process crash, but a power loss
	// or OS crash can roll back the recently committed tail still sitting
	// in the un-synced WAL. The at-least-once, idempotent, deterministic
	// OOR/outbox/serverconn stack recovers from a process crash with zero
	// loss and replays that dropped tail after power loss, so the
	// per-commit fsync of "full" is an unneeded throughput ceiling.
	// "normal" never corrupts the database ("off" is not corruption-safe).
	defaultSqliteSynchronous = SqliteSynchronousNormal

	// SqliteSynchronousFull is the strictest SQLite synchronous level. It
	// fsyncs on every commit, trading throughput for the strongest
	// per-commit durability guarantee.
	SqliteSynchronousFull = "full"

	// SqliteSynchronousNormal relaxes the synchronous pragma to "normal".
	// Under WAL mode this omits the per-commit WAL fsync (syncing the WAL
	// only before a checkpoint), which is safe given our recoverable,
	// idempotent persistence stack.
	SqliteSynchronousNormal = "normal"

	// SqliteSynchronousOff disables synchronous flushing entirely. This is
	// the most aggressive (and least durable) level; a power loss may lose
	// recently committed transactions, but it never corrupts the database.
	SqliteSynchronousOff = "off"
)

// SqliteConfig holds all the config arguments needed to interact with our
// sqlite DB.
//
//nolint:ll
type SqliteConfig struct {
	// SkipMigrations if true, then all the tables will be created on start
	// up if they don't already exist.
	SkipMigrations bool `long:"skipmigrations" description:"Skip applying migrations on startup."`

	// SkipMigrationDBBackup if true, then a backup of the database will not
	// be created before applying migrations.
	SkipMigrationDBBackup bool `long:"skipmigrationdbbackup" description:"Skip creating a backup of the database before applying migrations."`

	// DatabaseFileName is the full file path where the database file can be
	// found.
	DatabaseFileName string `long:"dbfile" description:"The full path to the database."`

	// Synchronous controls the SQLite synchronous pragma, which governs
	// commit durability. Valid values are "full", "normal", and "off".
	// When empty it defaults to "normal", which under WAL mode omits the
	// per-commit WAL fsync of "full" (the WAL is synced only before a
	// checkpoint).
	Synchronous string `long:"synchronous" description:"The SQLite synchronous (commit durability) level. One of: full, normal, off."`

	// NoFullfsync disables the SQLite fullfsync pragma. The pragma only
	// matters on macOS, where a regular fsync does not guarantee the data
	// reached stable storage; with synchronous=normal it governs the WAL
	// checkpoint sync. Checkpoints fire continuously under a sustained
	// write load and F_FULLFSYNC waits on a full hardware cache flush, so
	// write-heavy deployments that accept the weaker flush guarantee can
	// disable it for substantially better throughput. The default keeps
	// fullfsync enabled.
	NoFullfsync bool `long:"nofullfsync" description:"Disable the macOS fullfsync pragma; trades power-loss flush guarantees on macOS for higher sustained write throughput. No effect on other platforms."`

	// Log is an optional logger for the SQLite store. When None, the store
	// falls back to the explicit constructor logger.
	Log fn.Option[btclog.Logger]
}

// SqliteStore is a sqlite3 based database for the daemon.
type SqliteStore struct {
	cfg *SqliteConfig

	log btclog.Logger

	*BaseDB
}

// NewSqliteStore attempts to open a new sqlite database based on the passed
// config. The explicit logger parameter is kept for backward compatibility
// with existing callers; when cfg.Log is set it takes precedence.
func NewSqliteStore(cfg *SqliteConfig,
	explicitLog btclog.Logger) (*SqliteStore, error) {

	// Resolve the effective logger: prefer the config option, then fall
	// back to the explicitly provided logger parameter.
	storeLog := cfg.Log.UnwrapOr(explicitLog)

	// Resolve and validate the configured synchronous level before we build
	// the DSN, normalizing an empty value to the safe default.
	synchronous, err := resolveSqliteSynchronous(cfg.Synchronous)
	if err != nil {
		return nil, err
	}

	// The set of pragma options are accepted using query options. For now
	// we only want to ensure that foreign key constraints are properly
	// enforced.
	pragmaOptions := []SQLitePragma{
		{
			Name:  "foreign_keys",
			Value: "on",
		},
		{
			Name:  "journal_mode",
			Value: "WAL",
		},
		{
			// busy_timeout caps how long SQLite will wait on
			// SQLITE_BUSY before failing. Multi-actor flows
			// (every VTXO actor + the unroll registry +
			// txconfirm + the ledger actor + receive scripts)
			// all write to the same DB; under aggressive block
			// churn or test-driven regtest mining the contention
			// window can comfortably exceed 5s. Bumping to 30s
			// tolerates these natural contention bursts without
			// surfacing them to callers as transient enqueue or
			// begin-tx failures, which masquerade as "mailbox
			// full" or "Failed to lease message" upstream and
			// confuse production diagnosis.
			Name:  "busy_timeout",
			Value: "30000",
		},
		{
			// The synchronous pragma governs commit durability.
			// Under WAL mode, "full" fsyncs the WAL on every
			// commit; "normal" (our default) omits that per-commit
			// fsync and syncs the WAL only before a checkpoint, for
			// substantially better throughput at the cost of the
			// recently committed tail on power loss. The value is
			// configurable so operators can trade durability for
			// performance. See resolveSqliteSynchronous for the
			// accepted values.
			Name:  "synchronous",
			Value: synchronous,
		},
		{
			// fullfsync uses the correct fsync system call on macOS
			// so that flushed data is genuinely durable. Under
			// "normal" it governs the WAL checkpoint sync rather
			// than a per-commit fsync, but checkpoints recur
			// continuously under sustained write load and each
			// F_FULLFSYNC waits on a full hardware cache flush, so
			// the config exposes an opt-out for write-heavy
			// deployments. Enabled by default.
			Name:  "fullfsync",
			Value: strconv.FormatBool(!cfg.NoFullfsync),
		},
	}
	ctx := context.Background()

	storeLog.InfoS(ctx, "Opening SQLite database",
		slog.String("db_file", cfg.DatabaseFileName),
		slog.String("synchronous", synchronous),
		slog.Bool("fullfsync", !cfg.NoFullfsync),
		slog.Int("max_conns", defaultMaxConns),
		slog.Duration("conn_max_lifetime", defaultConnMaxLifetime),
	)

	openResult, err := OpenSQLiteDatabase(SQLiteOpenConfig{
		DatabaseFileName: cfg.DatabaseFileName,
		Pragmas:          pragmaOptions,
		TxLockImmediate:  true,
		MaxOpenConns:     defaultMaxConns,
		MaxIdleConns:     defaultMaxConns,
		ConnMaxLifetime:  defaultConnMaxLifetime,
	})
	if err != nil {
		return nil, err
	}

	db := openResult.DB

	storeLog.DebugS(ctx, "SQLite connection pool configured",
		slog.String("driver", openResult.DriverName),
	)

	// Persist the resolved logger into the config option so the
	// logger(ctx) helper can retrieve it without keeping a separate
	// field.
	cfg.Log = fn.Some(storeLog)

	queries := sqlc.NewSqlite(db)
	s := &SqliteStore{
		cfg: cfg,
		log: storeLog,
		BaseDB: &BaseDB{
			DB:      db,
			Queries: queries,
		},
	}

	// Now that the database is open, populate the database with our set of
	// schemas based on our embedded in-memory file system.
	if !cfg.SkipMigrations {
		storeLog.InfoS(ctx, "Starting SQLite schema migrations")

		err := s.ExecuteMigrations(
			s.backupAndMigrate,
			WithPostStepCallbacks(
				makePostStepCallbacks(
					s, storeLog, postMigrationChecks,
				),
			),
		)
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
			ctx, "All SQLite migrations completed successfully",
		)
	} else {
		storeLog.InfoS(
			ctx, "Skipping SQLite migrations as configured",
		)
	}

	return s, nil
}

// resolveSqliteSynchronous normalizes and validates a configured SQLite
// synchronous level. An empty value resolves to the package default
// (defaultSqliteSynchronous); any other value must be one of "full",
// "normal", or "off". Unknown values are rejected with a descriptive error so
// a typo surfaces at startup rather than silently weakening durability.
func resolveSqliteSynchronous(value string) (string, error) {
	if value == "" {
		return defaultSqliteSynchronous, nil
	}

	// Normalize case before validating so an operator can spell the level
	// in any case (e.g. "NORMAL"), matching the uppercase form used in the
	// durability docs and prose. The resolved value is returned lowercased
	// so it feeds the pragma as SQLite expects.
	level := strings.ToLower(value)

	switch level {
	case SqliteSynchronousFull, SqliteSynchronousNormal,
		SqliteSynchronousOff:
		return level, nil

	default:
		return "", fmt.Errorf("invalid sqlite synchronous level %q: "+
			"must be one of %q, %q, or %q", value,
			SqliteSynchronousFull, SqliteSynchronousNormal,
			SqliteSynchronousOff)
	}
}

// backupSqliteDatabase creates a backup of the given SQLite database. The
// function uses the store's resolved logger for progress messages.
func backupSqliteDatabase(srcDB *sql.DB, dbFullFilePath string,
	backupLog btclog.Logger) error {

	if srcDB == nil {
		return fmt.Errorf("backup source database is nil")
	}

	// Create a database backup file full path from the given source
	// database full file path.
	//
	// Get the current time and format it as a Unix timestamp in
	// nanoseconds.
	timestamp := time.Now().UnixNano()

	// Add the timestamp to the backup name.
	backupFullFilePath := fmt.Sprintf("%s.%d.backup", dbFullFilePath,
		timestamp)

	backupLog.InfoS(
		context.Background(),
		"Creating backup of database file",
		slog.String("source", dbFullFilePath),
		slog.String("backup", backupFullFilePath),
	)

	// Create the database backup.
	vacuumIntoQuery := "VACUUM INTO ?;"
	stmt, err := srcDB.Prepare(vacuumIntoQuery)
	if err != nil {
		return err
	}
	defer func() {
		_ = stmt.Close()
	}()

	_, err = stmt.Exec(backupFullFilePath)
	if err != nil {
		return err
	}

	return nil
}

// backupAndMigrate is a helper function that creates a database backup before
// initiating the migration, and then migrates the database to the latest
// version.
func (s *SqliteStore) backupAndMigrate(mig *migrate.Migrate,
	currentDBVersion int, maxMigrationVersion uint) error {

	// Determine if a database migration is necessary given the current
	// database version and the maximum migration version.
	versionUpgradePending := currentDBVersion < int(maxMigrationVersion)
	if !versionUpgradePending {
		s.log.InfoS(
			context.Background(),
			"Current database version is up-to-date, skipping "+
				"migration attempt and backup creation",
			"current_db_version", currentDBVersion,
			"max_migration_version", maxMigrationVersion,
		)

		return nil
	}

	// At this point, we know that a database migration is necessary.
	// Create a backup of the database before starting the migration.
	if !s.cfg.SkipMigrationDBBackup {
		s.log.InfoS(
			context.Background(),
			"Creating database backup (before applying "+
				"migration(s))",
		)

		err := backupSqliteDatabase(
			s.DB, s.cfg.DatabaseFileName, s.log,
		)
		if err != nil {
			return err
		}
	} else {
		s.log.InfoS(
			context.Background(),
			"Skipping database backup creation before applying "+
				"migration(s)",
		)
	}

	s.log.InfoS(context.Background(), "Applying migrations to database")

	return mig.Up()
}

// ExecuteMigrations runs migrations for the sqlite database, depending on the
// target given, either all migrations or up to a given version.
func (s *SqliteStore) ExecuteMigrations(target MigrationTarget,
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
			DatabaseName:      "sqlite",
			LatestVersion:     &opts.latestVersion,
			PostStepCallbacks: opts.postStepCallbacks,
			Log:               s.log,
		},
	)
	if err != nil {
		return fmt.Errorf("apply sqlite migrations: %w", err)
	}

	return nil
}

// NewTestSqliteDB is a helper function that creates an SQLite database for
// testing.
func NewTestSqliteDB(t testing.TB) *SqliteStore {
	t.Helper()

	// TODO(roasbeef): if we pass :memory: for the file name, then we get
	// an in mem version to speed up tests.
	dbPath := filepath.Join(t.TempDir(), "tmp.db")
	t.Logf("Creating new SQLite DB handle for testing: %s", dbPath)

	// For tests, use a simple logger that outputs to the test log.
	log := btclog.Disabled

	return NewTestSqliteDBHandleFromPath(t, dbPath, log)
}

// NewTestSqliteDBHandleFromPath is a helper function that creates a SQLite
// database handle given a database file path.
func NewTestSqliteDBHandleFromPath(t testing.TB, dbPath string,
	log btclog.Logger) *SqliteStore {

	t.Helper()

	sqlDB, err := NewSqliteStore(&SqliteConfig{
		DatabaseFileName: dbPath,
		SkipMigrations:   false,
	}, log)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, sqlDB.DB.Close())
	})

	return sqlDB
}

// NewTestSqliteDBWithVersion is a helper function that creates an SQLite
// database for testing and migrates it to the given version.
func NewTestSqliteDBWithVersion(t testing.TB, version uint) *SqliteStore {
	t.Helper()

	t.Logf(
		"Creating new SQLite DB for testing, migrating to version %d",
		version,
	)

	// For tests, use a simple logger that outputs to the test log.
	log := btclog.Disabled

	// TODO(roasbeef): if we pass :memory: for the file name, then we get
	// an in mem version to speed up tests.
	dbFileName := filepath.Join(t.TempDir(), "tmp.db")
	sqlDB, err := NewSqliteStore(&SqliteConfig{
		DatabaseFileName: dbFileName,
		SkipMigrations:   true,
	}, log)
	require.NoError(t, err)

	err = sqlDB.ExecuteMigrations(TargetVersion(version))
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, sqlDB.DB.Close())
	})

	return sqlDB
}

package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/golang-migrate/migrate/v4"
	sqlite_migrate "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // Register relevant drivers.
)

const (
	// sqliteOptionPrefix is the string prefix sqlite uses to set various
	// options. This is used in the following format:
	//   * sqliteOptionPrefix || option_name = option_value.
	sqliteOptionPrefix = "_pragma"

	// sqliteTxLockImmediate is a dsn option used to ensure that write
	// transactions are started immediately.
	sqliteTxLockImmediate = "_txlock=immediate"

	// defaultMaxConns is the number of permitted active and idle
	// connections. We want to limit this so it isn't unlimited. We use the
	// same value for the number of idle connections as, this can speed up
	// queries given a new connection doesn't need to be established each
	// time.
	defaultMaxConns = 25

	// defaultConnMaxLifetime is the maximum amount of time a connection can
	// be reused for before it is closed.
	defaultConnMaxLifetime = 10 * time.Minute
)

var (
	// sqliteSchemaReplacements is a map of schema strings that need to be
	// replaced for sqlite. There currently aren't any replacements, because
	// the SQL files are written with SQLite compatibility in mind.
	sqliteSchemaReplacements = map[string]string{}
)

// SqliteConfig holds all the config arguments needed to interact with our
// sqlite DB.
//
//nolint:ll
type SqliteConfig struct {
	// SkipMigrations if true, then all the tables will be created on
	// start up if they don't already exist.
	SkipMigrations bool `long:"skipmigrations" mapstructure:"skipmigrations" description:"Skip applying migrations on startup."`

	// SkipMigrationDBBackup if true, then a backup of the database
	// will not be created before applying migrations.
	SkipMigrationDBBackup bool `long:"skipmigrationdbbackup" mapstructure:"skipmigrationdbbackup" description:"Skip creating a backup of the database before applying migrations."`

	// DatabaseFileName is the full file path where the database file
	// can be found.
	DatabaseFileName string `long:"dbfile" mapstructure:"dbfile" description:"The full path to the database."`
}

// SqliteStore is a sqlite3 based database for the daemon.
type SqliteStore struct {
	cfg *SqliteConfig

	log btclog.Logger

	*BaseDB
}

// NewSqliteStore attempts to open a new sqlite database based on the passed
// config. Extra migration sets registered via WithExtraMigrations apply after
// darepo's core migrations against the same *sql.DB connection.
func NewSqliteStore(cfg *SqliteConfig, log btclog.Logger,
	opts ...StoreOption) (*SqliteStore, error) {

	// The set of pragma options are accepted using query options. For now
	// we only want to ensure that foreign key constraints are properly
	// enforced.
	pragmaOptions := []struct {
		name  string
		value string
	}{
		{
			name:  "foreign_keys",
			value: "on",
		},
		{
			name:  "journal_mode",
			value: "WAL",
		},
		{
			name:  "busy_timeout",
			value: "30000",
		},
		{
			// With the WAL mode, this ensures that we also do an
			// extra WAL sync after each transaction. The normal
			// sync mode skips this and gives better performance,
			// but risks durability.
			name:  "synchronous",
			value: "full",
		},
		{
			// This is used to ensure proper durability for users
			// running on Mac OS. It uses the correct fsync system
			// call to ensure items are fully flushed to disk.
			name:  "fullfsync",
			value: "true",
		},
	}
	sqliteOptions := make(url.Values)
	for _, option := range pragmaOptions {
		sqliteOptions.Add(
			sqliteOptionPrefix,
			fmt.Sprintf("%v=%v", option.name, option.value),
		)
	}

	// Construct the DSN which is just the database file name, appended
	// with the series of pragma options as a query URL string. For more
	// details on the formatting here, see the modernc.org/sqlite docs:
	// https://pkg.go.dev/modernc.org/sqlite#Driver.Open.
	dsn := fmt.Sprintf("%v?%v&%v", cfg.DatabaseFileName,
		sqliteOptions.Encode(), sqliteTxLockImmediate)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(defaultMaxConns)
	db.SetMaxIdleConns(defaultMaxConns)
	db.SetConnMaxLifetime(defaultConnMaxLifetime)

	queries := sqlc.NewSqlite(db)
	s := &SqliteStore{
		cfg: cfg,
		log: log,
		BaseDB: &BaseDB{
			DB:      db,
			Queries: queries,
		},
	}

	// Now that the database is open, populate the database with our set of
	// schemas based on our embedded in-memory file system. The cfg-level
	// SkipMigrations switch is the all-or-nothing kill switch and overrides
	// every StoreOption; if it is set we leave the database untouched.
	if cfg.SkipMigrations {
		return s, nil
	}

	// Downstream consumers that want darepo's connection plumbing without
	// any of its tables can opt out of the core + actor-delivery
	// migrations by passing WithSkipCoreMigrations. The extension
	// migrations registered via WithExtraMigrations still apply against
	// the same *sql.DB, so they remain the sole source of schema.
	so := collectStoreOpts(opts)
	if !so.skipCore {
		err := s.ExecuteMigrations(s.backupAndMigrate)
		if err != nil {
			return nil, fmt.Errorf("error executing migrations: %w",
				err)
		}

		err = runActorDeliveryMigrations(s.DB, s.Backend())
		if err != nil {
			return nil, fmt.Errorf("error executing "+
				"actor-delivery migrations: %w", err)
		}
	}

	// Apply any downstream-registered extension migrations. Each set
	// tracks its own version in a schema_migrations_<Name> table, so
	// darepo's version counter (when present) stays independent of the
	// consumer's, and the extension version table is the only migration
	// state in skip-core mode.
	if err := applyExtraMigrationsSQLite(s, so.extras); err != nil {
		return nil, err
	}

	return s, nil
}

// backupSqliteDatabase creates a backup of the given SQLite database.
func backupSqliteDatabase(srcDB *sql.DB, dbFullFilePath string,
	log btclog.Logger) error {

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

	log.InfoS(context.Background(), "Creating backup of database file",
		"source", dbFullFilePath,
		"backup", backupFullFilePath,
	)

	// Create the database backup.
	vacuumIntoQuery := "VACUUM INTO ?;"
	stmt, err := srcDB.Prepare(vacuumIntoQuery)
	if err != nil {
		return err
	}
	defer stmt.Close()

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
		s.log.InfoS(context.Background(), "Current database version "+
			"is up-to-date, skipping migration attempt and backup "+
			"creation", "current_db_version", currentDBVersion,
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

		err := backupSqliteDatabase(s.DB, s.cfg.DatabaseFileName, s.log)
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

	driver, err := sqlite_migrate.WithInstance(
		s.DB, &sqlite_migrate.Config{},
	)
	if err != nil {
		return fmt.Errorf("error creating sqlite migration: %w", err)
	}

	sqliteFS := newReplacerFS(sqlSchemas, sqliteSchemaReplacements)

	return applyMigrations(
		sqliteFS, driver, "sqlc/migrations", "sqlite", target, opts,
		s.log,
	)
}

// NewTestSqliteDB is a helper function that creates an SQLite database for
// testing.
func NewTestSqliteDB(t testing.TB) *SqliteStore {
	t.Helper()

	// TODO(roasbeef): if we pass :memory: for the file name, then we get
	// an in mem version to speed up tests.
	dbPath := filepath.Join(t.TempDir(), "tmp.db")
	t.Logf("Creating new SQLite DB handle for testing: %s", dbPath)

	return NewTestSqliteDBHandleFromPath(t, dbPath)
}

// NewTestSqliteDBHandleFromPath is a helper function that creates a SQLite
// database handle given a database file path.
func NewTestSqliteDBHandleFromPath(t testing.TB, dbPath string) *SqliteStore {
	t.Helper()

	sqlDB, err := NewSqliteStore(&SqliteConfig{
		DatabaseFileName: dbPath,
		SkipMigrations:   false,
	}, btclog.Disabled)
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

	// TODO(roasbeef): if we pass :memory: for the file name, then we get
	// an in mem version to speed up tests.
	dbFileName := filepath.Join(t.TempDir(), "tmp.db")
	sqlDB, err := NewSqliteStore(&SqliteConfig{
		DatabaseFileName: dbFileName,
		SkipMigrations:   true,
	}, btclog.Disabled)
	require.NoError(t, err)

	err = sqlDB.ExecuteMigrations(TargetVersion(version))
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, sqlDB.DB.Close())
	})

	return sqlDB
}

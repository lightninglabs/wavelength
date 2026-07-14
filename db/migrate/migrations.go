package migrate

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"

	"github.com/btcsuite/btclog/v2"
	golangmigrate "github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/lightninglabs/wavelength/db/sqlc"
)

// Target applies a migration target against a configured migration instance.
type Target func(mig *golangmigrate.Migrate, currentDBVersion int,
	maxMigrationVersion uint) error

var (
	// TargetLatest is a Target that migrates to the latest available
	// version.
	TargetLatest = func(mig *golangmigrate.Migrate, _ int, _ uint) error {
		return mig.Up()
	}
)

var (
	// ErrMigrationDowngrade is returned when a database downgrade is
	// detected.
	ErrMigrationDowngrade = errors.New("database downgrade detected")
)

const (
	// integerPrimaryKeyAutoIncrement is the SQLite primary-key shape that
	// guarantees rowid values are not reused after deleting the current max
	// row.
	integerPrimaryKeyAutoIncrement = "INTEGER PRIMARY KEY AUTOINCREMENT"
)

var (
	// postgresSchemaReplacements contains schema token replacements used
	// when running SQLite-oriented migrations against Postgres.
	postgresSchemaReplacements = map[string]string{
		"BLOB":                         "BYTEA",
		integerPrimaryKeyAutoIncrement: "BIGSERIAL PRIMARY KEY",
		"INTEGER PRIMARY KEY":          "BIGSERIAL PRIMARY KEY",
		"TIMESTAMP":                    "TIMESTAMP WITHOUT TIME ZONE",
	}
)

// PostgresSchemaReplacements returns a copy of the Postgres schema
// replacement rules used by migration sources.
func PostgresSchemaReplacements() map[string]string {
	replacements := make(map[string]string, len(postgresSchemaReplacements))
	for from, to := range postgresSchemaReplacements {
		replacements[from] = to
	}

	return replacements
}

// Config configures RunMigrations behavior.
type Config struct {
	// MigrationsTable is the migration bookkeeping table.
	MigrationsTable string

	// DatabaseName is the migration instance name.
	DatabaseName string

	// LatestVersion enables downgrade protection when set.
	LatestVersion *uint

	// PostStepCallbacks are invoked after each SQL migration step.
	PostStepCallbacks map[uint]golangmigrate.PostStepCallback

	// PostgresReplacements are source replacements applied for Postgres.
	PostgresReplacements map[string]string

	// Log enables migration progress logging when non-nil.
	Log btclog.Logger
}

// RunMigrations applies migrations for the given backend, source filesystem,
// and source path.
func RunMigrations(db *sql.DB, backend sqlc.BackendType, sourceFS fs.FS,
	sourcePath string, target Target, cfg Config) error {

	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if sourceFS == nil {
		return fmt.Errorf("source fs is nil")
	}
	if target == nil {
		target = TargetLatest
	}

	driver, err := newMigrationDriver(
		db, backend, cfg.MigrationsTable,
	)
	if err != nil {
		return err
	}

	if backend == sqlc.BackendTypePostgres &&
		len(cfg.PostgresReplacements) > 0 {

		sourceFS = newReplacerFS(sourceFS, cfg.PostgresReplacements)
	}

	source, err := iofs.New(sourceFS, sourcePath)
	if err != nil {
		return fmt.Errorf("create migration source: %w", err)
	}

	postStepCallbacks := cfg.PostStepCallbacks
	if postStepCallbacks == nil {
		postStepCallbacks = make(
			map[uint]golangmigrate.PostStepCallback,
		)
	}

	mig, err := golangmigrate.NewWithInstance(
		"iofs", source, cfg.DatabaseName, driver,
		golangmigrate.WithPostStepCallbacks(postStepCallbacks),
	)
	if err != nil {
		return fmt.Errorf("create migration instance: %w", err)
	}

	currentDBVersion, err := verifyVersionState(mig, cfg.LatestVersion)
	if err != nil {
		return err
	}

	if cfg.Log != nil {
		cfg.Log.InfoS(
			context.Background(),
			"Attempting to apply migration(s)",
			"current_db_version", currentDBVersion,
		)

		mig.Log = &migrationLogger{log: cfg.Log}
	}

	maxVersion := uint(0)
	if cfg.LatestVersion != nil {
		maxVersion = *cfg.LatestVersion
	}

	err = target(mig, currentDBVersion, maxVersion)
	if err != nil && !errors.Is(err, golangmigrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}

	if cfg.Log != nil {
		postVersion, _, postErr := mig.Version()
		if postErr != nil &&
			!errors.Is(postErr, golangmigrate.ErrNilVersion) {
			return fmt.Errorf("unable to determine current "+
				"migration version: %w", postErr)
		}

		cfg.Log.InfoS(
			context.Background(),
			"Database version after migration",
			"current_db_version", postVersion,
		)
	}

	return nil
}

// verifyVersionState verifies migration version state and applies downgrade
// protection when latestVersion is configured.
func verifyVersionState(mig *golangmigrate.Migrate,
	latestVersion *uint) (int, error) {

	version, dirty, err := mig.Version()
	switch {
	case errors.Is(err, golangmigrate.ErrNilVersion):
		return -1, nil

	case err != nil:
		return 0, fmt.Errorf("unable to determine current migration "+
			"version: %w", err)
	}

	if dirty {
		return 0, fmt.Errorf("database is in a dirty state at version "+
			"%v, manual intervention required", version)
	}

	if latestVersion != nil && version > *latestVersion {
		return 0, fmt.Errorf("%w: database version is newer than the "+
			"latest migration version, preventing downgrade: "+
			"db_version=%v, latest_migration_version=%v",
			ErrMigrationDowngrade, version, *latestVersion)
	}

	return int(version), nil
}

// migrationLogger wraps a btclog.Logger for golang-migrate logging.
type migrationLogger struct {
	log btclog.Logger
}

// Printf logs migration output through btclog.
func (m *migrationLogger) Printf(format string, v ...interface{}) {
	format = strings.TrimRight(format, "\n")

	switch m.log.Level() {
	case btclog.LevelTrace:
		m.log.Tracef(format, v...)

	case btclog.LevelDebug:
		m.log.Debugf(format, v...)

	case btclog.LevelInfo:
		m.log.Infof(format, v...)

	case btclog.LevelWarn:
		m.log.Warnf(format, v...)

	case btclog.LevelError:
		m.log.Errorf(format, v...)

	case btclog.LevelCritical:
		m.log.Criticalf(format, v...)

	case btclog.LevelOff:
	}
}

// Verbose returns true when verbose migration logs are enabled.
func (m *migrationLogger) Verbose() bool {
	return m.log.Level() <= btclog.LevelDebug
}

// replacerFS wraps a file system and applies search-and-replace operations
// when opening files.
type replacerFS struct {
	parentFS        fs.FS
	replaces        map[string]string
	replacementKeys []string
}

// A compile-time assertion to make sure replacerFS implements fs.FS.
var _ fs.FS = (*replacerFS)(nil)

// newReplacerFS creates a new replacement-wrapping file system.
func newReplacerFS(parent fs.FS, replaces map[string]string) *replacerFS {
	keys := make([]string, 0, len(replaces))
	for from := range replaces {
		keys = append(keys, from)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		return len(keys[i]) > len(keys[j])
	})

	return &replacerFS{
		parentFS:        parent,
		replaces:        replaces,
		replacementKeys: keys,
	}
}

// Open opens a file from the wrapped file system.
func (t *replacerFS) Open(name string) (fs.File, error) {
	f, err := t.parentFS.Open(name)
	if err != nil {
		return nil, err
	}

	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()

		return nil, err
	}

	if stat.IsDir() {
		return f, nil
	}

	replacer, err := newReplacerFile(f, t.replaces, t.replacementKeys)
	if err != nil {
		_ = f.Close()

		return nil, err
	}

	return replacer, nil
}

// replacerFile is a file wrapper that serves replacement-transformed content.
type replacerFile struct {
	parentFile fs.File
	buf        bytes.Buffer
}

// A compile-time assertion to make sure replacerFile implements fs.File.
var _ fs.File = (*replacerFile)(nil)

// newReplacerFile creates a file wrapper with content replacements applied.
func newReplacerFile(parent fs.File, replaces map[string]string,
	replacementKeys []string) (*replacerFile, error) {

	content, err := io.ReadAll(parent)
	if err != nil {
		return nil, err
	}

	contentStr := string(content)
	for _, from := range replacementKeys {
		to := replaces[from]
		contentStr = strings.ReplaceAll(contentStr, from, to)
	}

	var buf bytes.Buffer
	_, err = buf.WriteString(contentStr)
	if err != nil {
		return nil, err
	}

	return &replacerFile{
		parentFile: parent,
		buf:        buf,
	}, nil
}

// Stat returns info for the wrapped file.
func (t *replacerFile) Stat() (fs.FileInfo, error) {
	return t.parentFile.Stat()
}

// Read reads replacement-transformed bytes from the file.
func (t *replacerFile) Read(bytes []byte) (int, error) {
	return t.buf.Read(bytes)
}

// Close closes the wrapped file.
func (t *replacerFile) Close() error {
	return t.parentFile.Close()
}

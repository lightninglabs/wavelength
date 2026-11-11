// Package relaydb owns the publisher relay's dedicated SQL database. It has
// no wallet schema, provider I/O or device-authentication policy.
package relaydb

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // Register PostgreSQL's pgx driver.
	"github.com/lightninglabs/wavelength/notifyrelay/relaydb/sqlc"
	sqlitedriver "modernc.org/sqlite"
)

// migrations contains the same typed schema for both database engines.
//
//go:embed migrations/*.sql
var migrations embed.FS

// ErrUnavailable hides database endpoints, credentials and provider details.
var ErrUnavailable = errors.New("relay database unavailable")

// Database owns a dedicated relay SQL pool and its migration source. Callers
// must join HTTP admission, source ingress and delivery before closing it.
type Database struct {
	DB       *sql.DB
	Queries  *sqlc.Queries
	migrator *migrate.Migrate
}

// OpenSQLite opens a private relay file using WAL and synchronous commits.
// A fresh SQL database intentionally has no Bolt import or fallback path.
func OpenSQLite(ctx context.Context, path string) (*Database, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, unavailable("sqlite file", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return nil, unavailable("sqlite file", err)
	}
	// The publisher selects this path in local daemon configuration.
	//nolint:gosec // No device or producer request controls the file path.
	file, err := os.OpenFile(absolute, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, unavailable("sqlite file", err)
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()

		return nil, unavailable("sqlite file", err)
	}
	if err := file.Close(); err != nil {
		return nil, unavailable("sqlite file", err)
	}
	options := url.Values{}
	for _, pragma := range []string{
		"foreign_keys=ON",
		"journal_mode=WAL",
		"synchronous=FULL",
		"busy_timeout=5000",
	} {
		options.Add("_pragma", pragma)
	}
	options.Set("_txlock", "immediate")
	location := url.URL{
		Scheme:   "file",
		Path:     absolute,
		RawQuery: options.Encode(),
	}
	pool, err := sql.Open("sqlite", location.String())
	if err != nil {
		return nil, unavailable("sqlite pool", err)
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	driver, err := sqlite.WithInstance(
		pool, &sqlite.Config{
			MigrationsTable: "wavenotifyd_migrations",
		},
	)
	if err != nil {
		_ = pool.Close()

		return nil, unavailable("sqlite migration driver", err)
	}

	return initialize(ctx, pool, driver, "sqlite")
}

// OpenPostgres opens a publisher-provided dedicated database. DSN is secret;
// neither this package nor its errors include its contents.
func OpenPostgres(ctx context.Context, dsn string) (*Database, error) {
	pool, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, unavailable("postgres pool", err)
	}
	pool.SetMaxOpenConns(25)
	pool.SetMaxIdleConns(5)
	pool.SetConnMaxLifetime(10 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.PingContext(pingCtx); err != nil {
		_ = pool.Close()

		return nil, unavailable("postgres connection", err)
	}
	driver, err := postgres.WithInstance(
		pool, &postgres.Config{
			MigrationsTable: "wavenotifyd_migrations",
		},
	)
	if err != nil {
		_ = pool.Close()

		return nil, unavailable("postgres migration driver", err)
	}

	return initialize(ctx, pool, driver, "postgres")
}

// initialize applies relay-only embedded migrations before exposing queries.
func initialize(ctx context.Context, pool *sql.DB, driver database.Driver,
	name string) (*Database, error) {

	if err := ctx.Err(); err != nil {
		_ = driver.Close()

		return nil, err
	}
	source, err := iofs.New(migrations, "migrations")
	if err != nil {
		_ = driver.Close()

		return nil, unavailable("migration source", err)
	}
	migration, err := migrate.NewWithInstance("iofs", source, name, driver)
	if err != nil {
		_ = source.Close()
		_ = driver.Close()

		return nil, unavailable("migration setup", err)
	}
	if err := migration.Up(); err != nil &&
		!errors.Is(err, migrate.ErrNoChange) {

		_, _ = migration.Close()

		return nil, unavailable("schema migration", err)
	}

	return &Database{
		DB:       pool,
		Queries:  sqlc.New(pool),
		migrator: migration,
	}, nil
}

// unavailable retains a fixed startup phase and safe typed failure details.
// Driver text can contain connection credentials and is deliberately excluded.
func unavailable(phase string, cause error) error {
	var dirty migrate.ErrDirty
	if errors.As(cause, &dirty) {
		return fmt.Errorf("%w: %s: dirty version %d", ErrUnavailable,
			phase, dirty.Version)
	}
	if errors.Is(cause, os.ErrPermission) {
		return fmt.Errorf("%w: %s: permission denied", ErrUnavailable,
			phase)
	}

	return fmt.Errorf("%w: %s", ErrUnavailable, phase)
}

// Close releases the source and pool after all domain owners have joined.
func (d *Database) Close() error {
	sourceErr, databaseErr := d.migrator.Close()

	return errors.Join(sourceErr, databaseErr)
}

// Write owns a serializable transaction and retries only database conflicts.
// The closure must contain SQL/local computation only, never provider I/O or
// a remote ACK. Reset output values at each attempt; failed attempts roll back.
func (d *Database) Write(ctx context.Context,
	apply func(*sqlc.Queries) error) error {

	for attempt := range 10 {
		err := d.writeOnce(ctx, apply)
		if err == nil || !retryable(err) || attempt == 9 {
			return err
		}
		timer := time.NewTimer(
			time.Duration(attempt+1) * 5 * time.Millisecond,
		)
		select {
		case <-ctx.Done():
			timer.Stop()

			return ctx.Err()

		case <-timer.C:
		}
	}

	return ErrUnavailable
}

// writeOnce rolls back closure and commit failures before returning the error.
func (d *Database) writeOnce(ctx context.Context,
	apply func(*sqlc.Queries) error) error {

	tx, err := d.DB.BeginTx(
		ctx, &sql.TxOptions{
			Isolation: sql.LevelSerializable,
		},
	)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if err := apply(d.Queries.WithTx(tx)); err != nil {
		return err
	}

	return tx.Commit()
}

// retryable identifies engine conflict codes without parsing error strings.
func retryable(err error) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code == "40001" || pg.Code == "40P01"
	}
	var sqliteErr *sqlitedriver.Error
	if errors.As(err, &sqliteErr) {
		// The low byte is SQLite's primary result code: BUSY or LOCKED.
		code := sqliteErr.Code() & 0xff

		return code == 5 || code == 6
	}

	return false
}

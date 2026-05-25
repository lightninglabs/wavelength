//go:build js && wasm

package migrate

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"github.com/golang-migrate/migrate/v4/database"
)

// wasmSQLiteDriver implements golang-migrate's database.Driver contract for
// the browser-backed wasmsqlite database/sql driver.
type wasmSQLiteDriver struct {
	db              *sql.DB
	migrationsTable string
	isLocked        atomic.Bool
}

// newWASMSQLiteMigrationDriver creates a migration driver that avoids
// importing golang-migrate's modernc-backed sqlite driver in js/wasm builds.
func newWASMSQLiteMigrationDriver(db *sql.DB,
	migrationsTable string) (database.Driver, error) {

	if db == nil {
		return nil, fmt.Errorf("db is nil")
	}
	if migrationsTable == "" {
		migrationsTable = "schema_migrations"
	}

	driver := &wasmSQLiteDriver{
		db:              db,
		migrationsTable: migrationsTable,
	}
	if err := driver.ensureVersionTable(); err != nil {
		return nil, err
	}

	return driver, nil
}

// Open is unsupported because callers provide an already-open browser DB.
func (d *wasmSQLiteDriver) Open(string) (database.Driver, error) {
	return nil, fmt.Errorf("open is unsupported for wasm sqlite migrations")
}

// Close leaves the caller-owned database handle open.
func (d *wasmSQLiteDriver) Close() error {
	return nil
}

// Lock acquires a process-local migration lock.
func (d *wasmSQLiteDriver) Lock() error {
	if !d.isLocked.CompareAndSwap(false, true) {
		return database.ErrLocked
	}

	return nil
}

// Unlock releases a process-local migration lock.
func (d *wasmSQLiteDriver) Unlock() error {
	if !d.isLocked.CompareAndSwap(true, false) {
		return database.ErrNotLocked
	}

	return nil
}

// Run executes one migration inside a transaction.
func (d *wasmSQLiteDriver) Run(migration io.Reader) error {
	migrationBytes, err := io.ReadAll(migration)
	if err != nil {
		return err
	}

	query := string(migrationBytes)
	tx, err := d.db.Begin()
	if err != nil {
		return &database.Error{
			OrigErr: err,
			Err:     "transaction start failed",
		}
	}

	if _, err := tx.Exec(query); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			err = errors.Join(err, rollbackErr)
		}

		return &database.Error{
			OrigErr: err,
			Query:   migrationBytes,
		}
	}

	if err := tx.Commit(); err != nil {
		return &database.Error{
			OrigErr: err,
			Err:     "transaction commit failed",
		}
	}

	return nil
}

// SetVersion updates the migration bookkeeping row.
func (d *wasmSQLiteDriver) SetVersion(version int, dirty bool) error {
	tx, err := d.db.Begin()
	if err != nil {
		return &database.Error{
			OrigErr: err,
			Err:     "transaction start failed",
		}
	}

	deleteQuery := fmt.Sprintf("DELETE FROM %s",
		quoteSQLiteIdentifier(d.migrationsTable))
	if _, err := tx.Exec(deleteQuery); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			err = errors.Join(err, rollbackErr)
		}

		return &database.Error{
			OrigErr: err,
			Query:   []byte(deleteQuery),
		}
	}

	if version >= 0 || (version == database.NilVersion && dirty) {
		insertQuery := fmt.Sprintf("INSERT INTO %s (version, dirty) "+
			"VALUES (?, ?)",
			quoteSQLiteIdentifier(d.migrationsTable))
		if _, err := tx.Exec(insertQuery, version, dirty); err != nil {
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				err = errors.Join(err, rollbackErr)
			}

			return &database.Error{
				OrigErr: err,
				Query:   []byte(insertQuery),
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return &database.Error{
			OrigErr: err,
			Err:     "transaction commit failed",
		}
	}

	return nil
}

// Version returns the current migration version.
func (d *wasmSQLiteDriver) Version() (int, bool, error) {
	query := fmt.Sprintf("SELECT version, dirty FROM %s LIMIT 1",
		quoteSQLiteIdentifier(d.migrationsTable))

	var version int
	var dirty bool
	err := d.db.QueryRow(query).Scan(&version, &dirty)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return database.NilVersion, false, nil

	case err != nil:
		return database.NilVersion, false, &database.Error{
			OrigErr: err,
			Query:   []byte(query),
		}

	default:
		return version, dirty, nil
	}
}

// Drop drops user tables and vacuums the database.
func (d *wasmSQLiteDriver) Drop() error {
	const query = `SELECT name FROM sqlite_master WHERE type = 'table' ` +
		`AND name NOT LIKE 'sqlite_%'`

	rows, err := d.db.Query(query)
	if err != nil {
		return &database.Error{
			OrigErr: err,
			Query:   []byte(query),
		}
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return &database.Error{
				OrigErr: err,
				Query:   []byte(query),
			}
		}

		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return &database.Error{
			OrigErr: err,
			Query:   []byte(query),
		}
	}

	for _, table := range tables {
		dropQuery := "DROP TABLE " + quoteSQLiteIdentifier(table)
		if _, err := d.db.Exec(dropQuery); err != nil {
			return &database.Error{
				OrigErr: err,
				Query:   []byte(dropQuery),
			}
		}
	}

	if _, err := d.db.Exec("VACUUM"); err != nil {
		return &database.Error{
			OrigErr: err,
			Query:   []byte("VACUUM"),
		}
	}

	return nil
}

func (d *wasmSQLiteDriver) ensureVersionTable() error {
	table := quoteSQLiteIdentifier(d.migrationsTable)
	index := quoteSQLiteIdentifier(d.migrationsTable + "_version_unique")
	query := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (version INTEGER, dirty BOOLEAN);
CREATE UNIQUE INDEX IF NOT EXISTS %s ON %s (version);
`,
		table, index, table)

	if _, err := d.db.Exec(query); err != nil {
		return &database.Error{
			OrigErr: err,
			Query:   []byte(query),
		}
	}

	return nil
}

func quoteSQLiteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

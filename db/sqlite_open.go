package db

import (
	"database/sql"
	"time"
)

// SQLitePragma is one PRAGMA setting applied when a SQLite database opens.
type SQLitePragma struct {
	Name  string
	Value string
}

// SQLiteOpenConfig contains the driver-neutral SQLite open settings shared by
// the native and browser-backed database handles.
type SQLiteOpenConfig struct {
	// DatabaseFileName is the native filename or logical browser OPFS name.
	DatabaseFileName string

	// Pragmas are applied by the selected driver at open time when
	// possible.
	Pragmas []SQLitePragma

	// TxLockImmediate requests immediate write transactions when the driver
	// supports that mode.
	TxLockImmediate bool

	// MaxOpenConns bounds the database/sql connection pool.
	MaxOpenConns int

	// MaxIdleConns bounds idle connections in the database/sql pool.
	MaxIdleConns int

	// ConnMaxLifetime limits how long one SQL connection is reused.
	ConnMaxLifetime time.Duration
}

// SQLiteOpenResult returns the opened SQL handle and driver details useful for
// logging and tests.
type SQLiteOpenResult struct {
	DB         *sql.DB
	DriverName string
	DSN        string
}

// OpenSQLiteDatabase opens a SQLite handle using the driver selected for the
// current build target.
func OpenSQLiteDatabase(cfg SQLiteOpenConfig) (*SQLiteOpenResult, error) {
	return openSQLiteDatabase(cfg)
}

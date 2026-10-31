//go:build (!js || !wasm) && !sqlite_cgo

package db

import (
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite" // Register native SQLite driver.
)

const (
	// sqliteOptionPrefix is the modernc SQLite DSN prefix for pragma
	// settings.
	sqliteOptionPrefix = "_pragma"

	// sqliteTxLockImmediate starts write transactions immediately.
	sqliteTxLockImmediate = "_txlock=immediate"

	// nativeSQLiteDriverName is the database/sql driver registered by
	// modernc.org/sqlite.
	nativeSQLiteDriverName = "sqlite"
)

// SQLiteDSN returns the database/sql driver name and DSN this build opens
// SQLite through for the given settings, without opening anything. Callers
// that hand a DSN to their own database/sql plumbing use it to stay on the
// driver the build selects, rather than hard-coding one. The pool settings in
// cfg are not part of a DSN and are ignored.
func SQLiteDSN(cfg SQLiteOpenConfig) (string, string, error) {
	sqliteOptions := make(url.Values)
	for _, pragma := range cfg.Pragmas {
		sqliteOptions.Add(
			sqliteOptionPrefix,
			fmt.Sprintf("%s=%s", pragma.Name, pragma.Value),
		)
	}

	dsn := fmt.Sprintf("%s?%s", cfg.DatabaseFileName,
		sqliteOptions.Encode())
	if cfg.TxLockImmediate {
		dsn = fmt.Sprintf("%s&%s", dsn, sqliteTxLockImmediate)
	}

	return nativeSQLiteDriverName, dsn, nil
}

// openSQLiteDatabase opens SQLite through the native modernc driver.
func openSQLiteDatabase(cfg SQLiteOpenConfig) (*SQLiteOpenResult, error) {
	driverName, dsn, err := SQLiteDSN(cfg)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}

	configureSQLitePool(db, cfg)

	return &SQLiteOpenResult{
		DB:         db,
		DriverName: driverName,
		DSN:        dsn,
	}, nil
}

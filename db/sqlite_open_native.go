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
)

// openSQLiteDatabase opens SQLite through the native modernc driver.
func openSQLiteDatabase(cfg SQLiteOpenConfig) (*SQLiteOpenResult, error) {
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

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	configureSQLitePool(db, cfg)

	return &SQLiteOpenResult{
		DB:         db,
		DriverName: "sqlite",
		DSN:        dsn,
	}, nil
}

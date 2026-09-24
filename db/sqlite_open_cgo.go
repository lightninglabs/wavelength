//go:build (!js || !wasm) && sqlite_cgo

package db

import (
	"database/sql"
	"fmt"
	"net/url"
	"strings"
)

// openSQLiteDatabase uses SQLite compiled against the host C library. Android
// builds select this driver to avoid modernc/libc's raw Linux syscalls, which
// Android's application sandbox can block. The tag also allows host testing.
func openSQLiteDatabase(cfg SQLiteOpenConfig) (*SQLiteOpenResult, error) {
	options := make(url.Values)
	for _, pragma := range cfg.Pragmas {
		name := strings.ToLower(pragma.Name)
		switch name {
		case "foreign_keys", "journal_mode", "busy_timeout",
			"synchronous", "fullfsync":

			// DSN options apply to every connection, including
			// those opened later when the pool grows or replaces a
			// handle.
			options.Set("_"+name, pragma.Value)

		default:
			// mattn silently ignores unknown DSN keys. Fail here so
			// new settings cannot silently lose their semantics.
			return nil, fmt.Errorf("unsupported CGO SQLite "+
				"pragma: %s", pragma.Name)
		}
	}
	if cfg.TxLockImmediate {
		options.Set("_txlock", "immediate")
	}

	dsn := fmt.Sprintf("%s?%s", cfg.DatabaseFileName, options.Encode())
	handle, err := sql.Open(cgoSQLiteDriverName, dsn)
	if err != nil {
		return nil, err
	}

	configureSQLitePool(handle, cfg)

	return &SQLiteOpenResult{
		DB:         handle,
		DriverName: cgoSQLiteDriverName,
		DSN:        dsn,
	}, nil
}

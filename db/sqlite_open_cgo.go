//go:build (!js || !wasm) && sqlite_cgo

package db

import (
	"database/sql"
	"fmt"
	"net/url"
	"strings"
)

// SQLiteDSN returns the database/sql driver name and DSN this build opens
// SQLite through for the given settings, without opening anything. Callers
// that hand a DSN to their own database/sql plumbing use it to stay on the
// driver the build selects, rather than hard-coding one. The pool settings in
// cfg are not part of a DSN and are ignored.
func SQLiteDSN(cfg SQLiteOpenConfig) (string, string, error) {
	options := make(url.Values)
	for _, pragma := range cfg.Pragmas {
		name := strings.ToLower(pragma.Name)
		switch name {
		case "foreign_keys", "journal_mode", "busy_timeout",
			"synchronous", "fullfsync", "locking_mode":

			// DSN options apply to every connection, including
			// those opened later when the pool grows or replaces a
			// handle.
			options.Set("_"+name, pragma.Value)

		default:
			// mattn silently ignores unknown DSN keys. Fail here so
			// new settings cannot silently lose their semantics.
			return "", "", fmt.Errorf("unsupported CGO SQLite "+
				"pragma: %s", pragma.Name)
		}
	}
	if cfg.TxLockImmediate {
		options.Set("_txlock", "immediate")
	}

	dsn := fmt.Sprintf("%s?%s", cfg.DatabaseFileName, options.Encode())

	return cgoSQLiteDriverName, dsn, nil
}

// openSQLiteDatabase uses SQLite compiled against the host C library. Android
// builds select this driver to avoid modernc/libc's raw Linux syscalls, which
// Android's application sandbox can block. The tag also allows host testing.
func openSQLiteDatabase(cfg SQLiteOpenConfig) (*SQLiteOpenResult, error) {
	driverName, dsn, err := SQLiteDSN(cfg)
	if err != nil {
		return nil, err
	}

	handle, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}

	configureSQLitePool(handle, cfg)

	return &SQLiteOpenResult{
		DB:         handle,
		DriverName: driverName,
		DSN:        dsn,
	}, nil
}

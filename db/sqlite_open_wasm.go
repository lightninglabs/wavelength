//go:build js && wasm

package db

import (
	"database/sql"
	"fmt"
	"hash/fnv"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/lightninglabs/go-wasmsqlite"
)

const (
	wasmSQLiteDriverName = "wasmsqlite"
	wasmSQLiteVFS        = "opfs"
)

// openSQLiteDatabase opens SQLite through the browser-backed wasmsqlite
// driver.
func openSQLiteDatabase(cfg SQLiteOpenConfig) (*SQLiteOpenResult, error) {
	values := url.Values{}
	values.Set("file", browserSQLiteFileName(cfg.DatabaseFileName))
	values.Set("vfs", wasmSQLiteVFS)
	values.Set("mode", "rwc")

	pragmas := make([]string, 0, len(cfg.Pragmas)+1)
	for _, pragma := range cfg.Pragmas {
		switch strings.ToLower(pragma.Name) {
		case "busy_timeout":
			values.Set("busy_timeout", pragma.Value)

		case "journal_mode":
			values.Set("journal_mode", pragma.Value)

		case "fullfsync":
			// fullfsync is a native filesystem durability hint and
			// is not meaningful for browser OPFS.

		default:
			pragmas = append(
				pragmas, pragma.Name+"="+pragma.Value,
			)
		}
	}

	pragmas = append(pragmas, "locking_mode=EXCLUSIVE")
	values.Set("pragma", strings.Join(pragmas, ";"))

	dsn := values.Encode()
	db, err := openWASMSQLiteWithRetry(dsn)
	if err != nil {
		return nil, err
	}

	return &SQLiteOpenResult{
		DB:         db,
		DriverName: wasmSQLiteDriverName,
		DSN:        dsn,
	}, nil
}

// openWASMSQLiteWithRetry smooths over reload-time OPFS release races. Each
// retry uses a fresh database/sql handle because a failed go-wasmsqlite open
// can leave the worker tracking the filename as open.
func openWASMSQLiteWithRetry(dsn string) (*sql.DB, error) {
	var lastErr error

	for attempt := 0; attempt < 25; attempt++ {
		db, err := sql.Open(wasmSQLiteDriverName, dsn)
		if err != nil {
			return nil, err
		}

		// OPFS SQLite handles must be single-connection handles.
		// Multiple SQL connections would race the same browser database
		// through one worker.
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)

		err = db.Ping()
		if err == nil {
			return db, nil
		}

		_ = db.Close()
		if !isWASMCantOpen(err) {
			return nil, err
		}

		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}

	return nil, lastErr
}

// isWASMCantOpen identifies the SQLite error returned while OPFS still holds a
// file lock from a just-unloaded page runtime.
func isWASMCantOpen(err error) bool {
	return strings.Contains(err.Error(), "SQLITE_CANTOPEN") ||
		strings.Contains(err.Error(), "unable to open database file")
}

// browserSQLiteFileName maps native paths to stable origin-local OPFS names.
// The full path is hashed into the name, not just its basename, so databases
// that share a basename across different data dirs or networks (e.g. the
// regtest and signet client.db, or two swaps.db) map to distinct OPFS files
// within one browser origin instead of silently colliding. This mirrors the
// scheme lwwallet uses for its own OPFS wallet database.
func browserSQLiteFileName(name string) string {
	normalized := filepath.ToSlash(filepath.Clean(name))
	base := filepath.Base(normalized)
	if base == "." || base == "/" || base == "" {
		base = "waved.db"
		normalized = base
	}

	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(normalized))

	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	return fmt.Sprintf("/%s-%016x%s", stem, hasher.Sum64(), ext)
}

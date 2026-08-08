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
	"github.com/lightninglabs/wavelength/internal/wasmhost"
)

const wasmSQLiteDriverName = "wasmsqlite"

// openSQLiteDatabase opens SQLite through the wasmsqlite driver, against
// whichever durable storage the host offers.
func openSQLiteDatabase(cfg SQLiteOpenConfig) (*SQLiteOpenResult, error) {
	values := url.Values{}
	values.Set("vfs", wasmhost.SQLiteVFS())
	if wasmhost.UnderNode() {
		// A Node host has a real filesystem, so the configured path is
		// usable as-is and the OPFS name mangling below would only
		// obscure where the database actually lives.
		values.Set("file", cfg.DatabaseFileName)
	} else {
		values.Set("file", browserSQLiteFileName(cfg.DatabaseFileName))
	}
	values.Set("mode", "rwc")

	// The daemon's databases are the only record of VTXO, swap, and round
	// state; there is no server to re-fetch them from. Refusing an
	// in-memory substitute makes a storage failure a startup error rather
	// than a wallet that looks healthy and forgets everything on exit.
	values.Set("require_persistent", "true")

	pragmas := make([]string, 0, len(cfg.Pragmas)+1)
	for _, pragma := range cfg.Pragmas {
		switch strings.ToLower(pragma.Name) {
		case "busy_timeout":
			values.Set("busy_timeout", pragma.Value)

		case "journal_mode":
			// The driver takes the journal mode as its own DSN key
			// rather than as a pragma, because it applies it last
			// and then reads the effective mode back. See the
			// locking_mode note below for why the order matters.
			values.Set("journal_mode", pragma.Value)

		case "fullfsync":
			// fullfsync asks Darwin for a stronger barrier than
			// fsync. Neither wasm VFS can express it: OPFS has no
			// such concept, and the node:fs VFS already issues a
			// full fsync on every xSync.

		default:
			pragmas = append(
				pragmas, pragma.Name+"="+pragma.Value,
			)
		}
	}

	// Exclusive locking is not merely an optimization for a single-connection
	// handle: it is what makes WAL reachable at all here. Neither wasm VFS
	// implements xShmMap, so the only WAL available is the mode SQLite
	// documents for hosts without shared memory, where an EXCLUSIVE
	// connection keeps the WAL index on the heap. The driver applies these
	// pragmas before the journal mode for exactly that reason, and then
	// verifies the mode it ended up in, so a regression here surfaces as a
	// startup error rather than as a database quietly running on the rollback
	// journal while `synchronous` was chosen for WAL.
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

		// A wasm SQLite handle must be a single-connection handle.
		// Multiple SQL connections would race the same database
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
//
// None of that applies to a Node host, which keeps the configured path as
// given: it is already unique, and it has the considerable advantage of being
// findable on disk.
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

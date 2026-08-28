//go:build !js || !wasm

package lwwallet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	btcwalletbase "github.com/btcsuite/btcwallet/wallet"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
)

const (
	// boltWalletDBTimeout is how long the BoltDB backend waits for
	// the database file lock before giving up.
	boltWalletDBTimeout = 60 * time.Second

	// sqliteWalletDBFileName is the SQLite wallet database's file
	// name under Config.DBDir. It deliberately differs from
	// btcwallet's BoltDB file name, so that neither backend can ever
	// be pointed at a database written by the other.
	sqliteWalletDBFileName = "wallet.sqlite.db"
)

// walletDBPath returns the path of the wallet database file the
// configured backend uses, and fails on an unknown backend so a typo
// cannot silently resolve to the default.
//
// Resolution also fails when the other backend's database file already
// exists in DBDir. The two backends store the wallet under different
// names, so without this check switching the backend of an initialized
// wallet directory looks exactly like a first start: btcwallet would
// happily create a second, empty wallet next to the existing — and
// possibly funded — one.
func walletDBPath(cfg Config) (string, error) {
	var backend, other string
	switch cfg.DBBackend {
	case "", DBBackendBolt:
		backend, other = DBBackendBolt, DBBackendSQLite

	case DBBackendSQLite:
		backend, other = DBBackendSQLite, DBBackendBolt

	default:
		return "", fmt.Errorf("unknown wallet database backend %q, "+
			"must be %q or %q", cfg.DBBackend, DBBackendBolt,
			DBBackendSQLite)
	}

	otherPath := walletDBFilePath(cfg.DBDir, other)
	otherExists, err := walletDBExists(otherPath, other)
	if err != nil {
		return "", err
	}
	if otherExists {
		return "", fmt.Errorf("wallet database %s exists but the %q "+
			"database backend is selected; set the backend to %q",
			otherPath, backend, other)
	}

	return walletDBFilePath(cfg.DBDir, backend), nil
}

// SQLiteWalletDBExists reports whether dbDir holds a wallet database
// written by the SQLite backend, and returns that database's path.
// Wallet implementations sharing the directory use it to refuse to start
// beside one: they only know their own file name, so to them a directory
// holding a SQLite wallet looks empty, and they would create a second,
// unrelated wallet next to it. The zero-length file a failed first open
// leaves behind does not count, the same as for the backend itself.
func SQLiteWalletDBExists(dbDir string) (string, bool, error) {
	path := walletDBFilePath(dbDir, DBBackendSQLite)
	exists, err := walletDBExists(path, DBBackendSQLite)

	return path, exists, err
}

// walletDBFilePath returns the file the given backend keeps the wallet
// database in.
func walletDBFilePath(dbDir, backend string) string {
	if backend == DBBackendSQLite {
		return filepath.Join(dbDir, sqliteWalletDBFileName)
	}

	return filepath.Join(dbDir, btcwalletbase.WalletDBName)
}

// newWalletLoaderOptions returns the btcwallet loader options for the
// configured wallet database backend, plus a func that releases
// whatever the resolution opened.
func newWalletLoaderOptions(cfg Config) ([]btcwallet.LoaderOption, func(),
	error) {

	// Resolving the path is also what validates the backend and
	// rejects a directory that already holds the other backend's
	// database.
	dbPath, err := walletDBPath(cfg)
	if err != nil {
		return nil, nil, err
	}

	// BoltDB is opened (and closed on failure) by btcwallet's own
	// loader, so there is nothing for the cleanup func to release.
	if cfg.DBBackend != DBBackendSQLite {
		return []btcwallet.LoaderOption{
			btcwallet.LoaderWithLocalWalletDB(
				cfg.DBDir, false, boltWalletDBTimeout,
			),
		}, func() {}, nil
	}

	// Build the DSN before anything touches the disk, so a path the
	// driver cannot represent leaves nothing behind.
	driverName, dsn, err := sqliteWalletDBDSN(dbPath)
	if err != nil {
		return nil, nil, err
	}

	if err := os.MkdirAll(cfg.DBDir, 0700); err != nil {
		return nil, nil, fmt.Errorf("create wallet database dir: %w",
			err)
	}

	// Create the database file ourselves before SQLite gets a chance
	// to. SQLite would create it with 0666 & ~umask, which under the
	// common umask 0022 leaves the encrypted seed, the key state and
	// the full transaction history world-readable; btcwallet passes
	// 0600 explicitly for its BoltDB file and the two backends should
	// not differ here. Doing it up front rather than chmod'ing
	// afterwards also covers the -wal and -shm sidecars, which SQLite
	// creates with the mode of the main database file.
	if err := createWalletDBFile(dbPath); err != nil {
		return nil, nil, err
	}

	walletDB, err := openSQLWalletDB(context.Background(), driverName, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open SQLite wallet database: %w",
			err)
	}

	// btcwallet's loader only closes a wallet database it created
	// itself, so a constructor failure after this point would leak
	// this handle, and its lock on the database file, for the process
	// lifetime.
	cleanup := func() {
		_ = walletDB.Close()
	}

	return []btcwallet.LoaderOption{
		btcwallet.LoaderWithExternalWalletDB(walletDB),
	}, cleanup, nil
}

// walletExists reports whether a wallet has already been created in
// the configured backend's database.
//
// The predicate has to be the same one btcwallet's loader will apply,
// because New pairs them: it refuses a seed when a wallet exists and
// lets a seedless open through when one does. btcwallet answers from a
// marker key committed inside the database once the wallet itself
// exists, and for the SQL backends that is not the same as the database
// file being there — openSQLWalletDB creates the file and its schema
// before btcwallet ever runs, so an interrupted first run leaves an
// empty database behind. Answering "exists" for that state would refuse
// the operator's real seed and send the seedless path into
// btcwallet.New, which creates a wallet from a random seed when handed
// no seed of its own.
func walletExists(cfg Config) (bool, error) {
	dbPath, err := walletDBPath(cfg)
	if err != nil {
		return false, err
	}

	// A missing database file settles the question without opening
	// anything, which is what keeps a probe of a fresh directory from
	// creating the very database it is asking about.
	exists, err := walletDBExists(dbPath, cfg.DBBackend)
	if err != nil || !exists {
		return false, err
	}

	opts, cleanup, err := newWalletLoaderOptions(cfg)
	if err != nil {
		return false, err
	}

	// A probe must not hand the wallet database on to anyone, so a
	// handle opened above is closed again here. It would otherwise
	// hold the database's write lock until the process exits, and the
	// real open in New would be the one to fail on it.
	defer cleanup()

	loader, err := btcwallet.NewWalletLoader(
		cfg.ChainParams, cfg.RecoveryWindow, opts...,
	)
	if err != nil {
		return false, err
	}

	return loader.WalletExists()
}

// sqliteWalletDBDSN returns the database/sql driver name and DSN for the
// wallet database at the given path. Both come from the db package, so
// the wallet database runs on the same SQLite driver as every other
// database in the process: modernc.org/sqlite by default, the host C
// library under the sqlite_cgo tag that Android builds set because the
// app sandbox can block modernc's raw syscalls.
func sqliteWalletDBDSN(dbPath string) (string, string, error) {
	// Both drivers split the DSN at its first '?', so one in the path
	// would cut it short: the database would land at a truncated
	// sibling path, outside the directory the caller controls, and
	// open without any of the settings below. The daemon's own SQLite
	// databases share the directory and the limitation, so refusing
	// it here loses nothing that works today.
	if strings.Contains(dbPath, "?") {
		return "", "", fmt.Errorf("wallet database path %q contains "+
			"'?', which SQLite would read as the start of its "+
			"connection options", dbPath)
	}

	return db.SQLiteDSN(db.SQLiteOpenConfig{
		DatabaseFileName: dbPath,
		Pragmas: []db.SQLitePragma{{
			// Hold the database's file lock for as long as the
			// connection is open, the way BoltDB's flock does,
			// so a second process opening the same directory
			// fails with SQLITE_BUSY once busy_timeout runs
			// out instead of deriving the same addresses as
			// the first. It precedes journal_mode so a driver
			// applying pragmas in order enters WAL mode
			// exclusively and keeps the WAL index in heap
			// memory rather than a -shm file. The wallet store
			// uses a single connection, which this mode
			// requires: a second in-process connection to the
			// file would be locked out just the same.
			Name:  "locking_mode",
			Value: "EXCLUSIVE",
		}, {
			// The key/value schema nests buckets through a
			// parent_id self-reference, and relies on the
			// cascade to drop a deleted bucket's children
			// instead of leaving them behind as unreachable
			// rows.
			Name:  "foreign_keys",
			Value: "on",
		}, {
			// WAL lets the wallet's reads proceed against the
			// last committed snapshot while a write
			// transaction is open, and keeps a crash from
			// tearing a commit.
			Name:  "journal_mode",
			Value: "WAL",
		}, {
			// The wallet's seed and key state have no second
			// copy anywhere, so this database does not follow
			// the daemon's configurable synchronous=normal
			// default. FULL is also SQLite's own default; it
			// is set explicitly so the guarantee does not rest
			// on a driver default, and because some drivers
			// lower it to NORMAL on their own when WAL is
			// selected.
			Name:  "synchronous",
			Value: "full",
		}, {
			// fullfsync only matters on Darwin, where a plain
			// fsync does not reach stable storage. The
			// daemon's own SQLite store enables it for exactly
			// that reason, and an embedded daemon on a mobile
			// platform is squarely in that case, so the wallet
			// database must not end up with weaker flush
			// semantics than the databases beside it.
			Name:  "fullfsync",
			Value: "true",
		}, {
			// busy_timeout caps how long SQLite waits for a
			// lock held by another connection to the same
			// file, such as a backup tool, before failing the
			// query.
			Name: "busy_timeout",
			Value: strconv.FormatInt(
				sqlWalletDBBusyTimeout.Milliseconds(), 10,
			),
		}},

		// Take the write lock when a write transaction begins
		// rather than on its first write statement. The wallet
		// reads before it writes within a single transaction —
		// deriving the next address reads the last-used index and
		// then bumps it — and a deferred transaction that tries
		// to upgrade to a writer after another connection
		// committed in the meantime fails with
		// SQLITE_BUSY_SNAPSHOT. That error bypasses the busy
		// handler, so busy_timeout above would not absorb it.
		TxLockImmediate: true,
	})
}

// createWalletDBFile creates the wallet database file with owner-only
// permissions if it does not exist yet. An existing file is left alone,
// including its mode: a database created by an older version stays
// readable rather than being silently tightened underneath a caller
// that may have chosen its own permissions.
func createWalletDBFile(dbPath string) error {
	// The path is the daemon's own wallet database under the
	// configured data directory, not caller-supplied input.
	//
	//nolint:gosec // G304: path is composed from DBDir and a constant
	f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("create wallet database file: %w", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close wallet database file: %w", err)
	}

	return nil
}

// walletDBExists reports whether the given backend's wallet database
// exists at the given path. An unexpected stat failure is returned as an
// error rather than as a missing database: reading, say, a permission
// problem as "no wallet here" would send the caller down the create
// path.
func walletDBExists(path, backend string) (bool, error) {
	info, err := os.Stat(path)
	switch {
	case err == nil:
		// The SQLite file is created empty ahead of the driver
		// opening it, so an open that fails right there, such as
		// on a driver the platform does not support, leaves a
		// zero-length file behind. It holds no database, and
		// counting it would refuse a switch back to BoltDB over a
		// wallet that never existed. BoltDB writes its file
		// non-empty from the start, and btcwallet's own loader
		// counts any file there, so the rule is SQLite's alone.
		return backend != DBBackendSQLite || info.Size() > 0, nil

	case errors.Is(err, os.ErrNotExist):
		return false, nil

	default:
		return false, err
	}
}

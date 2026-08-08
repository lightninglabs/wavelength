//go:build js && wasm

package lwwallet

import (
	"context"
	"fmt"
	"hash/fnv"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	_ "github.com/lightninglabs/go-wasmsqlite"
	"github.com/lightninglabs/wavelength/internal/sqlbase"
	"github.com/lightninglabs/wavelength/internal/wasmhost"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
)

const (
	wasmWalletDBDriverName      = "wasmsqlite"
	wasmWalletDBTablePrefix     = "walletdb"
	wasmWalletDBTimeout         = 30 * time.Second
	wasmWalletDBBusyTimeoutMS   = "30000"
	wasmWalletDBMaxConnections  = 1
	wasmWalletDBFileNamePattern = "/wallet-%016x.db"

	// nodeWalletDBFileName is the wallet store's name on a real filesystem,
	// where the containing directory already distinguishes one wallet from
	// another and the hashed browser name would only hide it.
	nodeWalletDBFileName = "wallet.db"
)

// newWalletLoaderOptions opens btcwallet through an OPFS-backed SQLite
// walletdb implementation for browser builds. The cleanup func closes
// the just-opened database handle: btcwallet's loader only closes
// external databases it fully adopted, so a constructor failure after
// this point would otherwise hold the EXCLUSIVE OPFS lock for the
// page runtime lifetime.
func newWalletLoaderOptions(cfg Config) ([]btcwallet.LoaderOption, func(),
	error) {

	db, err := openWASMWalletDB(cfg.DBDir)
	if err != nil {
		return nil, nil, err
	}

	cleanup := func() {
		_ = db.Close()
	}

	return []btcwallet.LoaderOption{
		btcwallet.LoaderWithExternalWalletDB(db),
	}, cleanup, nil
}

// walletExists reports whether a btcwallet database has already been
// initialized inside the OPFS SQLite store. The probe opens the
// database, checks for the wallet's namespace buckets, and closes it
// again: the store uses EXCLUSIVE locking with a single connection, so
// a handle left open here would block the real open in New.
func walletExists(cfg Config) (bool, error) {
	db, err := openWASMWalletDB(cfg.DBDir)
	if err != nil {
		return false, err
	}
	defer func() {
		_ = db.Close()
	}()

	loader, err := btcwallet.NewWalletLoader(
		cfg.ChainParams, cfg.RecoveryWindow,
		btcwallet.LoaderWithExternalWalletDB(db),
	)
	if err != nil {
		return false, err
	}

	return loader.WalletExists()
}

// openWASMWalletDB opens btcwallet's walletdb on top of the same browser
// SQLite/OPFS driver used by the daemon and swap stores.
func openWASMWalletDB(dbDir string) (walletdb.DB, error) {
	sqlbase.Init(wasmWalletDBMaxConnections)

	cfg := &sqlbase.Config{
		DriverName:      wasmWalletDBDriverName,
		Dsn:             wasmWalletDBDSN(dbDir),
		Timeout:         wasmWalletDBTimeout,
		TableNamePrefix: wasmWalletDBTablePrefix,
		WithTxLevelLock: true,
	}

	var lastErr error
	for attempt := 0; attempt < 25; attempt++ {
		db, err := sqlbase.NewSqlBackend(context.Background(), cfg)
		if err == nil {
			return db, nil
		}
		if !isWASMWalletCantOpen(err) {
			return nil, fmt.Errorf("open OPFS wallet database: %w",
				err)
		}

		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}

	return nil, fmt.Errorf("open OPFS wallet database: %w", lastErr)
}

// wasmWalletDBDSN returns a go-wasmsqlite DSN for btcwallet's SQL walletdb,
// pointed at whichever durable storage the js/wasm host provides.
func wasmWalletDBDSN(dbDir string) string {
	values := url.Values{}
	values.Set("vfs", wasmhost.SQLiteVFS())
	if wasmhost.UnderNode() {
		// The wallet database lives beside the daemon's own databases
		// on a real filesystem, under the name the caller asked for.
		values.Set(
			"file", filepath.Join(dbDir, nodeWalletDBFileName),
		)
	} else {
		values.Set("file", wasmWalletDBFileName(dbDir))
	}
	values.Set("mode", "rwc")
	values.Set("busy_timeout", wasmWalletDBBusyTimeoutMS)

	// WAL survives here only because of the locking_mode=EXCLUSIVE pragma
	// below. No wasm VFS implements xShmMap, so this is SQLite's WAL-without-
	// shared-memory mode, which holds the WAL index on the heap and requires
	// the connection to already be exclusive. The driver applies the pragmas
	// first and then checks the journal mode it actually got, so dropping the
	// exclusive lock would fail the open rather than silently move the wallet
	// onto the rollback journal.
	values.Set("journal_mode", "WAL")

	// The wallet's seed and key state have no second copy anywhere, so an
	// in-memory substitute for the store would be worse than not starting.
	values.Set("require_persistent", "true")

	values.Set(
		"pragma",
		strings.Join(
			[]string{
				"foreign_keys=on",
				"auto_vacuum=incremental",
				"locking_mode=EXCLUSIVE",
			}, ";",
		),
	)

	return values.Encode()
}

// wasmWalletDBFileName maps a native wallet DB directory to a stable
// origin-local OPFS database name. It is a browser concern only; a Node host
// keeps the real directory.
func wasmWalletDBFileName(dbDir string) string {
	normalized := filepath.ToSlash(filepath.Clean(dbDir))
	normalized = strings.TrimSpace(normalized)
	if normalized == "" || normalized == "." {
		normalized = "wallet"
	}

	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(normalized))

	return fmt.Sprintf(wasmWalletDBFileNamePattern, hasher.Sum64())
}

// isWASMWalletCantOpen identifies the SQLite error returned while OPFS still
// holds the wallet database from a just-unloaded page runtime.
func isWASMWalletCantOpen(err error) bool {
	return strings.Contains(err.Error(), "SQLITE_CANTOPEN") ||
		strings.Contains(err.Error(), "unable to open database file")
}

package lwwallet

import (
	"context"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightninglabs/wavelength/internal/sqlbase"
)

const (
	// DBBackendBolt keeps btcwallet's wallet database in the classic
	// BoltDB file under Config.DBDir. It is the default.
	DBBackendBolt = "bolt"

	// DBBackendSQLite keeps btcwallet's wallet database in a SQLite
	// database under Config.DBDir. It lets an embedded daemon hold
	// all of its state in one storage engine, in a directory the host
	// application picks, rather than an mmap'd BoltDB file alongside
	// the SQLite databases every other store already uses.
	//
	// Note that this gives up one property BoltDB provides: its file
	// lock excludes a second OS process, while SQLite in WAL mode
	// does not. Two daemons on one directory stay safe at the storage
	// layer but not at the wallet layer, where both would derive the
	// same addresses and keep their own idea of sync state.
	DBBackendSQLite = "sqlite"
)

const (
	// sqlWalletDBTablePrefix namespaces btcwallet's key/value table
	// inside the wallet database, so the resulting "walletdb_kv"
	// table can coexist with tables owned by other stores.
	sqlWalletDBTablePrefix = "walletdb"

	// sqlWalletDBBusyTimeout is how long SQLite waits for a lock held
	// by another connection to the same database before failing the
	// query.
	sqlWalletDBBusyTimeout = 30 * time.Second

	// sqlWalletDBTimeout is the per-query timeout applied to the
	// wallet database. It has to stay comfortably above
	// sqlWalletDBBusyTimeout: a query that legitimately waits out the
	// busy handler would otherwise race its own context deadline, and
	// the caller would see "context deadline exceeded" instead of the
	// SQLITE_BUSY that names the actual problem.
	sqlWalletDBTimeout = 2 * sqlWalletDBBusyTimeout

	// sqlWalletDBMaxConnections bounds the connection pool sqlbase
	// keeps per DSN. The walletdb emulation funnels its read-write
	// transactions through a single in-process lock anyway, so a
	// second connection would only add contention on the database's
	// own write lock.
	sqlWalletDBMaxConnections = 1
)

// openSQLWalletDB opens, creating it if needed, btcwallet's wallet
// database on the given database/sql driver and DSN. The driver must
// already be registered by the caller. The returned handle is owned by
// the caller and must be closed once the wallet has stopped.
func openSQLWalletDB(ctx context.Context, driverName,
	dsn string) (walletdb.DB, error) {

	// The connection set is process-global and its limit is fixed by
	// the first caller, so this fails rather than hand back a pool
	// wider than the wallet store's single-writer assumption.
	if err := sqlbase.Init(sqlWalletDBMaxConnections); err != nil {
		return nil, err
	}

	return sqlbase.NewSqlBackend(ctx, &sqlbase.Config{
		DriverName:      driverName,
		Dsn:             dsn,
		Timeout:         sqlWalletDBTimeout,
		TableNamePrefix: sqlWalletDBTablePrefix,
		WithTxLevelLock: true,
	})
}

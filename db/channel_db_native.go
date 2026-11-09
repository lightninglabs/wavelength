//go:build !js || !wasm

package db

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
	_ "modernc.org/sqlite"
)

// openChannelBackend uses LND's SQL implementation with the native SQLite
// driver. A legacy Bolt database is copied before channel state is opened.
func openChannelBackend(dataDir string) (kvdb.Backend, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	sqlbase.Init(1)
	dsn := filepath.Join(dataDir, "channel.sqlite") +
		"?_pragma=foreign_keys=on&_pragma=journal_mode=WAL" +
		"&_pragma=synchronous=FULL&_pragma=busy_timeout=30000" +
		"&_txlock=immediate"
	backend, err := sqlbase.NewSqlBackend(
		context.Background(), &sqlbase.Config{
			DriverName: "sqlite", Dsn: dsn,
			Timeout:         30 * time.Second,
			TableNamePrefix: "channel",
			WithTxLevelLock: true,
		},
	)
	if err != nil {
		return nil, err
	}
	if err := migrateChannelBolt(dataDir, backend); err != nil {
		_ = backend.Close()

		return nil, err
	}

	return backend, nil
}

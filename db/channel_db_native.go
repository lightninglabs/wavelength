//go:build !js || !wasm

package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
)

// openChannelBackend uses LND's SQL implementation with the native SQLite
// driver.
func openChannelBackend(dataDir string) (kvdb.Backend, error) {
	if strings.Contains(dataDir, "?") {
		return nil, fmt.Errorf("channel database directory %q "+
			"contains '?'", dataDir)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	sqlbase.Init(1)
	driver, dsn, err := SQLiteDSN(SQLiteOpenConfig{
		DatabaseFileName: filepath.Join(dataDir, "channel.sqlite"),
		Pragmas: []SQLitePragma{
			{Name: "foreign_keys", Value: "on"},
			{Name: "journal_mode", Value: "WAL"},
			{Name: "synchronous", Value: "FULL"},
			{Name: "fullfsync", Value: "true"},
			{Name: "busy_timeout", Value: "30000"},
		},
		TxLockImmediate: true,
	})
	if err != nil {
		return nil, err
	}

	return sqlbase.NewSqlBackend(
		context.Background(), &sqlbase.Config{
			DriverName: driver, Dsn: dsn,
			Timeout:         30 * time.Second,
			TableNamePrefix: "channel",
			WithTxLevelLock: true,
		},
	)
}

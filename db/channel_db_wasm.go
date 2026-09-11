//go:build js && wasm

package db

import (
	"context"
	"fmt"
	"hash/fnv"
	"net/url"
	"path/filepath"
	"time"

	_ "github.com/lightninglabs/go-wasmsqlite"
	"github.com/lightninglabs/wavelength/internal/wasmhost"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
)

// openChannelBackend binds LND's SQL implementation to persistent browser or
// Node storage. Exclusive WAL avoids requiring a shared-memory VFS.
func openChannelBackend(dataDir string) (kvdb.Backend, error) {
	name := filepath.Join(dataDir, "channel.sqlite")
	if !wasmhost.UnderNode() {
		hash := fnv.New64a()
		_, _ = hash.Write(
			[]byte(
				filepath.ToSlash(
					filepath.Clean(dataDir),
				),
			),
		)
		name = fmt.Sprintf("/channels-%016x.db", hash.Sum64())
	}
	values := url.Values{
		"vfs": {
			wasmhost.SQLiteVFS(),
		}, "file": {
			name,
		},
		"mode": {
			"rwc",
		}, "busy_timeout": {
			"30000",
		},
		"journal_mode": {
			"WAL",
		}, "require_persistent": {
			"true",
		},
		"pragma": {
			"foreign_keys=on;locking_mode=EXCLUSIVE;synchronous=FULL",
		},
	}
	sqlbase.Init(1)

	return sqlbase.NewSqlBackend(context.Background(), &sqlbase.Config{
		DriverName: "wasmsqlite", Dsn: values.Encode(),
		Timeout: 30 * time.Second, TableNamePrefix: "channel",
		WithTxLevelLock: true,
	})
}

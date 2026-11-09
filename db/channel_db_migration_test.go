//go:build !js || !wasm

package db

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
	"github.com/stretchr/testify/require"
)

// TestChannelBoltMigration verifies complete copying, rollback on an import
// error, and that reopening SQL never overwrites progress from the old backup.
func TestChannelBoltMigration(t *testing.T) {
	dir := t.TempDir()
	legacy, err := kvdb.GetBoltBackend(&kvdb.BoltBackendConfig{
		DBPath: dir, DBFileName: "channel.db", DBTimeout: time.Second,
	})
	require.NoError(t, err)
	old, err := channeldb.CreateWithBackend(legacy)
	require.NoError(t, err)
	require.NoError(
		t, old.Update(func(tx walletdb.ReadWriteTx) error {
			b, err := tx.CreateTopLevelBucket([]byte("fixture"))
			if err != nil {
				return err
			}
			if err := b.Put([]byte("empty"), []byte{}); err != nil {
				return err
			}
			nested, err := b.CreateBucket([]byte{0, 255})
			if err != nil {
				return err
			}
			if err := nested.SetSequence(17); err != nil {
				return err
			}
			if err := nested.Put(
				[]byte("value"), []byte{1, 0, 255},
			); err != nil {
				return err
			}

			return b.SetSequence(42)
		}, func() {}),
	)
	require.NoError(t, old.Close())
	sqlbase.Init(1)
	target, err := sqlbase.NewSqlBackend(
		context.Background(), &sqlbase.Config{
			DriverName:      "sqlite",
			Dsn:             filepath.Join(dir, "channel.sqlite"),
			TableNamePrefix: "channel",
			WithTxLevelLock: true,
		},
	)
	require.NoError(t, err)
	require.ErrorIs(
		t,
		migrateChannelBolt(
			dir, failingChannelMigration{target},
		),
		errChannelImport,
	)
	require.NoError(t, target.Close())
	backup, err := os.ReadFile(filepath.Join(dir, "channel.db"))
	require.NoError(t, err)

	store, err := OpenChannelDB(dir)
	require.NoError(t, err)
	require.NoError(
		t, store.Update(func(tx walletdb.ReadWriteTx) error {
			b := tx.ReadWriteBucket([]byte("fixture"))
			require.EqualValues(t, 42, b.Sequence())
			require.NotNil(t, b.Get([]byte("empty")))
			nested := b.NestedReadWriteBucket([]byte{0, 255})
			require.EqualValues(t, 17, nested.Sequence())
			require.Equal(
				t, []byte{1, 0, 255},
				nested.Get(
					[]byte("value"),
				),
			)

			return nested.Put([]byte("value"), []byte("new-state"))
		}, func() {}),
	)
	require.NoError(t, store.Close())

	store, err = OpenChannelDB(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.NoError(
		t, store.View(func(tx walletdb.ReadTx) error {
			b := tx.ReadBucket([]byte("fixture")).NestedReadBucket(
				[]byte{0, 255},
			)
			require.Equal(
				t, []byte("new-state"),
				b.Get(
					[]byte("value"),
				),
			)

			return nil
		}, func() {}),
	)
	after, err := os.ReadFile(filepath.Join(dir, "channel.db"))
	require.NoError(t, err)
	require.Equal(t, backup, after)
}

var errChannelImport = errors.New("interrupted channel import")

// failingChannelMigration interrupts the import before its SQL commit.
type failingChannelMigration struct {
	walletdb.DB
}

// Update rolls back a fully copied transaction to exercise durable retry.
func (f failingChannelMigration) Update(fn func(walletdb.ReadWriteTx) error,
	reset func()) error {

	return f.DB.Update(func(tx walletdb.ReadWriteTx) error {
		if err := fn(tx); err != nil {
			return err
		}

		return errChannelImport
	}, reset)
}

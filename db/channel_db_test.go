package db

import (
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/require"
)

// TestChannelSQLReopen checks persistence through LND's actual SQL-backed
// channel database, including the nested-bucket reads LND relies on.
func TestChannelSQLReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenChannelDB(dir)
	require.NoError(t, err)
	require.NoError(
		t, store.Update(func(tx walletdb.ReadWriteTx) error {
			bucket, err := tx.CreateTopLevelBucket(
				[]byte("test-channel"),
			)
			if err != nil {
				return err
			}
			_, err = bucket.CreateBucket([]byte("nested"))
			if err != nil {
				return err
			}

			return bucket.Put(
				[]byte("commitment"), []byte("signed-state"),
			)
		}, func() {}),
	)
	require.NoError(t, store.Close())

	store, err = OpenChannelDB(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.NoError(
		t, store.View(func(tx walletdb.ReadTx) error {
			bucket := tx.ReadBucket([]byte("test-channel"))
			require.NotNil(t, bucket)
			require.Equal(
				t, []byte("signed-state"),
				bucket.Get(
					[]byte("commitment"),
				),
			)
			require.Nil(t, bucket.Get([]byte("nested")))
			require.NotNil(
				t,
				bucket.NestedReadBucket(
					[]byte("nested"),
				),
			)

			return nil
		}, func() {}),
	)
	_, err = store.ChannelStateDB().FetchAllChannels()
	require.NoError(t, err)
}

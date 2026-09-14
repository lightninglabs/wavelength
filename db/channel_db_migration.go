//go:build !js || !wasm

package db

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/kvdb"
)

var channelSQLMarker = []byte("wavelength-channel-sql")

// migrateChannelBolt copies and verifies all buckets and sequences in one SQL
// transaction. The marker commits with the copy, making interrupted imports
// retryable. The original Bolt file remains untouched as an offline backup;
// it must never be used to resume channels after SQL has become authoritative.
func migrateChannelBolt(dataDir string, target kvdb.Backend) error {
	var complete bool
	err := target.View(func(tx walletdb.ReadTx) error {
		complete = tx.ReadBucket(channelSQLMarker) != nil

		return nil
	}, func() { complete = false })
	if err != nil || complete {
		return err
	}
	var source kvdb.Backend
	_, err = os.Stat(filepath.Join(dataDir, "channel.db"))
	switch {
	case err == nil:
		source, err = kvdb.GetBoltBackend(&kvdb.BoltBackendConfig{
			DBPath: dataDir, DBFileName: "channel.db",
			DBTimeout: 30 * time.Second, ReadOnly: true,
		})
		if err != nil {
			return fmt.Errorf("open legacy channel database: %w",
				err)
		}
		defer func() {
			_ = source.Close()
		}()

	case !errors.Is(err, os.ErrNotExist):
		return err
	}

	return target.Update(func(dst walletdb.ReadWriteTx) error {
		// Never merge unknown state into an existing destination.
		if err := dst.ForEachBucket(func([]byte) error {
			return fmt.Errorf("unmarked channel SQL database is " +
				"not empty")
		}); err != nil {
			return err
		}
		if source != nil {
			err := source.View(func(src walletdb.ReadTx) error {
				return copyAndVerifyChannelDB(src, dst)
			}, func() {})
			if err != nil {
				return err
			}
		}
		_, err := dst.CreateTopLevelBucket(channelSQLMarker)

		return err
	}, func() {})
}

// copyChannelBucket preserves the complete walletdb bucket tree and sequence.
func copyChannelBucket(src walletdb.ReadBucket,
	dst walletdb.ReadWriteBucket) error {

	if err := dst.SetSequence(src.Sequence()); err != nil {
		return err
	}

	return src.ForEach(func(key, value []byte) error {
		if value != nil {
			return dst.Put(key, value)
		}
		child, err := dst.CreateBucket(key)
		if err != nil {
			return err
		}

		return copyChannelBucket(src.NestedReadBucket(key), child)
	})
}

// channelDBDigest hashes ordered, length-prefixed names, values and sequences.
func channelDBDigest(tx walletdb.ReadTx) ([]byte, error) {
	h := sha256.New()
	err := tx.ForEachBucket(func(name []byte) error {
		_, _ = h.Write([]byte{3})
		hashChannelField(h, name)

		return hashChannelBucket(h, tx.ReadBucket(name))
	})

	return h.Sum(nil), err
}

// hashChannelField keeps adjacent keys and values unambiguous in the digest.
func hashChannelField(h hash.Hash, value []byte) {
	_, _ = h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(value))))
	_, _ = h.Write(value)
}

// hashChannelBucket includes type markers and bucket boundaries in the digest.
func hashChannelBucket(h hash.Hash, bucket walletdb.ReadBucket) error {
	_, _ = h.Write(binary.BigEndian.AppendUint64(nil, bucket.Sequence()))
	err := bucket.ForEach(func(key, value []byte) error {
		_, _ = h.Write([]byte{3})
		hashChannelField(h, key)
		if value != nil {
			_, _ = h.Write([]byte{1})
			hashChannelField(h, value)

			return nil
		}
		_, _ = h.Write([]byte{2})

		return hashChannelBucket(h, bucket.NestedReadBucket(key))
	})
	_, _ = h.Write([]byte{0})

	return err
}

// copyAndVerifyChannelDB verifies the copy before its transaction can commit.
func copyAndVerifyChannelDB(src walletdb.ReadTx,
	dst walletdb.ReadWriteTx) error {

	err := src.ForEachBucket(func(name []byte) error {
		bucket, err := dst.CreateTopLevelBucket(name)
		if err != nil {
			return err
		}

		return copyChannelBucket(src.ReadBucket(name), bucket)
	})
	if err != nil {
		return err
	}
	before, err := channelDBDigest(src)
	if err != nil {
		return err
	}
	after, err := channelDBDigest(dst)
	if err != nil {
		return err
	}
	if !bytes.Equal(before, after) {
		return fmt.Errorf("channel database migration verification " +
			"failed")
	}

	return nil
}

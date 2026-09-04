package sqlbase

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// testBucket is the top-level bucket the tests below operate in.
var testBucket = []byte("top")

// newTestDB opens a SQLite-backed walletdb in a temporary directory,
// configured the way the wallet store configures it: a single
// connection, serialized read-write transactions, foreign keys on so
// nested-bucket deletes cascade.
func newTestDB(t *testing.T) (walletdb.DB, string) {
	t.Helper()

	// The connection set is process-global and first-call-wins, so
	// every test in this package shares the single-connection limit
	// the wallet store asks for.
	Init(1)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	dsn := dbPath +
		"?_pragma=foreign_keys%3Don&_pragma=journal_mode%3DWAL" +
		"&_pragma=busy_timeout%3D5000&_txlock=immediate"

	db, err := NewSqlBackend(context.Background(), &Config{
		DriverName:      "sqlite",
		Dsn:             dsn,
		Timeout:         10 * time.Second,
		TableNamePrefix: "test",
		WithTxLevelLock: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	return db, dbPath
}

// countRows returns the number of rows in the key/value table, read
// through a connection of the test's own so it can see what the
// walletdb interface deliberately cannot: rows that are still present
// but no longer reachable from any bucket.
func countRows(t *testing.T, dbPath string) int {
	t.Helper()

	raw, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, raw.Close())
	}()

	var count int
	require.NoError(
		t, raw.QueryRow("SELECT COUNT(*) FROM test_kv").Scan(&count),
	)

	return count
}

// update runs f in a read-write transaction and requires it to commit.
func update(t *testing.T, db walletdb.DB,
	f func(walletdb.ReadWriteBucket) error) {

	t.Helper()

	require.NoError(
		t, db.Update(func(tx walletdb.ReadWriteTx) error {
			b, err := tx.CreateTopLevelBucket(testBucket)
			if err != nil {
				return err
			}

			return f(b)
		}, func() {}),
	)
}

// TestBucketRoundTrip covers the basic key/value contract: a value put
// in a bucket reads back byte-identical, an absent key reads as nil
// rather than an error, and a deleted key is gone.
func TestBucketRoundTrip(t *testing.T) {
	t.Parallel()

	db, _ := newTestDB(t)

	update(t, db, func(b walletdb.ReadWriteBucket) error {
		require.NoError(t, b.Put([]byte("k"), []byte("v")))
		require.Equal(t, []byte("v"), b.Get([]byte("k")))
		require.Nil(t, b.Get([]byte("absent")))

		// An empty value is a value, not an absent key.
		require.NoError(t, b.Put([]byte("empty"), []byte{}))
		require.NotNil(t, b.Get([]byte("empty")))
		require.Empty(t, b.Get([]byte("empty")))

		require.NoError(t, b.Delete([]byte("k")))
		require.Nil(t, b.Get([]byte("k")))

		return nil
	})

	// The value survives the commit and a fresh transaction.
	require.NoError(
		t, db.View(func(tx walletdb.ReadTx) error {
			b := tx.ReadBucket(testBucket)
			require.NotNil(t, b)
			require.Empty(t, b.Get([]byte("empty")))
			require.Nil(t, b.Get([]byte("k")))

			return nil
		}, func() {}),
	)
}

// TestNestedBucketCascade asserts a nested bucket's contents disappear
// with it, physically and not just logically. DeleteNestedBucket
// removes only the bucket's own row and leans on the schema's
// ON DELETE CASCADE for the rest, and the difference is invisible
// through the walletdb interface: orphaned children keep pointing at
// the deleted row's id, so a recreated bucket gets a new id and reads
// empty either way. The row count is what distinguishes a cascade that
// fired from one that silently did not.
func TestNestedBucketCascade(t *testing.T) {
	t.Parallel()

	db, dbPath := newTestDB(t)

	update(t, db, func(b walletdb.ReadWriteBucket) error {
		nested, err := b.CreateBucket([]byte("nested"))
		require.NoError(t, err)
		require.NoError(t, nested.Put([]byte("k"), []byte("v")))

		deeper, err := nested.CreateBucket([]byte("deeper"))
		require.NoError(t, err)
		require.NoError(t, deeper.Put([]byte("k"), []byte("v")))

		return nil
	})

	update(t, db, func(b walletdb.ReadWriteBucket) error {
		require.NotNil(t, b.NestedReadBucket([]byte("nested")))
		require.NoError(t, b.DeleteNestedBucket([]byte("nested")))
		require.Nil(t, b.NestedReadBucket([]byte("nested")))

		return nil
	})

	// Only the top-level bucket's own row is left; the nested bucket,
	// the bucket below it and both their keys are gone.
	require.Equal(t, 1, countRows(t, dbPath))

	update(t, db, func(b walletdb.ReadWriteBucket) error {
		nested, err := b.CreateBucket([]byte("nested"))
		require.NoError(t, err)
		require.Nil(t, nested.Get([]byte("k")))
		require.Nil(t, nested.NestedReadBucket([]byte("deeper")))

		return nil
	})
}

// TestForAll covers the optimized bucket walk: it must visit every key
// in ascending key order, and a callback error must abort the walk and
// surface unchanged.
func TestForAll(t *testing.T) {
	t.Parallel()

	db, _ := newTestDB(t)

	keys := []string{"a", "b", "c", "d"}
	update(t, db, func(b walletdb.ReadWriteBucket) error {
		// Insert out of order, so an implementation that returned
		// insertion order would fail below.
		for _, k := range []string{"c", "a", "d", "b"} {
			require.NoError(t, b.Put([]byte(k), []byte(k)))
		}

		return nil
	})

	require.NoError(
		t, db.View(func(tx walletdb.ReadTx) error {
			var seen []string
			err := kvdb.ForAll(
				tx.ReadBucket(testBucket),
				func(k, v []byte) error {
					require.Equal(t, k, v)
					seen = append(seen, string(k))

					return nil
				},
			)
			require.NoError(t, err)
			require.Equal(t, keys, seen)

			return nil
		}, func() {}),
	)

	// A callback error aborts the walk and is returned as-is.
	sentinel := errors.New("stop walking")
	err := db.View(func(tx walletdb.ReadTx) error {
		var calls int

		return kvdb.ForAll(
			tx.ReadBucket(testBucket),
			func(k, v []byte) error {
				calls++
				require.Equal(t, 1, calls)

				return sentinel
			},
		)
	}, func() {})
	require.ErrorIs(t, err, sentinel)

	// Aborting the walk must leave the database usable. The pool has
	// a single connection, so a result set that outlived its
	// transaction would starve every later query.
	require.NoError(
		t, db.View(func(tx walletdb.ReadTx) error {
			require.Equal(
				t, []byte("a"),
				tx.ReadBucket(
					testBucket).Get([]byte("a")),
			)

			return nil
		}, func() {}),
	)
}

// TestCursor covers the cursor's ordering contract, which buckets like
// the wallet's key-derivation state depend on: seeking lands on the
// first key at or after the target, and stepping past either end
// returns nil rather than wrapping around.
func TestCursor(t *testing.T) {
	t.Parallel()

	db, _ := newTestDB(t)

	update(t, db, func(b walletdb.ReadWriteBucket) error {
		for _, k := range []string{"a", "c", "e"} {
			require.NoError(t, b.Put([]byte(k), []byte(k)))
		}

		return nil
	})

	require.NoError(t, db.View(func(tx walletdb.ReadTx) error {
		c := tx.ReadBucket(testBucket).ReadCursor()

		k, v := c.First()
		require.Equal(t, "a", string(k))
		require.Equal(t, "a", string(v))

		k, _ = c.Next()
		require.Equal(t, "c", string(k))

		k, _ = c.Prev()
		require.Equal(t, "a", string(k))

		// Stepping off the front yields nothing.
		k, _ = c.Prev()
		require.Nil(t, k)

		k, _ = c.Last()
		require.Equal(t, "e", string(k))

		k, _ = c.Next()
		require.Nil(t, k)

		// Seek lands on the first key at or after the target.
		k, _ = c.Seek([]byte("b"))
		require.Equal(t, "c", string(k))

		k, _ = c.Seek([]byte("c"))
		require.Equal(t, "c", string(k))

		// Seeking past the last key yields nothing.
		k, _ = c.Seek([]byte("z"))
		require.Nil(t, k)

		return nil
	}, func() {}))

	// Cursor deletion removes the key the cursor sits on.
	update(t, db, func(b walletdb.ReadWriteBucket) error {
		c := b.ReadWriteCursor()
		k, _ := c.First()
		require.Equal(t, "a", string(k))
		require.NoError(t, c.Delete())

		return nil
	})

	require.NoError(
		t, db.View(func(tx walletdb.ReadTx) error {
			b := tx.ReadBucket(testBucket)
			require.Nil(t, b.Get([]byte("a")))
			k, _ := b.ReadCursor().First()
			require.Equal(t, "c", string(k))

			return nil
		}, func() {}),
	)
}

// TestUpdateRollback asserts a failed transaction leaves nothing
// behind: everything the callback wrote before erroring is rolled back.
func TestUpdateRollback(t *testing.T) {
	t.Parallel()

	db, _ := newTestDB(t)

	update(t, db, func(b walletdb.ReadWriteBucket) error {
		return b.Put([]byte("committed"), []byte("v"))
	})

	sentinel := errors.New("abort")
	err := db.Update(func(tx walletdb.ReadWriteTx) error {
		b, err := tx.CreateTopLevelBucket(testBucket)
		require.NoError(t, err)
		require.NoError(t, b.Put([]byte("rolled-back"), []byte("v")))

		return sentinel
	}, func() {})
	require.ErrorIs(t, err, sentinel)

	require.NoError(
		t, db.View(func(tx walletdb.ReadTx) error {
			b := tx.ReadBucket(testBucket)
			require.Equal(
				t, []byte("v"),
				b.Get(
					[]byte("committed"),
				),
			)
			require.Nil(t, b.Get([]byte("rolled-back")))

			return nil
		}, func() {}),
	)
}

package indexer

import (
	"errors"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/neutrino/cache"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// TestVTXOTreeCacheCachesSuccessfulLoads verifies that the store-level cache
// aliases immutable decoded trees by round and batch output.
func TestVTXOTreeCacheCachesSuccessfulLoads(t *testing.T) {
	t.Parallel()

	treeCache := newVTXOTreeCache()
	key := subtreeTreeKey{
		roundIDHex: "round",
		batchIdx:   2,
	}
	tree := &tree.Tree{}

	_, err := treeCache.get(key)
	require.True(t, errors.Is(err, cache.ErrElementNotFound))

	err = treeCache.put(key, tree)
	require.NoError(t, err)

	got, err := treeCache.get(key)
	require.NoError(t, err)
	require.Same(t, tree, got)
}

// TestVTXOTreeCacheEvictsLeastRecentlyUsed verifies that the decoded tree
// cache is bounded and refreshes LRU order on reads.
func TestVTXOTreeCacheEvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()

	treeCache := newVTXOTreeCacheWithLimit(2)
	key1 := subtreeTreeKey{
		roundIDHex: "round-1",
		batchIdx:   1,
	}
	key2 := subtreeTreeKey{
		roundIDHex: "round-2",
		batchIdx:   2,
	}
	key3 := subtreeTreeKey{
		roundIDHex: "round-3",
		batchIdx:   3,
	}
	tree1 := &tree.Tree{}
	tree2 := &tree.Tree{}
	tree3 := &tree.Tree{}

	err := treeCache.put(key1, tree1)
	require.NoError(t, err)
	err = treeCache.put(key2, tree2)
	require.NoError(t, err)

	got, err := treeCache.get(key1)
	require.NoError(t, err)
	require.Same(t, tree1, got)

	err = treeCache.put(key3, tree3)
	require.NoError(t, err)

	_, err = treeCache.get(key2)
	require.True(t, errors.Is(err, cache.ErrElementNotFound))

	got, err = treeCache.get(key1)
	require.NoError(t, err)
	require.Same(t, tree1, got)

	got, err = treeCache.get(key3)
	require.NoError(t, err)
	require.Same(t, tree3, got)
}

// TestSQLCStoreExecReadTxSharesTreeCache verifies that transactional query
// stores reuse the parent store's immutable tree cache.
func TestSQLCStoreExecReadTxSharesTreeCache(t *testing.T) {
	t.Parallel()

	sqlDB := db.NewTestDB(t)
	store := db.NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	sqlcStore := NewSQLCStore(
		store.Queries, WithBatchedQuerier(store),
	)

	err := sqlcStore.ExecReadTx(t.Context(), func(txStore Store) error {
		txSQLCStore, ok := txStore.(*SQLCStore)
		require.True(t, ok)
		require.Same(t, sqlcStore.treeCache, txSQLCStore.treeCache)

		return nil
	})
	require.NoError(t, err)
}

package db

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightninglabs/neutrino/cache"
	"github.com/stretchr/testify/require"
)

// fakeAncestryStore is a minimal in-memory ancestryStore used to
// exercise the defensive checks in upsertAncestryPaths without
// involving the real sqlc layer.
type fakeAncestryStore struct {
	inserts []sqlc.InsertVTXOAncestryPathParams
	deleted bool
}

func (f *fakeAncestryStore) InsertVTXOAncestryPath(_ context.Context,
	arg sqlc.InsertVTXOAncestryPathParams) error {

	f.inserts = append(f.inserts, arg)

	return nil
}

func (f *fakeAncestryStore) DeleteVTXOAncestryPaths(_ context.Context,
	_ sqlc.DeleteVTXOAncestryPathsParams) error {

	f.deleted = true

	return nil
}

func (f *fakeAncestryStore) ListVTXOAncestryPaths(_ context.Context,
	_ sqlc.ListVTXOAncestryPathsParams) ([]sqlc.VtxoAncestryPath, error) {

	return nil, nil
}

func (f *fakeAncestryStore) ListLiveVTXOAncestryPaths(_ context.Context) (
	[]sqlc.VtxoAncestryPath, error) {

	return nil, nil
}

func (f *fakeAncestryStore) ListVTXOAncestryPathsByStatus(_ context.Context,
	_ int32) ([]sqlc.VtxoAncestryPath, error) {

	return nil, nil
}

func (f *fakeAncestryStore) ListUnspentVTXOAncestryPaths(_ context.Context) (
	[]sqlc.VtxoAncestryPath, error) {

	return nil, nil
}

// TestUpsertAncestryPathsRejectsDuplicateCommitment ensures the Go-
// side defensive check fires before any DB row is touched when the
// supplied slice carries two entries with the same commitment txid.
// The schema-level UNIQUE would also reject this, but the in-Go
// check produces a clearer error and avoids a half-applied delete.
func TestUpsertAncestryPathsRejectsDuplicateCommitment(t *testing.T) {
	t.Parallel()

	commit := chainhash.Hash{0xaa}
	ancestry := []vtxo.Ancestry{
		{
			CommitmentTxID: commit,
		},
		{
			CommitmentTxID: commit,
		},
	}

	store := &fakeAncestryStore{}
	err := upsertAncestryPaths(
		t.Context(), store, make([]byte, 32), 0, ancestry,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate commitment_txid")
	require.False(t, store.deleted, "delete must not run before validation")
	require.Empty(t, store.inserts)
}

// TestUpsertAncestryPathsRejectsTooManyRows ensures the Go-side row-
// count cap fires before any DB row is touched. Mirrors the schema
// CHECK on path_order < 64.
func TestUpsertAncestryPathsRejectsTooManyRows(t *testing.T) {
	t.Parallel()

	ancestry := make([]vtxo.Ancestry, maxAncestryRowsPerVTXO+1)
	for i := range ancestry {
		// Distinct commitment txids so the duplicate check
		// would not fire first.
		var h chainhash.Hash
		h[0] = byte(i + 1)
		ancestry[i] = vtxo.Ancestry{CommitmentTxID: h}
	}

	store := &fakeAncestryStore{}
	err := upsertAncestryPaths(
		t.Context(), store, make([]byte, 32), 0, ancestry,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "max")
	require.False(t, store.deleted, "delete must not run before validation")
	require.Empty(t, store.inserts)
}

// TestUpsertAncestryPathsAcceptsValid sanity-checks that a well-
// formed slice with distinct commitments still inserts every row,
// and that path_order is a contiguous run starting at 0.
func TestUpsertAncestryPathsAcceptsValid(t *testing.T) {
	t.Parallel()

	ancestry := []vtxo.Ancestry{
		{
			CommitmentTxID: chainhash.Hash{
				0x01,
			},
		},
		{
			CommitmentTxID: chainhash.Hash{
				0x02,
			},
		},
		{
			CommitmentTxID: chainhash.Hash{
				0x03,
			},
		},
	}

	store := &fakeAncestryStore{}
	err := upsertAncestryPaths(
		t.Context(), store, make([]byte, 32), 0, ancestry,
	)
	require.NoError(t, err)
	require.True(t, store.deleted)
	require.Len(t, store.inserts, len(ancestry))

	for i, ins := range store.inserts {
		require.Equal(
			t, int32(i), ins.PathOrder,
			"path_order must be contiguous 0..N-1",
		)
	}
}

// TestAncestryTreeCacheEvictsLeastRecentlyUsed verifies the decoded tree cache
// is bounded and refreshes LRU order on reads.
func TestAncestryTreeCacheEvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()

	treeCache := newAncestryTreeCacheWithLimit(2)
	key1 := sha256.Sum256([]byte("tree-1"))
	key2 := sha256.Sum256([]byte("tree-2"))
	key3 := sha256.Sum256([]byte("tree-3"))
	tree1 := &tree.Tree{}
	tree2 := &tree.Tree{}
	tree3 := &tree.Tree{}

	_, err := treeCache.trees.Put(
		key1, &ancestryTreeCacheValue{
			tree: tree1,
		},
	)
	require.NoError(t, err)
	_, err = treeCache.trees.Put(
		key2, &ancestryTreeCacheValue{
			tree: tree2,
		},
	)
	require.NoError(t, err)

	got, err := treeCache.trees.Get(key1)
	require.NoError(t, err)
	require.Same(t, tree1, got.tree)

	_, err = treeCache.trees.Put(
		key3, &ancestryTreeCacheValue{
			tree: tree3,
		},
	)
	require.NoError(t, err)

	_, err = treeCache.trees.Get(key2)
	require.True(t, errors.Is(err, cache.ErrElementNotFound))

	got, err = treeCache.trees.Get(key1)
	require.NoError(t, err)
	require.Same(t, tree1, got.tree)

	got, err = treeCache.trees.Get(key3)
	require.NoError(t, err)
	require.Same(t, tree3, got.tree)
}

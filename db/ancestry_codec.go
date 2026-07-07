package db

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightninglabs/neutrino/cache"
	"github.com/lightninglabs/neutrino/cache/lru"
)

// ancestry_codec.go converts between vtxo.Ancestry slices and the
// vtxo_ancestry_paths normalized side table introduced by migration
// 000009.
//
// Storing ancestry paths in a side table (rather than inline as a single
// blob on the vtxos row) keeps routine queries (ListUnspentVTXOs,
// GetVTXO) cheap and only joins ancestry data when the unroller actually
// needs it. See 000004_vtxos.up.sql for the schema rationale.

// ancestryStore is the narrow query surface used to manage the
// vtxo_ancestry_paths side table. RoundStore satisfies this interface,
// as do test fakes.
type ancestryStore interface {
	InsertVTXOAncestryPath(ctx context.Context,
		arg sqlc.InsertVTXOAncestryPathParams) error

	DeleteVTXOAncestryPaths(ctx context.Context,
		arg sqlc.DeleteVTXOAncestryPathsParams) error

	ListVTXOAncestryPaths(ctx context.Context,
		arg sqlc.ListVTXOAncestryPathsParams) (
		[]sqlc.VtxoAncestryPath,
		error,
	)

	ListLiveVTXOAncestryPaths(ctx context.Context) (
		[]sqlc.VtxoAncestryPath,
		error,
	)

	ListVTXOAncestryPathsByStatus(ctx context.Context,
		status int32) ([]sqlc.VtxoAncestryPath, error)

	ListUnspentVTXOAncestryPaths(ctx context.Context) (
		[]sqlc.VtxoAncestryPath, error)
}

// maxAncestryRowsPerVTXO bounds the number of ancestry rows a single
// VTXO can persist. Mirrors the schema-level CHECK on path_order; the
// Go-side check fails earlier with a clearer error than a sqlite/pg
// constraint violation.
const maxAncestryRowsPerVTXO = 64

// maxAncestryTreeCacheEntries bounds the process-local decoded ancestry
// tree cache by entry count. Trees are immutable once committed, so
// eviction is opportunistic: a miss simply re-decodes the tree from durable
// storage.
const maxAncestryTreeCacheEntries = 4096

type ancestryTreeCacheValue struct {
	tree *tree.Tree
}

func (v *ancestryTreeCacheValue) Size() (uint64, error) {
	return 1, nil
}

type ancestryTreeCache struct {
	trees *lru.Cache[[sha256.Size]byte, *ancestryTreeCacheValue]
}

// newAncestryTreeCache creates a process-local decode cache for finalized
// VTXO ancestry trees. Tree paths are immutable once committed; callers must
// treat cached *tree.Tree values as read-only.
func newAncestryTreeCache() *ancestryTreeCache {
	return newAncestryTreeCacheWithLimit(maxAncestryTreeCacheEntries)
}

func newAncestryTreeCacheWithLimit(maxEntries int) *ancestryTreeCache {
	if maxEntries <= 0 {
		return &ancestryTreeCache{}
	}

	return &ancestryTreeCache{
		trees: lru.NewCache[
			[sha256.Size]byte, *ancestryTreeCacheValue,
		](
			uint64(maxEntries),
		),
	}
}

func (c *ancestryTreeCache) getOrDecode(treePath []byte) (*tree.Tree, error) {
	if c == nil || c.trees == nil {
		return DeserializeTree(treePath)
	}

	key := sha256.Sum256(treePath)

	cached, err := c.trees.Get(key)
	switch {
	case err == nil:
		return cached.tree, nil

	case !errors.Is(err, cache.ErrElementNotFound):
		return nil, fmt.Errorf("get ancestry tree cache: %w", err)
	}

	t, err := DeserializeTree(treePath)
	if err != nil {
		return nil, err
	}

	_, err = c.trees.Put(key, &ancestryTreeCacheValue{tree: t})
	if err != nil {
		return nil, fmt.Errorf("put ancestry tree cache: %w", err)
	}

	return t, nil
}

// upsertAncestryPaths replaces the persisted ancestry rows for one VTXO
// with the supplied slice. Must be called inside the same transaction
// that wrote (or upserted) the parent vtxos row to preserve referential
// integrity.
//
// An empty slice deletes any existing rows but inserts none. This
// matches the round-create path where the VTXO manager has not yet
// filled in finalized lineage.
//
// The function performs three defensive checks before any row is
// inserted: the slice is at most maxAncestryRowsPerVTXO long, every
// CommitmentTxID is pairwise distinct, and (implicitly, by deriving
// path_order from the loop index) path_order is a contiguous run
// starting at 0. These constraints are also enforced at the schema
// level (UNIQUE on commitment_txid, CHECK on path_order), but
// surfacing them in Go means a future caller bypassing
// BuildIncomingVTXODescriptor sees a clear error rather than a
// constraint violation hidden behind sqlc plumbing.
func upsertAncestryPaths(ctx context.Context, q ancestryStore,
	outpointHash []byte, outpointIndex int32,
	ancestry []vtxo.Ancestry) error {

	if len(ancestry) > maxAncestryRowsPerVTXO {
		return fmt.Errorf("ancestry has %d rows, max %d", len(ancestry),
			maxAncestryRowsPerVTXO)
	}

	seen := make(map[chainhash.Hash]struct{}, len(ancestry))
	for i, a := range ancestry {
		if _, dup := seen[a.CommitmentTxID]; dup {
			return fmt.Errorf("ancestry[%d] duplicate "+
				"commitment_txid %s", i, a.CommitmentTxID)
		}
		seen[a.CommitmentTxID] = struct{}{}
	}

	err := q.DeleteVTXOAncestryPaths(
		ctx, sqlc.DeleteVTXOAncestryPathsParams{
			VtxoOutpointHash:  outpointHash,
			VtxoOutpointIndex: outpointIndex,
		},
	)
	if err != nil {
		return fmt.Errorf("delete ancestry paths: %w", err)
	}

	for i, a := range ancestry {
		var treePath []byte
		if a.TreePath != nil {
			data, err := SerializeTree(a.TreePath)
			if err != nil {
				return fmt.Errorf("serialize ancestry[%d] "+
					"tree: %w", i, err)
			}

			treePath = data
		}

		err := q.InsertVTXOAncestryPath(
			ctx, sqlc.InsertVTXOAncestryPathParams{
				VtxoOutpointHash:  outpointHash,
				VtxoOutpointIndex: outpointIndex,
				PathOrder:         int32(i),
				CommitmentTxid:    a.CommitmentTxID[:],
				TreePath:          treePath,
				TreeDepth:         int32(a.TreeDepth),
				InputIndices: encodeUint32SliceBE(
					a.InputIndices,
				),
			},
		)
		if err != nil {
			return fmt.Errorf("insert ancestry path[%d]: %w", i,
				err)
		}
	}

	return nil
}

// loadAncestryPaths reads the persisted ancestry rows for one VTXO and
// reconstructs the vtxo.Ancestry slice. Rows are returned in path_order.
//
// Returns (nil, nil) when no ancestry rows exist. Callers can treat this
// as "ancestry not yet resolved" — the descriptor surfaces a zero-length
// Ancestry and downstream consumers (unroller, expiry) handle it
// according to their own contracts.
func loadAncestryPaths(ctx context.Context, q ancestryStore,
	outpointHash []byte, outpointIndex int32) ([]vtxo.Ancestry, error) {

	return loadAncestryPathsWithCache(
		ctx, q, outpointHash, outpointIndex, nil,
	)
}

func loadAncestryPathsWithCache(ctx context.Context, q ancestryStore,
	outpointHash []byte, outpointIndex int32,
	cache *ancestryTreeCache) ([]vtxo.Ancestry, error) {

	rows, err := q.ListVTXOAncestryPaths(
		ctx, sqlc.ListVTXOAncestryPathsParams{
			VtxoOutpointHash:  outpointHash,
			VtxoOutpointIndex: outpointIndex,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("list ancestry paths: %w", err)
	}

	if len(rows) == 0 {
		return nil, nil
	}

	out := make([]vtxo.Ancestry, 0, len(rows))
	for i, row := range rows {
		entry, err := ancestryRowToDomain(row, cache)
		if err != nil {
			return nil, fmt.Errorf("decode ancestry row[%d]: %w", i,
				err)
		}

		out = append(out, entry)
	}

	return out, nil
}

// groupAncestryRows decodes a flat slice of VtxoAncestryPath rows
// (returned by one of the batched list queries) into a per-outpoint
// map of vtxo.Ancestry slices. Rows are expected to be ordered by
// (outpoint_hash, outpoint_index, path_order), so each per-outpoint
// slice preserves the indexer's chosen path order.
//
// This is the read-side companion to upsertAncestryPaths and replaces
// the per-row ListVTXOAncestryPaths call that used to drive the
// list-paths N+1 query: callers issue one batched ListLiveVTXOs (or
// equivalent) plus one batched ancestry query and group with this
// helper, instead of one ancestry query per VTXO row.
func groupAncestryRows(rows []sqlc.VtxoAncestryPath) (
	map[wire.OutPoint][]vtxo.Ancestry, error) {

	return groupAncestryRowsWithCache(rows, nil)
}

func groupAncestryRowsWithCache(rows []sqlc.VtxoAncestryPath,
	cache *ancestryTreeCache) (map[wire.OutPoint][]vtxo.Ancestry, error) {

	if len(rows) == 0 {
		return nil, nil
	}

	out := make(map[wire.OutPoint][]vtxo.Ancestry)
	for i, row := range rows {
		entry, err := ancestryRowToDomain(row, cache)
		if err != nil {
			return nil, fmt.Errorf("decode ancestry row[%d]: %w", i,
				err)
		}

		var key wire.OutPoint
		if len(row.VtxoOutpointHash) != len(key.Hash) {
			return nil, fmt.Errorf("ancestry row[%d] outpoint "+
				"hash length %d, want %d", i,
				len(row.VtxoOutpointHash), len(key.Hash))
		}
		copy(key.Hash[:], row.VtxoOutpointHash)
		key.Index = uint32(row.VtxoOutpointIndex)

		out[key] = append(out[key], entry)
	}

	return out, nil
}

// ancestryRowToDomain decodes one sqlc VtxoAncestryPath row into
// vtxo.Ancestry, including deserializing the embedded tree fragment.
func ancestryRowToDomain(row sqlc.VtxoAncestryPath,
	cache *ancestryTreeCache) (vtxo.Ancestry, error) {

	var entry vtxo.Ancestry

	if len(row.CommitmentTxid) != len(entry.CommitmentTxID) {
		return vtxo.Ancestry{}, fmt.Errorf("ancestry row "+
			"commitment_txid length %d, want %d",
			len(row.CommitmentTxid), len(entry.CommitmentTxID))
	}
	copy(entry.CommitmentTxID[:], row.CommitmentTxid)

	entry.TreeDepth = uint32(row.TreeDepth)

	indices, err := decodeUint32SliceBE(row.InputIndices)
	if err != nil {
		return vtxo.Ancestry{}, fmt.Errorf("decode input_indices: %w",
			err)
	}
	entry.InputIndices = indices

	if len(row.TreePath) > 0 {
		t, err := cache.getOrDecode(row.TreePath)
		if err != nil {
			return vtxo.Ancestry{}, fmt.Errorf("deserialize "+
				"tree: %w", err)
		}

		entry.TreePath = t
	}

	return entry, nil
}

// encodeUint32SliceBE encodes a slice of uint32 as a 4-byte big-endian
// length followed by 4-byte big-endian elements. An empty slice encodes
// to four zero bytes (count=0). Used for the input_indices column.
func encodeUint32SliceBE(values []uint32) []byte {
	out := make([]byte, 4+4*len(values))

	count := uint32(len(values))
	out[0] = byte(count >> 24)
	out[1] = byte(count >> 16)
	out[2] = byte(count >> 8)
	out[3] = byte(count)

	for i, v := range values {
		off := 4 + 4*i
		out[off] = byte(v >> 24)
		out[off+1] = byte(v >> 16)
		out[off+2] = byte(v >> 8)
		out[off+3] = byte(v)
	}

	return out
}

// decodeUint32SliceBE inverts encodeUint32SliceBE. An empty input decodes
// to a nil slice; a count whose implied size disagrees with the input
// length is an error.
func decodeUint32SliceBE(raw []byte) ([]uint32, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	if len(raw) < 4 {
		return nil, fmt.Errorf("uint32 slice missing length prefix "+
			"(got %d bytes)", len(raw))
	}

	count := uint32(raw[0])<<24 | uint32(raw[1])<<16 |
		uint32(raw[2])<<8 | uint32(raw[3])

	// Compute the implied size in uint64 so a malicious count cannot
	// wrap int(count)*4 on 32-bit platforms and slip past the bounds
	// check. A wrap-around would crash the make() call below with an
	// out-of-memory or panic the runtime; treat any mismatch as a
	// hard decode error.
	implied := 4 + uint64(count)*4
	if uint64(len(raw)) != implied {
		return nil, fmt.Errorf("uint32 slice length mismatch: count "+
			"%d implies %d bytes, got %d", count, implied, len(raw))
	}

	out := make([]uint32, count)
	for i := range out {
		off := 4 + 4*i
		out[i] = uint32(raw[off])<<24 | uint32(raw[off+1])<<16 |
			uint32(raw[off+2])<<8 | uint32(raw[off+3])
	}

	return out, nil
}

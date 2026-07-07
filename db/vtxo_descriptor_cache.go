package db

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/neutrino/cache"
	"github.com/lightninglabs/neutrino/cache/lru"
)

// maxVTXODescriptorCacheEntries bounds the process-local decoded descriptor
// cache by entry count. The derived script material is immutable per
// outpoint, so eviction is opportunistic: a miss simply re-derives from the
// row.
const maxVTXODescriptorCacheEntries = 8192

// vtxoDescriptorCacheValue holds the expensive derived parts of one VTXO row
// that never change for a given outpoint: the parsed public keys, the
// reconstructed taproot script, and the policy-resolved relative expiry.
// Profiling showed this derivation (secp256k1 point math in
// StandardTapScript, policy template decode, pubkey parsing) dominating the
// hot listing paths, recomputed from scratch for every row on every call.
//
// All reference fields are shared across cache hits and MUST be treated as
// read-only by callers, exactly like the decoded trees in ancestryTreeCache.
type vtxoDescriptorCacheValue struct {
	clientPubkey   *btcec.PublicKey
	operatorPubkey *btcec.PublicKey
	policyTemplate []byte
	tapscript      *waddrmgr.Tapscript
	relativeExpiry uint32
}

// Size implements the lru.Value interface. Every entry counts as one unit so
// the cache is bounded by entry count.
func (v *vtxoDescriptorCacheValue) Size() (uint64, error) {
	return 1, nil
}

// vtxoDescriptorCacheKey is the fixed-size outpoint key: the 32-byte txid
// followed by the 4-byte little-endian output index.
type vtxoDescriptorCacheKey [36]byte

// vtxoDescriptorCache memoizes the immutable derived parts of VTXO rows by
// outpoint. A VTXO's script material is bound to its on-chain output, so an
// outpoint can never map to different derived values; no invalidation is
// needed and mutable row state (status, last update time) stays out of the
// cache.
type vtxoDescriptorCache struct {
	entries *lru.Cache[vtxoDescriptorCacheKey, *vtxoDescriptorCacheValue]
}

// newVTXODescriptorCache creates a process-local cache for the derived
// descriptor parts.
func newVTXODescriptorCache() *vtxoDescriptorCache {
	return &vtxoDescriptorCache{
		entries: lru.NewCache[
			vtxoDescriptorCacheKey, *vtxoDescriptorCacheValue,
		](
			uint64(maxVTXODescriptorCacheEntries),
		),
	}
}

// keyForOutpoint packs an outpoint into the fixed-size cache key.
func keyForOutpoint(op wire.OutPoint) vtxoDescriptorCacheKey {
	var key vtxoDescriptorCacheKey
	copy(key[:], op.Hash[:])
	key[32] = byte(op.Index)
	key[33] = byte(op.Index >> 8)
	key[34] = byte(op.Index >> 16)
	key[35] = byte(op.Index >> 24)

	return key
}

// get returns the cached derived parts for the outpoint, if present.
func (c *vtxoDescriptorCache) get(op wire.OutPoint) (*vtxoDescriptorCacheValue,
	bool) {

	if c == nil || c.entries == nil {
		return nil, false
	}

	cached, err := c.entries.Get(keyForOutpoint(op))
	if err != nil {
		return nil, false
	}

	return cached, true
}

// put stores the derived parts for the outpoint. Failures are surfaced so
// callers can decide whether to ignore them; a put failure only costs a
// future re-derivation.
func (c *vtxoDescriptorCache) put(op wire.OutPoint,
	value *vtxoDescriptorCacheValue) error {

	if c == nil || c.entries == nil {
		return nil
	}

	if _, err := c.entries.Put(keyForOutpoint(op), value); err != nil &&
		!errors.Is(err, cache.ErrElementNotFound) {
		return fmt.Errorf("put descriptor cache: %w", err)
	}

	return nil
}

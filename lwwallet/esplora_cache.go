package lwwallet

import (
	"errors"

	"github.com/btcsuite/btcd/wire/v2"
)

// errNilCacheEntry is returned by the cached-value Size methods when
// the wrapped pointer is nil. The neutrino LRU treats Size errors as
// a refusal-to-insert, so a stray nil-wrapped value cannot fill the
// LRU map without consuming any byte budget. None of the Put sites
// in esplora.go pass nil values today; this is defense in depth.
var errNilCacheEntry = errors.New("nil cache entry")

// Cache capacities are expressed in bytes (the unit returned by each
// cached value's Size()). They are sized for a typical client load:
// a few thousand cached transactions, a handful of recent blocks, and
// effectively unbounded room for the small fixed-size header types.
//
// These caches only hold immutable, hash-addressed data (transactions
// and blocks fetched by their content-hash, plus block headers). The
// cache fillers in esplora.go verify that the response body actually
// hashes to the requested key before insertion (txid for txs,
// BlockHash for headers and raw blocks); a buggy or compromised
// Esplora endpoint cannot pin an arbitrary entry under an
// attacker-chosen key. Once an entry is admitted, no reorg can stale
// it because the same hash can only ever map to the same content.
const (
	// txCacheCapacity caps the cumulative serialized size of the
	// transaction cache. Sized for a few hundred recent txs on a
	// lightweight wallet — we mostly need to absorb the
	// duplicate fetch of the same tx by chainBackend confirmation
	// lookup and the boarding tx-proof builder.
	txCacheCapacity uint64 = 5 * 1024 * 1024

	// rawBlockCacheCapacity caps the cumulative serialized size of
	// the raw-block cache. Sized for ~5 recent full blocks (mainnet
	// blocks approach 4 MiB), which covers the duplicate fetch by
	// chainSvc filterBlock and chainBackend includeBlock paths
	// without committing a hundreds-of-MiB resident set on
	// Pi-class hardware.
	rawBlockCacheCapacity uint64 = 20 * 1024 * 1024

	// rawHeaderCacheCapacity caps the cumulative size of the
	// raw-block-header cache. Each header is fixed at 80 bytes, so
	// 1 MiB holds ~13 K headers.
	rawHeaderCacheCapacity uint64 = 1 * 1024 * 1024

	// blockHeaderCacheCapacity caps the cumulative size of the
	// JSON block-header cache (height + timestamp metadata).
	blockHeaderCacheCapacity uint64 = 1 * 1024 * 1024
)

// cachedTx wraps a *wire.MsgTx so it satisfies the neutrino cache.Value
// interface for LRU bookkeeping. The byte budget is pinned to the
// raw HTTP response length captured at Put time so the LRU's repeated
// Size() calls (one per Put plus one per eviction candidate) do not
// re-serialize the transaction each time. wire.MsgTx.SerializeSize
// walks every input and output, which is wasted work given the same
// number is already on the stack at the cache-fill site.
type cachedTx struct {
	tx   *wire.MsgTx
	size uint64
}

// Size returns the cached serialized byte length of the wrapped
// transaction, or errNilCacheEntry when the wrapped pointer is nil
// so the LRU refuses the insert rather than admitting a 0-byte
// entry that would not count against the byte budget.
func (c cachedTx) Size() (uint64, error) {
	if c.tx == nil {
		return 0, errNilCacheEntry
	}

	return c.size, nil
}

// cachedBlock wraps a *wire.MsgBlock for LRU bookkeeping. Blocks can
// approach 4 MiB on mainnet, and SerializeSize iterates every
// transaction's inputs and outputs, so we pin the byte budget to the
// raw HTTP response length captured at Put time rather than walking
// the block on every LRU bookkeeping or eviction call.
type cachedBlock struct {
	block *wire.MsgBlock
	size  uint64
}

// Size returns the cached serialized byte length of the wrapped
// block, or errNilCacheEntry when the wrapped pointer is nil. See
// cachedTx.Size for the rationale.
func (c cachedBlock) Size() (uint64, error) {
	if c.block == nil {
		return 0, errNilCacheEntry
	}

	return c.size, nil
}

// cachedRawHeader wraps a *wire.BlockHeader for LRU bookkeeping.
// Block headers are always 80 bytes when serialized.
type cachedRawHeader struct {
	header *wire.BlockHeader
}

// Size reports the fixed 80-byte serialized header length, or
// errNilCacheEntry when the wrapped pointer is nil. See cachedTx.Size
// for the rationale.
func (c cachedRawHeader) Size() (uint64, error) {
	if c.header == nil {
		return 0, errNilCacheEntry
	}

	return 80, nil
}

// cachedBlockHeader wraps an *esploraBlock for LRU bookkeeping.
type cachedBlockHeader struct {
	header *esploraBlock
}

// Size reports a real-ish heap footprint for the wrapped JSON
// header. The struct itself is fixed-size apart from the ID hex
// string, which on a well-behaved Esplora is always 64 bytes; we
// charge len(ID) + the size of the two integer fields plus a
// generous fixed overhead so a hostile Esplora that returns a
// pathologically long ID string still consumes its budget against
// the cache cap rather than slipping under a hardcoded 128-byte
// figure.
func (c cachedBlockHeader) Size() (uint64, error) {
	if c.header == nil {
		return 0, errNilCacheEntry
	}

	const fixedOverhead = 64

	return uint64(len(c.header.ID)) + fixedOverhead, nil
}

package lwwallet

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// TestEsploraCacheRejectsTxHashMismatch verifies that GetRawTx
// refuses to cache (and refuses to return) a tx body whose
// computed TxHash differs from the requested key.
func TestEsploraCacheRejectsTxHashMismatch(t *testing.T) {
	t.Parallel()

	wantTxid := chainhash.HashH([]byte("victim"))

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			// Always serve the same minimal raw tx no matter
			// which txid was requested. Its actual TxHash will
			// not equal wantTxid, so the verifier must reject.
			_, _ = w.Write(minimalRawTx())
		},
	))
	defer srv.Close()

	c := NewEsploraClient(srv.URL, btclog.Disabled)

	tx, err := c.GetRawTx(t.Context(), wantTxid)
	require.Error(t, err)
	require.Nil(t, tx)
	require.Contains(t, err.Error(), "tx hash mismatch")

	// A second request still goes through the network (no cache
	// poison) and still rejects.
	tx2, err2 := c.GetRawTx(t.Context(), wantTxid)
	require.Error(t, err2)
	require.Nil(t, tx2)
}

// TestEsploraCacheRejectsBlockHashMismatch verifies that GetRawBlock
// refuses to cache a block whose computed BlockHash does not equal
// the requested key.
func TestEsploraCacheRejectsBlockHashMismatch(t *testing.T) {
	t.Parallel()

	wantHash := chainhash.HashH([]byte("victim-block"))

	// Build a serialized block with a known header so we know its
	// hash up front.
	var attackerBlock wire.MsgBlock
	attackerBlock.Header.Version = 4
	attackerBlock.Header.MerkleRoot = chainhash.HashH(
		[]byte("attacker-merkle"),
	)

	var blockBuf bytes.Buffer
	require.NoError(t, attackerBlock.Serialize(&blockBuf))

	srv := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(blockBuf.Bytes())
			},
		),
	)
	defer srv.Close()

	c := NewEsploraClient(srv.URL, btclog.Disabled)

	block, err := c.GetRawBlock(t.Context(), wantHash)
	require.Error(t, err)
	require.Nil(t, block)
	require.Contains(t, err.Error(), "raw block hash mismatch")
}

// TestEsploraCacheRejectsBlockHeaderHashMismatch verifies the JSON
// /block/:hash filler refuses entries where the response's id field
// doesn't equal the requested key.
func TestEsploraCacheRejectsBlockHeaderHashMismatch(t *testing.T) {
	t.Parallel()

	wantHash := chainhash.HashH([]byte("victim-jsonblock"))
	otherHash := chainhash.HashH([]byte("other"))

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(
				w, `{"id":%q,"height":42,"timestamp":1000}`,
				otherHash.String(),
			)
		},
	))
	defer srv.Close()

	c := NewEsploraClient(srv.URL, btclog.Disabled)

	hdr, err := c.GetBlockHeader(t.Context(), wantHash)
	require.Error(t, err)
	require.Nil(t, hdr)
	require.Contains(t, err.Error(), "block id mismatch")
}

// TestEsploraCacheTxHitCount verifies that a successful GetRawTx is
// memoized: a second call returns the cached value without hitting
// the network. We assert this by counting HTTP requests against the
// stub.
func TestEsploraCacheTxHitCount(t *testing.T) {
	t.Parallel()

	// Compute the txid of minimalRawTx so the verifier admits
	// the response.
	var tx wire.MsgTx
	require.NoError(t, tx.Deserialize(bytes.NewReader(minimalRawTx())))
	txid := tx.TxHash()

	var hits atomic.Int64
	srv := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				_, _ = w.Write(minimalRawTx())
			},
		),
	)
	defer srv.Close()

	c := NewEsploraClient(srv.URL, btclog.Disabled)

	// First call: cache miss, one HTTP hit.
	tx1, err := c.GetRawTx(t.Context(), txid)
	require.NoError(t, err)
	require.NotNil(t, tx1)
	require.Equal(t, int64(1), hits.Load())

	// Second call: cache hit, no additional HTTP hit.
	tx2, err := c.GetRawTx(t.Context(), txid)
	require.NoError(t, err)
	require.Same(t, tx1, tx2,
		"expected cache to return same pointer")
	require.Equal(
		t, int64(1), hits.Load(),
		"second GetRawTx should not have hit the network",
	)
}

// TestCachedSizeRefusesNil verifies the Size methods on each cache
// value type return errNilCacheEntry when the wrapped pointer is
// nil. This is the M-6 defense that prevents a 0-byte LRU entry
// from filling the map.
func TestCachedSizeRefusesNil(t *testing.T) {
	t.Parallel()

	checkRefuse := func(name string, sz func() (uint64, error)) {
		t.Helper()
		_, err := sz()
		require.ErrorIs(
			t, err, errNilCacheEntry, "%s: expected "+
				"errNilCacheEntry", name,
		)
	}

	checkRefuse("cachedTx", cachedTx{}.Size)
	checkRefuse("cachedBlock", cachedBlock{}.Size)
	checkRefuse("cachedRawHeader", cachedRawHeader{}.Size)
	checkRefuse("cachedBlockHeader", cachedBlockHeader{}.Size)
}

// TestCachedBlockHeaderSizeReflectsIDLen verifies the M-3 fix:
// cachedBlockHeader's Size accounts for the variable-length JSON
// id string so a hostile Esplora response with a pathologically
// long id consumes proportional cache budget rather than slipping
// under a hardcoded constant.
func TestCachedBlockHeaderSizeReflectsIDLen(t *testing.T) {
	t.Parallel()

	short := cachedBlockHeader{
		header: &esploraBlock{
			ID: "abcd",
		},
	}
	long := cachedBlockHeader{
		header: &esploraBlock{
			ID: hex.EncodeToString(
				make([]byte, 1024),
			),
		},
	}

	shortSize, err := short.Size()
	require.NoError(t, err)

	longSize, err := long.Size()
	require.NoError(t, err)

	require.Greater(
		t, longSize, shortSize,
		"longer ID must contribute to cache budget",
	)
}

package lwwallet

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/stretchr/testify/require"
)

// reorgTestTimeout is the per-step wait used by the lwwallet reorg
// tests. Generous enough to absorb scheduling jitter on overloaded
// CI machines but short enough that a hung backend surfaces as a
// fast failure.
const reorgTestTimeout = 3 * time.Second

// fakeChain is a mutable chain fixture for the reorg tests. Each
// height maps to a block whose hash, raw header, and contents the
// embedded HTTP handler will serve. Hashes are computable so the
// EsploraClient's content-hash verification passes.
//
// fakeChain is intentionally orthogonal to stubChain (used by the
// existing tip-poller tests): it supports same-height hash
// replacement (reorg), per-tx status overrides, per-outpoint
// outspend overrides, and serves /block/<hash>/header so the tip
// poller's continuity check can resolve PrevBlock.
type fakeChain struct {
	t  *testing.T
	mu sync.Mutex

	// tip is the current tip height.
	tip int32

	// blocks holds the per-height block model. Replacing the entry
	// at height h simulates a same-height reorg; appending new
	// heights simulates chain advance.
	blocks map[int32]*fakeBlock

	// txStatus is the response to /tx/<txid>/status.
	txStatus map[chainhash.Hash]esploraTxStatus

	// rawTx is the response to /tx/<txid>/raw.
	rawTx map[chainhash.Hash][]byte

	// outspends is the response to /tx/<txid>/outspend/<vout>.
	outspends map[wire.OutPoint]esploraOutspend

	// failRawHeader holds block hashes for which the
	// /block/<hash>/header endpoint should return 500. Used to
	// simulate a transient Esplora flake during continuity
	// checking.
	failRawHeader map[chainhash.Hash]struct{}
}

// fakeBlock describes one height's block in the fakeChain. The hash
// is derived from a synthetic 80-byte header so the EsploraClient's
// content-hash verification accepts the raw-header response.
type fakeBlock struct {
	height    int32
	hash      chainhash.Hash
	prevHash  chainhash.Hash
	header    *wire.BlockHeader
	timestamp int64
}

// newFakeChain seeds a fakeChain with a single block at the given
// tip height. tag is mixed into the block hash so independent
// fakeChains in parallel tests produce distinct hashes.
func newFakeChain(t *testing.T, tip int32, tag string) *fakeChain {
	t.Helper()

	c := &fakeChain{
		t:             t,
		tip:           tip,
		blocks:        make(map[int32]*fakeBlock),
		txStatus:      make(map[chainhash.Hash]esploraTxStatus),
		rawTx:         make(map[chainhash.Hash][]byte),
		outspends:     make(map[wire.OutPoint]esploraOutspend),
		failRawHeader: make(map[chainhash.Hash]struct{}),
	}

	c.blocks[tip] = c.mintBlock(tip, chainhash.Hash{}, tag)

	return c
}

// mintBlock builds a fakeBlock at height with the given prev hash
// and a tag that varies the resulting block hash. We synthesize a
// minimal 80-byte header with a unique nonce so BlockHash() varies
// reliably across invocations.
func (c *fakeChain) mintBlock(height int32, prev chainhash.Hash,
	tag string) *fakeBlock {

	c.t.Helper()

	hdr := &wire.BlockHeader{
		Version:    1,
		PrevBlock:  prev,
		MerkleRoot: chainhash.HashH([]byte(tag + "-merkle")),
		Timestamp:  time.Unix(int64(height)*600, 0),
		Bits:       0x207fffff,
		Nonce: uint32(height) ^
			uint32(chainhash.HashH([]byte(tag)).String()[0])<<16,
	}

	// Make the nonce truly unique per (height, tag) so two
	// adjacent tags do not collide on PrevBlock=zero replays.
	salt := chainhash.HashH([]byte(fmt.Sprintf("%d-%s", height, tag)))
	hdr.Nonce = uint32(salt[0])<<24 | uint32(salt[1])<<16 |
		uint32(salt[2])<<8 | uint32(salt[3])

	return &fakeBlock{
		height:    height,
		hash:      hdr.BlockHash(),
		prevHash:  prev,
		header:    hdr,
		timestamp: hdr.Timestamp.Unix(),
	}
}

// replaceTip swaps in a brand-new block at the current tip height
// (same height, different hash) and returns the new block. Used to
// drive a same-height reorg.
func (c *fakeChain) replaceTip(tag string) *fakeBlock {
	c.mu.Lock()
	defer c.mu.Unlock()

	prev, ok := c.blocks[c.tip-1]
	var prevHash chainhash.Hash
	if ok {
		prevHash = prev.hash
	}

	blk := c.mintBlock(c.tip, prevHash, tag)
	c.blocks[c.tip] = blk

	return blk
}

// extend appends a new block on top of the current tip and returns
// it. tag varies the resulting hash for parallel tests.
func (c *fakeChain) extend(tag string) *fakeBlock {
	c.mu.Lock()
	defer c.mu.Unlock()

	tipBlk := c.blocks[c.tip]
	blk := c.mintBlock(c.tip+1, tipBlk.hash, tag)
	c.tip++
	c.blocks[c.tip] = blk

	return blk
}

// rewriteFrom rebuilds the chain from height start upward, giving
// each new block a different hash than the previous occupant at
// the same height. Used to simulate a deeper reorg where multiple
// heights diverge at once.
func (c *fakeChain) rewriteFrom(start int32, tagPrefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var prev chainhash.Hash
	if prior, ok := c.blocks[start-1]; ok {
		prev = prior.hash
	}
	for h := start; h <= c.tip; h++ {
		blk := c.mintBlock(h, prev,
			fmt.Sprintf("%s-%d", tagPrefix, h))
		c.blocks[h] = blk
		prev = blk.hash
	}
}

// setTxStatus pins the response for /tx/<txid>/status.
func (c *fakeChain) setTxStatus(txid chainhash.Hash, status esploraTxStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.txStatus[txid] = status
}

// setRawTx pins the response for /tx/<txid>/raw.
func (c *fakeChain) setRawTx(txid chainhash.Hash, raw []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.rawTx[txid] = raw
}

// setOutspend pins the response for /tx/<txid>/outspend/<vout>.
func (c *fakeChain) setOutspend(op wire.OutPoint, outspend esploraOutspend) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.outspends[op] = outspend
}

// handler returns an http.HandlerFunc that serves the routes the
// chain backend / tip poller exercise. Anything else returns 404.
func (c *fakeChain) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()

		path := r.URL.Path
		switch {
		case path == "/blocks/tip/height":
			_, _ = fmt.Fprint(w, c.tip)

		case path == "/blocks/tip/hash":
			blk, ok := c.blocks[c.tip]
			if !ok {
				http.Error(w, "no tip", http.StatusNotFound)

				return
			}

			_, _ = fmt.Fprint(w, blk.hash.String())

		case strings.HasPrefix(path, "/block-height/"):
			heightStr := strings.TrimPrefix(
				path, "/block-height/",
			)
			height, err := strconv.ParseInt(heightStr, 10, 32)
			if err != nil {
				http.Error(
					w, "bad height", http.StatusBadRequest,
				)

				return
			}
			blk, ok := c.blocks[int32(height)]
			if !ok {
				http.Error(w, "not found",
					http.StatusNotFound)

				return
			}

			_, _ = fmt.Fprint(w, blk.hash.String())

		case strings.HasPrefix(path, "/block/"):
			c.serveBlockReq(w, r, path)

		case strings.HasPrefix(path, "/tx/"):
			c.serveTxReq(w, r, path)

		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}
}

// serveBlockReq handles /block/<hash> and /block/<hash>/header.
// Caller holds c.mu.
func (c *fakeChain) serveBlockReq(w http.ResponseWriter, _ *http.Request,
	path string) {

	rest := strings.TrimPrefix(path, "/block/")
	hashStr := rest
	suffix := ""
	if idx := strings.Index(rest, "/"); idx >= 0 {
		hashStr = rest[:idx]
		suffix = rest[idx:]
	}

	hash, err := chainhash.NewHashFromStr(hashStr)
	if err != nil {
		http.Error(w, "bad hash", http.StatusBadRequest)

		return
	}

	var found *fakeBlock
	for _, b := range c.blocks {
		if b.hash == *hash {
			found = b

			break
		}
	}
	if found == nil {
		http.Error(w, "not found", http.StatusNotFound)

		return
	}

	switch suffix {
	case "":
		// JSON header.
		_, _ = fmt.Fprintf(
			w, `{"id":%q,"height":%d,"timestamp":%d}`,
			found.hash.String(), found.height, found.timestamp,
		)

	case "/header":
		// Raw 80-byte header, hex-encoded.
		if _, fail := c.failRawHeader[found.hash]; fail {
			http.Error(
				w, "raw header unavailable",
				http.StatusInternalServerError,
			)

			return
		}
		var buf bytes.Buffer
		require.NoError(c.t, found.header.Serialize(&buf))
		_, _ = fmt.Fprint(w, hex.EncodeToString(buf.Bytes()))

	default:
		http.Error(w, "not implemented",
			http.StatusNotImplemented)
	}
}

// serveTxReq handles /tx/<txid>/status, /tx/<txid>/raw, and
// /tx/<txid>/outspend/<vout>. Caller holds c.mu.
func (c *fakeChain) serveTxReq(w http.ResponseWriter, _ *http.Request,
	path string) {

	rest := strings.TrimPrefix(path, "/tx/")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		http.Error(w, "not found", http.StatusNotFound)

		return
	}

	txid, err := chainhash.NewHashFromStr(parts[0])
	if err != nil {
		http.Error(w, "bad txid", http.StatusBadRequest)

		return
	}

	switch parts[1] {
	case "status":
		status, ok := c.txStatus[*txid]
		if !ok {
			// Default to unconfirmed when no override exists.
			status = esploraTxStatus{Confirmed: false}
		}

		err := json.NewEncoder(w).Encode(status)
		require.NoError(c.t, err)

	case "raw":
		raw, ok := c.rawTx[*txid]
		if !ok {
			http.Error(w, "not found",
				http.StatusNotFound)

			return
		}
		_, err := w.Write(raw)
		require.NoError(c.t, err)

	case "outspend":
		if len(parts) < 3 {
			http.Error(w, "not found",
				http.StatusNotFound)

			return
		}
		vout, err := strconv.ParseUint(parts[2], 10, 32)
		if err != nil {
			http.Error(w, "bad vout",
				http.StatusBadRequest)

			return
		}
		op := wire.OutPoint{
			Hash: *txid, Index: uint32(vout),
		}
		outspend, ok := c.outspends[op]
		if !ok {
			outspend = esploraOutspend{Spent: false}
		}
		err = json.NewEncoder(w).Encode(outspend)
		require.NoError(c.t, err)

	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// fakeChainServer wraps a fakeChain with an httptest.Server for
// drop-in use by the backend tests. The server is auto-closed via
// t.Cleanup.
func fakeChainServer(t *testing.T, chain *fakeChain) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(chain.handler())
	t.Cleanup(srv.Close)

	return srv
}

// awaitConf reads one TxConfirmation with a deadline.
func awaitConf(t *testing.T, ch <-chan *chainsourceConf) *chainsourceConf {
	t.Helper()

	select {
	case c, ok := <-ch:
		require.True(t, ok, "conf channel closed unexpectedly")

		return c

	case <-time.After(reorgTestTimeout):
		t.Fatal("timeout waiting for confirmation")

		return nil
	}
}

// awaitSpend reads one SpendDetail with a deadline.
func awaitSpend(t *testing.T, ch <-chan *chainsourceSpend) *chainsourceSpend {
	t.Helper()

	select {
	case s, ok := <-ch:
		require.True(t, ok, "spend channel closed unexpectedly")

		return s

	case <-time.After(reorgTestTimeout):
		t.Fatal("timeout waiting for spend")

		return nil
	}
}

// awaitSeqSignal is awaitSignal for the sequence-carrying Reorged
// channel (chainsource retyped it from struct{} to the ordering
// sequence; this backend always sends 0).
func awaitSeqSignal(t *testing.T, ch <-chan uint64, label string) {
	t.Helper()

	select {
	case _, ok := <-ch:
		require.True(t, ok,
			"%s channel closed unexpectedly", label)

	case <-time.After(reorgTestTimeout):
		t.Fatalf("timeout waiting for %s", label)
	}
}

// requireQuiet asserts that ch has no event for a short window.
// Used to ensure registrations are NOT double-firing.
func requireQuiet(t *testing.T, ch <-chan struct{}, label string,
	dur time.Duration) {

	t.Helper()

	select {
	case <-ch:
		t.Fatalf("unexpected %s signal", label)

	case <-time.After(dur):
	}
}

// chainsourceConf / chainsourceSpend are type aliases to keep the
// test helper signatures short.
type (
	chainsourceConf  = chainsource.TxConfirmation
	chainsourceSpend = chainsource.SpendDetail
)

// TestChainBackendSameHeightHashDrift verifies that a same-height
// hash replacement is detected by the tip poller and routed to the
// chain backend as a reorg, even when the chain height does not
// advance. This is the core "same-height reorgs invisible" gap the
// PR closes.
func TestChainBackendSameHeightHashDrift(t *testing.T) {
	t.Parallel()

	chain := newFakeChain(t, 100, "drift-init")
	srv := fakeChainServer(t, chain)

	esplora := NewEsploraClient(srv.URL, btclog.Disabled)
	backend := NewChainBackend(
		esplora, 20*time.Millisecond, btclog.Disabled,
	)
	require.NoError(t, backend.Start())
	t.Cleanup(func() {
		require.NoError(t, backend.Stop())
	})

	// Subscribe directly to the poller's reorg stream to verify
	// detection. The backend also subscribes; both subscribers
	// must observe the reorg.
	reorgSub, err := backend.tipPoller.SubscribeReorgs()
	require.NoError(t, err)
	defer reorgSub.Cancel()

	// Replace the tip block at the SAME height with a new hash.
	chain.replaceTip("drift-new")

	select {
	case ev := <-reorgSub.Updates():
		require.NotNil(t, ev)
		require.Equal(t, int32(99), ev.ForkHeight)
		require.Len(t, ev.Disconnected, 1)
		require.Len(t, ev.Connected, 1)
		require.Equal(t, int32(100), ev.Connected[0].Height)

	case <-time.After(reorgTestTimeout):
		t.Fatal("timed out waiting for same-height reorg")
	}
}

// TestChainBackendConfReorgRoundTrip drives a registration through
// Confirmed -> Reorged -> Confirmed by replacing the block that
// confirmed the tx with a new block at the same height that still
// confirms the tx (different BlockHash).
func TestChainBackendConfReorgRoundTrip(t *testing.T) {
	t.Parallel()

	chain := newFakeChain(t, 100, "conf-init")
	srv := fakeChainServer(t, chain)

	// Use minimalRawTx so the EsploraClient's content-hash
	// verification accepts the raw-tx response.
	rawTx := minimalRawTx()
	txid := minimalRawTxID(t)
	chain.setRawTx(txid, rawTx)

	confBlock := chain.blocks[100]
	chain.setTxStatus(txid, esploraTxStatus{
		Confirmed:   true,
		BlockHeight: 100,
		BlockHash:   confBlock.hash.String(),
	})

	esplora := NewEsploraClient(srv.URL, btclog.Disabled)
	backend := NewChainBackend(
		esplora, 20*time.Millisecond, btclog.Disabled,
	)
	require.NoError(t, backend.Start())
	t.Cleanup(func() {
		require.NoError(t, backend.Stop())
	})

	reg, err := backend.RegisterConf(
		t.Context(), &txid, nil, 1, 99, false,
	)
	require.NoError(t, err)
	defer reg.Cancel()

	conf1 := awaitConf(t, reg.Confirmed)
	require.Equal(t, uint32(100), conf1.BlockHeight)
	require.Equal(t, confBlock.hash, *conf1.BlockHash)

	// Same-height reorg: replace the confirming block with a
	// different one at height 100, and pin the tx status to
	// the new block hash so the post-reorg re-check finds the
	// tx confirmed in the new block.
	newBlock := chain.replaceTip("conf-replaced")
	chain.setTxStatus(txid, esploraTxStatus{
		Confirmed:   true,
		BlockHeight: 100,
		BlockHash:   newBlock.hash.String(),
	})

	awaitSeqSignal(t, reg.Reorged, "conf Reorged")

	conf2 := awaitConf(t, reg.Confirmed)
	require.Equal(t, uint32(100), conf2.BlockHeight)
	require.Equal(t, newBlock.hash, *conf2.BlockHash)
	require.NotEqual(
		t, conf1.BlockHash.String(), conf2.BlockHash.String(),
		"reorg should surface a different block hash",
	)
}

// TestChainBackendConfReorgEvictedDoesNotReConfirm verifies that
// when the post-reorg chain no longer contains the tx, Reorged
// fires but Confirmed does not re-fire.
func TestChainBackendConfReorgEvictedDoesNotReConfirm(t *testing.T) {
	t.Parallel()

	chain := newFakeChain(t, 100, "evict-init")
	srv := fakeChainServer(t, chain)

	rawTx := minimalRawTx()
	txid := minimalRawTxID(t)
	chain.setRawTx(txid, rawTx)

	confBlock := chain.blocks[100]
	chain.setTxStatus(txid, esploraTxStatus{
		Confirmed:   true,
		BlockHeight: 100,
		BlockHash:   confBlock.hash.String(),
	})

	esplora := NewEsploraClient(srv.URL, btclog.Disabled)
	backend := NewChainBackend(
		esplora, 20*time.Millisecond, btclog.Disabled,
	)
	require.NoError(t, backend.Start())
	t.Cleanup(func() {
		require.NoError(t, backend.Stop())
	})

	reg, err := backend.RegisterConf(
		t.Context(), &txid, nil, 1, 99, false,
	)
	require.NoError(t, err)
	defer reg.Cancel()

	awaitConf(t, reg.Confirmed)

	// Reorg evicts the confirming block AND the tx is no longer
	// found on the new chain.
	chain.replaceTip("evict-new")
	chain.setTxStatus(txid, esploraTxStatus{Confirmed: false})

	awaitSeqSignal(t, reg.Reorged, "conf Reorged")

	// No re-confirmation: assert silence.
	requireQuiet(
		t, structOnlyChan(reg.Confirmed),
		"unexpected re-Confirmed", 500*time.Millisecond,
	)
}

// structOnlyChan adapts a TxConfirmation channel to a struct{}
// channel for use with requireQuiet. The forwarder goroutine is
// short-lived; once the test ends both channels go out of scope.
func structOnlyChan(in <-chan *chainsourceConf) <-chan struct{} {
	out := make(chan struct{}, 4)

	go func() {
		for c := range in {
			if c == nil {
				continue
			}

			select {
			case out <- struct{}{}:
			default:
			}
		}
	}()

	return out
}

// TestChainBackendSpendReorgRoundTrip drives a spend registration
// through Spend -> Reorged -> Spend by reorging the block that
// confirmed the spending tx and pinning a different (still spent)
// outspend on the new chain.
func TestChainBackendSpendReorgRoundTrip(t *testing.T) {
	t.Parallel()

	chain := newFakeChain(t, 100, "spend-init")
	srv := fakeChainServer(t, chain)

	rawTx := minimalRawTx()
	spenderTxid := minimalRawTxID(t)
	chain.setRawTx(spenderTxid, rawTx)

	fundingTxid := chainhash.HashH([]byte("funding"))
	outpoint := wire.OutPoint{Hash: fundingTxid, Index: 0}

	chain.setOutspend(outpoint, esploraOutspend{
		Spent: true, Txid: spenderTxid.String(), Vin: 0,
		Status: esploraStatus{
			Confirmed: true, BlockHeight: 100,
		},
	})

	esplora := NewEsploraClient(srv.URL, btclog.Disabled)
	backend := NewChainBackend(
		esplora, 20*time.Millisecond, btclog.Disabled,
	)
	require.NoError(t, backend.Start())
	t.Cleanup(func() {
		require.NoError(t, backend.Stop())
	})

	reg, err := backend.RegisterSpend(
		t.Context(), &outpoint, nil, 90,
	)
	require.NoError(t, err)
	defer reg.Cancel()

	spend1 := awaitSpend(t, reg.Spend)
	require.Equal(t, spenderTxid, *spend1.SpenderTxHash)
	require.Equal(t, int32(100), spend1.SpendingHeight)

	// Replace the confirming block AND swap the spender on the
	// new chain. The spender tx id is the same (we're reusing
	// minimalRawTx) but the block hash differs, which is what
	// the reorg handler keys off.
	chain.replaceTip("spend-replaced")
	chain.setOutspend(outpoint, esploraOutspend{
		Spent: true, Txid: spenderTxid.String(), Vin: 0,
		Status: esploraStatus{
			Confirmed: true, BlockHeight: 100,
		},
	})

	awaitSeqSignal(t, reg.Reorged, "spend Reorged")

	spend2 := awaitSpend(t, reg.Spend)
	require.Equal(t, spenderTxid, *spend2.SpenderTxHash)
	require.Equal(t, int32(100), spend2.SpendingHeight)
}

// TestTipPollerSeedsHashHistoryOnStart pins the property that the
// poller seeds its recent-hash ring back through historySize-1
// heights at Start. Without this, a fresh poller would only ever
// cache the initial tip, leaving any reorg-event consumer with an
// incomplete disconnected set for reorgs deeper than 1 block but
// within the configured history. The chain.Interface consumer
// (EsploraChainService) depends on a complete disconnected set to
// emit a BlockDisconnected for every height btcwallet must roll
// back.
func TestTipPollerSeedsHashHistoryOnStart(t *testing.T) {
	t.Parallel()

	// Build a chain at tip 100 plus three lower heights. The poller
	// starts at tip 100; with seed-history, recentHashes will be
	// pre-populated with {97, 98, 99, 100}.
	chain := newFakeChain(t, 100, "seed-100")
	// fakeChain only populates the tip block; the test needs lower
	// heights in the response. Extend backwards by minting blocks
	// at 99, 98, 97 with the correct PrevBlock chain so the
	// poller's seed walk resolves each height.
	chain.mu.Lock()
	var prev chainhash.Hash
	for h := int32(97); h <= 100; h++ {
		blk := chain.mintBlock(h, prev, fmt.Sprintf("seed-%d", h))
		chain.blocks[h] = blk
		prev = blk.hash
	}
	chain.mu.Unlock()

	srv := fakeChainServer(t, chain)
	esplora := NewEsploraClient(srv.URL, btclog.Disabled)

	// Cap historySize at 4 so the seed walk fills exactly the
	// heights we care about.
	tipPoller := NewTipPollerWithConfig(
		esplora, 20*time.Millisecond, 4, btclog.Disabled,
	)
	require.NoError(t, tipPoller.Start())
	t.Cleanup(tipPoller.Stop)

	reorgSub, err := tipPoller.SubscribeReorgs()
	require.NoError(t, err)
	defer reorgSub.Cancel()

	// Reorg the deepest 3 heights (98, 99, 100). With seed-history
	// the poller's recentHashes contains 97, 98, 99, 100 so the
	// walk-back terminates at fork point 97 and disconnects 3
	// hashes. Without the seed, recentHashes would only contain
	// 100; walk-back would stop at 99 (no cached hash) and the
	// disconnected list would carry only 1 hash.
	chain.rewriteFrom(98, "seed-rewritten")

	select {
	case ev := <-reorgSub.Updates():
		require.NotNil(t, ev)
		require.Equal(
			t, int32(97), ev.ForkHeight, "fork point should be "+
				"the deepest unchanged height; "+
				"seed-history failed if it lands above 97",
		)
		require.Len(
			t, ev.Disconnected, 3, "all three reorged heights "+
				"must appear in Disconnected; seed-history "+
				"is what makes this property hold for "+
				"reorgs of depth > 1 against a freshly "+
				"started poller",
		)

	case <-time.After(reorgTestTimeout):
		t.Fatal("timed out waiting for reorg event")
	}
}

// TestChainBackendDeeperReorgDetected verifies that a multi-block
// reorg (where height advances AND PrevBlock continuity is broken)
// produces a ReorgEvent with the correct fork point.
//
// The test first advances the chain past the eventual fork point so
// the poller's recent-hash ring buffer contains the to-be-rewritten
// heights. Without that, the walk-back would terminate early on the
// first !haveCached probe and report a shallower fork than the test
// intends to exercise.
func TestChainBackendDeeperReorgDetected(t *testing.T) {
	t.Parallel()

	chain := newFakeChain(t, 100, "deep-init")

	srv := fakeChainServer(t, chain)
	esplora := NewEsploraClient(srv.URL, btclog.Disabled)
	tp := NewTipPoller(esplora, 20*time.Millisecond, btclog.Disabled)
	require.NoError(t, tp.Start())
	t.Cleanup(tp.Stop)

	// Subscribe BEFORE the rewrite so we receive the reorg
	// event live.
	reorgSub, err := tp.SubscribeReorgs()
	require.NoError(t, err)
	defer reorgSub.Cancel()

	tipSub, err := tp.Subscribe()
	require.NoError(t, err)
	defer tipSub.Cancel()

	// Advance the chain through the poller so it caches
	// heights 101 and 102 in its ring buffer.
	chain.extend("init-101")
	chain.extend("init-102")

	for expected := int32(101); expected <= 102; expected++ {
		select {
		case ev := <-tipSub.Updates():
			require.Equal(t, expected, ev.Height)

		case <-time.After(reorgTestTimeout):
			t.Fatalf("timed out waiting for height %d to be cached",
				expected)
		}
	}

	// Now rewrite from height 101 upward (heights 101 and 102
	// get new hashes) and extend by one to push the new tip
	// past the old.
	chain.rewriteFrom(101, "fork")
	chain.extend("fork-103")

	// Wait for ReorgEvent. ForkHeight should be 100 because
	// the rewrite started at 101 (so 100 is the last height
	// that agrees between old and new chains).
	select {
	case ev := <-reorgSub.Updates():
		require.NotNil(t, ev)
		require.Equal(t, int32(100), ev.ForkHeight)
		require.GreaterOrEqual(t, len(ev.Connected), 2)
		require.GreaterOrEqual(t, len(ev.Disconnected), 2)

	case <-time.After(reorgTestTimeout):
		t.Fatal("timed out waiting for deeper reorg")
	}
}

// TestChainBackendCancelCleanup verifies that Cancel on a
// conf/spend registration removes it from internal maps and does
// not leak goroutines.
func TestChainBackendCancelCleanup(t *testing.T) {
	t.Parallel()

	chain := newFakeChain(t, 100, "cancel-init")
	srv := fakeChainServer(t, chain)

	esplora := NewEsploraClient(srv.URL, btclog.Disabled)
	backend := NewChainBackend(
		esplora, time.Hour, btclog.Disabled,
	)
	require.NoError(t, backend.Start())
	t.Cleanup(func() {
		require.NoError(t, backend.Stop())
	})

	txid := minimalRawTxID(t)
	outpoint := wire.OutPoint{
		Hash: chainhash.HashH([]byte("funding")), Index: 0,
	}

	// Sample the baseline goroutine count AFTER the backend is
	// started so the poller / tip handler / reorg handler
	// goroutines are already included.
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	const iterations = 20
	for i := 0; i < iterations; i++ {
		confReg, err := backend.RegisterConf(
			t.Context(), &txid, nil, 1, 99, false,
		)
		require.NoError(t, err)

		spendReg, err := backend.RegisterSpend(
			t.Context(), &outpoint, nil, 90,
		)
		require.NoError(t, err)

		confReg.Cancel()
		spendReg.Cancel()

		// Double Cancel must be a safe no-op.
		confReg.Cancel()
		spendReg.Cancel()
	}

	// Give the spawned one-shot goroutines time to exit.
	require.Eventually(t, func() bool {

		// Allow a small tolerance for scheduling jitter; the
		// upper bound here is generous (20) versus the
		// per-iteration spawn count (2) to absorb any
		// in-flight Esplora request goroutines that have not
		// yet returned. The key invariant is that the count
		// does not GROW unbounded.
		return runtime.NumGoroutine() <= baseline+5
	}, 2*time.Second, 50*time.Millisecond,
		"goroutines leaked after Cancel: baseline=%d current=%d",
		baseline, runtime.NumGoroutine())

	// After Cancel, internal maps should be empty.
	backend.mu.Lock()
	confLen := len(backend.confRegs)
	spendLen := len(backend.spendRegs)
	backend.mu.Unlock()
	require.Equal(t, 0, confLen,
		"confRegs not cleaned up after Cancel")
	require.Equal(t, 0, spendLen,
		"spendRegs not cleaned up after Cancel")
}

// TestChainBackendConfStaysAliveAfterFirstFire verifies that the
// confirmation registration is NOT deleted after the first
// Confirmed delivery, since the chainsource ConfActor needs the
// registration to remain alive to receive future Reorged events
// and finally a synthesized Done.
func TestChainBackendConfStaysAliveAfterFirstFire(t *testing.T) {
	t.Parallel()

	chain := newFakeChain(t, 100, "alive-init")
	srv := fakeChainServer(t, chain)

	rawTx := minimalRawTx()
	txid := minimalRawTxID(t)
	chain.setRawTx(txid, rawTx)
	chain.setTxStatus(txid, esploraTxStatus{
		Confirmed:   true,
		BlockHeight: 100,
		BlockHash:   chain.blocks[100].hash.String(),
	})

	esplora := NewEsploraClient(srv.URL, btclog.Disabled)
	backend := NewChainBackend(
		esplora, 50*time.Millisecond, btclog.Disabled,
	)
	require.NoError(t, backend.Start())
	t.Cleanup(func() {
		require.NoError(t, backend.Stop())
	})

	reg, err := backend.RegisterConf(
		t.Context(), &txid, nil, 1, 99, false,
	)
	require.NoError(t, err)
	defer reg.Cancel()

	awaitConf(t, reg.Confirmed)

	// Give the polling loop time to fire several heartbeats.
	// The registration must still be present.
	time.Sleep(200 * time.Millisecond)

	backend.mu.Lock()
	confLen := len(backend.confRegs)
	backend.mu.Unlock()

	require.Equal(
		t, 1, confLen, "reg deleted after first fire; reorg-aware "+
			"contract requires it to stay alive",
	)
}

// TestChainBackendConfReorgBelowSeededHistory pins the property that a
// reorg deeper than the poller's seeded hash history still surfaces
// Reorged for a registration that delivered against the
// now-out-of-window block. The poller's disconnected set is bounded
// by the cached history, so without a canonical re-query fallback the
// reorg signal would be silently dropped.
func TestChainBackendConfReorgBelowSeededHistory(t *testing.T) {
	t.Parallel()

	// Seed a chain at height 100 and extend up to 105 so the
	// walk-back has live block hashes to compare against. Use a
	// tiny history size so the registration's conf block falls
	// outside the seeded window from the poller's perspective.
	chain := newFakeChain(t, 100, "below-100")
	chain.extend("below-101")
	chain.extend("below-102")
	chain.extend("below-103")
	chain.extend("below-104")
	chain.extend("below-105")

	confBlock := chain.blocks[100]
	srv := fakeChainServer(t, chain)

	rawTx := minimalRawTx()
	txid := minimalRawTxID(t)
	chain.setRawTx(txid, rawTx)
	chain.setTxStatus(txid, esploraTxStatus{
		Confirmed:   true,
		BlockHeight: 100,
		BlockHash:   confBlock.hash.String(),
	})

	esplora := NewEsploraClient(srv.URL, btclog.Disabled)
	// historySize=3 means the poller only retains the last three
	// canonical heights it has observed. The conf block at height
	// 100 will be five heights below the seeded tip (105) and is
	// guaranteed never to enter recentHashes.
	tipPoller := NewTipPollerWithConfig(
		esplora, 20*time.Millisecond, 3, btclog.Disabled,
	)
	require.NoError(t, tipPoller.Start())
	t.Cleanup(tipPoller.Stop)
	backend, err := NewChainBackendWithPoller(
		esplora, tipPoller, btclog.Disabled,
	)
	require.NoError(t, err)
	require.NoError(t, backend.Start())
	t.Cleanup(func() {
		require.NoError(t, backend.Stop())
	})

	reg, err := backend.RegisterConf(
		t.Context(), &txid, nil, 1, 99, false,
	)
	require.NoError(t, err)
	defer reg.Cancel()

	conf1 := awaitConf(t, reg.Confirmed)
	require.Equal(t, confBlock.hash, *conf1.BlockHash)

	// Deep reorg: rewrite every height from 100 upward with
	// different hashes. The new block at height 100 has a new
	// hash, but the tx is still pinned there so the canonical
	// re-query in reorgConfReg can fire a fresh Confirmed.
	chain.rewriteFrom(100, "below-new")
	newConfBlock := chain.blocks[100]
	chain.setTxStatus(txid, esploraTxStatus{
		Confirmed:   true,
		BlockHeight: 100,
		BlockHash:   newConfBlock.hash.String(),
	})

	awaitSeqSignal(t, reg.Reorged, "conf Reorged on below-history reorg")

	conf2 := awaitConf(t, reg.Confirmed)
	require.Equal(t, newConfBlock.hash, *conf2.BlockHash)
	require.NotEqual(
		t, conf1.BlockHash.String(), conf2.BlockHash.String(),
		"re-confirmation must surface the new block hash",
	)
}

// TestChainBackendAbortsOnRawHeaderFailure pins the property that a
// failed raw-header fetch during continuity check aborts the poll
// cycle rather than optimistically advancing. The optimistic path
// would permanently hide a reorg that crossed the old-tip boundary
// if the raw-header fetch flaked at exactly the wrong moment.
func TestChainBackendAbortsOnRawHeaderFailure(t *testing.T) {
	t.Parallel()

	chain := newFakeChain(t, 100, "abort-init")
	srv := fakeChainServer(t, chain)

	esplora := NewEsploraClient(srv.URL, btclog.Disabled)
	tipPoller := NewTipPoller(
		esplora, 20*time.Millisecond, btclog.Disabled,
	)
	require.NoError(t, tipPoller.Start())
	t.Cleanup(tipPoller.Stop)

	tipSub, err := tipPoller.Subscribe()
	require.NoError(t, err)
	defer tipSub.Cancel()
	reorgSub, err := tipPoller.SubscribeReorgs()
	require.NoError(t, err)
	defer reorgSub.Cancel()

	// Add a height-101 block whose PrevBlock does NOT chain off
	// the seeded tip at height 100. Then poison the raw-header
	// endpoint for that block so the continuity check cannot
	// resolve. The poller must abort the cycle: not broadcast
	// the new block, not update its cached tip, not fire a reorg.
	chain.mu.Lock()
	stranger := chain.mintBlock(
		101, chainhash.Hash{0xde, 0xad}, "abort-stranger",
	)
	chain.blocks[101] = stranger
	chain.tip = 101
	chain.failRawHeader[stranger.hash] = struct{}{}
	chain.mu.Unlock()

	// Sleep long enough for a few poll cycles to fire; verify
	// neither tip advance nor reorg ever fires.
	select {
	case ev := <-tipSub.Updates():
		t.Fatalf("poller broadcast a tip event despite raw-header "+
			"failure: height=%d hash=%s", ev.Height, ev.Hash)

	case ev := <-reorgSub.Updates():
		t.Fatalf("poller broadcast a reorg event despite raw-header "+
			"failure: fork=%d", ev.ForkHeight)

	case <-time.After(200 * time.Millisecond):
		// Expected: silent.
	}

	height, hash, _ := tipPoller.BestBlock()
	require.Equal(
		t, int32(100), height,
		"cached tip height advanced despite aborted cycle",
	)
	require.Equal(
		t, chain.blocks[100].hash, hash,
		"cached tip hash advanced despite aborted cycle",
	)
}

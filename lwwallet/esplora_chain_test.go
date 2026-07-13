package lwwallet

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/stretchr/testify/require"
)

// rawBlockStubChain is a tip+block fixture richer than stubChain: it
// builds real wire.MsgBlock values per height (so /block/:hash/raw
// can return bytes that deserialize and hash-verify) and supports
// per-hash injected failures on the raw-block endpoint. This lets us
// drive EsploraChainService end-to-end and reproduce the "transient
// GetRawBlock failure leads to a permanently skipped height" bug.
type rawBlockStubChain struct {
	mu sync.Mutex

	// tipHeight is the highest block currently visible on the chain.
	tipHeight int32

	// blockAt[h] is the synthesized block at height h. We construct
	// it once on premint/reveal and never mutate it again so callers
	// can hash and serialize without further locking.
	blockAt map[int32]*wire.MsgBlock

	// hashAt[h] is blockAt[h].BlockHash(), precomputed for the
	// height→hash route.
	hashAt map[int32]chainhash.Hash

	// heightFor[hash] is the inverse of hashAt, used to look up a
	// height from a hash supplied on the URL.
	heightFor map[chainhash.Hash]int32

	// failRawRemaining records the number of times /block/:hash/raw
	// should still return an error for a given hash. Each failing
	// fetch decrements the counter; once it reaches zero the route
	// returns the real block bytes.
	failRawRemaining map[chainhash.Hash]int

	// failHeaderRemaining records the number of times the JSON
	// header route /block/:hash should still return an error for a
	// given hash. Same decrement-on-fetch semantics as
	// failRawRemaining; used to exercise the gap-fill header
	// failure branch in EsploraChainService.fillGap.
	failHeaderRemaining map[chainhash.Hash]int
}

// newRawBlockStubChain builds a fixture seeded with blocks 0..tipHeight.
func newRawBlockStubChain(tipHeight int32) *rawBlockStubChain {
	c := &rawBlockStubChain{
		tipHeight:           tipHeight,
		blockAt:             make(map[int32]*wire.MsgBlock),
		hashAt:              make(map[int32]chainhash.Hash),
		heightFor:           make(map[chainhash.Hash]int32),
		failRawRemaining:    make(map[chainhash.Hash]int),
		failHeaderRemaining: make(map[chainhash.Hash]int),
	}

	for h := int32(0); h <= tipHeight; h++ {
		c.mintLocked(h)
	}

	return c
}

// mintLocked synthesizes a wire.MsgBlock for the given height. The
// header fields are derived from the height so each block has a
// unique hash; the body is empty (no transactions) so the block
// serializes to a small, deterministic byte string. Caller must hold
// c.mu.
func (c *rawBlockStubChain) mintLocked(height int32) {
	var prev chainhash.Hash
	if height > 0 {
		prev = c.hashAt[height-1]
	}

	hdr := wire.BlockHeader{
		Version:    1,
		PrevBlock:  prev,
		MerkleRoot: chainhash.Hash{},
		Timestamp:  time.Unix(int64(height)*600, 0),
		Bits:       0x207fffff,
		Nonce:      uint32(height),
	}

	block := &wire.MsgBlock{Header: hdr}
	hash := block.BlockHash()

	c.blockAt[height] = block
	c.hashAt[height] = hash
	c.heightFor[hash] = height
}

// premint constructs blocks for heights tipHeight+1..tipHeight+n
// without yet exposing them on the /blocks/tip/height endpoint.
// Callers use this to pin down hashes that will become visible after
// a later reveal(), so test set-up (e.g. failRawForHeight) can
// reference them by height before they go live.
func (c *rawBlockStubChain) premint(n int32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	base := c.tipHeight
	for i := int32(1); i <= n; i++ {
		h := base + i
		if _, ok := c.hashAt[h]; !ok {
			c.mintLocked(h)
		}
	}
}

// reveal advances tipHeight to include up to and including height H,
// without re-minting blocks that were already prepared by premint.
func (c *rawBlockStubChain) reveal(height int32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for h := c.tipHeight + 1; h <= height; h++ {
		if _, ok := c.hashAt[h]; !ok {
			c.mintLocked(h)
		}
	}

	if height > c.tipHeight {
		c.tipHeight = height
	}
}

// failRawForHeight schedules the next `count` /block/:hash/raw fetches
// for the block at the given height to return a 502. Used to simulate
// a transient Esplora outage on a single block.
func (c *rawBlockStubChain) failRawForHeight(height int32, count int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.failRawRemaining[c.hashAt[height]] = count
}

// failHeaderForHeight schedules the next `count` /block/:hash JSON
// header fetches for the block at the given height to return a 502.
// Used to exercise the gap-fill header-failure branch in
// EsploraChainService.fillGap.
func (c *rawBlockStubChain) failHeaderForHeight(height int32, count int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.failHeaderRemaining[c.hashAt[height]] = count
}

// hashFor returns the precomputed hash for the given height. The
// height must already have been minted (via the initial seed or a
// later premint/reveal); a missing entry is a test bug, not a
// caller error, so we panic to surface it loudly.
func (c *rawBlockStubChain) hashFor(height int32) chainhash.Hash {
	c.mu.Lock()
	defer c.mu.Unlock()

	hash, ok := c.hashAt[height]
	if !ok {
		panic(fmt.Sprintf("hashFor: height %d not minted", height))
	}

	return hash
}

// handler returns an http.HandlerFunc that serves the routes
// EsploraChainService and TipPoller actually hit.
func (c *rawBlockStubChain) handler(t *testing.T) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/blocks/tip/height":
			c.mu.Lock()
			h := c.tipHeight
			c.mu.Unlock()

			_, _ = fmt.Fprint(w, h)

		case len(r.URL.Path) > len("/block-height/") &&
			r.URL.Path[:len("/block-height/")] ==
				"/block-height/":

			heightStr := r.URL.Path[len("/block-height/"):]
			var height int32
			_, err := fmt.Sscanf(heightStr, "%d", &height)
			require.NoError(t, err)

			c.mu.Lock()
			hash, ok := c.hashAt[height]
			c.mu.Unlock()

			if !ok {
				http.Error(w, "not found",
					http.StatusNotFound)

				return
			}

			_, _ = fmt.Fprint(w, hash.String())

		case len(r.URL.Path) > len("/block/") &&
			r.URL.Path[:len("/block/")] == "/block/":

			c.serveBlockRoute(t, w, r)

		default:
			http.Error(w, "not found",
				http.StatusNotFound)
		}
	}
}

// serveBlockRoute serves the /block/:hash and /block/:hash/raw routes.
// The header (JSON) route always succeeds; the raw-block route honors
// failRawRemaining so tests can inject transient outages.
func (c *rawBlockStubChain) serveBlockRoute(t *testing.T, w http.ResponseWriter,
	r *http.Request) {

	t.Helper()

	rest := r.URL.Path[len("/block/"):]

	hashStr := rest
	suffix := ""
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			hashStr = rest[:i]
			suffix = rest[i:]

			break
		}
	}

	hash, err := chainhash.NewHashFromStr(hashStr)
	require.NoError(t, err)

	c.mu.Lock()
	height, ok := c.heightFor[*hash]
	if !ok {
		c.mu.Unlock()
		http.Error(w, "not found", http.StatusNotFound)

		return
	}
	block := c.blockAt[height]
	rawFailsLeft := c.failRawRemaining[*hash]
	headerFailsLeft := c.failHeaderRemaining[*hash]
	c.mu.Unlock()

	switch suffix {
	case "":
		// JSON header. Honor the per-hash failure counter so
		// tests can drive the fillGap header-fetch failure path.
		if headerFailsLeft > 0 {
			c.mu.Lock()
			c.failHeaderRemaining[*hash] = headerFailsLeft - 1
			c.mu.Unlock()

			http.Error(w, "injected outage",
				http.StatusBadGateway)

			return
		}

		resp := esploraBlock{
			ID:        hash.String(),
			Height:    height,
			Timestamp: int64(height) * 600,
		}
		err := json.NewEncoder(w).Encode(resp)
		require.NoError(t, err)

	case "/raw":
		if rawFailsLeft > 0 {
			c.mu.Lock()
			c.failRawRemaining[*hash] = rawFailsLeft - 1
			c.mu.Unlock()

			http.Error(w, "injected outage",
				http.StatusBadGateway)

			return
		}

		var buf bytes.Buffer
		require.NoError(t, block.Serialize(&buf))
		_, _ = w.Write(buf.Bytes())

	case "/header":
		// Hex-encoded 80-byte block header. The TipPoller fetches
		// this per new tip to verify PrevBlock continuity against
		// the cached tip hash; without it the poller cannot
		// distinguish a clean tip advance from a reorg crossing the
		// boundary and aborts the cycle.
		var buf bytes.Buffer
		require.NoError(t, block.Header.Serialize(&buf))
		_, _ = fmt.Fprint(w, hex.EncodeToString(buf.Bytes()))

	default:
		http.Error(w, "not implemented",
			http.StatusNotImplemented)
	}
}

// drainBlockConnected reads from notifications until it has captured
// the requested set of BlockConnected heights or the deadline expires.
// Returns the set of heights actually observed, in arrival order.
func drainBlockConnected(t *testing.T, notifications <-chan any,
	wantHeights []int32, deadline time.Duration) []int32 {

	t.Helper()

	want := make(map[int32]struct{}, len(wantHeights))
	for _, h := range wantHeights {
		want[h] = struct{}{}
	}

	var observed []int32
	timer := time.NewTimer(deadline)
	defer timer.Stop()

	for len(want) > 0 {
		select {
		case n := <-notifications:
			bc, ok := n.(chain.BlockConnected)
			if !ok {
				continue
			}

			observed = append(observed, bc.Height)
			delete(want, bc.Height)

		case <-timer.C:
			return observed
		}
	}

	return observed
}

// TestEsploraChainServiceRecoversFromTransientGetRawBlockFailure is
// the regression test for the "Esplora failures can make lwwallet
// permanently skip blocks" finding. A transient /block/:hash/raw
// outage on one height must NOT permanently strand that height
// between btcwallet's view and the chain tip — the chain service
// must walk forward from its own bestBlock cursor on the next
// TipBlock event and re-emit any heights it missed.
//
// Setup:
//   - TipPoller starts at height 100 and advances to 101 → 102 → 103.
//   - EsploraChainService has one watched address registered, so its
//     processTipEvent path fetches the raw block on every TipBlock.
//   - The /block/:hash/raw route for height 101 is configured to fail
//     once and then recover.
//
// Pre-fix behavior (before the gap-fill change):
//   - TipBlock(101) arrives; GetRawBlock(101) returns 502 so
//     processTipEvent returned without sending FilteredBlockConnected
//     or BlockConnected. TipPoller had already advanced its cached
//     tipHeight to 101, so it would never re-emit TipBlock(101).
//   - TipBlock(102) and TipBlock(103) succeeded; their BlockConnected
//     notifications were delivered.
//   - BlockConnected for 101 was NEVER delivered.
//
// Post-fix behavior asserted here:
//   - On TipBlock(102), the chain service notices that
//     s.bestBlock.Height == 100 < 102-1, walks the gap by fetching
//     block 101 again (which now succeeds), emits notifications for
//     101, advances bestBlock, then emits 102. By the time the chain
//     reaches 103, every height between the initial tip (exclusive)
//     and 103 has been delivered exactly once.
func TestEsploraChainServiceRecoversFromTransientGetRawBlockFailure(
	t *testing.T) {

	t.Parallel()

	chainStub := newRawBlockStubChain(100)
	srv := mockEsploraServer(t, chainStub.handler(t))

	esp := NewEsploraClient(srv.URL, btclog.Disabled)
	tp := NewTipPoller(esp, 5*time.Millisecond, btclog.Disabled)

	require.NoError(t, tp.Start())
	defer tp.Stop()

	svc := NewEsploraChainService(esp, tp, btclog.Disabled)

	// Watch a throwaway address so processTipEvent takes the
	// block-fetch path. The actual script does not need to match
	// anything in the synthesized blocks; the only branch we care
	// about here is whether the raw-block fetch is even attempted.
	addr, err := btcaddr.NewAddressPubKeyHash(
		make([]byte, 20), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	require.NoError(t, svc.NotifyReceived([]btcaddr.Address{addr}))

	require.NoError(t, svc.Start(t.Context()))
	defer svc.Stop()

	notifications := svc.Notifications()

	// Drain the initial ClientConnected so it does not skew the
	// per-event loop below.
	select {
	case n := <-notifications:
		_, ok := n.(chain.ClientConnected)
		require.True(t, ok, "expected ClientConnected first")

	case <-time.After(2 * time.Second):
		t.Fatal("did not receive initial ClientConnected")
	}

	// Pre-mint heights 101..103 so we know the hash for height
	// 101 before its block is exposed via /blocks/tip/height; this
	// lets us schedule the transient failure against the actual
	// hash the chain service will request.
	chainStub.premint(3)

	// Inject a single transient failure on the raw-block fetch for
	// height 101 so the failure fires on the first poll iteration
	// that reaches that height.
	chainStub.failRawForHeight(101, 1)

	// Reveal the new tip. TipPoller will fan out a TipBlock event
	// for each new height (101, 102, 103) in order.
	chainStub.reveal(103)

	// Every height we advanced through must surface as a
	// BlockConnected notification within a generous window AND
	// arrive in monotonically increasing height order — btcwallet's
	// sync pipeline relies on contiguous, in-order BlockConnected
	// delivery, so the assertion uses Equal (not ElementsMatch) to
	// catch any future refactor that accidentally reorders the gap-
	// fill / live-event interleaving. The pre-fix code never re-
	// emits 101 because the TipPoller has already moved past it, so
	// the drain would time out waiting for 101 and report the
	// observed prefix here.
	observed := drainBlockConnected(
		t, notifications, []int32{101, 102, 103}, 3*time.Second,
	)

	require.Equal(
		t, []int32{101, 102, 103}, observed, "every "+
			"advanced-through height must be delivered as a "+
			"BlockConnected in order, even when a transient "+
			"GetRawBlock failure dropped the original event "+
			"(observed=%v)", observed,
	)
}

// seedCursor sets the chain service's bestBlock to the given height
// without going through Start (which would require a live TipPoller).
// Tests that drive processTipEvent directly use this to plant a
// cursor and then assert that gap-fill / live-event delivery either
// advances it or, on failure paths, leaves it pinned.
func seedCursor(svc *EsploraChainService, height int32, hash chainhash.Hash) {
	svc.mu.Lock()
	svc.bestBlock.Height = height
	svc.bestBlock.Hash = hash
	svc.bestBlock.Timestamp = time.Unix(int64(height)*600, 0)
	svc.mu.Unlock()
}

// tipEventFor builds a synthetic TipBlock for the given height using
// the stub's precomputed hash. Mirrors the (height, hash, *esploraBlock
// header) shape TipPoller emits in production so processTipEvent
// sees the same input it would see end-to-end.
func tipEventFor(c *rawBlockStubChain, height int32) *TipBlock {
	hash := c.hashFor(height)

	return &TipBlock{
		Height: height,
		Hash:   hash,
		Header: &esploraBlock{
			ID:        hash.String(),
			Height:    height,
			Timestamp: int64(height) * 600,
		},
	}
}

// watchOne registers a single throwaway pubkey-hash address so
// processTipEvent takes the block-fetch path. The actual script does
// not need to match anything synthesized in the stub; the only branch
// we care about is whether the raw-block fetch is even attempted.
func watchOne(t *testing.T, svc *EsploraChainService) {
	t.Helper()

	addr, err := btcaddr.NewAddressPubKeyHash(
		make([]byte, 20), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	require.NoError(t, svc.NotifyReceived([]btcaddr.Address{addr}))
}

// requireNoNotification asserts the chain service emitted no
// notification within the given window. Used on failure paths where
// processTipEvent must return without sending FilteredBlockConnected
// or BlockConnected.
func requireNoNotification(t *testing.T, svc *EsploraChainService,
	window time.Duration, msg string) {

	t.Helper()

	select {
	case n := <-svc.Notifications():
		t.Fatalf("%s: unexpected notification %T %v", msg, n, n)

	case <-time.After(window):
	}
}

// requireCursor asserts the chain service's bestBlock height equals
// want. Used to verify the cursor invariant on every gap-fill /
// deliver-height path: success must advance, failure must pin.
func requireCursor(t *testing.T, svc *EsploraChainService, want int32) {
	t.Helper()

	svc.mu.Lock()
	got := svc.bestBlock.Height
	svc.mu.Unlock()

	require.Equal(t, want, got, "cursor mismatch")
}

// TestEsploraChainServiceGapFillHeaderFailureLeavesCursorPinned
// covers the fillGap header-fetch failure branch. Production reaches
// this when the blockHeaderCache has been evicted for a height that
// EsploraChainService still needs to back-fill (TipPoller already
// emitted the TipBlock but the raw-block fetch failed inside
// processTipEvent, so the cache may have rolled over before the next
// event triggers the retry). The invariant: a failed header fetch
// inside fillGap must leave s.bestBlock untouched so the next
// TipBlock event retries the same height.
//
// We drive processTipEvent directly rather than through TipPoller
// because TipPoller's own emit path warms the cache for every height
// it walks, which would mask the failure on retry. Direct invocation
// keeps the cache cold and exercises the live HTTP path inside
// fillGap exactly the way a cache-evicted production retry would.
func TestEsploraChainServiceGapFillHeaderFailureLeavesCursorPinned(
	t *testing.T) {

	t.Parallel()

	chainStub := newRawBlockStubChain(100)
	srv := mockEsploraServer(t, chainStub.handler(t))

	esp := NewEsploraClient(srv.URL, btclog.Disabled)
	svc := NewEsploraChainService(esp, nil, btclog.Disabled)

	seedCursor(svc, 100, chainStub.hashFor(100))
	watchOne(t, svc)

	// Premint 101..102 so we have hashes; inject a persistent
	// header failure for 101 so the gap walk hits it before the
	// live event can be delivered.
	chainStub.premint(2)
	chainStub.failHeaderForHeight(101, 999)

	// processTipEvent(102): lastDelivered=100, fillGap(100, 101)
	// fetches hash for 101 (succeeds, uncached), then header for
	// 101 (502). fillGap returns false; processTipEvent returns
	// without delivering the live event or advancing the cursor.
	svc.processTipEvent(t.Context(), tipEventFor(chainStub, 102))

	requireCursor(t, svc, 100)
	requireNoNotification(
		t, svc, 100*time.Millisecond,
		"gap-fill header failure must not emit any notification",
	)

	// Recover: clear the header-failure injection. The next
	// processTipEvent call must walk the gap (101 now succeeds),
	// emit notifications for 101, then process the live event 102.
	chainStub.mu.Lock()
	chainStub.failHeaderRemaining[chainStub.hashAt[101]] = 0
	chainStub.mu.Unlock()

	svc.processTipEvent(t.Context(), tipEventFor(chainStub, 102))

	observed := drainBlockConnected(
		t, svc.Notifications(), []int32{101, 102}, 2*time.Second,
	)
	require.Equal(
		t, []int32{101, 102}, observed,
		"retry after header recovery must deliver 101 then 102",
	)
	requireCursor(t, svc, 102)
}

// TestEsploraChainServiceGapFillRespectsPerEventCap covers the
// bounded-walk branch of fillGap. When a TipBlock event arrives with
// a height that is more than maxGapFillPerTipEvent ahead of the
// cursor, fillGap must walk only up to (cursor + cap), advance the
// cursor to that intermediate height, drop the live event, and rely
// on the next TipBlock event to make further progress.
//
// We override the cap to 3 via WithMaxGapFillPerTipEvent so the test
// can drive several capped invocations without revealing 256+ heights
// of HTTP traffic. This also doubles as functional coverage for the
// option wiring itself: an off-by-one in the option setter would
// either leak the default (failing the cap assertion) or zero out
// the cap (deadlocking on the first invocation).
func TestEsploraChainServiceGapFillRespectsPerEventCap(t *testing.T) {
	t.Parallel()

	const testCap int32 = 3

	chainStub := newRawBlockStubChain(100)
	srv := mockEsploraServer(t, chainStub.handler(t))

	esp := NewEsploraClient(srv.URL, btclog.Disabled)
	svc := NewEsploraChainService(
		esp, nil, btclog.Disabled, WithMaxGapFillPerTipEvent(testCap),
	)

	seedCursor(svc, 100, chainStub.hashFor(100))
	watchOne(t, svc)

	// Premint 101..110 so the live event at 110 is 9 heights
	// ahead of the cursor — three full cap-sized walks short of
	// catching up.
	chainStub.premint(10)
	event := tipEventFor(chainStub, 110)

	// First invocation: fillGap(100, 109). end-start=9 > cap=3,
	// walkEnd = 103. Walks 101..103, returns walkEnd != end, so
	// processTipEvent returns before delivering 110. Cursor at 103.
	svc.processTipEvent(t.Context(), event)
	requireCursor(t, svc, 103)
	observed := drainBlockConnected(
		t, svc.Notifications(), []int32{101, 102, 103}, 2*time.Second,
	)
	require.Equal(t, []int32{101, 102, 103}, observed)
	requireNoNotification(
		t, svc, 50*time.Millisecond,
		"live event 110 must not be delivered while cap pending",
	)

	// Second invocation: fillGap(103, 109). end-start=6 > cap=3,
	// walkEnd = 106. Walks 104..106. Cursor at 106.
	svc.processTipEvent(t.Context(), event)
	requireCursor(t, svc, 106)
	observed = drainBlockConnected(
		t, svc.Notifications(), []int32{104, 105, 106}, 2*time.Second,
	)
	require.Equal(t, []int32{104, 105, 106}, observed)
	requireNoNotification(
		t, svc, 50*time.Millisecond,
		"live event 110 still must not deliver mid-walk",
	)

	// Third invocation: fillGap(106, 109). end-start=3, NOT > cap,
	// walkEnd = 109. Walks 107..109, returns true. Live event 110
	// then delivers. Cursor advances to 110.
	svc.processTipEvent(t.Context(), event)
	requireCursor(t, svc, 110)
	observed = drainBlockConnected(
		t, svc.Notifications(), []int32{107, 108, 109, 110},
		2*time.Second,
	)
	require.Equal(t, []int32{107, 108, 109, 110}, observed)
}

// TestEsploraChainServiceDuplicateTipEventIsIgnored covers the
// duplicate / out-of-order guard at the top of processTipEvent.
// TipPoller dedupes by height on its emit path, but a subscribe-time
// race or a future retry path could still hand the chain service an
// event at or below the cursor; the guard must short-circuit before
// any HTTP fetch or notification send.
//
// We assert two cases: (a) an event at the cursor height (exact
// duplicate of the last delivered event), and (b) an event strictly
// below the cursor (out-of-order). Both must leave the cursor pinned
// and the notification channel idle.
func TestEsploraChainServiceDuplicateTipEventIsIgnored(t *testing.T) {
	t.Parallel()

	chainStub := newRawBlockStubChain(100)
	srv := mockEsploraServer(t, chainStub.handler(t))

	esp := NewEsploraClient(srv.URL, btclog.Disabled)
	svc := NewEsploraChainService(esp, nil, btclog.Disabled)

	// Plant the cursor at 102 — simulates a previous successful
	// delivery through that height. Premint 101..102 so the stub
	// can satisfy any hash lookup we might accidentally trigger.
	chainStub.premint(2)
	seedCursor(svc, 102, chainStub.hashFor(102))
	watchOne(t, svc)

	// (a) Exact duplicate. event.Height == lastDelivered.
	svc.processTipEvent(t.Context(), tipEventFor(chainStub, 102))
	requireCursor(t, svc, 102)
	requireNoNotification(
		t, svc, 100*time.Millisecond,
		"duplicate TipBlock at cursor height must not emit",
	)

	// (b) Strictly older event. event.Height < lastDelivered.
	svc.processTipEvent(t.Context(), tipEventFor(chainStub, 101))
	requireCursor(t, svc, 102)
	requireNoNotification(
		t, svc, 100*time.Millisecond,
		"out-of-order TipBlock below cursor must not emit",
	)
}

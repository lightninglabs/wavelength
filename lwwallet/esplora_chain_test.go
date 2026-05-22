package lwwallet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
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
}

// newRawBlockStubChain builds a fixture seeded with blocks 0..tipHeight.
func newRawBlockStubChain(tipHeight int32) *rawBlockStubChain {
	c := &rawBlockStubChain{
		tipHeight:        tipHeight,
		blockAt:          make(map[int32]*wire.MsgBlock),
		hashAt:           make(map[int32]chainhash.Hash),
		heightFor:        make(map[chainhash.Hash]int32),
		failRawRemaining: make(map[chainhash.Hash]int),
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
	failsLeft := c.failRawRemaining[*hash]
	c.mu.Unlock()

	switch suffix {
	case "":
		// JSON header.
		resp := esploraBlock{
			ID:        hash.String(),
			Height:    height,
			Timestamp: int64(height) * 600,
		}
		err := json.NewEncoder(w).Encode(resp)
		require.NoError(t, err)

	case "/raw":
		if failsLeft > 0 {
			c.mu.Lock()
			c.failRawRemaining[*hash] = failsLeft - 1
			c.mu.Unlock()

			http.Error(w, "injected outage",
				http.StatusBadGateway)

			return
		}

		var buf bytes.Buffer
		require.NoError(t, block.Serialize(&buf))
		_, _ = w.Write(buf.Bytes())

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

	// Skipped pending the gap-fill fix in the next commit. With the
	// current EsploraChainService.processTipEvent, a transient
	// GetRawBlock failure permanently strands a height between
	// btcwallet's view and the chain tip, so this test would fail
	// (heights=102,103 observed; 101 missing).
	t.Skip("regression reproducer; enabled by the follow-up gap-fill " +
		"fix in EsploraChainService.processTipEvent")

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
	addr, err := btcutil.NewAddressPubKeyHash(
		make([]byte, 20), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	require.NoError(t, svc.NotifyReceived([]btcutil.Address{addr}))

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

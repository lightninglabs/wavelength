package lwwallet

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// stubChain is a tiny test fixture that simulates an Esplora chain
// where the tip height can be advanced under test control. It is
// independent of the larger mockEsploraServer helper so tip-poller
// tests can drive the chain forward synchronously. Each height holds
// a real wire.BlockHeader whose PrevBlock chains back to the previous
// height; the poller's PrevBlock continuity check on advance therefore
// passes naturally rather than aborting.
type stubChain struct {
	mu        sync.Mutex
	tipHeight int32

	// blocks[h] holds the wire.BlockHeader for height h. Pre-
	// populated through 0..tipHeight at construction and grown
	// monotonically by advance.
	blocks map[int32]*wire.BlockHeader

	// hashAt[h] is the chainhash.Hash for height h.
	hashAt map[int32]chainhash.Hash
}

// mintStubHeader builds a deterministic wire.BlockHeader for a given
// height, chaining off the supplied previous hash. The salted nonce
// makes the resulting BlockHash stable per (height, prev) and unique
// across heights.
func mintStubHeader(height int32, prev chainhash.Hash) *wire.BlockHeader {
	salt := chainhash.HashH([]byte(fmt.Sprintf("stub-block-%d", height)))

	return &wire.BlockHeader{
		Version:   1,
		PrevBlock: prev,
		MerkleRoot: chainhash.HashH(
			[]byte(
				fmt.Sprintf("merkle-%d", height),
			),
		),
		Timestamp: time.Unix(int64(height)*600, 0),
		Bits:      0x207fffff,
		Nonce: uint32(salt[0])<<24 | uint32(salt[1])<<16 |
			uint32(salt[2])<<8 | uint32(salt[3]),
	}
}

func newStubChain(tipHeight int32) *stubChain {
	c := &stubChain{
		tipHeight: tipHeight,
		blocks:    make(map[int32]*wire.BlockHeader),
		hashAt:    make(map[int32]chainhash.Hash),
	}

	var prev chainhash.Hash
	for h := int32(0); h <= tipHeight; h++ {
		hdr := mintStubHeader(h, prev)
		c.blocks[h] = hdr
		hash := hdr.BlockHash()
		c.hashAt[h] = hash
		prev = hash
	}

	return c
}

func (c *stubChain) advance(t *testing.T, n int32) {
	t.Helper()

	c.mu.Lock()
	defer c.mu.Unlock()

	prev := c.hashAt[c.tipHeight]
	for i := int32(1); i <= n; i++ {
		h := c.tipHeight + i
		hdr := mintStubHeader(h, prev)
		c.blocks[h] = hdr
		hash := hdr.BlockHash()
		c.hashAt[h] = hash
		prev = hash
	}

	c.tipHeight += n
}

// stubEsploraHandler returns an http.HandlerFunc that serves the
// tip-poller's GET requests against the stubChain. Only the routes
// the poller actually hits are implemented.
func stubEsploraHandler(t *testing.T, chain *stubChain) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/blocks/tip/height":
			chain.mu.Lock()
			h := chain.tipHeight
			chain.mu.Unlock()

			_, _ = fmt.Fprint(w, h)

		case len(r.URL.Path) > len("/block-height/") &&
			r.URL.Path[:len("/block-height/")] ==
				"/block-height/":

			heightStr := r.URL.Path[len("/block-height/"):]
			var height int32
			_, err := fmt.Sscanf(heightStr, "%d", &height)
			require.NoError(t, err)

			chain.mu.Lock()
			h, ok := chain.hashAt[height]
			chain.mu.Unlock()

			if !ok {
				http.Error(w, "not found",
					http.StatusNotFound)

				return
			}

			_, _ = fmt.Fprint(w, h.String())

		case len(r.URL.Path) > len("/block/") &&
			r.URL.Path[:len("/block/")] == "/block/":

			rest := r.URL.Path[len("/block/"):]

			// Strip optional /header or /raw suffix.
			hashStr := rest
			suffix := ""
			for i := 0; i < len(rest); i++ {
				if rest[i] == '/' {
					hashStr = rest[:i]
					suffix = rest[i:]
					break
				}
			}

			h, err := chainhash.NewHashFromStr(hashStr)
			require.NoError(t, err)

			// Find the height for this hash so we can
			// craft a header whose BlockHash actually
			// matches.
			var height int32 = -1
			chain.mu.Lock()
			for hh, hash := range chain.hashAt {
				if hash == *h {
					height = hh

					break
				}
			}
			chain.mu.Unlock()

			if height < 0 {
				http.Error(w, "not found",
					http.StatusNotFound)

				return
			}

			switch suffix {
			case "":
				// JSON header.
				_, _ = fmt.Fprintf(w,
					`{"id":%q,"height":%d,`+
						`"timestamp":%d}`,
					h.String(), height,
					int64(height)*600)

			case "/header":
				// Raw 80-byte header, hex-encoded.
				chain.mu.Lock()
				hdr := chain.blocks[height]
				chain.mu.Unlock()
				if hdr == nil {
					http.Error(
						w, "not found",
						http.StatusNotFound,
					)

					return
				}
				var buf bytes.Buffer
				require.NoError(t, hdr.Serialize(&buf))
				_, _ = fmt.Fprint(
					w,
					hex.EncodeToString(
						buf.Bytes(),
					),
				)

			default:
				http.Error(
					w, "not implemented",
					http.StatusNotImplemented,
				)
			}

		default:
			http.Error(w, "not found",
				http.StatusNotFound)
		}
	}
}

// TestTipPollerStartStop verifies a clean start/stop cycle with no
// subscribers.
func TestTipPollerStartStop(t *testing.T) {
	t.Parallel()

	chain := newStubChain(100)
	srv := mockEsploraServer(t, stubEsploraHandler(t, chain))

	tp := NewTipPoller(
		NewEsploraClient(srv.URL, btclog.Disabled), 20*time.Millisecond,
		btclog.Disabled,
	)

	require.NoError(t, tp.Start())

	height, _, _ := tp.BestBlock()
	require.Equal(t, int32(100), height)

	tp.Stop()

	// Stop is idempotent.
	tp.Stop()
}

// TestTipPollerStartTwiceFails ensures double-Start is rejected so
// callers see a clear error rather than silently spawning two poll
// goroutines.
func TestTipPollerStartTwiceFails(t *testing.T) {
	t.Parallel()

	chain := newStubChain(50)
	srv := mockEsploraServer(t, stubEsploraHandler(t, chain))

	tp := NewTipPoller(
		NewEsploraClient(srv.URL, btclog.Disabled), 20*time.Millisecond,
		btclog.Disabled,
	)

	require.NoError(t, tp.Start())
	defer tp.Stop()

	require.Error(t, tp.Start())
}

// TestTipPollerMultiBlockCatchUp verifies that when the chain
// advances by N blocks between polls, the poller emits N events in
// strict height order.
func TestTipPollerMultiBlockCatchUp(t *testing.T) {
	t.Parallel()

	chain := newStubChain(100)
	srv := mockEsploraServer(t, stubEsploraHandler(t, chain))

	tp := NewTipPoller(
		NewEsploraClient(srv.URL, btclog.Disabled), 10*time.Millisecond,
		btclog.Disabled,
	)

	require.NoError(t, tp.Start())
	defer tp.Stop()

	_, _, _, sub, err := tp.BestBlockAndSubscribe()
	require.NoError(t, err)
	defer sub.Cancel()

	chain.advance(t, 5)

	for expected := int32(101); expected <= 105; expected++ {
		select {
		case ev := <-sub.Updates():
			require.NotNil(t, ev)
			require.Equal(
				t, expected, ev.Height,
				"events arrived out of order",
			)

		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for height %d", expected)
		}
	}
}

// TestTipPollerSubscribeCancelRace exercises the historical
// send-on-closed-channel hazard: spam Subscribe and cancel
// concurrently with active broadcasts. A pre-fix poller would
// panic; the subscribe.Server-backed implementation must not.
//
// The test runs under the race detector on CI so any latent
// race would surface.
func TestTipPollerSubscribeCancelRace(t *testing.T) {
	t.Parallel()

	chain := newStubChain(100)
	srv := mockEsploraServer(t, stubEsploraHandler(t, chain))

	tp := NewTipPoller(
		NewEsploraClient(srv.URL, btclog.Disabled), 1*time.Millisecond,
		btclog.Disabled,
	)

	require.NoError(t, tp.Start())
	defer tp.Stop()

	// Drive the chain forward continuously while Subscribe and
	// cancel race in many goroutines.
	stop := make(chan struct{})
	defer close(stop)

	go func() {
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				chain.advance(t, 1)

			case <-stop:
				return
			}
		}
	}()

	const workers = 8
	const iterations = 200

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				sub, err := tp.Subscribe()
				if err != nil {
					return
				}

				// Drain a few events then cancel mid-flight.
				select {
				case <-sub.Updates():
				case <-time.After(5 * time.Millisecond):
				}

				sub.Cancel()
			}
		}()
	}

	wg.Wait()

	// If we got here without panicking, the broadcast/cancel
	// race is fixed.
}

// TestTipPollerSlowSubscriberDoesNotWedge verifies that one slow
// subscriber that never reads its channel does not block other
// subscribers from receiving events. subscribe.Server's per-client
// queue.ConcurrentQueue is unbounded, so the slow subscriber
// accumulates a backlog without affecting fast subscribers.
func TestTipPollerSlowSubscriberDoesNotWedge(t *testing.T) {
	t.Parallel()

	chain := newStubChain(100)
	srv := mockEsploraServer(t, stubEsploraHandler(t, chain))

	tp := NewTipPoller(
		NewEsploraClient(srv.URL, btclog.Disabled), 5*time.Millisecond,
		btclog.Disabled,
	)

	require.NoError(t, tp.Start())
	defer tp.Stop()

	_, _, _, slow, err := tp.BestBlockAndSubscribe()
	require.NoError(t, err)
	defer slow.Cancel()

	_, _, _, fast, err := tp.BestBlockAndSubscribe()
	require.NoError(t, err)
	defer fast.Cancel()

	// Advance the chain. The slow subscriber never drains its
	// channel; the fast subscriber must still receive every
	// event in order.
	const advance = int32(20)
	chain.advance(t, advance)

	for expected := int32(101); expected <= 100+advance; expected++ {
		select {
		case ev := <-fast.Updates():
			require.Equal(t, expected, ev.Height)

		case <-time.After(3 * time.Second):
			t.Fatalf("fast subscriber wedged at height %d",
				expected)
		}
	}
}

// TestTipPollerBestBlockAndSubscribeAtomic verifies that the
// atomic helper either returns an old tip and delivers the next
// event, or returns the new tip and skips that event entirely.
// It never returns the new tip and ALSO delivers the same event as
// a duplicate, and never returns an old tip and FAILS to deliver
// the next event. We assert the invariant by registering a race
// between the helper and continuous tip advances and checking
// that received_event.Height > seed_height for every received
// event (no rewind, no duplicate at seed height).
func TestTipPollerBestBlockAndSubscribeAtomic(t *testing.T) {
	t.Parallel()

	chain := newStubChain(100)
	srv := mockEsploraServer(t, stubEsploraHandler(t, chain))

	tp := NewTipPoller(
		NewEsploraClient(srv.URL, btclog.Disabled), 1*time.Millisecond,
		btclog.Disabled,
	)

	require.NoError(t, tp.Start())
	defer tp.Stop()

	// Continuous advance.
	stop := make(chan struct{})
	defer close(stop)

	go func() {
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				chain.advance(t, 1)

			case <-stop:
				return
			}
		}
	}()

	for i := 0; i < 50; i++ {
		seed, _, _, sub, err := tp.BestBlockAndSubscribe()
		require.NoError(t, err)

		// Read up to one event with a tight timeout. If we
		// get one, assert it's strictly newer than seed.
		select {
		case ev := <-sub.Updates():
			require.Greater(
				t, ev.Height, seed,
				"received duplicate or stale event",
			)

		case <-time.After(50 * time.Millisecond):
			// No event in window — also fine; just unwind.
		}

		sub.Cancel()
	}
}

// TestTipPollerCancelStopsDelivery verifies that after Cancel,
// no further events are delivered. Sets up a slow consumer that
// cancels mid-stream and asserts the typed Updates channel
// observes either Quit or close after Cancel returns.
func TestTipPollerCancelStopsDelivery(t *testing.T) {
	t.Parallel()

	chain := newStubChain(100)
	srv := mockEsploraServer(t, stubEsploraHandler(t, chain))

	tp := NewTipPoller(
		NewEsploraClient(srv.URL, btclog.Disabled), 1*time.Millisecond,
		btclog.Disabled,
	)

	require.NoError(t, tp.Start())
	defer tp.Stop()

	_, _, _, sub, err := tp.BestBlockAndSubscribe()
	require.NoError(t, err)

	// Drain a few events.
	chain.advance(t, 3)

	for i := 0; i < 3; i++ {
		select {
		case <-sub.Updates():
		case <-time.After(1 * time.Second):
			t.Fatal("timed out before initial drain done")
		}
	}

	sub.Cancel()

	// After Cancel, the typed Updates channel must close eventually.
	select {
	case _, ok := <-sub.Updates():
		require.False(t, ok,
			"expected closed channel after Cancel")

	case <-time.After(2 * time.Second):
		t.Fatal("Updates channel did not close after Cancel")
	}
}

// TestTipPollerStopClosesSubscriptions verifies that stopping the
// poller propagates to every active subscription via the inner
// subscribe.Server's quit signal.
func TestTipPollerStopClosesSubscriptions(t *testing.T) {
	t.Parallel()

	chain := newStubChain(100)
	srv := mockEsploraServer(t, stubEsploraHandler(t, chain))

	tp := NewTipPoller(
		NewEsploraClient(srv.URL, btclog.Disabled), 20*time.Millisecond,
		btclog.Disabled,
	)

	require.NoError(t, tp.Start())

	subs := make([]*TipSubscription, 0, 4)
	for i := 0; i < 4; i++ {
		_, _, _, sub, err := tp.BestBlockAndSubscribe()
		require.NoError(t, err)
		subs = append(subs, sub)
	}

	tp.Stop()

	// Every sub's Updates channel must close after Stop.
	for i, sub := range subs {
		select {
		case _, ok := <-sub.Updates():
			require.False(
				t, ok, "sub %d Updates not closed after Stop",
				i,
			)

		case <-time.After(2 * time.Second):
			t.Fatalf("sub %d did not close on Stop", i)
		}
	}
}

// TestChainBackendWithPollerLifecycle verifies that when a
// ChainBackend is constructed with NewChainBackendWithPoller (i.e.
// ownsTipPoller=false), Stop on the backend does NOT stop the
// poller — the wallet that owns the poller must be the one to
// stop it.
func TestChainBackendWithPollerLifecycle(t *testing.T) {
	t.Parallel()

	chain := newStubChain(100)
	srv := mockEsploraServer(t, stubEsploraHandler(t, chain))

	esp := NewEsploraClient(srv.URL, btclog.Disabled)
	tp := NewTipPoller(esp, 20*time.Millisecond, btclog.Disabled)

	require.NoError(t, tp.Start())
	defer tp.Stop()

	be, err := NewChainBackendWithPoller(esp, tp, btclog.Disabled)
	require.NoError(t, err)
	require.NoError(t, be.Start())
	require.NoError(t, be.Stop())

	// Poller must still be alive: BestBlock returns non-zero,
	// and the chain advance + a fresh subscription must still
	// receive events.
	height, _, _ := tp.BestBlock()
	require.Equal(t, int32(100), height)

	_, _, _, sub, err := tp.BestBlockAndSubscribe()
	require.NoError(t, err)
	defer sub.Cancel()

	chain.advance(t, 1)

	select {
	case ev := <-sub.Updates():
		require.Equal(t, int32(101), ev.Height)

	case <-time.After(2 * time.Second):
		t.Fatal(
			"poller stopped after backend.Stop — " +
				"ownsTipPoller=false invariant violated",
		)
	}
}

// TestChainBackendWithPollerNilRejected verifies the H-9 nil-check
// surfaces at construction time rather than as a panic in Start.
func TestChainBackendWithPollerNilRejected(t *testing.T) {
	t.Parallel()

	srv := mockEsploraServer(
		t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		},
	)
	esp := NewEsploraClient(srv.URL, btclog.Disabled)

	be, err := NewChainBackendWithPoller(esp, nil, btclog.Disabled)
	require.Error(t, err)
	require.Nil(t, be)
}

// TestTipPollerEventsCounter sanity-checks subscribe.Server fan-out
// by comparing the count of advances driven into the chain with the
// count of events observed on a fast consumer. Used to catch a
// regression where SendUpdate silently drops events.
func TestTipPollerEventsCounter(t *testing.T) {
	t.Parallel()

	chain := newStubChain(0)
	srv := mockEsploraServer(t, stubEsploraHandler(t, chain))

	tp := NewTipPoller(
		NewEsploraClient(srv.URL, btclog.Disabled), 2*time.Millisecond,
		btclog.Disabled,
	)

	require.NoError(t, tp.Start())
	defer tp.Stop()

	_, _, _, sub, err := tp.BestBlockAndSubscribe()
	require.NoError(t, err)
	defer sub.Cancel()

	const advances = int32(25)

	var observed atomic.Int32

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case ev, ok := <-sub.Updates():
				if !ok {
					return
				}
				observed.Add(1)
				if ev.Height >= advances {
					return
				}

			case <-time.After(3 * time.Second):
				return
			}
		}
	}()

	chain.advance(t, advances)

	<-done

	require.Equal(
		t, advances, observed.Load(),
		"missed events between SendUpdate and consumer",
	)
}

//go:build systest

package systest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/harness"
	"github.com/lightninglabs/wavelength/lwwallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// countingEsplora is a reverse proxy in front of the harness Esplora
// endpoint that tallies how many requests each route class received. It
// is what makes "does the wallet walk the whole chain" an assertion
// rather than an impression.
type countingEsplora struct {
	*httptest.Server

	mu     sync.Mutex
	counts map[string]int
	paths  map[string]int
}

// newCountingEsplora spins up a counting reverse proxy in front of the
// given upstream Esplora base URL.
func newCountingEsplora(t *testing.T, upstream string) *countingEsplora {
	t.Helper()

	target, err := url.Parse(upstream)
	require.NoError(t, err)

	c := &countingEsplora{
		counts: make(map[string]int),
		paths:  make(map[string]int),
	}
	proxy := httputil.NewSingleHostReverseProxy(target)

	c.Server = httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				c.record(r.URL.Path)
				proxy.ServeHTTP(w, r)
			},
		),
	)
	t.Cleanup(c.Server.Close)

	return c
}

// record buckets one request path into a coarse route class.
func (c *countingEsplora) record(path string) {
	class := routeClass(path)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.counts[class]++
	c.counts["total"]++
	c.paths[path]++
}

// snapshot returns a copy of the current counters.
func (c *countingEsplora) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make(map[string]int, len(c.counts))
	for k, v := range c.counts {
		out[k] = v
	}

	return out
}

// blockFetches totals every route class that costs one block's worth of
// data, which is what a sequential chain walk inflates.
func (c *countingEsplora) blockFetches() int {
	counts := c.snapshot()

	return counts["block-height"] + counts["block-header"] +
		counts["block-raw"] + counts["block-json"]
}

// distinctHeights reports how many separate heights were resolved via
// /block-height/, which distinguishes a walk over a range from repeated
// lookups of the same few blocks.
func (c *countingEsplora) distinctHeights() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	n := 0
	for p := range c.paths {
		rest, ok := strings.CutPrefix(p, "/block-height/")
		if !ok {
			continue
		}

		if _, err := strconv.Atoi(rest); err == nil {
			n++
		}
	}

	return n
}

// lowestHeight returns the lowest height resolved via /block-height/,
// which is where the wallet's chain scan actually began. This is the
// property that separates "started at the wallet's first on-chain
// appearance" from "started at genesis", independently of how many
// blocks sit above that point.
func (c *countingEsplora) lowestHeight() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	lowest := -1
	for p := range c.paths {
		rest, ok := strings.CutPrefix(p, "/block-height/")
		if !ok {
			continue
		}

		h, err := strconv.Atoi(rest)
		if err != nil {
			continue
		}

		if lowest == -1 || h < lowest {
			lowest = h
		}
	}

	return lowest
}

// logCounts dumps the tally so a failure shows where the traffic went.
func (c *countingEsplora) logCounts(t *testing.T) {
	t.Helper()

	counts := c.snapshot()
	for _, k := range []string{
		"total", "block-height", "block-header", "block-raw",
		"block-json", "tip", "address", "scripthash", "other",
	} {
		t.Logf("  %-14s %d", k, counts[k])
	}

	t.Logf("  %-14s %d", "heights", c.distinctHeights())
}

// routeClass maps an Esplora path onto a stable bucket name so
// per-height scan traffic is distinguishable from tip polling and from
// index lookups.
func routeClass(path string) string {
	switch {
	case strings.HasPrefix(path, "/block-height/"):
		return "block-height"

	case strings.HasSuffix(path, "/raw"):
		return "block-raw"

	case strings.HasSuffix(path, "/header"):
		return "block-header"

	case strings.HasPrefix(path, "/blocks/tip"):
		return "tip"

	case strings.HasPrefix(path, "/block/"):
		return "block-json"

	case strings.HasPrefix(path, "/address/"):
		return "address"

	case strings.HasPrefix(path, "/scripthash/"):
		return "scripthash"

	default:
		return "other"
	}
}

// newSyncTestWallet builds an lwwallet against the given Esplora URL
// with no birthday configured, which is the input reported in issue
// #1073: a zero time.Time is January of year 1, so btcwallet's
// timestamp search for a matching block bottoms out at genesis. A
// pinned aezeed genesis birthday and a seed genuinely older than the
// chain both land in the same place.
func newSyncTestWallet(t *testing.T, esploraURL, dbDir string,
	seed []byte) *lwwallet.Wallet {

	t.Helper()

	logger := btclog.NewSLogger(btclog.NewDefaultHandler(&testWriter{t: t}))
	logger.SetLevel(btclog.LevelInfo)

	w, err := lwwallet.New(lwwallet.Config{
		Seed:           seed,
		WalletPassword: []byte("issue-1073-password"),
		EsploraURL:     esploraURL,
		ChainParams:    &chaincfg.RegressionNetParams,
		PollInterval:   time.Second,
		RecoveryWindow: 100,
		DBDir:          dbDir,
		Log:            fn.Some[btclog.Logger](logger),
	})
	require.NoError(t, err)

	return w
}

// testWriter routes wallet logs through the test log so they are
// captured per test rather than interleaved on stdout.
type testWriter struct {
	t *testing.T
}

// Write implements io.Writer.
func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", strings.TrimRight(string(p), "\n"))

	return len(p), nil
}

// TestLwWalletSyncSkipsFullChainScan is the regression test for issue
// #1073. A wallet created without a birthday used to resolve its
// birthday block to genesis and then fetch every block between there
// and the tip over Esplora. It now resolves its start height from the
// Esplora address index instead, so startup traffic is bounded by the
// address gap limit rather than by the length of the chain.
func TestLwWalletSyncSkipsFullChainScan(t *testing.T) {
	h := harness.NewHarness(t, nil)
	h.Start()
	t.Cleanup(h.Stop)

	// Grow the chain so a genesis-anchored scan is unmistakable in the
	// request tally.
	h.Generate(400)

	tipHeight := int(h.BlockCount())
	t.Logf("chain tip height: %d", tipHeight)

	esplora := newCountingEsplora(t, h.EsploraURL)

	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 7)
	}

	w := newSyncTestWallet(t, esplora.URL, t.TempDir(), seed[:])

	start := time.Now()
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	require.NoError(t, w.WaitForSync(ctx))

	t.Logf("sync took %v", time.Since(start))
	esplora.logCounts(t)

	// The bound is expressed against the chain height rather than as a
	// fixed number, so it keeps meaning the same thing as the
	// harness's pre-mine count changes. A wallet still walking the
	// chain sequentially fetches at least one block per height.
	require.Less(
		t, esplora.blockFetches(), tipHeight, "wallet fetched %d "+
			"blocks for a %d block chain, so it is still "+
			"walking the chain sequentially",
		esplora.blockFetches(), tipHeight,
	)
}

// firstReceiveAddress brings a wallet up just long enough to hand out
// one receive address, then shuts it down again. The address is what a
// later restore of the same seed has to rediscover from the index.
func firstReceiveAddress(t *testing.T, esploraURL string, seed []byte) string {
	t.Helper()

	w := newSyncTestWallet(t, esploraURL, t.TempDir(), seed)
	require.NoError(t, w.Start())

	defer w.Stop()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	require.NoError(t, w.WaitForSync(ctx))

	addr, err := w.NewAddress(ctx)
	require.NoError(t, err)

	return addr.String()
}

// TestLwWalletRestoreStartsAtFirstUse is the safety half of the fix.
// Resolving a start height from the index is only correct if it never
// lands above the wallet's own funds, so this funds a wallet at a known
// height, discards its database, grows the chain well past that point,
// and restores from the same seed.
//
// Two things must hold. The restored wallet recovers the full balance,
// which is what would break if the index-resolved height drifted too
// high. And its scan begins at the funding block rather than at genesis,
// which is the original defect. The second is asserted as the lowest
// height fetched rather than as a request count, because a wallet funded
// early legitimately has to scan everything above its funding block.
func TestLwWalletRestoreStartsAtFirstUse(t *testing.T) {
	h := harness.NewHarness(t, nil)
	h.Start()
	t.Cleanup(h.Stop)

	const fundAmount = btcutil.Amount(50_000)

	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 11)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	// Push the funding well above genesis so "started at the funding
	// block" and "started at genesis" are far apart.
	h.Generate(300)

	addr := firstReceiveAddress(t, h.EsploraURL, seed[:])

	t.Logf("funding %s with %v", addr, fundAmount)
	h.Faucet(addr, fundAmount)
	h.Generate(1)

	fundedAt := int(h.BlockCount())

	// Grow the chain past the funding so a genesis-anchored scan and a
	// first-use-anchored scan look nothing alike. The growth is derived
	// from the tip poller's history seed rather than hard coded: that
	// seed walks back DefaultHashHistorySize heights from the tip on
	// every start, and if it reached below the funding block it would
	// swamp the assertion below.
	h.Generate(lwwallet.DefaultHashHistorySize + 50)

	tipHeight := int(h.BlockCount())
	t.Logf("funded at %d, tip %d", fundedAt, tipHeight)

	// Restore into a fresh database from the same seed. This is the
	// path where a start height resolved too high silently loses the
	// funds below it.
	esplora := newCountingEsplora(t, h.EsploraURL)

	restored := newSyncTestWallet(t, esplora.URL, t.TempDir(), seed[:])
	require.NoError(t, restored.Start())
	t.Cleanup(restored.Stop)

	require.NoError(t, restored.WaitForSync(ctx))

	// btcwallet records recovered credits as its recovery batches
	// commit, which trails the sync height, so poll rather than
	// reading the balance once.
	require.Eventually(t, func() bool {
		confirmed, _, err := restored.Balance(ctx)
		if err != nil {
			t.Logf("balance error: %v", err)

			return false
		}

		return confirmed == fundAmount
	}, time.Minute, 250*time.Millisecond,
		"restored wallet never recovered the funded balance")

	esplora.logCounts(t)

	// The scan must start at the block below the funding, which is
	// where btcwallet's recovery resumes from. Allowing a little slack
	// keeps this from encoding the exact off-by-one, while still
	// failing loudly on anything that reaches back toward genesis.
	lowest := esplora.lowestHeight()
	t.Logf("lowest height fetched: %d (funded at %d)", lowest, fundedAt)

	require.GreaterOrEqual(
		t, lowest, fundedAt-3, "restore reached down to height %d "+
			"on a wallet first funded at %d, so it is scanning "+
			"below its own first use", lowest, fundedAt,
	)
}

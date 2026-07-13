package lwwallet

import (
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// mintStubHeaderGen is like mintStubHeader but mixes a fork generation
// into the salt so re-minting the same height on a forked chain yields
// a distinct BlockHash. This lets a test rebuild a height range on a
// different chain whose hashes provably diverge from the original.
func mintStubHeaderGen(height, gen int32,
	prev chainhash.Hash) *wire.BlockHeader {

	salt := chainhash.HashH(
		fmt.Appendf(nil, "stub-block-%d-gen-%d", height, gen),
	)

	return &wire.BlockHeader{
		Version:   1,
		PrevBlock: prev,
		MerkleRoot: chainhash.HashH(
			fmt.Appendf(nil, "merkle-%d-gen-%d", height, gen),
		),
		Timestamp: time.Unix(int64(height)*600, 0),
		Bits:      0x207fffff,
		Nonce: uint32(salt[0])<<24 | uint32(salt[1])<<16 |
			uint32(salt[2])<<8 | uint32(salt[3]),
	}
}

// reorgTo rewrites the chain onto a fork that branches at forkHeight and
// extends to newTip. newTip may be lower than the current tip (a shorter
// but higher-work chain), equal to it (a same-height hash replacement),
// or higher (a deeper forward reorg). Heights above forkHeight are
// replaced with generation-salted blocks whose hashes diverge from the
// old chain; any old heights above newTip are orphaned (removed) so the
// stubbed backend no longer serves them.
func (c *stubChain) reorgTo(t *testing.T, forkHeight, newTip, gen int32) {
	t.Helper()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Drop every height above the fork point; the new chain rebuilds
	// forkHeight+1 .. newTip and orphans anything beyond newTip.
	for h := range c.hashAt {
		if h > forkHeight {
			delete(c.hashAt, h)
			delete(c.blocks, h)
		}
	}

	prev := c.hashAt[forkHeight]
	for h := forkHeight + 1; h <= newTip; h++ {
		hdr := mintStubHeaderGen(h, gen, prev)
		c.blocks[h] = hdr
		hash := hdr.BlockHash()
		c.hashAt[h] = hash
		prev = hash
	}

	c.tipHeight = newTip
}

// setTipHeight lowers (or raises) the reported tip height WITHOUT
// touching any cached hashes. It models a transient indexer hiccup
// where the remote momentarily reports fewer blocks than it served a
// moment ago, but is still on the same chain.
func (c *stubChain) setTipHeight(h int32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.tipHeight = h
}

// requireTipEventually blocks until the poller's BestBlock height
// reaches want, failing the test if it does not within the deadline.
func requireTipEventually(t *testing.T, tp *TipPoller, want int32) {
	t.Helper()

	require.Eventually(t, func() bool {
		h, _, _ := tp.BestBlock()

		return h == want
	}, 2*time.Second, 5*time.Millisecond,
		"poller never reached tip height %d", want)
}

// newReorgTestPoller spins up a TipPoller over a stubChain seeded at the
// given height and returns both so a test can drive reorgs.
func newReorgTestPoller(t *testing.T,
	seedHeight int32) (*TipPoller, *stubChain) {

	t.Helper()

	chain := newStubChain(seedHeight)
	srv := mockEsploraServer(t, stubEsploraHandler(t, chain))

	tp := NewTipPoller(
		NewEsploraClient(srv.URL, btclog.Disabled), 10*time.Millisecond,
		btclog.Disabled,
	)
	require.NoError(t, tp.Start())
	t.Cleanup(tp.Stop)

	return tp, chain
}

// TestTipPollerShorterChainReorg covers the case Roasbeef flagged: a
// reorg onto a SHORTER but higher-work chain, where blocks above the new
// tip are orphaned. The poller must detect this even though the remote
// reports a lower height than the cached tip, and roll the tip back to
// the shorter chain's tip.
func TestTipPollerShorterChainReorg(t *testing.T) {
	t.Parallel()

	tp, chain := newReorgTestPoller(t, 100)

	reorgSub, err := tp.SubscribeReorgs()
	require.NoError(t, err)
	defer reorgSub.Cancel()

	// Grow to 110 so the poller caches 101..110, then confirm it
	// observed the advance before we reorg out from under it.
	chain.advance(t, 10)
	requireTipEventually(t, tp, 110)

	// Fork at 105 onto a shorter chain that tips at 108 (< 110).
	chain.reorgTo(t, 105, 108, 1)

	select {
	case ev := <-reorgSub.Updates():
		require.NotNil(t, ev)
		require.Equal(t, int32(105), ev.ForkHeight)

		// Old chain 106..110 disconnects (5 blocks), even though
		// the new chain only carries 106..108.
		require.Len(t, ev.Disconnected, 5)

		// New chain connects 106..108 (3 blocks) in ascending
		// order, tipping at 108.
		require.Len(t, ev.Connected, 3)
		require.Equal(
			t, int32(106), ev.Connected[0].Height,
		)
		require.Equal(
			t, int32(108), ev.Connected[len(ev.Connected)-1].Height,
		)

	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for shorter-chain reorg event")
	}

	// The poller's tip must now be the shorter chain's tip.
	requireTipEventually(t, tp, 108)
}

// TestTipPollerShorterHeightSameChainNoReorg verifies the no-op half of
// the shorter-height branch: a transient indexer hiccup where the remote
// reports fewer blocks but is still on the same chain (the hash at the
// reported height matches our cache). No reorg must fire and the cached
// tip must NOT roll back.
func TestTipPollerShorterHeightSameChainNoReorg(t *testing.T) {
	t.Parallel()

	tp, chain := newReorgTestPoller(t, 100)

	reorgSub, err := tp.SubscribeReorgs()
	require.NoError(t, err)
	defer reorgSub.Cancel()

	chain.advance(t, 10)
	requireTipEventually(t, tp, 110)

	// Remote briefly reports height 108 with the SAME hash history
	// (no fork). This is lag, not a reorg.
	chain.setTipHeight(108)

	select {
	case ev := <-reorgSub.Updates():
		t.Fatalf("unexpected reorg on transient lag: %+v", ev)

	case <-time.After(300 * time.Millisecond):
		// No reorg fired: correct.
	}

	// The tip must remain at 110: we never roll back on a lagging
	// remote that is still on our chain.
	h, _, _ := tp.BestBlock()
	require.Equal(t, int32(110), h)
}

// TestTipPollerSameHeightReorg covers a same-height hash replacement: the
// tip height does not change but the block at the tip is replaced by a
// different block on a competing chain.
func TestTipPollerSameHeightReorg(t *testing.T) {
	t.Parallel()

	tp, chain := newReorgTestPoller(t, 100)

	reorgSub, err := tp.SubscribeReorgs()
	require.NoError(t, err)
	defer reorgSub.Cancel()

	chain.advance(t, 10)
	requireTipEventually(t, tp, 110)

	oldHash := chain.hashAt[110]

	// Replace block 110 with a different block at the same height.
	chain.reorgTo(t, 109, 110, 1)

	select {
	case ev := <-reorgSub.Updates():
		require.NotNil(t, ev)
		require.Equal(t, int32(109), ev.ForkHeight)
		require.Len(t, ev.Disconnected, 1)
		require.Equal(t, oldHash, ev.Disconnected[0])
		require.Len(t, ev.Connected, 1)
		require.Equal(t, int32(110), ev.Connected[0].Height)

	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for same-height reorg event")
	}

	requireTipEventually(t, tp, 110)
}

// TestTipPollerDeeperForwardReorg covers a reorg that also extends the
// chain: the new chain forks below the old tip yet ends higher than it.
// The forward-advance path must notice the broken PrevBlock continuity
// at the old tip boundary and emit a reorg before the new tip events.
func TestTipPollerDeeperForwardReorg(t *testing.T) {
	t.Parallel()

	tp, chain := newReorgTestPoller(t, 100)

	reorgSub, err := tp.SubscribeReorgs()
	require.NoError(t, err)
	defer reorgSub.Cancel()

	chain.advance(t, 10)
	requireTipEventually(t, tp, 110)

	// Fork at 107 onto a longer chain that tips at 113.
	chain.reorgTo(t, 107, 113, 1)

	select {
	case ev := <-reorgSub.Updates():
		require.NotNil(t, ev)
		require.Equal(t, int32(107), ev.ForkHeight)

		// Old chain 108..110 disconnects (3 blocks).
		require.Len(t, ev.Disconnected, 3)

		// New chain connects 108..113 (6 blocks).
		require.Len(t, ev.Connected, 6)
		require.Equal(
			t, int32(113), ev.Connected[len(ev.Connected)-1].Height,
		)

	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for deeper forward reorg event")
	}

	requireTipEventually(t, tp, 113)
}

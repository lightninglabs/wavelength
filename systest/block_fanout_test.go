//go:build systest

package systest

import (
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/stretchr/testify/require"
)

// TestLNDBlockFanoutReorg checks the shared stream against real lnd gRPC:
// initial tip, live blocks, and same-height replacement blocks must carry the
// hashes reported by bitcoind, in order, for every subscriber.
func TestLNDBlockFanoutReorg(t *testing.T) {
	ParallelN(t)
	h := NewSysTestHarness(t)
	backend := h.NewChainBackend()
	require.NoError(t, backend.Start())
	t.Cleanup(func() { require.NoError(t, backend.Stop()) })

	tip := h.Harness.BestBlockHeader()
	var subs []*chainsource.BlockRegistration
	for range 32 {
		sub, err := backend.RegisterBlocks(h.Context())
		require.NoError(t, err)
		subs = append(subs, sub)
	}
	assertBlock := func(height int32, hash string) {
		t.Helper()
		for _, sub := range subs {
			select {
			case epoch, ok := <-sub.Epochs:
				require.True(t, ok)
				require.Equal(t, height, epoch.Height)
				require.Equal(t, hash, epoch.Hash.String())

			case <-time.After(reorgSystestEventTimeout):
				t.Fatal("block fanout timed out")
			}
		}
	}
	assertBlock(int32(tip.Height), tip.Hash)
	for _, block := range h.Harness.Generate(2) {
		assertBlock(int32(block.Height), block.Hash)
	}
	for _, block := range h.Harness.Reorg(1, 2).Connected {
		assertBlock(int32(block.Height), block.Hash)
	}

	// Explicit cancellation must release one subscriber without ending the
	// shared stream. The remaining subscribers still see the next block.
	subs[0].Cancel()
	subs = subs[1:]
	block := h.Harness.Generate(1)[0]
	assertBlock(int32(block.Height), block.Hash)
}

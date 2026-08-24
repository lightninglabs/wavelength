package chainbackends

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/stretchr/testify/require"
)

// backendNotifierTestBackend provides the block methods exercised by the lnd
// notifier adapter. The embedded interface keeps unrelated backend methods out
// of this focused contract test.
type backendNotifierTestBackend struct {
	chainsource.ChainBackend

	height   int32
	hash     chainhash.Hash
	epochs   chan *chainsource.BlockEpoch
	canceled atomic.Bool
}

// BestBlock returns the fixed registration tip.
func (b *backendNotifierTestBackend) BestBlock(context.Context) (int32,
	chainhash.Hash, error) {

	return b.height, b.hash, nil
}

// RegisterBlocks returns a stream that does not seed its current tip, matching
// the production backend contract that the adapter must bridge.
func (b *backendNotifierTestBackend) RegisterBlocks(context.Context) (
	*chainsource.BlockRegistration, error) {

	return &chainsource.BlockRegistration{
		Epochs: b.epochs,
		Cancel: func() {
			b.canceled.Store(true)
		},
	}, nil
}

// TestBackendChainNotifierSeedsCurrentTip verifies lnd can use registration as
// a startup barrier even when no new block arrives after the daemon starts.
func TestBackendChainNotifierSeedsCurrentTip(t *testing.T) {
	t.Parallel()

	backend := &backendNotifierTestBackend{
		height: 133,
		hash: chainhash.Hash{
			1,
			3,
			3,
			7,
		},
		epochs: make(chan *chainsource.BlockEpoch, 2),
	}
	notifier, err := NewBackendChainNotifier(backend)
	require.NoError(t, err)
	event, err := notifier.RegisterBlockEpochNtfn(nil)
	require.NoError(t, err)
	t.Cleanup(event.Cancel)

	select {
	case epoch := <-event.Epochs:
		require.Equal(t, backend.height, epoch.Height)
		require.Equal(t, backend.hash, *epoch.Hash)

	case <-time.After(time.Second):
		t.Fatal("current block epoch was not delivered")
	}

	// A backend may also seed the same tip. The adapter suppresses that
	// duplicate while preserving the next connected block.
	backend.epochs <- &chainsource.BlockEpoch{
		Height: backend.height, Hash: backend.hash,
	}
	nextHash := chainhash.Hash{1, 3, 3, 8}
	backend.epochs <- &chainsource.BlockEpoch{
		Height: backend.height + 1, Hash: nextHash,
	}

	select {
	case epoch := <-event.Epochs:
		require.Equal(t, backend.height+1, epoch.Height)
		require.Equal(t, nextHash, *epoch.Hash)

	case <-time.After(time.Second):
		t.Fatal("next block epoch was not delivered")
	}
}

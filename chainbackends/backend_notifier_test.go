package chainbackends

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightningnetwork/lnd/chainntnfs"
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

	confHint      atomic.Uint32
	spendHint     atomic.Uint32
	confCanceled  atomic.Bool
	spendCanceled atomic.Bool
	confContext   chan context.Context
	spendContext  chan context.Context
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

// RegisterConf captures the clamped hint and returns an idle registration.
func (b *backendNotifierTestBackend) RegisterConf(ctx context.Context,
	_ *chainhash.Hash, _ []byte, _ uint32, heightHint uint32, _ bool) (
	*chainsource.ConfRegistration, error) {

	b.confHint.Store(heightHint)
	if b.confContext != nil {
		b.confContext <- ctx
	}

	return &chainsource.ConfRegistration{
		Confirmed: make(chan *chainsource.TxConfirmation),
		Reorged:   make(chan uint64),
		Done:      make(chan struct{}),
		Cancel: func() {
			b.confCanceled.Store(true)
		},
	}, nil
}

// RegisterSpend captures the clamped hint and returns an idle registration.
func (b *backendNotifierTestBackend) RegisterSpend(ctx context.Context,
	_ *wire.OutPoint, _ []byte, heightHint uint32) (
	*chainsource.SpendRegistration, error) {

	b.spendHint.Store(heightHint)
	if b.spendContext != nil {
		b.spendContext <- ctx
	}

	return &chainsource.SpendRegistration{
		Spend:   make(chan *chainsource.SpendDetail),
		Reorged: make(chan uint64),
		Done:    make(chan struct{}),
		Cancel: func() {
			b.spendCanceled.Store(true)
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

// TestBackendChainNotifierSkipsPreSnapshotEpochs verifies queued blocks cannot
// regress the stream after the adapter seeds its registration snapshot.
func TestBackendChainNotifierSkipsPreSnapshotEpochs(t *testing.T) {
	t.Parallel()

	backend := &backendNotifierTestBackend{
		height: 135,
		hash: chainhash.Hash{
			1,
			3,
			5,
		},
		epochs: make(chan *chainsource.BlockEpoch, 3),
	}
	backend.epochs <- &chainsource.BlockEpoch{
		Height: 134, Hash: chainhash.Hash{
			1,
			3,
			4,
		},
	}
	backend.epochs <- &chainsource.BlockEpoch{
		Height: backend.height, Hash: backend.hash,
	}

	notifier, err := NewBackendChainNotifier(backend)
	require.NoError(t, err)
	event, err := notifier.RegisterBlockEpochNtfn(&chainntnfs.BlockEpoch{
		Height: 133, Hash: &chainhash.Hash{1, 3, 3},
	})
	require.NoError(t, err)
	t.Cleanup(event.Cancel)

	seed := receiveBlockEpoch(t, event.Epochs)
	require.Equal(t, backend.height, seed.Height)
	require.Equal(t, backend.hash, *seed.Hash)

	reorgHash := chainhash.Hash{9, 9, 9}
	backend.epochs <- &chainsource.BlockEpoch{
		Height: backend.height, Hash: reorgHash,
	}
	reorg := receiveBlockEpoch(t, event.Epochs)
	require.Equal(t, backend.height, reorg.Height)
	require.Equal(t, reorgHash, *reorg.Hash)
}

// TestBackendChainNotifierClampsHeightHints verifies a hash-derived virtual
// SCID cannot move a historical chain scan above the backend's current tip.
func TestBackendChainNotifierClampsHeightHints(t *testing.T) {
	t.Parallel()

	backend := &backendNotifierTestBackend{
		height: 144,
		epochs: make(chan *chainsource.BlockEpoch),
	}
	notifier, err := NewBackendChainNotifier(backend)
	require.NoError(t, err)

	confirmation, err := notifier.RegisterConfirmationsNtfn(
		&chainhash.Hash{1}, []byte{2}, 1, 16_000_000,
	)
	require.NoError(t, err)
	t.Cleanup(confirmation.Cancel)
	spend, err := notifier.RegisterSpendNtfn(
		&wire.OutPoint{
			Hash: chainhash.Hash{3},
		},
		[]byte{4},
		16_000_001,
	)
	require.NoError(t, err)
	t.Cleanup(spend.Cancel)

	require.Equal(t, uint32(backend.height), backend.confHint.Load())
	require.Equal(t, uint32(backend.height), backend.spendHint.Load())
}

// TestBackendChainNotifierStopCancelsRegistrations verifies Stop owns adapter
// workers even though the embedding wallet keeps the shared backend running.
func TestBackendChainNotifierStopCancelsRegistrations(t *testing.T) {
	t.Parallel()

	backend := &backendNotifierTestBackend{
		height:       144,
		epochs:       make(chan *chainsource.BlockEpoch),
		confContext:  make(chan context.Context, 1),
		spendContext: make(chan context.Context, 1),
	}
	notifier, err := NewBackendChainNotifier(backend)
	require.NoError(t, err)
	_, err = notifier.RegisterConfirmationsNtfn(
		&chainhash.Hash{1}, []byte{2}, 1, 1,
	)
	require.NoError(t, err)
	_, err = notifier.RegisterSpendNtfn(
		&wire.OutPoint{
			Hash: chainhash.Hash{3},
		},
		[]byte{4},
		1,
	)
	require.NoError(t, err)
	blockEvent, err := notifier.RegisterBlockEpochNtfn(nil)
	require.NoError(t, err)

	confCtx := <-backend.confContext
	spendCtx := <-backend.spendContext
	require.NoError(t, notifier.Stop())
	require.False(t, notifier.Started())
	require.True(t, backend.confCanceled.Load())
	require.True(t, backend.spendCanceled.Load())
	require.True(t, backend.canceled.Load())
	require.Error(t, confCtx.Err())
	require.Error(t, spendCtx.Err())
	for range blockEvent.Epochs {
	}

	_, err = notifier.RegisterSpendNtfn(nil, nil, 0)
	require.ErrorContains(t, err, "stopped")
	require.ErrorContains(t, notifier.Start(), "stopped")
}

// TestForwardBackendConfirmationsOrdersReorgs verifies confirmation and reorg
// channels retain the backend's shared sequence order.
func TestForwardBackendConfirmationsOrdersReorgs(t *testing.T) {
	t.Parallel()

	confirmed := make(chan *chainsource.TxConfirmation)
	reorged := make(chan uint64)
	registration := &chainsource.ConfRegistration{
		Confirmed: confirmed, Reorged: reorged,
		Done: make(chan struct{}), Cancel: func() {},
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	event := chainntnfs.NewConfirmationEvent(1, cancel)
	go forwardBackendConfirmations(ctx, registration, event)

	confirmed <- &chainsource.TxConfirmation{Seq: 3}
	require.NotNil(t, <-event.Confirmed)
	reorged <- 2
	reorged <- 4
	require.Equal(t, int32(1), <-event.NegativeConf)
	confirmed <- &chainsource.TxConfirmation{Seq: 3}
	confirmed <- &chainsource.TxConfirmation{Seq: 5}
	require.NotNil(t, <-event.Confirmed)
}

// TestForwardBackendSpendsOrdersReorgs verifies spend and reorg channels retain
// the backend's shared sequence order.
func TestForwardBackendSpendsOrdersReorgs(t *testing.T) {
	t.Parallel()

	spends := make(chan *chainsource.SpendDetail)
	reorged := make(chan uint64)
	registration := &chainsource.SpendRegistration{
		Spend: spends, Reorged: reorged,
		Done: make(chan struct{}), Cancel: func() {},
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	event := chainntnfs.NewSpendEvent(cancel)
	go forwardBackendSpends(ctx, registration, event)

	spends <- &chainsource.SpendDetail{Seq: 3}
	require.NotNil(t, <-event.Spend)
	reorged <- 2
	reorged <- 4
	<-event.Reorg
	spends <- &chainsource.SpendDetail{Seq: 3}
	spends <- &chainsource.SpendDetail{Seq: 5}
	require.NotNil(t, <-event.Spend)
}

// receiveBlockEpoch waits for one block epoch without allowing a hung test.
func receiveBlockEpoch(t *testing.T,
	epochs <-chan *chainntnfs.BlockEpoch) *chainntnfs.BlockEpoch {

	t.Helper()

	select {
	case epoch := <-epochs:
		return epoch

	case <-time.After(time.Second):
		t.Fatal("block epoch was not delivered")

		return nil
	}
}

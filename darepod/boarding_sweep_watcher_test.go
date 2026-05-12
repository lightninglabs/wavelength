package darepod

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// TestBoardingSweepWatcherResolvesPublishedSweep verifies the watcher reloads
// a published sweep, rebroadcasts its raw transaction, watches its input, and
// marks the boarding intent swept after a confirmed spend.
func TestBoardingSweepWatcherResolvesPublishedSweep(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newBoardingSweepWatcherStore(t)

	sweep := createTestBoardingSweep(
		t, ctx, store, db.BoardingSweepStatusPublished,
	)

	backend := newFakeBoardingSweepBackend()
	watcher := newBoardingSweepWatcher(
		store, backend, btclog.Disabled, time.Hour,
	)
	require.NoError(t, watcher.Start(ctx))
	defer watcher.Stop()

	require.Eventually(t, func() bool {
		return backend.broadcastCount() == 1 &&
			backend.spendChan(sweep.intent.Outpoint) != nil
	}, time.Second, 10*time.Millisecond)

	require.NoError(t, watcher.Refresh(ctx))
	require.Equal(t, 1, backend.broadcastCount())

	backend.spendChan(sweep.intent.Outpoint) <- &chainsource.SpendDetail{
		SpentOutPoint:     &sweep.intent.Outpoint,
		SpenderTxHash:     &sweep.txid,
		SpendingTx:        sweep.tx,
		SpendingHeight:    200,
		SpenderInputIndex: 0,
	}

	require.Eventually(t, func() bool {
		updated, err := store.GetIntent(ctx, sweep.intent.Outpoint)
		require.NoError(t, err)

		return updated.Status == wallet.BoardingStatusSwept
	}, time.Second, 10*time.Millisecond)

	pending, err := store.ListPendingBoardingSweeps(ctx)
	require.NoError(t, err)
	require.Empty(t, pending)
}

// TestBoardingSweepWatcherPublishesPendingSweep verifies a sweep persisted
// before a crash is rebroadcast and promoted to published on watcher startup.
func TestBoardingSweepWatcherPublishesPendingSweep(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newBoardingSweepWatcherStore(t)
	createTestBoardingSweep(t, ctx, store, db.BoardingSweepStatusPending)

	backend := newFakeBoardingSweepBackend()
	watcher := newBoardingSweepWatcher(
		store, backend, btclog.Disabled, time.Hour,
	)
	require.NoError(t, watcher.Start(ctx))
	defer watcher.Stop()

	require.Eventually(t, func() bool {
		sweeps, err := store.ListBoardingSweeps(
			ctx, db.BoardingSweepStatusPublished, 10, 0,
		)
		require.NoError(t, err)

		return backend.broadcastCount() == 1 && len(sweeps) == 1
	}, time.Second, 10*time.Millisecond)
}

// TestBoardingSweepWatcherRefreshIsIdempotent verifies repeated refreshes do
// not create duplicate watches or rebroadcast published sweeps within backoff.
func TestBoardingSweepWatcherRefreshIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newBoardingSweepWatcherStore(t)
	sweep := createTestBoardingSweep(
		t, ctx, store, db.BoardingSweepStatusPublished,
	)

	backend := newFakeBoardingSweepBackend()
	watcher := newBoardingSweepWatcher(
		store, backend, btclog.Disabled, time.Hour,
	)
	require.NoError(t, watcher.Start(ctx))
	defer watcher.Stop()

	require.Eventually(t, func() bool {
		return backend.broadcastCount() == 1 &&
			backend.registerCount() == 1 &&
			backend.spendChan(sweep.intent.Outpoint) != nil
	}, time.Second, 10*time.Millisecond)

	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			errs <- watcher.Refresh(ctx)
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	require.Equal(t, 1, backend.broadcastCount())
	require.Equal(t, 1, backend.registerCount())
}

// newBoardingSweepWatcherStore creates a migrated boarding store for watcher
// tests.
func newBoardingSweepWatcherStore(t *testing.T) *db.BoardingWalletStore {
	t.Helper()

	testDB := db.NewTestDB(t)
	sqlStore := db.NewStore(
		testDB.DB, testDB.Queries, testDB.Backend(), btclog.Disabled,
	)

	return sqlStore.NewBoardingStore(
		&chaincfg.RegressionNetParams, clock.NewDefaultClock(),
	)
}

// testBoardingSweepState groups a persisted sweep and its single input.
type testBoardingSweepState struct {
	intent wallet.BoardingIntent
	tx     *wire.MsgTx
	txid   chainhash.Hash
}

// createTestBoardingSweep persists one sweep in the requested status.
func createTestBoardingSweep(t *testing.T, ctx context.Context,
	store *db.BoardingWalletStore, status string) testBoardingSweepState {

	t.Helper()

	intent := testBoardingSweepIntent(t, 50_000, 100, 10)
	require.NoError(t, store.InsertBoardingAddress(ctx, &intent.Address))
	require.NoError(t, store.InsertBoardingIntents(ctx, intent))

	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: intent.Outpoint,
	})
	sweepTx.AddTxOut(&wire.TxOut{
		Value:    49_000,
		PkScript: []byte{txscript.OP_TRUE},
	})
	sweepTxid := sweepTx.TxHash()

	require.NoError(
		t,
		store.CreatePendingBoardingSweep(
			ctx, db.NewBoardingSweep{
				Tx:                 sweepTx,
				TotalAmount:        intent.ChainInfo.Amount,
				FeeAmount:          1_000,
				FeeRateSatPerVByte: 2,
				VBytes:             500,
				CreatedHeight:      120,
				Inputs: []db.NewBoardingSweepInput{{
					Outpoint:       intent.Outpoint,
					Amount:         intent.ChainInfo.Amount,
					PreviousStatus: intent.Status,
				}},
			},
		),
	)
	if status == db.BoardingSweepStatusPublished {
		require.NoError(
			t, store.MarkBoardingSweepPublished(
				ctx, sweepTxid,
			),
		)
	}

	return testBoardingSweepState{
		intent: intent,
		tx:     sweepTx,
		txid:   sweepTxid,
	}
}

// fakeBoardingSweepBackend is a minimal chain backend for watcher tests.
type fakeBoardingSweepBackend struct {
	mu         sync.Mutex
	broadcasts int
	registers  int
	spends     map[wire.OutPoint]chan *chainsource.SpendDetail
	cancels    map[wire.OutPoint]*sync.Once
}

// newFakeBoardingSweepBackend creates a test chain backend.
func newFakeBoardingSweepBackend() *fakeBoardingSweepBackend {
	return &fakeBoardingSweepBackend{
		spends:  make(map[wire.OutPoint]chan *chainsource.SpendDetail),
		cancels: make(map[wire.OutPoint]*sync.Once),
	}
}

// EstimateFee returns a fixed test fee rate.
func (f *fakeBoardingSweepBackend) EstimateFee(context.Context, uint32) (
	btcutil.Amount, error) {

	return 1, nil
}

// BestBlock returns a fixed test tip.
func (f *fakeBoardingSweepBackend) BestBlock(context.Context) (int32,
	chainhash.Hash, error) {

	return 200, chainhash.Hash{}, nil
}

// TestMempoolAccept is unused by the watcher.
func (f *fakeBoardingSweepBackend) TestMempoolAccept(context.Context,
	...*wire.MsgTx) ([]chainsource.MempoolAcceptResult, error) {

	return nil, nil
}

// BroadcastTx records one broadcast attempt.
func (f *fakeBoardingSweepBackend) BroadcastTx(context.Context, *wire.MsgTx,
	string) error {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.broadcasts++

	return nil
}

// RegisterConf is unused by the watcher.
func (f *fakeBoardingSweepBackend) RegisterConf(context.Context,
	*chainhash.Hash, []byte, uint32, uint32, bool) (
	*chainsource.ConfRegistration, error) {

	return nil, nil
}

// RegisterSpend records one spend watch channel by outpoint.
func (f *fakeBoardingSweepBackend) RegisterSpend(_ context.Context,
	outpoint *wire.OutPoint, _ []byte, _ uint32) (
	*chainsource.SpendRegistration, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.registers++
	ch := make(chan *chainsource.SpendDetail, 1)
	f.spends[*outpoint] = ch
	once := &sync.Once{}
	f.cancels[*outpoint] = once

	return &chainsource.SpendRegistration{
		Spend: ch,
		Cancel: func() {
			once.Do(func() {
				close(ch)
			})
		},
	}, nil
}

// RegisterBlocks is unused by the watcher.
func (f *fakeBoardingSweepBackend) RegisterBlocks(context.Context) (
	*chainsource.BlockRegistration, error) {

	return nil, nil
}

// SubmitPackage is unused by the watcher.
func (f *fakeBoardingSweepBackend) SubmitPackage(context.Context, []*wire.MsgTx,
	*wire.MsgTx) error {

	return nil
}

// Start is a no-op for the test backend.
func (f *fakeBoardingSweepBackend) Start() error {
	return nil
}

// Stop is a no-op for the test backend.
func (f *fakeBoardingSweepBackend) Stop() error {
	return nil
}

// broadcastCount returns the number of recorded broadcast attempts.
func (f *fakeBoardingSweepBackend) broadcastCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.broadcasts
}

// registerCount returns the number of recorded spend registrations.
func (f *fakeBoardingSweepBackend) registerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.registers
}

// spendChan returns the registered spend channel for an outpoint.
func (f *fakeBoardingSweepBackend) spendChan(
	outpoint wire.OutPoint) chan *chainsource.SpendDetail {

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.spends[outpoint]
}

var _ chainsource.ChainBackend = (*fakeBoardingSweepBackend)(nil)

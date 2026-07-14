package chainsource

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// mockBackend implements ChainBackend for testing.
type mockBackend struct {
	confChan  chan *TxConfirmation
	spendChan chan *SpendDetail
	epochChan chan *BlockEpoch

	epochCancel chan struct{}

	feeRate    btcutil.Amount
	bestHeight int32
	bestHash   chainhash.Hash
}

type blockEpochRegistration struct {
	epochs chan *BlockEpoch
	cancel chan struct{}
}

type reconnectBlockBackend struct {
	*mockBackend

	registrations chan *blockEpochRegistration
}

// newMockBackend creates a new mock backend for testing.
func newMockBackend() *mockBackend {
	return &mockBackend{
		confChan:    make(chan *TxConfirmation, 1),
		spendChan:   make(chan *SpendDetail, 1),
		epochChan:   make(chan *BlockEpoch, 10),
		epochCancel: make(chan struct{}, 10),
		feeRate:     1000,
		bestHeight:  100,
	}
}

func newReconnectBlockBackend() *reconnectBlockBackend {
	return &reconnectBlockBackend{
		mockBackend:   newMockBackend(),
		registrations: make(chan *blockEpochRegistration, 10),
	}
}

func (m *reconnectBlockBackend) RegisterBlocks(ctx context.Context) (
	*BlockRegistration, error) {

	reg := &blockEpochRegistration{
		epochs: make(chan *BlockEpoch, 10),
		cancel: make(chan struct{}, 1),
	}

	select {
	case m.registrations <- reg:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return &BlockRegistration{
		Epochs: reg.epochs,
		Cancel: func() {
			select {
			case reg.cancel <- struct{}{}:
			default:
			}
		},
	}, nil
}

func (m *reconnectBlockBackend) nextRegistration(
	t *testing.T) *blockEpochRegistration {

	t.Helper()

	select {
	case reg := <-m.registrations:
		return reg

	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for block registration")
	}

	return nil
}

func (r *blockEpochRegistration) send(height int32) chainhash.Hash {
	hash := chainhash.Hash{}
	hash[0] = byte(height)
	hash[1] = byte(height >> 8)

	r.epochs <- &BlockEpoch{
		Height:    height,
		Hash:      hash,
		Timestamp: 0,
	}

	return hash
}

func (r *blockEpochRegistration) assertCanceled(t *testing.T) {
	t.Helper()

	select {
	case <-r.cancel:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for registration cancel")
	}
}

func (m *mockBackend) EstimateFee(ctx context.Context, targetConf uint32) (
	btcutil.Amount, error) {

	return m.feeRate, nil
}

func (m *mockBackend) BestBlock(ctx context.Context) (int32, chainhash.Hash,
	error) {

	return m.bestHeight, m.bestHash, nil
}

func (m *mockBackend) TestMempoolAccept(_ context.Context, txs ...*wire.MsgTx) (
	[]MempoolAcceptResult, error) {

	results := make([]MempoolAcceptResult, len(txs))
	for i, tx := range txs {
		results[i] = MempoolAcceptResult{
			Txid:     tx.TxHash(),
			Accepted: true,
		}
	}

	return results, nil
}

func (m *mockBackend) BroadcastTx(ctx context.Context, tx *wire.MsgTx,
	label string) error {

	return nil
}

// SubmitPackage is a no-op for the mock backend.
func (m *mockBackend) SubmitPackage(ctx context.Context, parents []*wire.MsgTx,
	child *wire.MsgTx) error {

	return nil
}

// broadcastErrorBackend is a mock backend that allows injecting broadcast
// errors and mempool accept behavior while reusing the base mock backend for
// other operations.
type broadcastErrorBackend struct {
	*mockBackend

	broadcastErr error

	mempoolAccepted bool
	mempoolReason   string
	mempoolErr      error
}

// BroadcastTx returns the configured error for testing error handling paths.
func (b *broadcastErrorBackend) BroadcastTx(ctx context.Context, tx *wire.MsgTx,
	label string) error {

	return b.broadcastErr
}

// TestMempoolAccept returns the configured mempool acceptance values.
func (b *broadcastErrorBackend) TestMempoolAccept(_ context.Context,
	txs ...*wire.MsgTx) ([]MempoolAcceptResult, error) {

	if b.mempoolErr != nil {
		return nil, b.mempoolErr
	}

	results := make([]MempoolAcceptResult, len(txs))
	for i, tx := range txs {
		results[i] = MempoolAcceptResult{
			Txid:     tx.TxHash(),
			Accepted: b.mempoolAccepted,
			Reason:   b.mempoolReason,
		}
	}

	return results, nil
}

func (m *mockBackend) RegisterConf(ctx context.Context, txid *chainhash.Hash,
	pkScript []byte, numConfs uint32, heightHint uint32,
	includeBlock bool) (*ConfRegistration, error) {

	return &ConfRegistration{
		Confirmed: m.confChan,
		Cancel:    func() {},
	}, nil
}

func (m *mockBackend) RegisterSpend(ctx context.Context,
	outpoint *wire.OutPoint, pkScript []byte, heightHint uint32) (
	*SpendRegistration, error) {

	return &SpendRegistration{
		Spend:  m.spendChan,
		Cancel: func() {},
	}, nil
}

func (m *mockBackend) RegisterBlocks(ctx context.Context) (*BlockRegistration,
	error) {

	return &BlockRegistration{
		Epochs: m.epochChan,
		Cancel: func() {
			select {
			case m.epochCancel <- struct{}{}:
			default:
			}
		},
	}, nil
}

func (m *mockBackend) Start() error {
	return nil
}

func (m *mockBackend) Stop() error {
	close(m.confChan)
	close(m.spendChan)
	close(m.epochChan)

	return nil
}

// TestChainSourceActorFeeEstimate tests fee estimation through the ChainSource
// actor.
func TestChainSourceActorFeeEstimate(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	system := actor.NewActorSystem()
	defer func() {
		_ = system.Shutdown(t.Context())
	}()

	chainSource := NewChainSourceActor(ChainSourceConfig{
		Backend: backend,
		System:  system,
	})
	ref := ChainSourceKey.Spawn(
		system, "chainsource-1", chainSource,
	)

	ctx := t.Context()
	future := ref.Ask(ctx, &FeeEstimateRequest{TargetConf: 6})

	result := future.Await(ctx)
	require.True(t, result.IsOk())

	resp, err := result.Unpack()
	require.NoError(t, err)
	feeResp, ok := resp.(*FeeEstimateResponse)
	require.True(t, ok)
	require.Equal(t, backend.feeRate, feeResp.SatPerVByte)
}

// TestChainSourceActorTestMempoolAccept tests mempool acceptance testing
// through the ChainSource actor.
func TestChainSourceActorTestMempoolAccept(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	system := actor.NewActorSystem()
	defer func() {
		_ = system.Shutdown(t.Context())
	}()

	chainSource := NewChainSourceActor(ChainSourceConfig{
		Backend: backend,
		System:  system,
	})
	ref := ChainSourceKey.Spawn(system, "chainsource-mempool", chainSource)

	ctx := t.Context()
	tx := wire.NewMsgTx(2)
	future := ref.Ask(ctx, &TestMempoolAcceptRequest{
		Txs: []*wire.MsgTx{tx},
	})

	result := future.Await(ctx)
	require.True(t, result.IsOk())

	resp, err := result.Unpack()
	require.NoError(t, err)
	acceptResp, ok := resp.(*TestMempoolAcceptResponse)
	require.True(t, ok)
	require.Len(t, acceptResp.Results, 1)
	require.True(t, acceptResp.Results[0].Accepted)
	require.Empty(t, acceptResp.Results[0].Reason)
}

// TestChainSourceActorBroadcastTx tests transaction broadcasting through the
// ChainSource actor.
func TestChainSourceActorBroadcastTx(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	system := actor.NewActorSystem()
	defer func() {
		_ = system.Shutdown(t.Context())
	}()

	chainSource := NewChainSourceActor(ChainSourceConfig{
		Backend: backend,
		System:  system,
	})
	ref := ChainSourceKey.Spawn(
		system, "chainsource-broadcast", chainSource,
	)

	ctx := t.Context()
	tx := wire.NewMsgTx(2)
	future := ref.Ask(ctx, &BroadcastTxRequest{
		Tx:    tx,
		Label: "test-tx",
	})

	result := future.Await(ctx)
	require.True(t, result.IsOk())

	resp, err := result.Unpack()
	require.NoError(t, err)
	broadcastResp, ok := resp.(*BroadcastTxResponse)
	require.True(t, ok)
	expectedHash := tx.TxHash()
	require.Equal(t, expectedHash, broadcastResp.Txid)
}

// TestChainSourceActorSubmitPackage tests package submission through the
// ChainSource actor.
func TestChainSourceActorSubmitPackage(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	system := actor.NewActorSystem()
	defer func() {
		_ = system.Shutdown(t.Context())
	}()

	chainSource := NewChainSourceActor(ChainSourceConfig{
		Backend: backend,
		System:  system,
	})
	ref := ChainSourceKey.Spawn(
		system, "chainsource-submit-package", chainSource,
	)

	ctx := t.Context()
	parent := wire.NewMsgTx(3)
	child := wire.NewMsgTx(3)
	future := ref.Ask(ctx, &SubmitPackageRequest{
		Parents: []*wire.MsgTx{parent},
		Child:   child,
	})

	result := future.Await(ctx)
	require.True(t, result.IsOk())

	resp, err := result.Unpack()
	require.NoError(t, err)
	_, ok := resp.(*SubmitPackageResponse)
	require.True(t, ok)
}

// TestChainSourceActorBroadcastTxIgnoresRebroadcastErrors tests that broadcast
// errors expected during re-broadcast are treated as success.
func TestChainSourceActorBroadcastTxIgnoresRebroadcastErrors(t *testing.T) {
	t.Parallel()

	baseBackend := newMockBackend()
	backend := &broadcastErrorBackend{
		mockBackend: baseBackend,

		broadcastErr: chain.ErrInsufficientFee,

		// If we regress and call TestMempoolAccept on ignorable errors,
		// the broadcast is treated as a failure,
		// and this test should fail.
		mempoolErr: errors.New("should not call testmempoolaccept"),
	}

	system := actor.NewActorSystem()
	defer func() {
		_ = system.Shutdown(t.Context())
	}()

	chainSource := NewChainSourceActor(ChainSourceConfig{
		Backend: backend,
		System:  system,
	})
	ref := ChainSourceKey.Spawn(
		system, "chainsource-broadcast-ignore", chainSource,
	)

	ctx := t.Context()
	tx := wire.NewMsgTx(2)
	future := ref.Ask(ctx, &BroadcastTxRequest{
		Tx:    tx,
		Label: "test-tx",
	})

	result := future.Await(ctx)
	require.True(t, result.IsOk())

	resp, err := result.Unpack()
	require.NoError(t, err)
	broadcastResp, ok := resp.(*BroadcastTxResponse)
	require.True(t, ok)
	require.Equal(t, tx.TxHash(), broadcastResp.Txid)
}

// TestChainSourceActorBroadcastTxFallsBackToTestMempoolAccept tests that the
// ChainSourceActor treats a failed broadcast as success if the backend reports
// that the transaction would be accepted by the mempool.
func TestChainSourceActorBroadcastTxFallsBackToTestMempoolAccept(t *testing.T) {
	t.Parallel()

	baseBackend := newMockBackend()
	backend := &broadcastErrorBackend{
		mockBackend: baseBackend,

		broadcastErr: errors.New("non-standard backend error"),

		mempoolAccepted: true,
		mempoolReason:   "",
		mempoolErr:      nil,
	}

	system := actor.NewActorSystem()
	defer func() {
		_ = system.Shutdown(t.Context())
	}()

	chainSource := NewChainSourceActor(ChainSourceConfig{
		Backend: backend,
		System:  system,
	})
	ref := ChainSourceKey.Spawn(
		system, "chainsource-broadcast-mempool", chainSource,
	)

	ctx := t.Context()
	tx := wire.NewMsgTx(2)
	future := ref.Ask(ctx, &BroadcastTxRequest{
		Tx:    tx,
		Label: "test-tx",
	})

	result := future.Await(ctx)
	require.True(t, result.IsOk())

	resp, err := result.Unpack()
	require.NoError(t, err)
	broadcastResp, ok := resp.(*BroadcastTxResponse)
	require.True(t, ok)
	require.Equal(t, tx.TxHash(), broadcastResp.Txid)
}

// TestChainSourceActorBroadcastTxFallsBackToIgnorableRejectReason tests that
// the ChainSourceActor treats a failed broadcast as success if the backend
// reports an ignorable reject reason from TestMempoolAccept.
func TestChainSourceActorBroadcastTxFallsBackToIgnorableRejectReason(
	t *testing.T) {

	t.Parallel()

	baseBackend := newMockBackend()
	backend := &broadcastErrorBackend{
		mockBackend: baseBackend,

		broadcastErr: errors.New("non-standard backend error"),

		mempoolAccepted: false,
		mempoolReason:   "already in mempool",
		mempoolErr:      nil,
	}

	system := actor.NewActorSystem()
	defer func() {
		_ = system.Shutdown(t.Context())
	}()

	chainSource := NewChainSourceActor(ChainSourceConfig{
		Backend: backend,
		System:  system,
	})
	ref := ChainSourceKey.Spawn(
		system, "chainsource-broadcast-mempool-reject", chainSource,
	)

	ctx := t.Context()
	tx := wire.NewMsgTx(2)
	future := ref.Ask(ctx, &BroadcastTxRequest{
		Tx:    tx,
		Label: "test-tx",
	})

	result := future.Await(ctx)
	require.True(t, result.IsOk())

	resp, err := result.Unpack()
	require.NoError(t, err)
	broadcastResp, ok := resp.(*BroadcastTxResponse)
	require.True(t, ok)
	require.Equal(t, tx.TxHash(), broadcastResp.Txid)
}

// TestChainSourceActorBestHeight tests best height query through the
// ChainSource actor.
func TestChainSourceActorBestHeight(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	system := actor.NewActorSystem()
	defer func() {
		_ = system.Shutdown(t.Context())
	}()

	chainSource := NewChainSourceActor(ChainSourceConfig{
		Backend: backend,
		System:  system,
	})
	ref := ChainSourceKey.Spawn(
		system, "chainsource-1", chainSource,
	)

	ctx := t.Context()
	future := ref.Ask(ctx, &BestHeightRequest{})

	result := future.Await(ctx)
	require.True(t, result.IsOk())

	resp, err := result.Unpack()
	require.NoError(t, err)
	heightResp, ok := resp.(*BestHeightResponse)
	require.True(t, ok)
	require.Equal(t, backend.bestHeight, heightResp.Height)
	require.Equal(t, backend.bestHash, heightResp.Hash)
}

// TestChainSourceActorRegisterConf ensures confirmation registrations routed
// through the ChainSource actor deliver events.
func TestChainSourceActorRegisterConf(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	system := actor.NewActorSystem()
	defer func() {
		_ = system.Shutdown(t.Context())
	}()

	chainSource := NewChainSourceActor(ChainSourceConfig{
		Backend: backend,
		System:  system,
	})
	ref := ChainSourceKey.Spawn(system, "chainsource-conf", chainSource)

	ctx := t.Context()
	txHash := chainhash.Hash{}
	result := ref.Ask(
		ctx, &RegisterConfRequest{
			CallerID:    "test-chainsource-conf",
			Txid:        &txHash,
			PkScript:    []byte{0x00, 0x14},
			TargetConfs: 1,
		},
	).Await(ctx)
	require.True(t, result.IsOk())

	resp, err := result.Unpack()
	require.NoError(t, err)
	confResp, ok := resp.(*RegisterConfResponse)
	require.True(t, ok)
	require.NotNil(t, confResp.Future)

	tx := wire.NewMsgTx(2)
	blockHash := chainhash.Hash{}

	backend.confChan <- &TxConfirmation{
		BlockHash:   &blockHash,
		BlockHeight: 150,
		TxIndex:     0,
		Tx:          tx,
	}

	eventCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	eventResult := confResp.Future.Await(eventCtx)
	require.True(t, eventResult.IsOk())

	event, err := eventResult.Unpack()
	require.NoError(t, err)
	require.Equal(t, blockHash, event.BlockHash)
	require.Equal(t, int32(150), event.BlockHeight)
}

// TestChainSourceActorRegisterSpend ensures spend registrations routed through
// the ChainSource actor deliver events.
func TestChainSourceActorRegisterSpend(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	system := actor.NewActorSystem()
	defer func() {
		_ = system.Shutdown(t.Context())
	}()

	chainSource := NewChainSourceActor(ChainSourceConfig{
		Backend: backend,
		System:  system,
	})
	ref := ChainSourceKey.Spawn(system, "chainsource-spend", chainSource)

	ctx := t.Context()
	result := ref.Ask(
		ctx, &RegisterSpendRequest{
			CallerID: "test-chainsource-spend",
			Outpoint: &wire.OutPoint{},
			PkScript: []byte{0x00, 0x14},
		},
	).Await(ctx)
	require.True(t, result.IsOk())

	resp, err := result.Unpack()
	require.NoError(t, err)
	spendResp, ok := resp.(*RegisterSpendResponse)
	require.True(t, ok)
	require.NotNil(t, spendResp.Future)

	spendingTx := wire.NewMsgTx(2)
	spendingHash := spendingTx.TxHash()
	backend.spendChan <- &SpendDetail{
		SpentOutPoint:     &wire.OutPoint{},
		SpenderTxHash:     &spendingHash,
		SpendingTx:        spendingTx,
		SpenderInputIndex: 0,
		SpendingHeight:    45,
	}

	eventCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	eventResult := spendResp.Future.Await(eventCtx)
	require.True(t, eventResult.IsOk())

	event, err := eventResult.Unpack()
	require.NoError(t, err)
	require.Equal(t, spendingHash, event.SpendingTxid)
	require.Equal(t, int32(45), event.SpendingHeight)
}

// TestChainSourceActorUnregisterConf tests cancelling a confirmation
// subscription via the ChainSource actor.
func TestChainSourceActorUnregisterConf(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	system := actor.NewActorSystem()
	defer func() {
		_ = system.Shutdown(t.Context())
	}()

	chainSource := NewChainSourceActor(ChainSourceConfig{
		Backend: backend,
		System:  system,
	})
	ref := ChainSourceKey.Spawn(
		system, "chainsource-unreg-conf", chainSource,
	)

	ctx := t.Context()
	txHash := chainhash.Hash{}

	regResult := ref.Ask(
		ctx, &RegisterConfRequest{
			CallerID:    "test-unreg-conf",
			Txid:        &txHash,
			PkScript:    []byte{0x00, 0x14},
			TargetConfs: 1,
		},
	).Await(ctx)
	require.True(t, regResult.IsOk())

	unregResult := ref.Ask(
		ctx, &UnregisterConfRequest{
			CallerID:    "test-unreg-conf",
			Txid:        &txHash,
			PkScript:    []byte{0x00, 0x14},
			TargetConfs: 1,
		},
	).Await(ctx)
	require.True(t, unregResult.IsOk())

	resp, err := unregResult.Unpack()
	require.NoError(t, err)
	_, ok := resp.(*UnregisterConfResponse)
	require.True(t, ok)
}

// TestChainSourceActorUnregisterSpend tests cancelling a spend subscription
// via the ChainSource actor.
func TestChainSourceActorUnregisterSpend(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	system := actor.NewActorSystem()
	defer func() {
		_ = system.Shutdown(t.Context())
	}()

	chainSource := NewChainSourceActor(ChainSourceConfig{
		Backend: backend,
		System:  system,
	})
	ref := ChainSourceKey.Spawn(
		system, "chainsource-unreg-spend", chainSource,
	)

	ctx := t.Context()
	outpoint := &wire.OutPoint{Hash: chainhash.Hash{}, Index: 0}

	regResult := ref.Ask(
		ctx, &RegisterSpendRequest{
			CallerID: "test-unreg-spend",
			Outpoint: outpoint,
			PkScript: []byte{0x00, 0x14},
		},
	).Await(ctx)
	require.True(t, regResult.IsOk())

	unregResult := ref.Ask(
		ctx, &UnregisterSpendRequest{
			CallerID: "test-unreg-spend",
			Outpoint: outpoint,
			PkScript: []byte{0x00, 0x14},
		},
	).Await(ctx)
	require.True(t, unregResult.IsOk())

	resp, err := unregResult.Unpack()
	require.NoError(t, err)
	_, ok := resp.(*UnregisterSpendResponse)
	require.True(t, ok)
}

// TestChainSourceActorUnsubscribeBlocks tests cancelling a block subscription
// via the ChainSource actor.
func TestChainSourceActorUnsubscribeBlocks(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	system := actor.NewActorSystem()
	defer func() {
		_ = system.Shutdown(t.Context())
	}()

	chainSource := NewChainSourceActor(ChainSourceConfig{
		Backend: backend,
		System:  system,
	})
	ref := ChainSourceKey.Spawn(
		system, "chainsource-unsub-blocks", chainSource,
	)

	ctx := t.Context()

	subResult := ref.Ask(
		ctx, &SubscribeBlocksRequest{
			CallerID: "test-unsub-blocks",
		},
	).Await(ctx)
	require.True(t, subResult.IsOk())

	unsubResult := ref.Ask(
		ctx, &UnsubscribeBlocksRequest{
			CallerID: "test-unsub-blocks",
		},
	).Await(ctx)
	require.True(t, unsubResult.IsOk())

	resp, err := unsubResult.Unpack()
	require.NoError(t, err)
	_, ok := resp.(*UnsubscribeBlocksResponse)
	require.True(t, ok)
}

// TestChainSourceActorSubscribeBlocks ensures block subscriptions routed
// through the ChainSource actor provide iterators.
func TestChainSourceActorSubscribeBlocks(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	system := actor.NewActorSystem()
	defer func() {
		_ = system.Shutdown(t.Context())
	}()

	chainSource := NewChainSourceActor(ChainSourceConfig{
		Backend: backend,
		System:  system,
	})
	ref := ChainSourceKey.Spawn(system, "chainsource-epoch", chainSource)

	ctx := t.Context()
	result := ref.Ask(
		ctx, &SubscribeBlocksRequest{
			CallerID: "test-chainsource-epoch",
		},
	).Await(ctx)
	require.True(t, result.IsOk())

	resp, err := result.Unpack()
	require.NoError(t, err)
	epochResp, ok := resp.(*SubscribeBlocksResponse)
	require.True(t, ok)
	require.NotNil(t, epochResp.Iterator)

	done := make(chan struct{})
	go func() {
		for epoch := range epochResp.Iterator {
			require.Equal(t, int32(201), epoch.Height)
			close(done)

			return
		}
	}()

	hash := chainhash.Hash{}
	backend.epochChan <- &BlockEpoch{
		Height:    201,
		Hash:      hash,
		Timestamp: 0,
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("did not receive block epoch")
	}
}

// TestConfActorFutureMode tests confirmation monitoring in Future mode.
func TestConfActorFutureMode(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()

	ctx := t.Context()
	confActor := NewConfActor(ConfActorConfig{Backend: backend})
	defer confActor.Stop()

	txHash := chainhash.Hash{}

	// Register for confirmation in Future mode (no NotifyActor).
	result := confActor.Receive(ctx, &RegisterConfRequest{
		CallerID:    "test-conf-future",
		Txid:        &txHash,
		PkScript:    []byte{0x00, 0x14},
		TargetConfs: 1,
	})

	require.True(t, result.IsOk())
	resp, err := result.Unpack()
	require.NoError(t, err)
	confResp, ok := resp.(*RegisterConfResponse)
	require.True(t, ok)
	require.NotNil(t, confResp.Future)

	tx := wire.NewMsgTx(2)
	blockHash := chainhash.Hash{}
	backend.confChan <- &TxConfirmation{
		BlockHash:   &blockHash,
		BlockHeight: 101,
		TxIndex:     0,
		Tx:          tx,
	}

	eventCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	eventResult := confResp.Future.Await(eventCtx)
	require.True(t, eventResult.IsOk())

	event, err := eventResult.Unpack()
	require.NoError(t, err)
	require.Equal(t, int32(101), event.BlockHeight)
	require.Equal(t, blockHash, event.BlockHash)
}

// TestConfActorNotifyMode tests confirmation monitoring in Actor notify mode,
// where events are sent to a registered actor rather than via a Future.
func TestConfActorNotifyMode(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	confActor := NewConfActor(ConfActorConfig{Backend: backend})
	defer confActor.Stop()

	txHash := chainhash.Hash{}
	notifier := actor.NewChannelTellOnlyRef[ConfirmationEvent](
		"test-notify", 10,
	)

	// Register for confirmation in notify mode (with NotifyActor).
	notifyRef := actor.TellOnlyRef[ConfirmationEvent](notifier)
	result := confActor.Receive(ctx, &RegisterConfRequest{
		CallerID:    "test-conf-notify",
		Txid:        &txHash,
		PkScript:    []byte{0x00, 0x14},
		TargetConfs: 1,
		NotifyActor: fn.Some(notifyRef),
	})

	require.True(t, result.IsOk())
	resp, err := result.Unpack()
	require.NoError(t, err)
	confResp, ok := resp.(*RegisterConfResponse)
	require.True(t, ok)

	// In notify mode, Future should not be set (it's the zero value).
	require.True(t, confResp.Future == nil)

	tx := wire.NewMsgTx(2)
	blockHash := chainhash.Hash{}
	backend.confChan <- &TxConfirmation{
		BlockHash:   &blockHash,
		BlockHeight: 101,
		TxIndex:     0,
		Tx:          tx,
	}

	// Wait for notification via the notify actor.
	event, ok := notifier.AwaitMessage(5 * time.Second)
	require.True(t, ok, "timeout waiting for notification")
	require.Equal(t, int32(101), event.BlockHeight)
	require.Equal(t, blockHash, event.BlockHash)
}

// TestConfActorHandlesNilTx ensures a backend that omits the transaction still
// produces a confirmation event without panicking.
func TestConfActorHandlesNilTx(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	confActor := NewConfActor(ConfActorConfig{Backend: backend})
	defer confActor.Stop()

	txHash, _ := chainhash.NewHashFromStr(
		"8f3d3d4456f2f4dfbdd3e3f7b9e36dcd58e445b7344a5ebe2b4b5a6e7d9" +
			"b3c01",
	)

	result := confActor.Receive(ctx, &RegisterConfRequest{
		CallerID:    "test-conf-nil-tx",
		Txid:        txHash,
		PkScript:    []byte{0x00, 0x14},
		TargetConfs: 1,
	})
	require.True(t, result.IsOk())
	resp, err := result.Unpack()
	require.NoError(t, err)
	confResp, ok := resp.(*RegisterConfResponse)
	require.True(t, ok)

	blockHash := chainhash.Hash{}
	backend.confChan <- &TxConfirmation{
		BlockHash:   &blockHash,
		BlockHeight: 200,
		TxIndex:     0,
		Tx:          nil,
	}

	eventCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	eventResult := confResp.Future.Await(eventCtx)
	require.True(t, eventResult.IsOk())

	event, err := eventResult.Unpack()
	require.NoError(t, err)
	require.Equal(t, *txHash, event.Txid)
	require.Equal(t, int32(200), event.BlockHeight)
}

// TestConfActorHandlesClosedChannel ensures channel closure returns an error.
func TestConfActorHandlesClosedChannel(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	confActor := NewConfActor(ConfActorConfig{Backend: backend})
	defer confActor.Stop()

	txHash := chainhash.Hash{}

	result := confActor.Receive(ctx, &RegisterConfRequest{
		CallerID:    "test-conf-closed-chan",
		Txid:        &txHash,
		PkScript:    []byte{0x00, 0x14},
		TargetConfs: 1,
	})
	require.True(t, result.IsOk())
	resp, err := result.Unpack()
	require.NoError(t, err)
	confResp, ok := resp.(*RegisterConfResponse)
	require.True(t, ok)

	close(backend.confChan)

	eventCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	eventResult := confResp.Future.Await(eventCtx)
	require.True(t, eventResult.IsErr())
	require.Contains(t, eventResult.Err().Error(), "subscription closed")
}

// TestSpendActorFutureMode tests spend monitoring in Future mode.
func TestSpendActorFutureMode(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()

	ctx := t.Context()
	spendActor := NewSpendActor(SpendActorConfig{Backend: backend})
	defer spendActor.Stop()

	txHash := chainhash.Hash{}
	outpoint := wire.OutPoint{Hash: txHash, Index: 0}

	result := spendActor.Receive(ctx, &RegisterSpendRequest{
		CallerID: "test-spend-future",
		Outpoint: &outpoint,
		PkScript: []byte{0x00, 0x14},
	})

	require.True(t, result.IsOk())
	resp, err := result.Unpack()
	require.NoError(t, err)
	spendResp, ok := resp.(*RegisterSpendResponse)
	require.True(t, ok)
	require.NotNil(t, spendResp.Future)

	spendingTx := wire.NewMsgTx(2)
	spendingHash := spendingTx.TxHash()
	backend.spendChan <- &SpendDetail{
		SpentOutPoint:     &outpoint,
		SpenderTxHash:     &spendingHash,
		SpendingTx:        spendingTx,
		SpenderInputIndex: 0,
		SpendingHeight:    102,
	}

	eventCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	eventResult := spendResp.Future.Await(eventCtx)
	require.True(t, eventResult.IsOk())

	event, err := eventResult.Unpack()
	require.NoError(t, err)
	require.Equal(t, outpoint, event.Outpoint)
	require.Equal(t, int32(102), event.SpendingHeight)
}

// TestSpendActorNotifyMode tests spend monitoring in Actor notify mode, where
// events are sent to a registered actor rather than via a Future.
func TestSpendActorNotifyMode(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	spendActor := NewSpendActor(SpendActorConfig{Backend: backend})
	defer spendActor.Stop()

	txHash := chainhash.Hash{}
	outpoint := wire.OutPoint{Hash: txHash, Index: 0}
	notifier := actor.NewChannelTellOnlyRef[SpendEvent]("test-notify", 10)

	// Register for spend in notify mode (with NotifyActor).
	result := spendActor.Receive(ctx, &RegisterSpendRequest{
		CallerID:    "test-spend-notify",
		Outpoint:    &outpoint,
		PkScript:    []byte{0x00, 0x14},
		NotifyActor: fn.Some(actor.TellOnlyRef[SpendEvent](notifier)),
	})

	require.True(t, result.IsOk())
	resp, err := result.Unpack()
	require.NoError(t, err)
	spendResp, ok := resp.(*RegisterSpendResponse)
	require.True(t, ok)

	// In notify mode, Future should not be set (it's the zero value).
	require.True(t, spendResp.Future == nil)

	spendingTx := wire.NewMsgTx(2)
	spendingHash := spendingTx.TxHash()
	backend.spendChan <- &SpendDetail{
		SpentOutPoint:     &outpoint,
		SpenderTxHash:     &spendingHash,
		SpendingTx:        spendingTx,
		SpenderInputIndex: 0,
		SpendingHeight:    102,
	}

	// Wait for notification via the notify actor.
	event, ok := notifier.AwaitMessage(5 * time.Second)
	require.True(t, ok, "timeout waiting for notification")
	require.Equal(t, outpoint, event.Outpoint)
	require.Equal(t, int32(102), event.SpendingHeight)
}

// TestSpendActorHandlesMissingTxHash ensures the actor falls back to the
// spending transaction contents when the backend omits the tx hash.
func TestSpendActorHandlesMissingTxHash(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	spendActor := NewSpendActor(SpendActorConfig{Backend: backend})
	defer spendActor.Stop()

	outpoint := &wire.OutPoint{Hash: chainhash.Hash{}, Index: 0}

	result := spendActor.Receive(ctx, &RegisterSpendRequest{
		CallerID: "test-spend-missing-hash",
		Outpoint: outpoint,
		PkScript: []byte{0x00, 0x14},
	})
	require.True(t, result.IsOk())
	resp, err := result.Unpack()
	require.NoError(t, err)
	spendResp, ok := resp.(*RegisterSpendResponse)
	require.True(t, ok)

	spendingTx := wire.NewMsgTx(2)
	spendingHash := spendingTx.TxHash()
	backend.spendChan <- &SpendDetail{
		SpentOutPoint:     outpoint,
		SpenderTxHash:     nil,
		SpendingTx:        spendingTx,
		SpenderInputIndex: 0,
		SpendingHeight:    300,
	}

	eventCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	eventResult := spendResp.Future.Await(eventCtx)
	require.True(t, eventResult.IsOk())

	event, err := eventResult.Unpack()
	require.NoError(t, err)
	require.Equal(t, spendingHash, event.SpendingTxid)
}

// TestSpendActorHandlesClosedChannel ensures a closed backend channel returns
// an error instead of panicking.
func TestSpendActorHandlesClosedChannel(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	spendActor := NewSpendActor(SpendActorConfig{Backend: backend})
	defer spendActor.Stop()

	outpoint := &wire.OutPoint{Hash: chainhash.Hash{}, Index: 0}

	result := spendActor.Receive(ctx, &RegisterSpendRequest{
		CallerID: "test-spend-closed-chan",
		Outpoint: outpoint,
		PkScript: []byte{0x00, 0x14},
	})
	require.True(t, result.IsOk())
	resp, err := result.Unpack()
	require.NoError(t, err)
	spendResp, ok := resp.(*RegisterSpendResponse)
	require.True(t, ok)

	close(backend.spendChan)

	eventCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	eventResult := spendResp.Future.Await(eventCtx)
	require.True(t, eventResult.IsErr())
	require.Contains(t, eventResult.Err().Error(), "subscription closed")
}

// TestBlockEpochActorNotifyMode tests block subscription in Actor notify mode,
// where events are sent to a registered actor rather than via an iterator.
func TestBlockEpochActorNotifyMode(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	epochActor := NewBlockEpochActor(BlockEpochConfig{Backend: backend})
	defer epochActor.Stop()

	notifier := actor.NewChannelTellOnlyRef[BlockEpoch]("test-notify", 10)

	// Subscribe in notify mode (with NotifyActor).
	result := epochActor.Receive(ctx, &SubscribeBlocksRequest{
		CallerID:    "test-epoch-notify",
		NotifyActor: fn.Some(actor.TellOnlyRef[BlockEpoch](notifier)),
	})

	require.True(t, result.IsOk())
	resp, err := result.Unpack()
	require.NoError(t, err)
	epochResp, ok := resp.(*SubscribeBlocksResponse)
	require.True(t, ok)

	// In notify mode, Iterator should not be set.
	require.Nil(t, epochResp.Iterator)
	require.NotNil(t, epochResp.Cancel)

	hash := chainhash.Hash{}
	hash[0] = 0x01
	backend.epochChan <- &BlockEpoch{
		Height:    150,
		Hash:      hash,
		Timestamp: 0,
	}

	// Wait for notification via the notify actor.
	event, ok := notifier.AwaitMessage(5 * time.Second)
	require.True(t, ok, "timeout waiting for notification")
	require.Equal(t, int32(150), event.Height)
	require.Equal(t, hash, event.Hash)
}

// TestBlockEpochActorReconnectsAfterBackendStreamCloses reproduces the
// production failure mode where LND closes the long-lived block epoch stream
// after the subscription was already established. The wallet relies on this
// stream to process new tips, scan confirmed backing-wallet outputs, and
// promote confirmed boarding UTXOs into boarding intents. If the actor treats
// a closed backend stream as terminal, the daemon can keep serving RPCs while
// silently missing later confirmations until it is restarted. This test closes
// the first backend registration, verifies the actor cancels it, then asserts
// the same subscriber receives a block from a fresh registration.
func TestBlockEpochActorReconnectsAfterBackendStreamCloses(t *testing.T) {
	t.Parallel()

	backend := newReconnectBlockBackend()
	ctx := t.Context()
	epochActor := NewBlockEpochActor(BlockEpochConfig{
		Backend:             backend,
		ReconnectBackoff:    5 * time.Millisecond,
		MaxReconnectBackoff: 5 * time.Millisecond,
	})
	defer epochActor.Stop()

	notifier := actor.NewChannelTellOnlyRef[BlockEpoch](
		"test-reconnect-notify", 10,
	)

	result := epochActor.Receive(ctx, &SubscribeBlocksRequest{
		CallerID: "test-epoch-reconnect",
		NotifyActor: fn.Some(
			actor.TellOnlyRef[BlockEpoch](notifier),
		),
	})
	require.True(t, result.IsOk())

	resp, err := result.Unpack()
	require.NoError(t, err)
	epochResp, ok := resp.(*SubscribeBlocksResponse)
	require.True(t, ok)
	require.NotNil(t, epochResp.Cancel)

	firstReg := backend.nextRegistration(t)
	firstHash := firstReg.send(150)

	firstEvent, ok := notifier.AwaitMessage(time.Second)
	require.True(t, ok, "timeout waiting for first block epoch")
	require.Equal(t, int32(150), firstEvent.Height)
	require.Equal(t, firstHash, firstEvent.Hash)

	close(firstReg.epochs)
	firstReg.assertCanceled(t)

	secondReg := backend.nextRegistration(t)
	secondHash := secondReg.send(151)

	secondEvent, ok := notifier.AwaitMessage(time.Second)
	require.True(t, ok, "timeout waiting for reconnected block epoch")
	require.Equal(t, int32(151), secondEvent.Height)
	require.Equal(t, secondHash, secondEvent.Hash)
}

// TestBlockEpochActorIteratorMode tests block subscription in Iterator mode.
func TestBlockEpochActorIteratorMode(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()

	ctx := t.Context()
	epochActor := NewBlockEpochActor(BlockEpochConfig{Backend: backend})
	defer epochActor.Stop()

	result := epochActor.Receive(ctx, &SubscribeBlocksRequest{
		CallerID: "test-epoch-iterator",
	})

	require.True(t, result.IsOk())
	resp, err := result.Unpack()
	require.NoError(t, err)
	epochResp, ok := resp.(*SubscribeBlocksResponse)
	require.True(t, ok)
	require.NotNil(t, epochResp.Iterator)

	for i := int32(1); i <= 3; i++ {
		hash := chainhash.Hash{}
		hash[0] = byte(i)

		backend.epochChan <- &BlockEpoch{
			Height:    100 + i,
			Hash:      hash,
			Timestamp: 0,
		}
	}

	var blocks []BlockEpoch
	iterCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	go func() {
		<-iterCtx.Done()
		_ = backend.Stop()
	}()

	for epoch := range epochResp.Iterator {
		blocks = append(blocks, epoch)
		if len(blocks) >= 3 {
			break
		}
	}

	require.Len(t, blocks, 3)
	require.Equal(t, int32(101), blocks[0].Height)
	require.Equal(t, int32(102), blocks[1].Height)
	require.Equal(t, int32(103), blocks[2].Height)
}

// TestBlockEpochActorIteratorCancel ensures iterator mode releases backend
// resources when the consumer stops or explicitly cancels.
func TestBlockEpochActorIteratorCancel(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	epochActor := NewBlockEpochActor(BlockEpochConfig{Backend: backend})
	defer epochActor.Stop()

	// Subscription that stops after the first block.
	result := epochActor.Receive(ctx, &SubscribeBlocksRequest{
		CallerID: "test-epoch-cancel-1",
	})
	require.True(t, result.IsOk())

	resp, err := result.Unpack()
	require.NoError(t, err)
	subResp, ok := resp.(*SubscribeBlocksResponse)
	require.True(t, ok)
	require.NotNil(t, subResp.Iterator)
	require.NotNil(t, subResp.Cancel)

	done := make(chan struct{})
	go func() {
		for epoch := range subResp.Iterator {
			require.Equal(t, int32(150), epoch.Height)
			break
		}
		close(done)
	}()

	hash := chainhash.Hash{}
	hash[0] = 0x01
	backend.epochChan <- &BlockEpoch{
		Height:    150,
		Hash:      hash,
		Timestamp: 0,
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("iterator did not finish")
	}

	select {
	case <-backend.epochCancel:
	case <-time.After(time.Second):
		t.Fatalf("expected backend cancel signal")
	}

	// Second subscription requires a new actor (each actor serves exactly
	// one subscription). This tests explicit Cancel() without consuming
	// blocks.
	epochActor2 := NewBlockEpochActor(BlockEpochConfig{Backend: backend})
	defer epochActor2.Stop()

	result2 := epochActor2.Receive(ctx, &SubscribeBlocksRequest{
		CallerID: "test-epoch-cancel-2",
	})
	require.True(t, result2.IsOk())

	resp2, err := result2.Unpack()
	require.NoError(t, err)
	subResp2, ok := resp2.(*SubscribeBlocksResponse)
	require.True(t, ok)
	require.NotNil(t, subResp2.Cancel)

	subResp2.Cancel()

	select {
	case <-backend.epochCancel:
	case <-time.After(time.Second):
		t.Fatalf("expected backend cancel signal after explicit cancel")
	}
}

// TestTxidOrScriptKey tests all parameter combinations for txidOrScriptKey.
func TestTxidOrScriptKey(t *testing.T) {
	t.Parallel()

	txid := &chainhash.Hash{}
	txid[0] = 0x01
	pkScript := []byte{0x00, 0x14, 0xab, 0xcd}

	tests := []struct {
		name        string
		txid        *chainhash.Hash
		pkScript    []byte
		expectError bool
		contains    []string
	}{
		{
			name:     "both txid and pkScript",
			txid:     txid,
			pkScript: pkScript,
			contains: []string{
				txid.String(),
				"script:",
				"+",
			},
		},
		{
			name:     "txid only",
			txid:     txid,
			pkScript: nil,
			contains: []string{
				txid.String(),
			},
		},
		{
			name:     "pkScript only",
			txid:     nil,
			pkScript: pkScript,
			contains: []string{
				"script:",
			},
		},
		{
			name:        "neither txid nor pkScript",
			txid:        nil,
			pkScript:    nil,
			expectError: true,
		},
		{
			name:        "txid nil with empty pkScript",
			txid:        nil,
			pkScript:    []byte{},
			expectError: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			key, err := txidOrScriptKey(tt.txid, tt.pkScript)

			if tt.expectError {
				require.Error(t, err)
				require.Empty(t, key)
			} else {
				require.NoError(t, err)
				for _, substr := range tt.contains {
					require.Contains(t, key, substr)
				}
			}
		})
	}
}

// TestOutpointOrScriptKey tests all parameter combinations for
// outpointOrScriptKey.
func TestOutpointOrScriptKey(t *testing.T) {
	t.Parallel()

	hash := chainhash.Hash{}
	hash[0] = 0xaa
	outpoint := &wire.OutPoint{Hash: hash, Index: 5}
	pkScript := []byte{0x00, 0x14, 0xab, 0xcd}

	tests := []struct {
		name        string
		outpoint    *wire.OutPoint
		pkScript    []byte
		expectError bool
		contains    []string
	}{
		{
			name:     "both outpoint and pkScript",
			outpoint: outpoint,
			pkScript: pkScript,
			contains: []string{
				hash.String(),
				":5",
				"script:",
				"+",
			},
		},
		{
			name:     "outpoint only",
			outpoint: outpoint,
			pkScript: nil,
			contains: []string{
				hash.String(),
				":5",
			},
		},
		{
			name:     "pkScript only",
			outpoint: nil,
			pkScript: pkScript,
			contains: []string{
				"script:",
			},
		},
		{
			name:        "neither outpoint nor pkScript",
			outpoint:    nil,
			pkScript:    nil,
			expectError: true,
		},
		{
			name:        "outpoint nil with empty pkScript",
			outpoint:    nil,
			pkScript:    []byte{},
			expectError: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			key, err := outpointOrScriptKey(
				tt.outpoint, tt.pkScript,
			)

			if tt.expectError {
				require.Error(t, err)
				require.Empty(t, key)
			} else {
				require.NoError(t, err)
				for _, substr := range tt.contains {
					require.Contains(t, key, substr)
				}
			}
		})
	}
}

// TestConfActorIncludeBlock verifies that the IncludeBlock option controls
// whether the full block is returned in the confirmation event.
func TestConfActorIncludeBlock(t *testing.T) {
	t.Parallel()

	// Use multiple transactions so we can verify TxIndex points to the
	// correct one when the full block is returned.
	testBlock := &wire.MsgBlock{
		Header: wire.BlockHeader{
			Version: 1,
			Nonce:   12345,
		},
		Transactions: []*wire.MsgTx{
			wire.NewMsgTx(2),
			wire.NewMsgTx(2),
		},
	}

	testCases := []struct {
		name string

		includeBlock bool
		//
		// backendBlock is what the backend returns; nil when
		// IncludeBlock was not requested.
		backendBlock *wire.MsgBlock

		expectBlock bool
	}{
		{
			name:         "include block",
			includeBlock: true,
			backendBlock: testBlock,
			expectBlock:  true,
		},
		{
			name:         "exclude block",
			includeBlock: false,
			backendBlock: nil,
			expectBlock:  false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			backend := newMockBackend()
			cfg := ConfActorConfig{Backend: backend}
			confActor := NewConfActor(cfg)

			// Send a register conf request directly into the
			// actor.
			txHash := chainhash.Hash{0x01, 0x02}
			result := confActor.Receive(ctx, &RegisterConfRequest{
				CallerID:     "test-include-block",
				Txid:         &txHash,
				PkScript:     []byte{0x00, 0x14},
				TargetConfs:  1,
				IncludeBlock: tc.includeBlock,
			})
			require.True(t, result.IsOk())

			resp, err := result.Unpack()
			require.NoError(t, err)
			confResp, ok := resp.(*RegisterConfResponse)
			require.True(t, ok)
			require.NotNil(t, confResp.Future)

			// We'll now send in a confirmation from the backend,
			// including the block based on our rquest.
			blockHash := chainhash.Hash{0xaa, 0xbb}
			backend.confChan <- &TxConfirmation{
				BlockHash:   &blockHash,
				BlockHeight: 200,
				TxIndex:     1,
				Tx:          testBlock.Transactions[1],
				Block:       tc.backendBlock,
			}

			eventCtx, cancel := context.WithTimeout(
				ctx, 5*time.Second,
			)
			defer cancel()
			eventResult := confResp.Future.Await(eventCtx)
			require.True(t, eventResult.IsOk())

			event, err := eventResult.Unpack()
			require.NoError(t, err)

			// Assert that the block is, or isn't included as
			// expected.
			if tc.expectBlock {
				require.NotNil(t, event.Block)
				require.Equal(
					t, testBlock.Header.Nonce,
					event.Block.Header.Nonce,
				)
				require.Len(t, event.Block.Transactions, 2)
			} else {
				require.Nil(t, event.Block)
			}

			require.Equal(t, blockHash, event.BlockHash)
			require.Equal(t, int32(200), event.BlockHeight)
			require.NotNil(t, event.Tx)
		})
	}
}

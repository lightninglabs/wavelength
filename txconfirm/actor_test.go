package txconfirm

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// testTimeout is the default timeout used by txconfirm actor tests.
const testTimeout = time.Second

// confNotifyRef is the confirmation-event notification ref type used in
// the fake chainsource test double.
type confNotifyRef = actor.TellOnlyRef[chainsource.ConfirmationEvent]

// fakeChainSourceRef is a controllable chainsource actor ref used by unit
// tests.
type fakeChainSourceRef struct {
	mu sync.Mutex

	bestHeight int32
	feeRate    btcutil.Amount

	broadcastErr error
	packageErr   error

	blockNotify actor.TellOnlyRef[chainsource.BlockEpoch]
	confNotify  map[chainhash.Hash]confNotifyRef
	confConfs   map[chainhash.Hash]uint32

	alreadyConfirmed map[chainhash.Hash]chainsource.ConfirmationEvent

	broadcastCalls    []*chainsource.BroadcastTxRequest
	packageCalls      []*chainsource.SubmitPackageRequest
	registerConfs     []*chainsource.RegisterConfRequest
	unregisterConfs   []*chainsource.UnregisterConfRequest
	subscribeBlocks   []*chainsource.SubscribeBlocksRequest
	unsubscribeBlocks []*chainsource.UnsubscribeBlocksRequest
}

// newFakeChainSourceRef creates a new controllable chainsource test double.
func newFakeChainSourceRef(bestHeight int32) *fakeChainSourceRef {
	return &fakeChainSourceRef{
		bestHeight: bestHeight,
		feeRate:    5,
		confNotify: make(map[chainhash.Hash]confNotifyRef),
		confConfs:  make(map[chainhash.Hash]uint32),
		alreadyConfirmed: make(
			map[chainhash.Hash]chainsource.ConfirmationEvent,
		),
	}
}

// packageCallCount returns the number of recorded package submissions.
func (f *fakeChainSourceRef) packageCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.packageCalls)
}

// broadcastCallCount returns the number of recorded direct broadcasts.
func (f *fakeChainSourceRef) broadcastCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.broadcastCalls)
}

// registerConfCount returns the number of confirmation registrations.
func (f *fakeChainSourceRef) registerConfCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.registerConfs)
}

// unregisterConfCount returns the number of confirmation unregistrations.
func (f *fakeChainSourceRef) unregisterConfCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.unregisterConfs)
}

// ID returns the fake actor ID.
func (f *fakeChainSourceRef) ID() string {
	return "fake-chainsource"
}

// Tell satisfies the actor.ActorRef interface.
func (f *fakeChainSourceRef) Tell(_ context.Context,
	_ chainsource.ChainSourceMsg) error {

	return nil
}

// Ask handles the chainsource request synchronously and returns an already
// completed future.
func (f *fakeChainSourceRef) Ask(ctx context.Context,
	msg chainsource.ChainSourceMsg,
) actor.Future[chainsource.ChainSourceResp] {

	promise := actor.NewPromise[chainsource.ChainSourceResp]()
	resp, err := f.handleAsk(ctx, msg)
	if err != nil {
		promise.Complete(fn.Err[chainsource.ChainSourceResp](err))
	} else {
		promise.Complete(fn.Ok(resp))
	}

	return promise.Future()
}

// handleAsk handles one chainsource message for the fake backend.
func (f *fakeChainSourceRef) handleAsk(_ context.Context,
	msg chainsource.ChainSourceMsg) (chainsource.ChainSourceResp, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	switch req := msg.(type) {
	case *chainsource.BestHeightRequest:
		return &chainsource.BestHeightResponse{
			Height: f.bestHeight,
		}, nil

	case *chainsource.FeeEstimateRequest:
		return &chainsource.FeeEstimateResponse{
			SatPerVByte: f.feeRate,
		}, nil

	case *chainsource.BroadcastTxRequest:
		f.broadcastCalls = append(f.broadcastCalls, req)
		if f.broadcastErr != nil {
			return nil, f.broadcastErr
		}

		return &chainsource.BroadcastTxResponse{
			Txid: req.Tx.TxHash(),
		}, nil

	case *chainsource.SubmitPackageRequest:
		f.packageCalls = append(f.packageCalls, req)
		if f.packageErr != nil {
			return nil, f.packageErr
		}

		return &chainsource.SubmitPackageResponse{}, nil

	case *chainsource.RegisterConfRequest:
		f.registerConfs = append(f.registerConfs, req)
		if req.Txid != nil && req.NotifyActor.IsSome() {
			f.confNotify[*req.Txid] = req.NotifyActor.UnwrapOr(nil)
			f.confConfs[*req.Txid] = req.TargetConfs
			if event, ok := f.alreadyConfirmed[*req.Txid]; ok {
				notifyRef := req.NotifyActor.UnwrapOr(nil)
				_ = notifyRef.Tell(context.Background(), event)
			}
		}

		return &chainsource.RegisterConfResponse{}, nil

	case *chainsource.UnregisterConfRequest:
		f.unregisterConfs = append(f.unregisterConfs, req)
		if req.Txid != nil {
			delete(f.confNotify, *req.Txid)
			delete(f.confConfs, *req.Txid)
		}

		return &chainsource.UnregisterConfResponse{}, nil

	case *chainsource.SubscribeBlocksRequest:
		f.subscribeBlocks = append(f.subscribeBlocks, req)
		f.blockNotify = req.NotifyActor.UnwrapOr(nil)
		return &chainsource.SubscribeBlocksResponse{}, nil

	case *chainsource.UnsubscribeBlocksRequest:
		f.unsubscribeBlocks = append(f.unsubscribeBlocks, req)
		f.blockNotify = nil
		return &chainsource.UnsubscribeBlocksResponse{}, nil

	default:
		return nil, fmt.Errorf(
			"unsupported chainsource message %T", msg,
		)
	}
}

// emitConfirmation delivers a confirmation event for one tracked txid.
func (f *fakeChainSourceRef) emitConfirmation(t *testing.T,
	txid chainhash.Hash, blockHeight int32) {

	t.Helper()

	f.mu.Lock()
	notifyRef := f.confNotify[txid]
	targetConfs := f.confConfs[txid]
	f.mu.Unlock()

	require.NotNil(t, notifyRef)
	err := notifyRef.Tell(t.Context(), chainsource.ConfirmationEvent{
		Txid:        txid,
		BlockHeight: blockHeight,
		NumConfs:    targetConfs,
	})
	require.NoError(t, err)
}

// emitBlock delivers a new block epoch to the shared block subscriber.
func (f *fakeChainSourceRef) emitBlock(t *testing.T, height int32) {
	t.Helper()

	f.mu.Lock()
	f.bestHeight = height
	notifyRef := f.blockNotify
	f.mu.Unlock()

	require.NotNil(t, notifyRef)
	err := notifyRef.Tell(t.Context(), chainsource.BlockEpoch{
		Height: height,
	})
	require.NoError(t, err)
}

// fakeWallet is a minimal wallet test double for CPFP child construction.
type fakeWallet struct {
	listErr error
	utxos   []*wallet.Utxo
}

// ListUnspent returns the configured confirmed UTXOs.
func (w *fakeWallet) ListUnspent(_ context.Context,
	_, _ int32) ([]*wallet.Utxo, error) {

	return w.utxos, w.listErr
}

// NewWalletPkScript returns a fresh deterministic change script.
func (w *fakeWallet) NewWalletPkScript(_ context.Context) ([]byte, error) {
	return []byte{txscript.OP_TRUE}, nil
}

// FinalizePsbt finalizes the PSBT with dummy witnesses for all wallet-owned
// inputs.
func (w *fakeWallet) FinalizePsbt(_ context.Context,
	packetBytes []byte) (*wire.MsgTx, error) {

	packet, err := psbt.NewFromRawBytes(bytes.NewReader(packetBytes), false)
	if err != nil {
		return nil, err
	}

	tx := packet.UnsignedTx.Copy()
	for i := range tx.TxIn {
		if len(packet.Inputs[i].FinalScriptWitness) > 0 {
			tx.TxIn[i].Witness = wire.TxWitness{}
			continue
		}

		tx.TxIn[i].Witness = wire.TxWitness{
			make([]byte, 64),
		}
	}

	return tx, nil
}

// newTestActor creates and starts a txconfirm actor plus its behavior.
func newTestActor(t *testing.T, cfg Config) (*actor.Actor[Msg, Resp],
	*TxBroadcasterActor) {

	t.Helper()

	behavior := NewTxBroadcasterActor(cfg)
	actorInstance := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "txconfirm-test",
		Behavior:    behavior,
		MailboxSize: 64,
	})
	behavior.SetSelfRef(actorInstance.TellRef())
	actorInstance.Start()
	t.Cleanup(actorInstance.Stop)

	return actorInstance, behavior
}

// mustEnsure sends an EnsureConfirmedReq and returns the typed response.
func mustEnsure(t *testing.T, ref actor.ActorRef[Msg, Resp],
	req *EnsureConfirmedReq) *EnsureConfirmedResp {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	resp, err := ref.Ask(ctx, req).Await(ctx).Unpack()
	require.NoError(t, err)

	typed, ok := resp.(*EnsureConfirmedResp)
	require.True(t, ok)

	return typed
}

// mustCancel sends a CancelInterestReq and returns the typed response.
func mustCancel(t *testing.T, ref actor.ActorRef[Msg, Resp],
	req *CancelInterestReq) *CancelInterestResp {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	resp, err := ref.Ask(ctx, req).Await(ctx).Unpack()
	require.NoError(t, err)

	typed, ok := resp.(*CancelInterestResp)
	require.True(t, ok)

	return typed
}

// mustAwaitNotification waits for exactly one notification from the supplied
// subscriber channel.
func mustAwaitNotification(t *testing.T,
	ref *actor.ChannelTellOnlyRef[Notification]) Notification {

	t.Helper()

	msg, ok := ref.AwaitMessage(testTimeout)
	require.True(t, ok, "expected notification")

	return msg
}

// mustHaveNoNotification verifies that no notification arrives before the
// timeout expires.
func mustHaveNoNotification(t *testing.T,
	ref *actor.ChannelTellOnlyRef[Notification]) {

	t.Helper()

	msg, ok := ref.AwaitMessage(100 * time.Millisecond)
	require.False(t, ok, "unexpected notification: %v", msg)
}

// mustEventually packages a polling assertion with the default test timeout.
func mustEventually(t *testing.T, predicate func() bool, msgAndArgs ...any) {
	t.Helper()

	require.Eventually(t, predicate, testTimeout, 10*time.Millisecond,
		msgAndArgs...)
}

// makeTestTx constructs a simple signed transaction for tests.
func makeTestTx(withAnchor bool) *wire.MsgTx {
	tx := wire.NewMsgTx(3)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 0,
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    10_000,
		PkScript: []byte{txscript.OP_TRUE},
	})
	if withAnchor {
		tx.AddTxOut(arkscript.AnchorOutput())
	}

	return tx
}

// makeWalletUTXO constructs a confirmed wallet UTXO suitable for CPFP.
func makeWalletUTXO() *wallet.Utxo {
	return &wallet.Utxo{
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{2},
			Index: 1,
		},
		Amount:   50_000,
		PkScript: []byte{txscript.OP_TRUE},
	}
}

// TestEnsureConfirmedDedupesTwoSubscribers verifies that the actor deduplicates
// by txid while notifying all subscribers on confirmation.
func TestEnsureConfirmedDedupesTwoSubscribers(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	ref, _ := newTestActor(t, Config{
		ChainSource: chain,
	})

	tx := makeTestTx(false)
	subA := actor.NewChannelTellOnlyRef[Notification]("sub-a", 4)
	subB := actor.NewChannelTellOnlyRef[Notification]("sub-b", 4)

	firstResp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: subA,
	})
	secondResp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: subB,
	})

	require.True(t, firstResp.Created)
	require.False(t, secondResp.Created)
	require.Equal(t, TxStateAwaitingConfirmation, firstResp.State)
	require.Equal(t, TxStateAwaitingConfirmation, secondResp.State)
	require.Equal(t, 1, chain.broadcastCallCount())
	require.Equal(t, 1, chain.registerConfCount())

	chain.emitConfirmation(t, tx.TxHash(), 101)

	confirmedA := mustAwaitNotification(t, subA)
	confirmedB := mustAwaitNotification(t, subB)

	require.IsType(t, &TxConfirmed{}, confirmedA)
	require.IsType(t, &TxConfirmed{}, confirmedB)
	mustEventually(t, func() bool {
		return chain.unregisterConfCount() == 1
	})
}

// TestEnsureConfirmedAlreadyConfirmedUsesSuccessPath verifies that a
// transaction already confirmed elsewhere is treated as success.
func TestEnsureConfirmedAlreadyConfirmedUsesSuccessPath(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	tx := makeTestTx(false)
	txid := tx.TxHash()
	chain.alreadyConfirmed[txid] = chainsource.ConfirmationEvent{
		Txid:        txid,
		BlockHeight: 99,
		NumConfs:    1,
	}
	chain.broadcastErr = fmt.Errorf("already in block chain")

	ref, _ := newTestActor(t, Config{
		ChainSource: chain,
	})

	subA := actor.NewChannelTellOnlyRef[Notification]("sub-a", 4)
	subB := actor.NewChannelTellOnlyRef[Notification]("sub-b", 4)

	resp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: subA,
	})
	require.True(t, resp.Created)

	msg := mustAwaitNotification(t, subA)
	confirmed, ok := msg.(*TxConfirmed)
	require.True(t, ok)
	require.Equal(t, int32(99), confirmed.BlockHeight)

	replayResp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: subB,
	})
	require.False(t, replayResp.Created)
	require.Equal(t, TxStateConfirmed, replayResp.State)

	replayed := mustAwaitNotification(t, subB)
	require.IsType(t, &TxConfirmed{}, replayed)
	require.Equal(t, 1, chain.broadcastCallCount())
	require.Equal(t, 1, chain.registerConfCount())
}

// TestEnsureConfirmedBroadcastFailureNotifiesFailure verifies that terminal
// broadcast errors transition the tracked txid to failed and notify the
// subscriber.
func TestEnsureConfirmedBroadcastFailureNotifiesFailure(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	chain.broadcastErr = fmt.Errorf("mempool reject")
	ref, _ := newTestActor(t, Config{
		ChainSource: chain,
	})

	tx := makeTestTx(false)
	subA := actor.NewChannelTellOnlyRef[Notification]("sub-a", 4)
	subB := actor.NewChannelTellOnlyRef[Notification]("sub-b", 4)

	resp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: subA,
	})
	require.Equal(t, TxStateFailed, resp.State)

	failed := mustAwaitNotification(t, subA)
	failedMsg, ok := failed.(*TxFailed)
	require.True(t, ok)
	require.Contains(t, failedMsg.Reason, "broadcast")

	replayResp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: subB,
	})
	require.False(t, replayResp.Created)
	require.Equal(t, TxStateFailed, replayResp.State)

	replayed := mustAwaitNotification(t, subB)
	require.IsType(t, &TxFailed{}, replayed)
}

// TestCancelInterestStopsTracking verifies that removing the final subscriber
// drops the active watch and prevents later callbacks from notifying it.
func TestCancelInterestStopsTracking(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	walletRef := &fakeWallet{
		utxos: []*wallet.Utxo{makeWalletUTXO()},
	}
	ref, _ := newTestActor(t, Config{
		ChainSource:           chain,
		Wallet:                walletRef,
		FeeBumpIntervalBlocks: 1,
	})

	tx := makeTestTx(true)
	sub := actor.NewChannelTellOnlyRef[Notification]("sub-a", 4)
	mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: sub,
	})

	cancelResp := mustCancel(t, ref.Ref(), &CancelInterestReq{
		Txid:         tx.TxHash(),
		SubscriberID: sub.ID(),
	})
	require.True(t, cancelResp.Removed)
	require.True(t, cancelResp.StoppedTracking)
	mustEventually(t, func() bool {
		return chain.unregisterConfCount() == 1
	})
	require.Equal(t, 1, chain.packageCallCount())

	chain.emitBlock(t, 101)
	require.Equal(t, 1, chain.packageCallCount())
	mustHaveNoNotification(t, sub)
}

// TestFeeBumpOnNewBlocks verifies that block-height observations trigger a
// rebroadcast after the configured interval.
func TestFeeBumpOnNewBlocks(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	walletRef := &fakeWallet{
		utxos: []*wallet.Utxo{makeWalletUTXO()},
	}
	ref, _ := newTestActor(t, Config{
		ChainSource:           chain,
		Wallet:                walletRef,
		FeeBumpIntervalBlocks: 2,
	})

	tx := makeTestTx(true)
	sub := actor.NewChannelTellOnlyRef[Notification]("sub-a", 4)
	resp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: sub,
	})
	require.Equal(t, TxStateAwaitingConfirmation, resp.State)
	require.Equal(t, 1, chain.packageCallCount())

	chain.emitBlock(t, 101)
	require.Equal(t, 1, chain.packageCallCount())

	chain.emitBlock(t, 102)
	mustEventually(t, func() bool {
		return chain.packageCallCount() == 2
	})

	chain.emitConfirmation(t, tx.TxHash(), 103)
	confirmed := mustAwaitNotification(t, sub)
	require.IsType(t, &TxConfirmed{}, confirmed)
}

// TestEnsureConfirmedWaitsForInitialCPFPInput verifies that an anchor parent
// stays retryable when its first broadcast attempt lacks a confirmed fee
// input.
func TestEnsureConfirmedWaitsForInitialCPFPInput(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	walletRef := &fakeWallet{
		listErr: fmt.Errorf("list failed"),
	}
	ref, _ := newTestActor(t, Config{
		ChainSource:           chain,
		Wallet:                walletRef,
		FeeBumpIntervalBlocks: 2,
	})

	tx := makeTestTx(true)
	sub := actor.NewChannelTellOnlyRef[Notification]("sub-a", 4)
	resp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: sub,
	})
	require.Equal(t, TxStateAwaitingConfirmation, resp.State)
	require.Equal(t, 0, chain.packageCallCount())
	require.Equal(t, 0, chain.broadcastCallCount())
	mustHaveNoNotification(t, sub)

	chain.emitBlock(t, 101)
	require.Equal(t, 0, chain.packageCallCount())

	chain.emitBlock(t, 102)
	require.Equal(t, 0, chain.packageCallCount())
}

// TestEnsureConfirmedRepeatedEnsureIsIdempotent verifies that repeating the
// same ensure request for one subscriber does not duplicate work.
func TestEnsureConfirmedRepeatedEnsureIsIdempotent(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	ref, _ := newTestActor(t, Config{
		ChainSource: chain,
	})

	tx := makeTestTx(false)
	sub := actor.NewChannelTellOnlyRef[Notification]("sub-a", 4)

	firstResp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: sub,
	})
	secondResp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: sub,
	})

	require.True(t, firstResp.Created)
	require.False(t, secondResp.Created)
	require.Equal(t, 1, chain.broadcastCallCount())
	require.Equal(t, 1, chain.registerConfCount())

	chain.emitConfirmation(t, tx.TxHash(), 101)
	confirmed := mustAwaitNotification(t, sub)
	require.IsType(t, &TxConfirmed{}, confirmed)
	mustHaveNoNotification(t, sub)
}

// TestEnsureConfirmedRegistersConfirmationPkScript verifies that txconfirm
// registers the same confirmation script old unroller used: an explicit caller
// override when present, otherwise the first tx output script.
func TestEnsureConfirmedRegistersConfirmationPkScript(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	ref, _ := newTestActor(t, Config{
		ChainSource: chain,
	})

	tx := makeTestTx(false)
	subA := actor.NewChannelTellOnlyRef[Notification]("sub-a", 4)

	mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: subA,
	})

	require.Len(t, chain.registerConfs, 1)
	require.Equal(
		t, tx.TxOut[0].PkScript, chain.registerConfs[0].PkScript,
	)
	require.Equal(t, uint32(100), chain.registerConfs[0].HeightHint)

	explicitPkScript := []byte{txscript.OP_FALSE, txscript.OP_TRUE}
	subB := actor.NewChannelTellOnlyRef[Notification]("sub-b", 4)
	overrideTx := makeTestTx(false)
	overrideTx.TxIn[0].PreviousOutPoint.Hash = chainhash.Hash{9}

	mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:                   overrideTx,
		ConfirmationPkScript: explicitPkScript,
		HeightHint:           55,
		Subscriber:           subB,
	})

	require.Len(t, chain.registerConfs, 2)
	require.Equal(t, explicitPkScript, chain.registerConfs[1].PkScript)
	require.Equal(t, uint32(55), chain.registerConfs[1].HeightHint)
}

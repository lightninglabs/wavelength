package txconfirm

import (
	"bytes"
	"context"
	"database/sql"
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

	// mempoolAcceptFn lets tests control the outcome of
	// TestMempoolAcceptRequest. If nil, the fake returns
	// "not supported" for every call so preflight code paths that
	// treat unsupported as a soft-miss can still be exercised.
	mempoolAcceptFn func(
		txs []*wire.MsgTx,
	) ([]chainsource.MempoolAcceptResult, error)

	blockNotify actor.TellOnlyRef[chainsource.BlockEpoch]
	confNotify  map[chainhash.Hash]confNotifyRef
	confConfs   map[chainhash.Hash]uint32

	alreadyConfirmed map[chainhash.Hash]chainsource.ConfirmationEvent

	broadcastCalls     []*chainsource.BroadcastTxRequest
	packageCalls       []*chainsource.SubmitPackageRequest
	registerConfs      []*chainsource.RegisterConfRequest
	unregisterConfs    []*chainsource.UnregisterConfRequest
	subscribeBlocks    []*chainsource.SubscribeBlocksRequest
	unsubscribeBlocks  []*chainsource.UnsubscribeBlocksRequest
	mempoolAcceptCalls [][]*wire.MsgTx
}

// retryNotifyRef is a subscriber that fails a configured number of Tell calls
// before accepting notifications into an internal channel.
type retryNotifyRef struct {
	id string

	mu             sync.Mutex
	failuresRemain int
	attempts       int

	msgs chan Notification
}

// blockingNotifyRef blocks terminal delivery until released by the test.
type blockingNotifyRef struct {
	id string

	started chan struct{}
	release chan struct{}
	once    sync.Once

	mu       sync.Mutex
	attempts int
}

// contextInspectNotifyRef records the context used for terminal notification
// delivery.
type contextInspectNotifyRef struct {
	id string

	mu     sync.Mutex
	hasTx  bool
	ctxErr error
	msgs   []Notification
}

// newRetryNotifyRef creates a subscriber that fails the first failures calls.
func newRetryNotifyRef(id string, failures int) *retryNotifyRef {
	return &retryNotifyRef{
		id:             id,
		failuresRemain: failures,
		msgs:           make(chan Notification, 4),
	}
}

// newBlockingNotifyRef creates a subscriber that blocks until released.
func newBlockingNotifyRef(id string) *blockingNotifyRef {
	return &blockingNotifyRef{
		id:      id,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// ID returns the fake subscriber ID.
func (b *blockingNotifyRef) ID() string {
	return b.id
}

// Tell records the delivery attempt and blocks until release is closed.
func (b *blockingNotifyRef) Tell(ctx context.Context, _ Notification) error {
	b.mu.Lock()
	b.attempts++
	b.mu.Unlock()

	b.once.Do(func() {
		close(b.started)
	})

	<-b.release

	return ctx.Err()
}

// attemptsCount returns the number of attempted notifications.
func (b *blockingNotifyRef) attemptsCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.attempts
}

// ID returns the fake subscriber ID.
func (r *contextInspectNotifyRef) ID() string {
	return r.id
}

// Tell records the context visible to a subscriber.
func (r *contextInspectNotifyRef) Tell(ctx context.Context,
	msg Notification) error {

	r.mu.Lock()
	defer r.mu.Unlock()

	r.hasTx = actor.HasTx(ctx)
	r.ctxErr = ctx.Err()
	r.msgs = append(r.msgs, msg)

	if r.hasTx {
		return fmt.Errorf("notification context leaked tx")
	}
	if r.ctxErr != nil {
		return r.ctxErr
	}

	return nil
}

// snapshot returns the inspected context values and delivered messages.
func (r *contextInspectNotifyRef) snapshot() (bool, error, []Notification) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.hasTx, r.ctxErr, append([]Notification(nil), r.msgs...)
}

// ID returns the fake subscriber ID.
func (r *retryNotifyRef) ID() string {
	return r.id
}

// Tell records a delivery attempt and either fails or stores the notification.
func (r *retryNotifyRef) Tell(ctx context.Context, msg Notification) error {
	r.mu.Lock()
	r.attempts++
	if r.failuresRemain > 0 {
		r.failuresRemain--
		r.mu.Unlock()

		return fmt.Errorf("notify failed")
	}
	r.mu.Unlock()

	select {
	case r.msgs <- msg:
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

// attemptsCount returns the number of attempted notifications.
func (r *retryNotifyRef) attemptsCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.attempts
}

// awaitMessage waits for one accepted notification.
func (r *retryNotifyRef) awaitMessage(timeout time.Duration) (Notification,
	bool) {

	select {
	case msg := <-r.msgs:
		return msg, true

	case <-time.After(timeout):
		return nil, false
	}
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
				//nolint:contextcheck // fake backend
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

	case *chainsource.TestMempoolAcceptRequest:
		f.mempoolAcceptCalls = append(f.mempoolAcceptCalls, req.Txs)

		if f.mempoolAcceptFn == nil {
			return nil, fmt.Errorf("test mempool accept not " +
				"supported")
		}

		results, err := f.mempoolAcceptFn(req.Txs)
		if err != nil {
			return nil, err
		}

		return &chainsource.TestMempoolAcceptResponse{
			Results: results,
		}, nil

	default:
		return nil, fmt.Errorf("unsupported chainsource message %T",
			msg)
	}
}

// emitConfirmation delivers a confirmation event for one tracked txid.
func (f *fakeChainSourceRef) emitConfirmation(t *testing.T, txid chainhash.Hash,
	blockHeight int32) {

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
	mu sync.Mutex

	listErr error
	utxos   []*wallet.Utxo

	leaseErr        error
	leaseCalls      []wire.OutPoint
	leaseExpiryLast time.Duration
	leaseLockID     wallet.LockID

	releaseErr    error
	releaseCalls  []wire.OutPoint
	releaseLockID wallet.LockID
}

// ListUnspent returns the configured confirmed UTXOs.
func (w *fakeWallet) ListUnspent(_ context.Context, _, _ int32) ([]*wallet.Utxo,
	error) {

	return w.utxos, w.listErr
}

// NewWalletPkScript returns a fresh deterministic change script.
func (w *fakeWallet) NewWalletPkScript(_ context.Context) ([]byte, error) {
	return []byte{txscript.OP_TRUE}, nil
}

// FinalizePsbt finalizes the PSBT with dummy witnesses for all wallet-owned
// inputs.
func (w *fakeWallet) FinalizePsbt(_ context.Context, packetBytes []byte) (
	*wire.MsgTx, error) {

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

// LeaseOutput records the lease call and returns a fixed expiry plus
// the configured error (if any). Tests that care about lease behaviour
// can inspect leaseCalls and leaseLockID.
func (w *fakeWallet) LeaseOutput(_ context.Context, id wallet.LockID,
	op wire.OutPoint, expiry time.Duration) (time.Time, error) {

	w.mu.Lock()
	defer w.mu.Unlock()

	w.leaseCalls = append(w.leaseCalls, op)
	w.leaseExpiryLast = expiry
	w.leaseLockID = id
	if w.leaseErr != nil {
		return time.Time{}, w.leaseErr
	}

	return time.Now().Add(expiry), nil
}

// ReleaseOutput records the release call and returns the configured
// error (if any).
func (w *fakeWallet) ReleaseOutput(_ context.Context, id wallet.LockID,
	op wire.OutPoint) error {

	w.mu.Lock()
	defer w.mu.Unlock()

	w.releaseCalls = append(w.releaseCalls, op)
	w.releaseLockID = id

	return w.releaseErr
}

// leaseSnapshot returns the wallet lease calls recorded so far.
func (w *fakeWallet) leaseSnapshot() ([]wire.OutPoint, time.Duration,
	wallet.LockID) {

	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]wire.OutPoint(nil), w.leaseCalls...),
		w.leaseExpiryLast, w.leaseLockID
}

// releaseSnapshot returns the wallet release calls recorded so far.
func (w *fakeWallet) releaseSnapshot() ([]wire.OutPoint, wallet.LockID) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]wire.OutPoint(nil), w.releaseCalls...),
		w.releaseLockID
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

	require.Eventually(
		t, predicate, testTimeout, 10*time.Millisecond, msgAndArgs...,
	)
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

// makeWalletUTXO constructs a confirmed wallet UTXO suitable for CPFP
// fee-input selection. The PkScript is a real P2TR script so fee
// estimation against this UTXO exercises the script-aware vsize path
// rather than the non-standard fallback.
func makeWalletUTXO(t *testing.T) *wallet.Utxo {
	t.Helper()

	return &wallet.Utxo{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				2,
			},
			Index: 1,
		},
		Amount:   50_000,
		PkScript: p2trTestPkScript(t),
	}
}

// p2trTestPkScript returns a fixed, canonical P2TR pkScript
// (OP_1 <32-byte x-only key>) used across broadcaster tests that need
// a realistic wallet output shape for fee estimation.
func p2trTestPkScript(t *testing.T) []byte {
	t.Helper()

	var xOnly [32]byte
	for i := range xOnly {
		xOnly[i] = byte(i + 1)
	}
	script, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_1).
		AddData(xOnly[:]).
		Script()
	require.NoError(t, err)

	return script
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

// TestConfirmationDeliveryRetriesAfterTellFailure verifies that a transient
// subscriber delivery failure does not permanently drop a terminal
// confirmation notification.
func TestConfirmationDeliveryRetriesAfterTellFailure(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	ref, _ := newTestActor(t, Config{
		ChainSource: chain,
	})

	tx := makeTestTx(false)
	txid := tx.TxHash()
	sub := newRetryNotifyRef("sub-retry", 1)

	resp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: sub,
	})
	require.True(t, resp.Created)

	chain.emitConfirmation(t, txid, 101)
	mustEventually(t, func() bool {
		return sub.attemptsCount() == 1
	})

	msg, ok := sub.awaitMessage(100 * time.Millisecond)
	require.False(t, ok, "unexpected notification: %v", msg)
	mustEventually(t, func() bool {
		return chain.unregisterConfCount() == 1
	})

	chain.emitBlock(t, 102)
	msg, ok = sub.awaitMessage(testTimeout)
	require.True(t, ok, "expected retried notification")

	confirmed, ok := msg.(*TxConfirmed)
	require.True(t, ok)
	require.Equal(t, txid, confirmed.Txid)
	require.Equal(t, int32(101), confirmed.BlockHeight)
	require.Equal(t, uint32(1), confirmed.NumConfs)
	require.Equal(t, 2, sub.attemptsCount())

	freshSub := actor.NewChannelTellOnlyRef[Notification]("sub-fresh", 4)
	replayResp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: freshSub,
	})
	require.True(t, replayResp.Created)
	require.Equal(t, 2, chain.registerConfCount())
	require.Equal(t, 2, chain.broadcastCallCount())
}

// TestTerminalNotificationsDoNotInheritCallerContext verifies that terminal
// delivery is independent of the txconfirm actor's transaction and cancellation
// context.
func TestTerminalNotificationsDoNotInheritCallerContext(t *testing.T) {
	behavior := NewTxBroadcasterActor(Config{
		ChainSource: newFakeChainSourceRef(100),
	})

	ctx, cancel := context.WithCancel(context.Background())
	ctx = actor.WithTx(ctx, (*sql.Tx)(nil))
	cancel()

	txid := chainhash.Hash{1}
	confirmedSub := &contextInspectNotifyRef{id: "confirmed-sub"}
	ok := behavior.notifyOneConfirmed(
		ctx, confirmedSub, txid, 101, 1,
	)
	require.True(t, ok)

	hasTx, ctxErr, msgs := confirmedSub.snapshot()
	require.False(t, hasTx)
	require.NoError(t, ctxErr)
	require.Len(t, msgs, 1)
	require.IsType(t, &TxConfirmed{}, msgs[0])

	failedSub := &contextInspectNotifyRef{id: "failed-sub"}
	ok = behavior.notifyOneFailed(ctx, failedSub, txid, "boom")
	require.True(t, ok)

	hasTx, ctxErr, msgs = failedSub.snapshot()
	require.False(t, hasTx)
	require.NoError(t, ctxErr)
	require.Len(t, msgs, 1)
	require.IsType(t, &TxFailed{}, msgs[0])
}

// TestTerminalNotificationTimeoutDoesNotBlockActor verifies that a slow
// subscriber cannot pin txconfirm's actor loop while holding a terminal entry.
func TestTerminalNotificationTimeoutDoesNotBlockActor(t *testing.T) {
	oldTimeout := terminalNotifyTimeout
	terminalNotifyTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		terminalNotifyTimeout = oldTimeout
	})

	behavior := NewTxBroadcasterActor(Config{
		ChainSource: newFakeChainSourceRef(100),
	})
	selfRef := actor.NewChannelTellOnlyRef[Msg]("txconfirm", 2)
	behavior.SetSelfRef(selfRef)

	txid := chainhash.Hash{2}
	sub := newBlockingNotifyRef("blocking-sub")

	started := make(chan bool, 1)
	go func() {
		select {
		case <-sub.started:
			started <- true

		case <-time.After(testTimeout):
			started <- false
		}
	}()

	start := time.Now()
	ok := behavior.notifyOneConfirmed(
		context.Background(), sub, txid, 101, 1,
	)
	require.False(t, ok)
	require.Less(t, time.Since(start), testTimeout)
	require.True(t, <-started)

	key := terminalNotifyKey(txid, sub.ID(), "confirmed")
	_, inflight := behavior.terminalNotifyInflight[key]
	require.True(t, inflight)
	require.Equal(t, 1, sub.attemptsCount())

	close(sub.release)

	msg, ok := selfRef.AwaitMessage(testTimeout)
	require.True(t, ok, "expected deferred terminal result")
	result, ok := msg.(*terminalNotifyResultMsg)
	require.True(t, ok)
	require.Equal(t, txid, result.txid)
	require.Equal(t, sub.ID(), result.subscriberID)
	require.Equal(t, key, result.inflightKey)
	require.ErrorIs(t, result.err, context.DeadlineExceeded)

	behavior.handleTerminalNotifyResult(context.Background(), result)
	_, inflight = behavior.terminalNotifyInflight[key]
	require.False(t, inflight)
}

// TestEnsureConfirmedRejectsMismatchedTargetConfs verifies that a second
// caller asking to confirm the same txid with a different TargetConfs
// value than the in-flight tracker is rejected with
// ErrEnsureParamsMismatch instead of silently sharing the existing
// tracker.
func TestEnsureConfirmedRejectsMismatchedTargetConfs(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	ref, _ := newTestActor(t, Config{
		ChainSource: chain,
	})

	tx := makeTestTx(false)
	subA := actor.NewChannelTellOnlyRef[Notification]("sub-a", 4)
	subB := actor.NewChannelTellOnlyRef[Notification]("sub-b", 4)

	firstResp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:          tx,
		TargetConfs: 1,
		Subscriber:  subA,
	})
	require.True(t, firstResp.Created)

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	_, err := ref.Ref().Ask(ctx, &EnsureConfirmedReq{
		Tx:          tx,
		TargetConfs: 3,
		Subscriber:  subB,
	}).Await(ctx).Unpack()
	require.ErrorIs(t, err, ErrEnsureParamsMismatch)
}

// TestEnsureConfirmedRejectsMismatchedPkScript verifies that a second
// caller asking to confirm the same txid with a different
// ConfirmationPkScript than the in-flight tracker is rejected rather
// than silently reusing the existing watch (which keys on the original
// script).
func TestEnsureConfirmedRejectsMismatchedPkScript(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	ref, _ := newTestActor(t, Config{
		ChainSource: chain,
	})

	tx := makeTestTx(false)
	subA := actor.NewChannelTellOnlyRef[Notification]("sub-a", 4)
	subB := actor.NewChannelTellOnlyRef[Notification]("sub-b", 4)

	firstResp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:                   tx,
		ConfirmationPkScript: tx.TxOut[0].PkScript,
		Subscriber:           subA,
	})
	require.True(t, firstResp.Created)

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	_, err := ref.Ref().Ask(ctx, &EnsureConfirmedReq{
		Tx:                   tx,
		ConfirmationPkScript: []byte{0x00, 0x20, 0x01, 0x02},
		Subscriber:           subB,
	}).Await(ctx).Unpack()
	require.ErrorIs(t, err, ErrEnsureParamsMismatch)
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

	// Once subA's TxConfirmed has been delivered, terminal eviction drops
	// the tracked entry. A subsequent EnsureConfirmedReq for the same
	// txid therefore starts fresh tracking rather than replaying cached
	// state. Chainsource immediately re-fires the confirmation for the
	// already-confirmed tx, so subB still receives TxConfirmed.
	replayResp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: subB,
	})
	require.True(t, replayResp.Created)

	replayed := mustAwaitNotification(t, subB)
	require.IsType(t, &TxConfirmed{}, replayed)
	require.Equal(t, 2, chain.broadcastCallCount())
	require.Equal(t, 2, chain.registerConfCount())
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

	// Terminal eviction means the subsequent ensure creates a fresh
	// tracked entry. The second broadcast hits the same configured
	// mempool reject and the fresh entry transitions into Failed, so
	// subB still receives TxFailed.
	replayResp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: subB,
	})
	require.True(t, replayResp.Created)
	require.Equal(t, TxStateFailed, replayResp.State)

	replayed := mustAwaitNotification(t, subB)
	require.IsType(t, &TxFailed{}, replayed)
}

// TestCancelInterestStopsTracking verifies that removing the final subscriber
// drops the active watch and prevents later callbacks from notifying it.
func TestCancelInterestStopsTracking(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	walletRef := &fakeWallet{
		utxos: []*wallet.Utxo{
			makeWalletUTXO(t),
		},
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

	// The anchor tx held a wallet-level lease on its CPFP fee input.
	// Canceling the last subscriber must release that lease under the
	// same LockID — otherwise the UTXO stays locked until the wallet's
	// configured expiry and starves later broadcasts.
	leaseCalls, _, _ := walletRef.leaseSnapshot()
	mustEventually(t, func() bool {
		releaseCalls, releaseLockID := walletRef.releaseSnapshot()

		return len(releaseCalls) == len(leaseCalls) &&
			releaseLockID == txconfirmLockID
	})
	releaseCalls, releaseLockID := walletRef.releaseSnapshot()
	require.Equal(
		t, leaseCalls, releaseCalls,
		"every leased outpoint must be released on cancel",
	)
	require.Equal(t, txconfirmLockID, releaseLockID)

	chain.emitBlock(t, 101)
	require.Equal(t, 1, chain.packageCallCount())
	mustHaveNoNotification(t, sub)
}

// TestOnStopEvictsWalletLeases verifies that stopping the actor while an
// anchor-bearing tracked tx is still in flight releases the wallet-level
// fee-input lease. Without this, a restart leaves the lease pinned until
// the backend's configured expiry, blocking unrelated coin selection.
func TestOnStopEvictsWalletLeases(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	walletRef := &fakeWallet{
		utxos: []*wallet.Utxo{
			makeWalletUTXO(t),
		},
	}
	ref, behavior := newTestActor(t, Config{
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

	// Sanity check: the CPFP path should have leased a UTXO.
	leaseCalls, _, _ := walletRef.leaseSnapshot()
	require.Len(t, leaseCalls, 1)
	releaseCalls, _ := walletRef.releaseSnapshot()
	require.Empty(t, releaseCalls,
		"lease must still be held before OnStop")

	require.NoError(t, behavior.OnStop(t.Context()))

	// Every previously-leased outpoint must have a matching release
	// call under the same txconfirm LockID.
	mustEventually(t, func() bool {
		releaseCalls, releaseLockID := walletRef.releaseSnapshot()

		return len(releaseCalls) == len(leaseCalls) &&
			releaseLockID == txconfirmLockID
	})
	releaseCalls, releaseLockID := walletRef.releaseSnapshot()
	require.Equal(
		t, leaseCalls, releaseCalls,
		"OnStop must release every active fee-input lease",
	)
	require.Equal(t, txconfirmLockID, releaseLockID)
}

// TestFeeBumpOnNewBlocks verifies that block-height observations trigger a
// rebroadcast after the configured interval.
func TestFeeBumpOnNewBlocks(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	walletRef := &fakeWallet{
		utxos: []*wallet.Utxo{
			makeWalletUTXO(t),
		},
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

// TestUnregisterConfMatchesRegisterServiceKey verifies that every field
// chainsource hashes into a conf-actor service key (CallerID, Txid,
// PkScript, TargetConfs) is present in both the Register and Unregister
// requests with identical values. An earlier revision of this package
// omitted PkScript from the unregister request, producing a service key
// that did not match the one chainsource created at register time and
// silently leaking one conf sub-actor per tracked tx. This test is the
// white-box guard against that regression.
func TestUnregisterConfMatchesRegisterServiceKey(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	ref, _ := newTestActor(t, Config{
		ChainSource: chain,
	})

	tx := makeTestTx(false)
	sub := actor.NewChannelTellOnlyRef[Notification]("sub-a", 4)

	mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: sub,
	})

	chain.emitConfirmation(t, tx.TxHash(), 101)

	confirmed := mustAwaitNotification(t, sub)
	require.IsType(t, &TxConfirmed{}, confirmed)

	mustEventually(t, func() bool {
		return chain.unregisterConfCount() == 1
	})

	require.Len(t, chain.registerConfs, 1)
	require.Len(t, chain.unregisterConfs, 1)

	reg := chain.registerConfs[0]
	unreg := chain.unregisterConfs[0]

	require.Equal(
		t, reg.CallerID, unreg.CallerID,
		"unregister must reuse the register CallerID",
	)
	require.Equal(
		t, reg.Txid, unreg.Txid,
		"unregister must reuse the register Txid",
	)
	require.Equal(
		t, reg.PkScript, unreg.PkScript, "unregister must include "+
			"the same PkScript as the register; dropping it "+
			"produces a different service key and leaks the "+
			"conf sub-actor",
	)
	require.Equal(
		t, reg.TargetConfs, unreg.TargetConfs,
		"unregister must reuse the register TargetConfs",
	)
}

// TestTerminalEntriesEvictedAfterConfirmation verifies that once a tracked
// transaction reaches Confirmed and all subscribers have been notified, the
// actor evicts the entry and does not retain per-tx FSM goroutines or
// cached transaction bytes. This guards against the unbounded a.tracked
// growth pattern flagged by review finding H-1.
func TestTerminalEntriesEvictedAfterConfirmation(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	ref, _ := newTestActor(t, Config{
		ChainSource: chain,
	})

	// Track three independent transactions end-to-end. Once all three
	// confirm, the actor should have zero entries retained. A single
	// txid would verify the eviction mechanism; using a batch ensures
	// we are not accidentally re-observing the same slot.
	const numTxs = 3
	subs := make([]*actor.ChannelTellOnlyRef[Notification], numTxs)
	txids := make([]chainhash.Hash, numTxs)
	for i := 0; i < numTxs; i++ {
		tx := makeTestTx(false)
		tx.TxIn[0].PreviousOutPoint.Hash = chainhash.Hash{byte(i + 10)}
		txids[i] = tx.TxHash()

		id := fmt.Sprintf("sub-%d", i)
		subs[i] = actor.NewChannelTellOnlyRef[Notification](id, 4)

		mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
			Tx:         tx,
			Subscriber: subs[i],
		})
	}

	for i := 0; i < numTxs; i++ {
		chain.emitConfirmation(t, txids[i], 101)

		confirmed := mustAwaitNotification(t, subs[i])
		require.IsType(t, &TxConfirmed{}, confirmed)
	}

	// Every confirmation should have produced exactly one unregister.
	mustEventually(t, func() bool {
		return chain.unregisterConfCount() == numTxs
	})

	// If eviction worked, issuing a Cancel for any of the confirmed
	// txids finds no tracked entry, so Removed is false and the
	// returned txid simply mirrors the request. This is the
	// externally-observable proxy for "len(a.tracked) == 0" without
	// racing against the actor goroutine.
	for i := 0; i < numTxs; i++ {
		cancelResp := mustCancel(t, ref.Ref(), &CancelInterestReq{
			Txid:         txids[i],
			SubscriberID: subs[i].ID(),
		})
		require.False(
			t, cancelResp.Removed, "terminal entry %d should "+
				"have been evicted before cancel", i,
		)
		require.Equal(t, 0, cancelResp.RemainingSubscribers)
	}

	// A fresh EnsureConfirmedReq for an already-terminated txid must
	// create a new entry rather than attach to a cached terminal one.
	// This is the other side of the eviction contract.
	freshSub := actor.NewChannelTellOnlyRef[Notification]("sub-fresh", 4)
	fresh := makeTestTx(false)
	fresh.TxIn[0].PreviousOutPoint.Hash = chainhash.Hash{byte(10)}
	require.Equal(t, txids[0], fresh.TxHash())

	resp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         fresh,
		Subscriber: freshSub,
	})
	require.True(
		t, resp.Created, "late ensure for a previously-confirmed "+
			"txid should start fresh tracking after terminal "+
			"eviction",
	)
}

// TestTerminalEntryEvictedAfterFailure verifies that failTrackedTx evicts
// the entry from the actor's tracking map, matching the confirmation path
// so long-lived daemons do not accumulate failed entries indefinitely.
func TestTerminalEntryEvictedAfterFailure(t *testing.T) {
	chain := newFakeChainSourceRef(100)
	chain.broadcastErr = fmt.Errorf("mempool reject")

	ref, _ := newTestActor(t, Config{
		ChainSource: chain,
	})

	tx := makeTestTx(false)
	sub := actor.NewChannelTellOnlyRef[Notification]("sub-a", 4)

	resp := mustEnsure(t, ref.Ref(), &EnsureConfirmedReq{
		Tx:         tx,
		Subscriber: sub,
	})
	require.Equal(t, TxStateFailed, resp.State)

	failed := mustAwaitNotification(t, sub)
	require.IsType(t, &TxFailed{}, failed)

	// Cancel-as-probe: if the failed entry was evicted, the cancel
	// finds nothing and reports Removed=false.
	cancelResp := mustCancel(t, ref.Ref(), &CancelInterestReq{
		Txid:         tx.TxHash(),
		SubscriberID: sub.ID(),
	})
	require.False(
		t, cancelResp.Removed,
		"failed entry should have been evicted before cancel",
	)
}

package unroll

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/recovery"
	"github.com/lightninglabs/darepo-client/txconfirm"
	"github.com/lightninglabs/darepo-client/unrollplan"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

const testTimeout = time.Second

// mockProofAssembler is a programmable proof assembler test double.
type mockProofAssembler struct {
	proof *recovery.Proof
	err   error
}

// EnsureProof returns the configured result.
func (m *mockProofAssembler) EnsureProof(_ context.Context,
	_ wire.OutPoint) (*recovery.Proof, error) {

	return m.proof, m.err
}

// mockVTXOStore is a minimal descriptor store test double.
type mockVTXOStore struct {
	desc *vtxo.Descriptor
	err  error
}

// SaveVTXO is unused in these tests.
func (m *mockVTXOStore) SaveVTXO(context.Context, *vtxo.Descriptor) error {
	return nil
}

// GetVTXO returns the configured descriptor.
func (m *mockVTXOStore) GetVTXO(context.Context,
	wire.OutPoint) (*vtxo.Descriptor, error) {

	return m.desc, m.err
}

// ListLiveVTXOs is unused in these tests.
func (m *mockVTXOStore) ListLiveVTXOs(context.Context) ([]*vtxo.Descriptor,
	error) {

	return nil, nil
}

// ListVTXOsByStatus is unused in these tests.
func (m *mockVTXOStore) ListVTXOsByStatus(context.Context,
	vtxo.VTXOStatus) ([]*vtxo.Descriptor, error) {

	return nil, nil
}

// UpdateVTXOStatus is unused in these tests.
func (m *mockVTXOStore) UpdateVTXOStatus(context.Context,
	wire.OutPoint, vtxo.VTXOStatus) error {

	return nil
}

// MarkForfeiting is unused in these tests.
func (m *mockVTXOStore) MarkForfeiting(context.Context, wire.OutPoint,
	string, *wire.MsgTx) error {

	return nil
}

// GetForfeitTx is unused in these tests.
func (m *mockVTXOStore) GetForfeitTx(context.Context,
	wire.OutPoint) (*wire.MsgTx, error) {

	return nil, nil
}

// MarkForfeited is unused in these tests.
func (m *mockVTXOStore) MarkForfeited(context.Context, wire.OutPoint,
	chainhash.Hash) error {

	return nil
}

// DeleteVTXO is unused in these tests.
func (m *mockVTXOStore) DeleteVTXO(context.Context, wire.OutPoint) error {
	return nil
}

// fakeTxConfirmRef is a programmable txconfirm actor test double.
type fakeTxConfirmRef struct {
	mu sync.Mutex

	requests       []*txconfirm.EnsureConfirmedReq
	responseStates map[chainhash.Hash]txconfirm.TxState
	confirmHeights map[chainhash.Hash]int32
	failureReasons map[chainhash.Hash]string
}

// ID returns the fake actor ID.
func (f *fakeTxConfirmRef) ID() string {
	return "fake-txconfirm"
}

// Tell is unused for these tests.
func (f *fakeTxConfirmRef) Tell(context.Context, txconfirm.Msg) error {
	return nil
}

// Ask records the request and returns an awaiting-confirmation response.
func (f *fakeTxConfirmRef) Ask(_ context.Context,
	msg txconfirm.Msg) actor.Future[txconfirm.Resp] {

	promise := actor.NewPromise[txconfirm.Resp]()

	req, ok := msg.(*txconfirm.EnsureConfirmedReq)
	if !ok {
		promise.Complete(fn.Err[txconfirm.Resp](
			fmt.Errorf("unexpected txconfirm msg %T", msg),
		))

		return promise.Future()
	}

	f.mu.Lock()
	f.requests = append(f.requests, req)
	state := f.responseStates[req.Tx.TxHash()]
	height := f.confirmHeights[req.Tx.TxHash()]
	f.mu.Unlock()

	if state == 0 {
		state = txconfirm.TxStateAwaitingConfirmation
	}

	if state == txconfirm.TxStateConfirmed {
		if height == 0 {
			height = 1
		}

		err := req.Subscriber.Tell(
			context.Background(),
			&txconfirm.TxConfirmed{
				Txid:        req.Tx.TxHash(),
				BlockHeight: height,
				NumConfs:    1,
			},
		)
		if err != nil {
			promise.Complete(fn.Err[txconfirm.Resp](err))
			return promise.Future()
		}
	}

	promise.Complete(fn.Ok[txconfirm.Resp](&txconfirm.EnsureConfirmedResp{
		Txid:    req.Tx.TxHash(),
		State:   state,
		Created: true,
	}))

	return promise.Future()
}

// lastRequest returns the latest txconfirm request.
func (f *fakeTxConfirmRef) lastRequest(
	t *testing.T) *txconfirm.EnsureConfirmedReq {

	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	require.NotEmpty(t, f.requests)

	return f.requests[len(f.requests)-1]
}

// requestCount returns the number of recorded ensure requests.
func (f *fakeTxConfirmRef) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.requests)
}

// requestCountForTxid returns how many ensure requests were made for one txid.
func (f *fakeTxConfirmRef) requestCountForTxid(txid chainhash.Hash) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	count := 0
	for _, req := range f.requests {
		if req.Tx.TxHash() == txid {
			count++
		}
	}

	return count
}

// requestedTxids returns the txids in request order.
func (f *fakeTxConfirmRef) requestedTxids() []chainhash.Hash {
	f.mu.Lock()
	defer f.mu.Unlock()

	txids := make([]chainhash.Hash, 0, len(f.requests))
	for _, req := range f.requests {
		txids = append(txids, req.Tx.TxHash())
	}

	return txids
}

// setImmediateConfirmed configures one txid to confirm as soon as it is
// ensured.
func (f *fakeTxConfirmRef) setImmediateConfirmed(txid chainhash.Hash,
	height int32) {

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.responseStates == nil {
		f.responseStates = make(map[chainhash.Hash]txconfirm.TxState)
	}
	if f.confirmHeights == nil {
		f.confirmHeights = make(map[chainhash.Hash]int32)
	}

	f.responseStates[txid] = txconfirm.TxStateConfirmed
	f.confirmHeights[txid] = height
}

// setImmediateFailed configures one txid to fail as soon as it is ensured.
func (f *fakeTxConfirmRef) setImmediateFailed(txid chainhash.Hash,
	reason string) {

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.responseStates == nil {
		f.responseStates = make(map[chainhash.Hash]txconfirm.TxState)
	}
	if f.failureReasons == nil {
		f.failureReasons = make(map[chainhash.Hash]string)
	}

	f.responseStates[txid] = txconfirm.TxStateFailed
	f.failureReasons[txid] = reason
}

// emitConfirmed delivers a txconfirm success notification to the subscriber.
func (f *fakeTxConfirmRef) emitConfirmed(t *testing.T, index int,
	txid chainhash.Hash, height int32) {

	t.Helper()

	f.mu.Lock()
	require.Less(t, index, len(f.requests))
	subscriber := f.requests[index].Subscriber
	targetConfs := f.requests[index].TargetConfs
	f.mu.Unlock()

	if targetConfs == 0 {
		targetConfs = 1
	}

	err := subscriber.Tell(t.Context(), &txconfirm.TxConfirmed{
		Txid:        txid,
		BlockHeight: height,
		NumConfs:    targetConfs,
	})
	require.NoError(t, err)
}

// emitFailed delivers a txconfirm failure notification to the subscriber.
func (f *fakeTxConfirmRef) emitFailed(t *testing.T, index int,
	txid chainhash.Hash, reason string) {

	t.Helper()

	f.mu.Lock()
	require.Less(t, index, len(f.requests))
	subscriber := f.requests[index].Subscriber
	f.mu.Unlock()

	err := subscriber.Tell(t.Context(), &txconfirm.TxFailed{
		Txid:   txid,
		Reason: reason,
	})
	require.NoError(t, err)
}

// fakeChainSourceRef is a minimal chainsource actor ref for sweep fee
// estimation tests.
type fakeChainSourceRef struct {
	mu         sync.Mutex
	bestHeight int32
	feeRate    int64
	feeErr     error
	blockRef   actor.TellOnlyRef[chainsource.BlockEpoch]
	spendRef   actor.TellOnlyRef[chainsource.SpendEvent]
}

// ID returns the fake actor ID.
func (f *fakeChainSourceRef) ID() string {
	return "fake-chain"
}

// Tell is unused by the unroll actor tests.
func (f *fakeChainSourceRef) Tell(_ context.Context,
	msg chainsource.ChainSourceMsg) error {

	switch msg.(type) {
	case *chainsource.UnsubscribeBlocksRequest:
		return nil
	case *chainsource.UnregisterSpendRequest:
		return nil
	}

	return nil
}

// Ask returns fixed fee-estimate responses.
func (f *fakeChainSourceRef) Ask(_ context.Context,
	msg chainsource.ChainSourceMsg,
) actor.Future[chainsource.ChainSourceResp] {

	promise := actor.NewPromise[chainsource.ChainSourceResp]()
	switch msg := msg.(type) {
	case *chainsource.BestHeightRequest:
		height := f.bestHeight
		if height == 0 {
			height = 100
		}

		promise.Complete(fn.Ok[chainsource.ChainSourceResp](
			&chainsource.BestHeightResponse{Height: height},
		))

	case *chainsource.FeeEstimateRequest:
		if f.feeErr != nil {
			promise.Complete(fn.Err[chainsource.ChainSourceResp](
				f.feeErr,
			))

			return promise.Future()
		}

		feeRate := f.feeRate
		if feeRate == 0 {
			feeRate = 5
		}

		promise.Complete(fn.Ok[chainsource.ChainSourceResp](
			&chainsource.FeeEstimateResponse{
				SatPerVByte: btcutil.Amount(feeRate),
			},
		))

	case *chainsource.SubscribeBlocksRequest:
		f.mu.Lock()
		f.blockRef = msg.NotifyActor.UnwrapOr(nil)
		f.mu.Unlock()
		promise.Complete(fn.Ok[chainsource.ChainSourceResp](
			&chainsource.SubscribeBlocksResponse{},
		))

	case *chainsource.RegisterSpendRequest:
		f.mu.Lock()
		f.spendRef = msg.NotifyActor.UnwrapOr(nil)
		f.mu.Unlock()
		promise.Complete(fn.Ok[chainsource.ChainSourceResp](
			&chainsource.RegisterSpendResponse{},
		))

	default:
		promise.Complete(fn.Err[chainsource.ChainSourceResp](
			fmt.Errorf("unexpected chainsource msg %T", msg),
		))
	}

	return promise.Future()
}

// emitSpend delivers one spend event for the target outpoint to the subscribed
// actor.
func (f *fakeChainSourceRef) emitSpend(t *testing.T,
	spendingTxid chainhash.Hash, height int32) {

	t.Helper()

	f.mu.Lock()
	ref := f.spendRef
	f.mu.Unlock()

	require.NotNil(t, ref)
	require.NoError(t, ref.Tell(
		t.Context(),
		chainsource.SpendEvent{
			SpendingTxid:   spendingTxid,
			SpendingHeight: height,
		},
	))
}

// fakeSweepWallet is a minimal signer plus wallet-destination test double.
type fakeSweepWallet struct{}

// NewWalletPkScript returns a deterministic destination script.
func (w *fakeSweepWallet) NewWalletPkScript(context.Context) ([]byte, error) {
	return []byte{txscript.OP_TRUE}, nil
}

// SignOutputRaw returns a dummy schnorr signature.
func (w *fakeSweepWallet) SignOutputRaw(*wire.MsgTx,
	*input.SignDescriptor) (input.Signature, error) {

	return testSignature{}, nil
}

// ComputeInputScript is unused by the timeout-path helper.
func (w *fakeSweepWallet) ComputeInputScript(*wire.MsgTx,
	*input.SignDescriptor) (*input.Script, error) {

	return nil, fmt.Errorf("unused")
}

// MuSig2CreateSession is unused in these tests.
func (w *fakeSweepWallet) MuSig2CreateSession(input.MuSig2Version,
	keychain.KeyLocator, []*btcec.PublicKey, *input.MuSig2Tweaks,
	[][musig2.PubNonceSize]byte, *musig2.Nonces) (*input.MuSig2SessionInfo,
	error) {

	return nil, fmt.Errorf("unused")
}

// MuSig2RegisterNonces is unused in these tests.
func (w *fakeSweepWallet) MuSig2RegisterNonces(input.MuSig2SessionID,
	[][musig2.PubNonceSize]byte) (bool, error) {

	return false, fmt.Errorf("unused")
}

// MuSig2RegisterCombinedNonce is unused in these tests.
func (w *fakeSweepWallet) MuSig2RegisterCombinedNonce(
	input.MuSig2SessionID, [musig2.PubNonceSize]byte,
) error {

	return fmt.Errorf("unused")
}

// MuSig2GetCombinedNonce is unused in these tests.
func (w *fakeSweepWallet) MuSig2GetCombinedNonce(
	input.MuSig2SessionID,
) ([musig2.PubNonceSize]byte, error) {

	return [musig2.PubNonceSize]byte{}, fmt.Errorf("unused")
}

// MuSig2Sign is unused in these tests.
func (w *fakeSweepWallet) MuSig2Sign(input.MuSig2SessionID,
	[sha256.Size]byte, bool) (*musig2.PartialSignature, error) {

	return nil, fmt.Errorf("unused")
}

// MuSig2CombineSig is unused in these tests.
func (w *fakeSweepWallet) MuSig2CombineSig(input.MuSig2SessionID,
	[]*musig2.PartialSignature) (*schnorr.Signature, bool, error) {

	return nil, false, fmt.Errorf("unused")
}

// MuSig2Cleanup is unused in these tests.
func (w *fakeSweepWallet) MuSig2Cleanup(input.MuSig2SessionID) error {
	return nil
}

// testSignature is a fixed-size dummy signature implementing input.Signature.
type testSignature struct{}

// Serialize returns a fixed-size signature blob.
func (s testSignature) Serialize() []byte {
	return bytes.Repeat([]byte{1}, 64)
}

// Verify always succeeds in tests.
func (s testSignature) Verify([]byte, *btcec.PublicKey) bool {
	return true
}

// memCheckpointStore is a minimal in-memory checkpoint store for durable actor
// tests.
type memCheckpointStore struct {
	mu          sync.Mutex
	checkpoints map[string]*actor.Checkpoint
}

// newMemCheckpointStore creates a new in-memory checkpoint store.
func newMemCheckpointStore() *memCheckpointStore {
	return &memCheckpointStore{
		checkpoints: make(map[string]*actor.Checkpoint),
	}
}

// SaveCheckpoint stores one checkpoint in memory.
func (s *memCheckpointStore) SaveCheckpoint(_ context.Context,
	params actor.CheckpointParams) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	s.checkpoints[params.ActorID] = &actor.Checkpoint{
		ActorID:   params.ActorID,
		StateType: params.StateType,
		StateData: append([]byte(nil), params.StateData...),
		Version:   params.Version,
	}

	return nil
}

// LoadCheckpoint returns one checkpoint when present.
func (s *memCheckpointStore) LoadCheckpoint(_ context.Context,
	actorID string) (*actor.Checkpoint, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	checkpoint, ok := s.checkpoints[actorID]
	if !ok {
		return nil, nil
	}

	copyCheckpoint := *checkpoint
	copyCheckpoint.StateData = append([]byte(nil), checkpoint.StateData...)

	return &copyCheckpoint, nil
}

// DeleteCheckpoint deletes one stored checkpoint.
func (s *memCheckpointStore) DeleteCheckpoint(_ context.Context,
	actorID string) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.checkpoints, actorID)

	return nil
}

// EnqueueMessage is unused in these tests.
func (s *memCheckpointStore) EnqueueMessage(context.Context,
	actor.EnqueueParams) error {

	return nil
}

// LeaseNextMessage is unused in these tests.
func (s *memCheckpointStore) LeaseNextMessage(context.Context, string, string,
	time.Duration) (*actor.LeasedMessage, error) {

	return nil, nil
}

// AckMessage is unused in these tests.
func (s *memCheckpointStore) AckMessage(context.Context, string,
	string) (int64, error) {

	return 1, nil
}

// NackMessage is unused in these tests.
func (s *memCheckpointStore) NackMessage(context.Context, string,
	string, time.Duration) (int64, error) {

	return 1, nil
}

// ExtendLease is unused in these tests.
func (s *memCheckpointStore) ExtendLease(context.Context, string,
	string, time.Duration) (int64, error) {

	return 1, nil
}

// MoveToDeadLetter is unused in these tests.
func (s *memCheckpointStore) MoveToDeadLetter(context.Context, string,
	string) error {

	return nil
}

// DeleteMessage is unused in these tests.
func (s *memCheckpointStore) DeleteMessage(context.Context, string) error {
	return nil
}

// SaveAskResult is unused in these tests.
func (s *memCheckpointStore) SaveAskResult(context.Context,
	actor.AskResultParams) error {

	return nil
}

// GetAskResult is unused in these tests.
func (s *memCheckpointStore) GetAskResult(context.Context,
	string) (*actor.AskResult, error) {

	return nil, nil
}

// DeleteAskResult is unused in these tests.
func (s *memCheckpointStore) DeleteAskResult(context.Context,
	string) error {

	return nil
}

// EnqueueOutbox is unused in these tests.
func (s *memCheckpointStore) EnqueueOutbox(context.Context,
	actor.OutboxParams) error {

	return nil
}

// ClaimOutboxBatch is unused in these tests.
func (s *memCheckpointStore) ClaimOutboxBatch(context.Context,
	actor.OutboxClaimParams) ([]actor.OutboxMessage, error) {

	return nil, nil
}

// CompleteOutbox is unused in these tests.
func (s *memCheckpointStore) CompleteOutbox(context.Context, string,
	string) error {

	return nil
}

// FailOutbox is unused in these tests.
func (s *memCheckpointStore) FailOutbox(context.Context, string,
	string) error {

	return nil
}

// IsProcessed is unused in these tests.
func (s *memCheckpointStore) IsProcessed(context.Context,
	string) (bool, error) {

	return false, nil
}

// MarkProcessed is unused in these tests.
func (s *memCheckpointStore) MarkProcessed(context.Context, string,
	string, time.Duration) error {

	return nil
}

// GetDeadLetter is unused in these tests.
func (s *memCheckpointStore) GetDeadLetter(context.Context,
	string) (*actor.DeadLetter, error) {

	return nil, nil
}

// ListDeadLetters is unused in these tests.
func (s *memCheckpointStore) ListDeadLetters(context.Context, string,
	int) ([]actor.DeadLetter, error) {

	return nil, nil
}

// DeleteDeadLetter is unused in these tests.
func (s *memCheckpointStore) DeleteDeadLetter(context.Context, string) error {
	return nil
}

// ExpireLeases is unused in these tests.
func (s *memCheckpointStore) ExpireLeases(context.Context) error {
	return nil
}

// CleanupExpired is unused in these tests.
func (s *memCheckpointStore) CleanupExpired(context.Context) error {
	return nil
}

// newActorHarness creates a new unroll actor behavior behind a regular
// in-memory actor while still persisting checkpoints to the fake store.
func newActorHarness(t *testing.T, proof *recovery.Proof,
	desc *vtxo.Descriptor) (*actor.Actor[Msg, Resp], *behavior,
	*fakeTxConfirmRef, *memCheckpointStore) {

	t.Helper()

	txconfirmRef := &fakeTxConfirmRef{}
	store := newMemCheckpointStore()
	cfg := Config{
		TargetOutpoint: proof.TargetOutpoint(),
		ActorID:        "unroll-test",
		DeliveryStore:  store,
		ProofAssembler: &mockProofAssembler{proof: proof},
		VTXOStore:      &mockVTXOStore{desc: desc},
		TxConfirmRef:   txconfirmRef,
		ChainSource:    &fakeChainSourceRef{},
		Wallet:         &fakeSweepWallet{},
		Log:            fn.Some(btclog.Disabled),
	}
	behavior := &behavior{
		cfg: cfg,
		log: btclog.Disabled,
	}
	err := behavior.restoreCheckpoint(t.Context())
	require.NoError(t, err)

	actorInstance := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "unroll-test",
		Behavior:    behavior,
		MailboxSize: 64,
	})
	behavior.selfRef = actorInstance.TellRef()
	actorInstance.Start()
	t.Cleanup(actorInstance.Stop)

	return actorInstance, behavior, txconfirmRef, store
}

// mustAsk asks the actor and unwraps the response.
func mustAsk(t *testing.T, ref actor.ActorRef[Msg, Resp],
	msg Msg) Resp {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	resp, err := ref.Ask(ctx, msg).Await(ctx).Unpack()
	require.NoError(t, err)

	return resp
}

// testDescriptor returns a sweep-capable descriptor.
func testDescriptor(t *testing.T, outpoint wire.OutPoint,
	csvDelay uint32) *vtxo.Descriptor {

	t.Helper()

	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	tapscript, err := arkscript.VTXOTapScript(
		ownerPriv.PubKey(), operatorPriv.PubKey(), csvDelay,
	)
	require.NoError(t, err)

	outputKey := txscript.ComputeTaprootOutputKey(
		tapscript.ControlBlock.InternalKey, tapscript.RootHash,
	)
	pkScript, err := txscript.PayToTaprootScript(outputKey)
	require.NoError(t, err)

	return &vtxo.Descriptor{
		Outpoint: outpoint,
		Amount:   50_000,
		PkScript: pkScript,
		ClientKey: keychain.KeyDescriptor{
			PubKey: ownerPriv.PubKey(),
		},
		OperatorKey:    operatorPriv.PubKey(),
		TapScript:      tapscript,
		RelativeExpiry: csvDelay,
	}
}

// buildLinearProof creates a simple root->target proof.
func buildLinearProof(t *testing.T) *recovery.Proof {
	t.Helper()

	rootTx := wire.NewMsgTx(2)
	rootTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 0,
		},
	})
	rootTx.AddTxOut(&wire.TxOut{
		Value:    70_000,
		PkScript: []byte{txscript.OP_TRUE},
	})

	targetTx := wire.NewMsgTx(2)
	targetTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  rootTx.TxHash(),
			Index: 0,
		},
	})
	targetTx.AddTxOut(&wire.TxOut{
		Value:    50_000,
		PkScript: []byte{txscript.OP_TRUE},
	})

	proof, err := recovery.NewProof(
		wire.OutPoint{Hash: targetTx.TxHash(), Index: 0},
		2,
		&recovery.Node{Kind: recovery.NodeKindTree, Tx: rootTx},
		&recovery.Node{Kind: recovery.NodeKindTree, Tx: targetTx},
	)
	require.NoError(t, err)

	return proof
}

// buildMergeProof creates a two-root proof whose target depends on both
// ancestors.
func buildMergeProof(t *testing.T) *recovery.Proof {
	t.Helper()

	leftRootTx := wire.NewMsgTx(2)
	leftRootTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 0,
		},
	})
	leftRootTx.AddTxOut(&wire.TxOut{
		Value:    40_000,
		PkScript: []byte{txscript.OP_TRUE},
	})

	rightRootTx := wire.NewMsgTx(2)
	rightRootTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{2},
			Index: 0,
		},
	})
	rightRootTx.AddTxOut(&wire.TxOut{
		Value:    45_000,
		PkScript: []byte{txscript.OP_TRUE},
	})

	targetTx := wire.NewMsgTx(2)
	targetTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  leftRootTx.TxHash(),
			Index: 0,
		},
	})
	targetTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  rightRootTx.TxHash(),
			Index: 0,
		},
	})
	targetTx.AddTxOut(&wire.TxOut{
		Value:    70_000,
		PkScript: []byte{txscript.OP_TRUE},
	})

	proof, err := recovery.NewProof(
		wire.OutPoint{Hash: targetTx.TxHash(), Index: 0},
		2,
		&recovery.Node{Kind: recovery.NodeKindTree, Tx: leftRootTx},
		&recovery.Node{Kind: recovery.NodeKindTree, Tx: rightRootTx},
		&recovery.Node{Kind: recovery.NodeKindTree, Tx: targetTx},
	)
	require.NoError(t, err)

	return proof
}

// mustDecodeCheckpoint loads and decodes one stored actor checkpoint.
func mustDecodeCheckpoint(t *testing.T, store *memCheckpointStore,
	actorID string) *actorCheckpoint {

	t.Helper()

	checkpoint, err := store.LoadCheckpoint(t.Context(), actorID)
	require.NoError(t, err)
	require.NotNil(t, checkpoint)

	decoded, err := decodeCheckpoint(checkpoint.StateData)
	require.NoError(t, err)

	return decoded
}

// TestStartUnrollSubmitsInitialFrontier verifies that actor start resolves the
// proof, plans the frontier, and sends the first ready tx to txconfirm.
func TestStartUnrollSubmitsInitialFrontier(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, _, txconfirmRef, store := newActorHarness(t, proof, desc)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})

	require.Equal(t, 1, txconfirmRef.requestCount())
	require.Equal(
		t, proof.RootTxids()[0],
		txconfirmRef.lastRequest(t).Tx.TxHash(),
	)

	checkpoint, err := store.LoadCheckpoint(t.Context(), "unroll-test")
	require.NoError(t, err)
	require.NotNil(t, checkpoint)
}

// TestConfirmedNodesAdvanceToSweep verifies that node confirmations move the
// actor from proof materialization into final sweep submission.
func TestConfirmedNodesAdvanceToSweep(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, _, txconfirmRef, _ := newActorHarness(t, proof, desc)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})
	require.Equal(t, 1, txconfirmRef.requestCount())

	txconfirmRef.emitConfirmed(t, 0, proof.RootTxids()[0], 101)

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() >= 2
	}, testTimeout, 10*time.Millisecond)
	require.Equal(t, proof.TargetOutpoint().Hash,
		txconfirmRef.lastRequest(t).Tx.TxHash())

	txconfirmRef.emitConfirmed(t, 1, proof.TargetOutpoint().Hash, 102)

	mustAsk(t, unrollActor.Ref(), &HeightObservedMsg{Height: 103})
	require.Equal(t, 2, txconfirmRef.requestCount())

	mustAsk(t, unrollActor.Ref(), &HeightObservedMsg{Height: 104})
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() >= 3
	}, testTimeout, 10*time.Millisecond)

	stateResp, ok := mustAsk(
		t, unrollActor.Ref(), &GetStateRequest{},
	).(*GetStateResp)
	require.True(t, ok)
	require.Equal(t, PhaseSweepConfirmation, stateResp.Phase)
	require.NotNil(t, stateResp.SweepTxid)
}

// TestResumeReissuesInflightWork verifies that resume reattaches the actor to
// in-flight proof txs without importing the old unroller subsystem.
func TestResumeReissuesInflightWork(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	_, _, txconfirmRef, store := newActorHarness(t, proof, desc)

	raw, err := encodeCheckpoint(&actorCheckpoint{
		Version: checkpointVersion,
		Height:  110,
		Started: true,
		Trigger: TriggerRestart,
		State: unrollplan.State{
			InFlightTxids: []chainhash.Hash{proof.RootTxids()[0]},
		},
	})
	require.NoError(t, err)

	err = store.SaveCheckpoint(t.Context(), actor.CheckpointParams{
		ActorID:   "resume-test",
		StateType: checkpointStateType,
		StateData: raw,
		Version:   checkpointVersion,
	})
	require.NoError(t, err)

	cfg := Config{
		TargetOutpoint: proof.TargetOutpoint(),
		ActorID:        "resume-test",
		DeliveryStore:  store,
		ProofAssembler: &mockProofAssembler{proof: proof},
		VTXOStore:      &mockVTXOStore{desc: desc},
		TxConfirmRef:   txconfirmRef,
		ChainSource:    &fakeChainSourceRef{},
		Wallet:         &fakeSweepWallet{},
		Log:            fn.Some(btclog.Disabled),
	}
	resumeBehavior := &behavior{
		cfg: cfg,
		log: btclog.Disabled,
	}
	err = resumeBehavior.restoreCheckpoint(t.Context())
	require.NoError(t, err)
	resumedActor := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "resume-test",
		Behavior:    resumeBehavior,
		MailboxSize: 64,
	})
	resumeBehavior.selfRef = resumedActor.TellRef()
	resumedActor.Start()
	t.Cleanup(resumedActor.Stop)

	mustAsk(t, resumedActor.Ref(), &ResumeUnrollRequest{Height: 111})

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() >= 1
	}, testTimeout, 10*time.Millisecond)
	require.Equal(
		t, proof.RootTxids()[0],
		txconfirmRef.lastRequest(t).Tx.TxHash(),
	)
}

// TestStartUnrollMultiParentSubmitsAllRoots verifies that the initial planner
// frontier contains every independent root transaction.
func TestStartUnrollMultiParentSubmitsAllRoots(t *testing.T) {
	proof := buildMergeProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, _, txconfirmRef, store := newActorHarness(t, proof, desc)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  200,
		Trigger: TriggerManual,
	})

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() == 2
	}, testTimeout, 10*time.Millisecond)

	require.ElementsMatch(
		t, proof.RootTxids(),
		txconfirmRef.requestedTxids(),
	)

	checkpoint := mustDecodeCheckpoint(t, store, "unroll-test")
	require.ElementsMatch(
		t, proof.RootTxids(), checkpoint.State.InFlightTxids,
	)
}

// TestMultiParentChildBlockedUntilAllParentsConfirm verifies that a merge node
// is not submitted until every required parent is confirmed.
func TestMultiParentChildBlockedUntilAllParentsConfirm(t *testing.T) {
	proof := buildMergeProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, _, txconfirmRef, _ := newActorHarness(t, proof, desc)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  200,
		Trigger: TriggerManual,
	})

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() == 2
	}, testTimeout, 10*time.Millisecond)

	rootTxids := proof.RootTxids()
	txconfirmRef.emitConfirmed(t, 0, rootTxids[0], 201)

	time.Sleep(25 * time.Millisecond)
	require.Equal(t, 2, txconfirmRef.requestCount())
	require.Equal(t, 0,
		txconfirmRef.requestCountForTxid(proof.TargetOutpoint().Hash))

	txconfirmRef.emitConfirmed(t, 1, rootTxids[1], 202)

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(
			proof.TargetOutpoint().Hash,
		) == 1
	}, testTimeout, 10*time.Millisecond)
}

// TestStartUnrollAlreadyConfirmedRootAdvances verifies that an already
// confirmed ancestor reported by txconfirm does not stall unroll progress.
func TestStartUnrollAlreadyConfirmedRootAdvances(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	txid := proof.RootTxids()[0]
	unrollActor, _, txconfirmRef, _ := newActorHarness(t, proof, desc)
	txconfirmRef.setImmediateConfirmed(txid, 101)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(
			proof.TargetOutpoint().Hash,
		) == 1
	}, testTimeout, 10*time.Millisecond)
}

// TestProofTxFailureTransitionsToFailed verifies that proof-transaction
// failure terminates the actor and persists the failure state.
func TestProofTxFailureTransitionsToFailed(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	rootTxid := proof.RootTxids()[0]
	unrollActor, _, txconfirmRef, store := newActorHarness(t, proof, desc)
	txconfirmRef.setImmediateFailed(rootTxid, "rejected")

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})

	require.Eventually(t, func() bool {
		stateResp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return stateResp.Phase == PhaseFailed
	}, testTimeout, 10*time.Millisecond)

	stateResp, ok := mustAsk(
		t, unrollActor.Ref(), &GetStateRequest{},
	).(*GetStateResp)
	require.True(t, ok)
	require.Contains(t, stateResp.FailReason, "txconfirm returned failed")

	checkpoint := mustDecodeCheckpoint(t, store, "unroll-test")
	require.Equal(t, "proof tx "+rootTxid.String()+
		" failed: txconfirm returned failed state", checkpoint.Fail)
}

// TestResumeReissuesSweepConfirmation verifies that resume reattaches
// txconfirm to an already-built sweep transaction.
func TestResumeReissuesSweepConfirmation(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	sweepTx, err := buildSweepTx(
		t.Context(), &fakeSweepWallet{}, &fakeChainSourceRef{},
		proof, desc, 0,
	)
	require.NoError(t, err)

	txconfirmRef := &fakeTxConfirmRef{}
	store := newMemCheckpointStore()
	sweepTxid := sweepTx.TxHash()

	raw, err := encodeCheckpoint(&actorCheckpoint{
		Version: checkpointVersion,
		Height:  110,
		Started: true,
		Trigger: TriggerRestart,
		State: unrollplan.State{
			ConfirmedTxids: []chainhash.Hash{
				proof.RootTxids()[0],
				proof.TargetOutpoint().Hash,
			},
			TargetConfirmHeight: fn.Some[int32](108),
			Sweep: unrollplan.SweepState{
				Status: unrollplan.SweepStatusBroadcasted,
				Txid:   fn.Some(sweepTxid),
			},
		},
		SweepTx: sweepTx,
	})
	require.NoError(t, err)

	err = store.SaveCheckpoint(t.Context(), actor.CheckpointParams{
		ActorID:   "resume-sweep-test",
		StateType: checkpointStateType,
		StateData: raw,
		Version:   checkpointVersion,
	})
	require.NoError(t, err)

	cfg := Config{
		TargetOutpoint: proof.TargetOutpoint(),
		ActorID:        "resume-sweep-test",
		DeliveryStore:  store,
		ProofAssembler: &mockProofAssembler{proof: proof},
		VTXOStore:      &mockVTXOStore{desc: desc},
		TxConfirmRef:   txconfirmRef,
		ChainSource:    &fakeChainSourceRef{},
		Wallet:         &fakeSweepWallet{},
		Log:            fn.Some(btclog.Disabled),
	}
	resumeBehavior := &behavior{
		cfg: cfg,
		log: btclog.Disabled,
	}
	err = resumeBehavior.restoreCheckpoint(t.Context())
	require.NoError(t, err)

	resumedActor := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "resume-sweep-test",
		Behavior:    resumeBehavior,
		MailboxSize: 64,
	})
	resumeBehavior.selfRef = resumedActor.TellRef()
	resumedActor.Start()
	t.Cleanup(resumedActor.Stop)

	mustAsk(t, resumedActor.Ref(), &ResumeUnrollRequest{Height: 111})

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(sweepTxid) == 1
	}, testTimeout, 10*time.Millisecond)
}

// TestBuildSweepTx verifies the copied sweep-construction helper works without
// importing the legacy unroller package.
func TestBuildSweepTx(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())

	sweepTx, err := buildSweepTx(
		t.Context(), &fakeSweepWallet{}, &fakeChainSourceRef{},
		proof, desc, 0,
	)
	require.NoError(t, err)
	require.Len(t, sweepTx.TxIn, 1)
	require.Len(t, sweepTx.TxOut, 1)
	require.NotEmpty(t, sweepTx.TxIn[0].Witness)
}

// TestBuildSweepTxFallsBackWithoutFeeEstimate verifies the sweep builder uses
// the regtest fallback fee when the backend has no estimate available yet.
func TestBuildSweepTxFallsBackWithoutFeeEstimate(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())

	sweepTx, err := buildSweepTx(
		t.Context(), &fakeSweepWallet{}, &fakeChainSourceRef{
			feeErr: fmt.Errorf("no fee estimates available"),
		}, proof, desc, 0,
	)
	require.NoError(t, err)

	targetOutput, err := proof.TargetOutput()
	require.NoError(t, err)

	expectedFee := defaultSweepFallbackFeeRateSatPerVByte *
		estimatedSweepVBytes
	require.Equal(t, targetOutput.Value-expectedFee, sweepTx.TxOut[0].Value)
}

// TestSweepConfirmationCompletesActor verifies that confirming the final sweep
// transitions the actor into terminal completion and persists that state.
func TestSweepConfirmationCompletesActor(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, _, txconfirmRef, store := newActorHarness(t, proof, desc)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})

	txconfirmRef.emitConfirmed(t, 0, proof.RootTxids()[0], 101)
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(
			proof.TargetOutpoint().Hash,
		) == 1
	}, testTimeout, 10*time.Millisecond)

	txconfirmRef.emitConfirmed(t, 1, proof.TargetOutpoint().Hash, 102)
	mustAsk(t, unrollActor.Ref(), &HeightObservedMsg{Height: 104})

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() == 3
	}, testTimeout, 10*time.Millisecond)

	sweepTxid := txconfirmRef.lastRequest(t).Tx.TxHash()
	txconfirmRef.emitConfirmed(t, 2, sweepTxid, 105)

	require.Eventually(t, func() bool {
		stateResp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return stateResp.Phase == PhaseCompleted
	}, testTimeout, 10*time.Millisecond)

	checkpoint := mustDecodeCheckpoint(t, store, "unroll-test")
	require.Equal(t, unrollplan.SweepStatusConfirmed,
		checkpoint.State.Sweep.Status)
	require.True(t, checkpoint.State.Sweep.ConfirmHeight.IsSome())

	// Late chain notifications can be queued behind the terminal
	// transition while the registry is draining the actor for cleanup.
	// They should ack as idempotent no-ops instead of retrying forever
	// against a terminal FSM state.
	mustAsk(t, unrollActor.Ref(), &SpendObservedMsg{
		SpendingTxid:   sweepTxid,
		SpendingHeight: 106,
	})
	mustAsk(t, unrollActor.Ref(), &HeightObservedMsg{Height: 106})
	mustAsk(t, unrollActor.Ref(), &TxConfirmedMsg{
		Txid:     sweepTxid,
		Height:   106,
		NumConfs: 1,
	})
}

// TestGetStateAfterFSMShutdownKeepsCompletedCheckpoint verifies that callers
// still observe the last checkpointed completed state after the protofsm has
// been stopped during actor teardown.
func TestGetStateAfterFSMShutdownKeepsCompletedCheckpoint(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, beh, txconfirmRef, _ := newActorHarness(t, proof, desc)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})

	txconfirmRef.emitConfirmed(t, 0, proof.RootTxids()[0], 101)
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(
			proof.TargetOutpoint().Hash,
		) == 1
	}, testTimeout, 10*time.Millisecond)

	txconfirmRef.emitConfirmed(t, 1, proof.TargetOutpoint().Hash, 102)
	mustAsk(t, unrollActor.Ref(), &HeightObservedMsg{Height: 104})

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() == 3
	}, testTimeout, 10*time.Millisecond)

	sweepTxid := txconfirmRef.lastRequest(t).Tx.TxHash()
	txconfirmRef.emitConfirmed(t, 2, sweepTxid, 105)

	require.Eventually(t, func() bool {
		stateResp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return stateResp.Phase == PhaseCompleted
	}, testTimeout, 10*time.Millisecond)

	require.NotNil(t, beh.session)
	require.NotNil(t, beh.session.FSM)

	beh.session.FSM.Stop()

	stateResp, ok := mustAsk(
		t, unrollActor.Ref(), &GetStateRequest{},
	).(*GetStateResp)
	require.True(t, ok)
	require.Equal(t, PhaseCompleted, stateResp.Phase)
	require.Empty(t, stateResp.FailReason)
	require.Equal(t, sweepTxid, *stateResp.SweepTxid)
}

// TestGetStateUsesStoredSweepTxid verifies that GetState keeps exposing the
// sweep txid when the checkpointed planner state is terminal but missing the
// txid field.
func TestGetStateUsesStoredSweepTxid(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, beh, _, _ := newActorHarness(t, proof, desc)

	sweepTx := wire.NewMsgTx(2)
	beh.pending = &actorCheckpoint{
		Version: checkpointVersion,
		Height:  106,
		Started: true,
		Trigger: TriggerManual,
		State: unrollplan.State{
			ConfirmedTxids: []chainhash.Hash{
				proof.TargetOutpoint().Hash,
			},
			TargetConfirmHeight: fn.Some[int32](103),
			Sweep: unrollplan.SweepState{
				Status:        unrollplan.SweepStatusConfirmed,
				ConfirmHeight: fn.Some[int32](106),
			},
		},
	}
	beh.sweepTx = sweepTx

	stateResp, ok := mustAsk(
		t, unrollActor.Ref(), &GetStateRequest{},
	).(*GetStateResp)
	require.True(t, ok)
	require.Equal(t, PhaseCompleted, stateResp.Phase)
	require.NotNil(t, stateResp.SweepTxid)
	require.Equal(t, sweepTx.TxHash(), *stateResp.SweepTxid)
}

// TestResumeMultiParentPartialConfirmation verifies that resume only reissues
// the remaining root transaction and advances once that root confirms.
func TestResumeMultiParentPartialConfirmation(t *testing.T) {
	proof := buildMergeProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	rootTxids := proof.RootTxids()
	store := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}

	raw, err := encodeCheckpoint(&actorCheckpoint{
		Version: checkpointVersion,
		Height:  210,
		Started: true,
		Trigger: TriggerRestart,
		State: unrollplan.State{
			ConfirmedTxids: []chainhash.Hash{rootTxids[0]},
			InFlightTxids:  []chainhash.Hash{rootTxids[1]},
		},
	})
	require.NoError(t, err)

	err = store.SaveCheckpoint(t.Context(), actor.CheckpointParams{
		ActorID:   "resume-partial-merge",
		StateType: checkpointStateType,
		StateData: raw,
		Version:   checkpointVersion,
	})
	require.NoError(t, err)

	cfg := Config{
		TargetOutpoint: proof.TargetOutpoint(),
		ActorID:        "resume-partial-merge",
		DeliveryStore:  store,
		ProofAssembler: &mockProofAssembler{proof: proof},
		VTXOStore:      &mockVTXOStore{desc: desc},
		TxConfirmRef:   txconfirmRef,
		ChainSource:    &fakeChainSourceRef{},
		Wallet:         &fakeSweepWallet{},
		Log:            fn.Some(btclog.Disabled),
	}
	resumeBehavior := &behavior{
		cfg: cfg,
		log: btclog.Disabled,
	}
	err = resumeBehavior.restoreCheckpoint(t.Context())
	require.NoError(t, err)

	resumedActor := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "resume-partial-merge",
		Behavior:    resumeBehavior,
		MailboxSize: 64,
	})
	resumeBehavior.selfRef = resumedActor.TellRef()
	resumedActor.Start()
	t.Cleanup(resumedActor.Stop)

	mustAsk(t, resumedActor.Ref(), &ResumeUnrollRequest{Height: 211})

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(rootTxids[1]) == 1
	}, testTimeout, 10*time.Millisecond)
	require.Equal(t, 0,
		txconfirmRef.requestCountForTxid(proof.TargetOutpoint().Hash))

	txconfirmRef.emitConfirmed(t, 0, rootTxids[1], 212)

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(
			proof.TargetOutpoint().Hash,
		) == 1
	}, testTimeout, 10*time.Millisecond)
}

// TestResumeCSVWaitDoesNotSweepUntilMature verifies that resuming in the CSV
// waiting phase does not build the sweep until a mature height is observed.
func TestResumeCSVWaitDoesNotSweepUntilMature(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	store := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}

	raw, err := encodeCheckpoint(&actorCheckpoint{
		Version: checkpointVersion,
		Height:  103,
		Started: true,
		Trigger: TriggerRestart,
		State: unrollplan.State{
			ConfirmedTxids: []chainhash.Hash{
				proof.RootTxids()[0],
				proof.TargetOutpoint().Hash,
			},
			TargetConfirmHeight: fn.Some[int32](102),
		},
	})
	require.NoError(t, err)

	err = store.SaveCheckpoint(t.Context(), actor.CheckpointParams{
		ActorID:   "resume-csv-test",
		StateType: checkpointStateType,
		StateData: raw,
		Version:   checkpointVersion,
	})
	require.NoError(t, err)

	cfg := Config{
		TargetOutpoint: proof.TargetOutpoint(),
		ActorID:        "resume-csv-test",
		DeliveryStore:  store,
		ProofAssembler: &mockProofAssembler{proof: proof},
		VTXOStore:      &mockVTXOStore{desc: desc},
		TxConfirmRef:   txconfirmRef,
		ChainSource:    &fakeChainSourceRef{},
		Wallet:         &fakeSweepWallet{},
		Log:            fn.Some(btclog.Disabled),
	}
	resumeBehavior := &behavior{
		cfg: cfg,
		log: btclog.Disabled,
	}
	err = resumeBehavior.restoreCheckpoint(t.Context())
	require.NoError(t, err)

	resumedActor := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "resume-csv-test",
		Behavior:    resumeBehavior,
		MailboxSize: 64,
	})
	resumeBehavior.selfRef = resumedActor.TellRef()
	resumedActor.Start()
	t.Cleanup(resumedActor.Stop)

	mustAsk(t, resumedActor.Ref(), &ResumeUnrollRequest{Height: 103})

	stateResp, ok := mustAsk(
		t, resumedActor.Ref(), &GetStateRequest{},
	).(*GetStateResp)
	require.True(t, ok)
	require.Equal(t, PhaseCSVPending, stateResp.Phase)
	require.Equal(t, 0, txconfirmRef.requestCount())

	mustAsk(t, resumedActor.Ref(), &HeightObservedMsg{Height: 104})

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() == 1
	}, testTimeout, 10*time.Millisecond)

	stateResp, ok = mustAsk(
		t, resumedActor.Ref(), &GetStateRequest{},
	).(*GetStateResp)
	require.True(t, ok)
	require.Equal(t, PhaseSweepConfirmation, stateResp.Phase)
}

// TestSweepFailureRetriesThenFails verifies that sweep txconfirm failures
// are retried up to maxSweepAttempts before the actor terminates.
func TestSweepFailureRetriesThenFails(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, _, txconfirmRef, store := newActorHarness(t, proof, desc)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})

	txconfirmRef.emitConfirmed(t, 0, proof.RootTxids()[0], 101)
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(
			proof.TargetOutpoint().Hash,
		) == 1
	}, testTimeout, 10*time.Millisecond)

	txconfirmRef.emitConfirmed(t, 1, proof.TargetOutpoint().Hash, 102)
	mustAsk(t, unrollActor.Ref(), &HeightObservedMsg{Height: 104})

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() == 3
	}, testTimeout, 10*time.Millisecond)

	// First sweep failure: should retry (attempt 1 of 3).
	sweep1Txid := txconfirmRef.lastRequest(t).Tx.TxHash()
	txconfirmRef.emitFailed(t, 2, sweep1Txid, "sweep rejected")

	// The retry should submit a new sweep to txconfirm.
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() == 4
	}, testTimeout, 10*time.Millisecond)

	// Actor must still be running, not failed.
	stateResp, ok := mustAsk(
		t, unrollActor.Ref(), &GetStateRequest{},
	).(*GetStateResp)
	require.True(t, ok)
	require.NotEqual(t, PhaseFailed, stateResp.Phase)

	// Second sweep failure: should retry (attempt 2 of 3).
	sweep2Txid := txconfirmRef.lastRequest(t).Tx.TxHash()
	txconfirmRef.emitFailed(t, 3, sweep2Txid, "sweep rejected again")

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() == 5
	}, testTimeout, 10*time.Millisecond)

	stateResp, ok = mustAsk(
		t, unrollActor.Ref(), &GetStateRequest{},
	).(*GetStateResp)
	require.True(t, ok)
	require.NotEqual(t, PhaseFailed, stateResp.Phase)

	// Third sweep failure: should transition to terminal failure.
	sweep3Txid := txconfirmRef.lastRequest(t).Tx.TxHash()
	txconfirmRef.emitFailed(t, 4, sweep3Txid, "sweep rejected final")

	require.Eventually(t, func() bool {
		stateResp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return stateResp.Phase == PhaseFailed
	}, testTimeout, 10*time.Millisecond)

	stateResp, ok = mustAsk(
		t, unrollActor.Ref(), &GetStateRequest{},
	).(*GetStateResp)
	require.True(t, ok)
	require.Contains(t, stateResp.FailReason, "sweep tx")

	checkpoint := mustDecodeCheckpoint(t, store, "unroll-test")
	require.Contains(t, checkpoint.Fail, "sweep tx")
	require.Equal(t, maxSweepAttempts, checkpoint.SweepAttempts)
}

// TestExternalSpendTerminatesActor verifies that an external spend of the
// target VTXO (not our proof nodes or sweep) terminates the actor.
func TestExternalSpendTerminatesActor(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, beh, _, _ := newActorHarness(t, proof, desc)

	chainSource, ok := beh.cfg.ChainSource.(*fakeChainSourceRef)
	require.True(t, ok)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})

	// Ensure spend watch is registered.
	require.Eventually(t, func() bool {
		chainSource.mu.Lock()
		defer chainSource.mu.Unlock()

		return chainSource.spendRef != nil
	}, testTimeout, 10*time.Millisecond)

	// Simulate an external party spending the target VTXO.
	externalTxid := chainhash.Hash{0xee}
	chainSource.emitSpend(t, externalTxid, 101)

	require.Eventually(t, func() bool {
		stateResp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return stateResp.Phase == PhaseFailed
	}, testTimeout, 10*time.Millisecond)

	stateResp, ok := mustAsk(
		t, unrollActor.Ref(), &GetStateRequest{},
	).(*GetStateResp)
	require.True(t, ok)
	require.Contains(t, stateResp.FailReason, "spent externally")
}

// TestStartUnrollIsIdempotent verifies that reissuing start does not duplicate
// already-in-flight proof requests.
func TestStartUnrollIsIdempotent(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, _, txconfirmRef, _ := newActorHarness(t, proof, desc)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})
	require.Equal(t, 1, txconfirmRef.requestCount())

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  101,
		Trigger: TriggerManual,
	})

	time.Sleep(25 * time.Millisecond)
	require.Equal(
		t, 1,
		txconfirmRef.requestCountForTxid(
			proof.RootTxids()[0],
		),
	)
}

var _ input.Signature = testSignature{}
var _ SweepWallet = (*fakeSweepWallet)(nil)
var _ vtxo.VTXOStore = (*mockVTXOStore)(nil)
var _ actor.DeliveryStore = (*memCheckpointStore)(nil)
var _ actor.ActorRef[txconfirm.Msg, txconfirm.Resp] = (*fakeTxConfirmRef)(nil)
var _ actor.ActorRef[
	chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
] = (*fakeChainSourceRef)(nil)
var _ *waddrmgr.Tapscript
var _ = btcutil.Amount(0)
var _ = psbt.Packet{}

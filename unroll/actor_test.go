package unroll

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/ledger"
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
func (m *mockProofAssembler) EnsureProof(_ context.Context, _ wire.OutPoint) (
	*recovery.Proof, error) {

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
func (m *mockVTXOStore) GetVTXO(context.Context, wire.OutPoint) (
	*vtxo.Descriptor, error) {

	return m.desc, m.err
}

// ListLiveVTXOs is unused in these tests.
func (m *mockVTXOStore) ListLiveVTXOs(context.Context) ([]*vtxo.Descriptor,
	error) {

	return nil, nil
}

// ListVTXOsByStatus is unused in these tests.
func (m *mockVTXOStore) ListVTXOsByStatus(context.Context, vtxo.VTXOStatus) (
	[]*vtxo.Descriptor, error) {

	return nil, nil
}

// ListSelectionCandidatesByStatus is unused in these tests.
func (m *mockVTXOStore) ListSelectionCandidatesByStatus(context.Context,
	vtxo.VTXOStatus) ([]vtxo.SelectedVTXO, error) {

	return nil, nil
}

// UpdateVTXOStatus is unused in these tests.
func (m *mockVTXOStore) UpdateVTXOStatus(context.Context, wire.OutPoint,
	vtxo.VTXOStatus) error {

	return nil
}

func (m *mockVTXOStore) UpdateVTXOStatusReleasingReservation(context.Context,
	wire.OutPoint, vtxo.VTXOStatus) error {

	return nil
}

// MarkForfeiting is unused in these tests.
func (m *mockVTXOStore) MarkForfeiting(context.Context, wire.OutPoint, string,
	*wire.MsgTx) error {

	return nil
}

// GetForfeitTx is unused in these tests.
func (m *mockVTXOStore) GetForfeitTx(context.Context, wire.OutPoint) (
	*wire.MsgTx, error) {

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

	// onAsk, when set, is invoked with each EnsureConfirmedReq as it is
	// recorded (outside the store lock). Tests use it to assert ordering
	// and locking invariants at the exact moment of the txconfirm broadcast
	// IO.
	onAsk func(*txconfirm.EnsureConfirmedReq)
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
		promise.Complete(
			fn.Err[txconfirm.Resp](
				fmt.Errorf("unexpected txconfirm msg %T", msg),
			),
		)

		return promise.Future()
	}

	f.mu.Lock()
	f.requests = append(f.requests, req)
	state := f.responseStates[req.Tx.TxHash()]
	height := f.confirmHeights[req.Tx.TxHash()]
	onAsk := f.onAsk
	f.mu.Unlock()

	// Fire the ordering hook outside the store lock, after recording the
	// request but before the response resolves, so a test observes the
	// exact moment the unroll actor is doing its txconfirm IO.
	if onAsk != nil {
		onAsk(req)
	}

	if state == 0 {
		state = txconfirm.TxStateAwaitingConfirmation
	}

	if state == txconfirm.TxStateConfirmed {
		if height == 0 {
			height = 1
		}

		//nolint:contextcheck // fake txconfirm delivers immediately
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

	promise.Complete(
		fn.Ok[txconfirm.Resp](
			&txconfirm.EnsureConfirmedResp{
				Txid:    req.Tx.TxHash(),
				State:   state,
				Created: true,
			},
		),
	)

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

// requestByTxid returns the first txconfirm request matching txid.
func (f *fakeTxConfirmRef) requestByTxid(t *testing.T,
	txid chainhash.Hash) *txconfirm.EnsureConfirmedReq {

	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	for _, req := range f.requests {
		if req.Tx.TxHash() == txid {
			return req
		}
	}

	require.Failf(t, "txconfirm request not found", "txid=%s", txid)

	return nil
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

// emitConfirmedByTxid delivers a txconfirm success notification for the first
// request matching txid.
func (f *fakeTxConfirmRef) emitConfirmedByTxid(t *testing.T,
	txid chainhash.Hash, height int32) {

	t.Helper()

	f.mu.Lock()
	index := -1
	for i, req := range f.requests {
		if req.Tx.TxHash() == txid {
			index = i
			break
		}
	}
	f.mu.Unlock()

	require.NotEqual(t, -1, index)
	f.emitConfirmed(t, index, txid, height)
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

// emitReorged delivers a txconfirm reorg notification to the subscriber
// behind the request at index.
func (f *fakeTxConfirmRef) emitReorged(t *testing.T, index int,
	txid chainhash.Hash) {

	t.Helper()

	f.mu.Lock()
	require.Less(t, index, len(f.requests))
	subscriber := f.requests[index].Subscriber
	f.mu.Unlock()

	err := subscriber.Tell(t.Context(), &txconfirm.TxReorged{
		Txid: txid,
	})
	require.NoError(t, err)
}

// emitFinalized delivers a txconfirm finalized notification to the
// subscriber behind the request at index.
func (f *fakeTxConfirmRef) emitFinalized(t *testing.T, index int,
	txid chainhash.Hash) {

	t.Helper()

	f.mu.Lock()
	require.Less(t, index, len(f.requests))
	subscriber := f.requests[index].Subscriber
	f.mu.Unlock()

	err := subscriber.Tell(t.Context(), &txconfirm.TxFinalized{
		Txid: txid,
	})
	require.NoError(t, err)
}

// confRef aliases the chainsource confirmation notification target.
type confRef = actor.TellOnlyRef[chainsource.ConfirmationEvent]

// confReq aliases the chainsource confirmation request.
type confReq = chainsource.RegisterConfRequest

// fakeChainSourceRef is a minimal chainsource actor ref for sweep fee
// estimation tests.
type fakeChainSourceRef struct {
	mu                sync.Mutex
	bestHeight        int32
	feeRate           int64
	feeErr            error
	blockRef          actor.TellOnlyRef[chainsource.BlockEpoch]
	spendRefs         map[wire.OutPoint]spendEventRef
	spendRegs         []wire.OutPoint
	spendReorgedRef   actor.TellOnlyRef[chainsource.SpendReorgedEvent]
	spendFinalizedRef actor.TellOnlyRef[chainsource.SpendDoneEvent]
	confRefs          map[chainhash.Hash]confRef
	confReqs          map[chainhash.Hash]*confReq
}

// spendEventRef is the fake chain-source spend notification actor reference.
type spendEventRef = actor.TellOnlyRef[chainsource.SpendEvent]

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

	case *chainsource.UnregisterConfRequest:
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

		promise.Complete(
			fn.Ok[chainsource.ChainSourceResp](
				&chainsource.BestHeightResponse{
					Height: height,
				},
			),
		)

	case *chainsource.FeeEstimateRequest:
		if f.feeErr != nil {
			promise.Complete(
				fn.Err[chainsource.ChainSourceResp](
					f.feeErr,
				),
			)

			return promise.Future()
		}

		feeRate := f.feeRate
		if feeRate == 0 {
			feeRate = 5
		}

		promise.Complete(
			fn.Ok[chainsource.ChainSourceResp](
				&chainsource.FeeEstimateResponse{
					SatPerVByte: btcutil.Amount(feeRate),
				},
			),
		)

	case *chainsource.SubscribeBlocksRequest:
		f.mu.Lock()
		f.blockRef = msg.NotifyActor.UnwrapOr(nil)
		f.mu.Unlock()
		promise.Complete(
			fn.Ok[chainsource.ChainSourceResp](
				&chainsource.SubscribeBlocksResponse{},
			),
		)

	case *chainsource.RegisterSpendRequest:
		f.mu.Lock()
		if f.spendRefs == nil {
			f.spendRefs = make(
				map[wire.OutPoint]spendEventRef,
			)
		}

		outpoint := wire.OutPoint{}
		if msg.Outpoint != nil {
			outpoint = *msg.Outpoint
		}
		f.spendRefs[outpoint] = msg.NotifyActor.UnwrapOr(nil)
		f.spendRegs = append(f.spendRegs, outpoint)

		// Reorg/finalized refs are only wired by ensureSpendWatch
		// (target outpoint). Proof-node spend watches from
		// ensureProofSpendWatches leave these unset; capture them
		// only when the caller actually provided them so a later
		// proof-node registration cannot wipe the target's refs.
		if msg.NotifyReorged.IsSome() {
			f.spendReorgedRef = msg.NotifyReorged.UnwrapOr(nil)
		}
		if msg.NotifyDone.IsSome() {
			f.spendFinalizedRef = msg.NotifyDone.UnwrapOr(nil)
		}
		f.mu.Unlock()
		promise.Complete(
			fn.Ok[chainsource.ChainSourceResp](
				&chainsource.RegisterSpendResponse{},
			),
		)

	case *chainsource.RegisterConfRequest:
		if msg.Txid == nil {
			promise.Complete(
				fn.Err[chainsource.ChainSourceResp](
					fmt.Errorf("register conf txid " +
						"required"),
				),
			)

			return promise.Future()
		}

		f.mu.Lock()
		if f.confRefs == nil {
			f.confRefs = make(map[chainhash.Hash]confRef)
		}
		if f.confReqs == nil {
			f.confReqs = map[chainhash.Hash]*confReq{}
		}

		reqCopy := *msg
		txidCopy := *msg.Txid
		reqCopy.Txid = &txidCopy
		reqCopy.PkScript = append([]byte(nil), msg.PkScript...)
		f.confRefs[*msg.Txid] = msg.NotifyActor.UnwrapOr(nil)
		f.confReqs[*msg.Txid] = &reqCopy
		f.mu.Unlock()
		promise.Complete(
			fn.Ok[chainsource.ChainSourceResp](
				&chainsource.RegisterConfResponse{},
			),
		)

	default:
		promise.Complete(
			fn.Err[chainsource.ChainSourceResp](
				fmt.Errorf("unexpected chainsource msg %T",
					msg),
			),
		)
	}

	return promise.Future()
}

// confWatchCount returns the number of registered confirmation watches.
func (f *fakeChainSourceRef) confWatchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.confRefs)
}

// confRequest returns the registered confirmation request for txid.
func (f *fakeChainSourceRef) confRequest(t *testing.T,
	txid chainhash.Hash) *confReq {

	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	req := f.confReqs[txid]
	require.NotNil(t, req)

	return req
}

// emitConfirmed delivers one chainsource confirmation event to a watcher.
func (f *fakeChainSourceRef) emitConfirmed(t *testing.T, txid chainhash.Hash,
	height int32) {

	t.Helper()

	f.mu.Lock()
	ref := f.confRefs[txid]
	f.mu.Unlock()

	require.NotNil(t, ref)
	require.NoError(
		t,
		ref.Tell(
			t.Context(), chainsource.ConfirmationEvent{
				Txid:        txid,
				BlockHeight: height,
				NumConfs:    1,
			},
		),
	)
}

// emitSpendForOutpoint delivers one spend event for a specific watched
// outpoint.
func (f *fakeChainSourceRef) emitSpendForOutpoint(t *testing.T,
	outpoint wire.OutPoint, spendingTxid chainhash.Hash, height int32) {

	t.Helper()

	f.mu.Lock()
	ref := f.spendRefs[outpoint]
	f.mu.Unlock()

	require.NotNil(t, ref)
	require.NoError(
		t,
		ref.Tell(
			t.Context(), chainsource.SpendEvent{
				Outpoint:       outpoint,
				SpendingTxid:   spendingTxid,
				SpendingHeight: height,
			},
		),
	)
}

// spendRegistrations returns a snapshot of registered spend outpoints.
func (f *fakeChainSourceRef) spendRegistrations() []wire.OutPoint {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]wire.OutPoint(nil), f.spendRegs...)
}

// emitSpendReorged delivers a SpendReorgedEvent to the subscribed actor.
func (f *fakeChainSourceRef) emitSpendReorged(t *testing.T) {
	t.Helper()

	f.mu.Lock()
	ref := f.spendReorgedRef
	f.mu.Unlock()

	require.NotNil(t, ref)
	require.NoError(
		t,
		ref.Tell(
			t.Context(), chainsource.SpendReorgedEvent{},
		),
	)
}

// emitSpendFinalized delivers a SpendDoneEvent to the subscribed actor.
func (f *fakeChainSourceRef) emitSpendFinalized(t *testing.T) {
	t.Helper()

	f.mu.Lock()
	ref := f.spendFinalizedRef
	f.mu.Unlock()

	require.NotNil(t, ref)
	require.NoError(
		t,
		ref.Tell(
			t.Context(), chainsource.SpendDoneEvent{},
		),
	)
}

// fakeSweepWallet is a minimal signer plus wallet-destination test double.
type fakeSweepWallet struct{}

// NewWalletPkScript returns a deterministic destination script.
func (w *fakeSweepWallet) NewWalletPkScript(context.Context) ([]byte, error) {
	return []byte{txscript.OP_TRUE}, nil
}

// SignOutputRaw returns a dummy schnorr signature.
func (w *fakeSweepWallet) SignOutputRaw(*wire.MsgTx, *input.SignDescriptor) (
	input.Signature, error) {

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
func (w *fakeSweepWallet) MuSig2GetCombinedNonce(input.MuSig2SessionID) (
	[musig2.PubNonceSize]byte, error) {

	return [musig2.PubNonceSize]byte{}, fmt.Errorf("unused")
}

// MuSig2Sign is unused in these tests.
func (w *fakeSweepWallet) MuSig2Sign(input.MuSig2SessionID, [sha256.Size]byte,
	bool) (*musig2.PartialSignature, error) {

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
func (s *memCheckpointStore) LoadCheckpoint(_ context.Context, actorID string) (
	*actor.Checkpoint, error) {

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

// PeekNextMessage is unused in these tests.
func (s *memCheckpointStore) PeekNextMessage(context.Context, string) (
	*actor.LeasedMessage, error) {

	return nil, nil
}

// AckMessage is unused in these tests.
func (s *memCheckpointStore) AckMessage(context.Context, string, string) (int64,
	error) {

	return 1, nil
}

// AckMessageByID is unused in these tests.
func (s *memCheckpointStore) AckMessageByID(context.Context, string) (int64,
	error) {

	return 1, nil
}

// NackMessage is unused in these tests.
func (s *memCheckpointStore) NackMessage(context.Context, string, string,
	time.Duration) (int64, error) {

	return 1, nil
}

// NackMessageByID is unused in these tests.
func (s *memCheckpointStore) NackMessageByID(context.Context, string,
	time.Duration) (int64, error) {

	return 1, nil
}

// ExtendLease is unused in these tests.
func (s *memCheckpointStore) ExtendLease(context.Context, string, string,
	time.Duration) (int64, error) {

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
func (s *memCheckpointStore) GetAskResult(context.Context, string) (
	*actor.AskResult, error) {

	return nil, nil
}

// DeleteAskResult is unused in these tests.
func (s *memCheckpointStore) DeleteAskResult(context.Context, string) error {
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
func (s *memCheckpointStore) FailOutbox(context.Context, string, string) error {
	return nil
}

// IsProcessed is unused in these tests.
func (s *memCheckpointStore) IsProcessed(context.Context, string) (bool,
	error) {

	return false, nil
}

// MarkProcessed is unused in these tests.
func (s *memCheckpointStore) MarkProcessed(context.Context, string, string,
	time.Duration) error {

	return nil
}

// GetDeadLetter is unused in these tests.
func (s *memCheckpointStore) GetDeadLetter(context.Context, string) (
	*actor.DeadLetter, error) {

	return nil, nil
}

// ListDeadLetters is unused in these tests.
func (s *memCheckpointStore) ListDeadLetters(context.Context, string, int) (
	[]actor.DeadLetter, error) {

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

// memExec is an in-memory actor.Exec[unrollTx] for unroll tests. Read, Stage,
// and Commit each run their closure directly against the shared checkpoint
// store; there is no real lease fence, so Commit ordinarily "consumes" by just
// running its write. Hooks let tests observe Stage/Commit ordering relative to
// the txconfirm IO and inject a lost-lease failure.
type memExec struct {
	store   actor.DeliveryStore
	actorID string

	// inWriteTxn is set while a Stage or Commit closure runs and cleared
	// once it returns, so a test can assert no writer transaction is held
	// across an outbox Ask (the contention win this migration targets).
	inWriteTxn atomic.Bool

	// stageCount and commitCount record how many Stage/Commit transactions
	// ran, for ordering and "exactly one consume" assertions.
	stageCount  atomic.Int64
	commitCount atomic.Int64

	// commitLeaseLost, when set, makes Commit fail with actor.ErrLeaseLost
	// without running its closure (and without acking), simulating a lost
	// lease at the consume step. The Staged writes that already ran are
	// unaffected. It is an atomic so a test can flip it between messages
	// without racing the actor goroutine.
	commitLeaseLost atomic.Bool
}

// newMemExec builds an in-memory Exec writing through the given store under the
// given actor ID.
func newMemExec(store actor.DeliveryStore, actorID string) *memExec {
	return &memExec{store: store, actorID: actorID}
}

// newMemExecFor builds an in-memory Exec for a behavior from its own config, so
// the test Exec writes through the same DeliveryStore and actor ID the behavior
// would use in production.
func newMemExecFor(b *behavior) *memExec {
	return newMemExec(b.cfg.DeliveryStore, b.cfg.ActorID)
}

// tx returns the transaction-scoped store handed to each closure.
func (e *memExec) tx() unrollTx {
	return unrollTx{store: e.store, actorID: e.actorID}
}

// Read implements actor.Exec by running fn against the store with no writer.
func (e *memExec) Read(ctx context.Context,
	fn func(context.Context, unrollTx) error) error {

	return fn(ctx, e.tx())
}

// Stage implements actor.Exec by running fn in a (simulated) short writer
// transaction that neither acks nor dedups the message.
func (e *memExec) Stage(ctx context.Context,
	fn func(context.Context, unrollTx) error) error {

	e.inWriteTxn.Store(true)
	defer e.inWriteTxn.Store(false)

	if err := fn(ctx, e.tx()); err != nil {
		return err
	}

	e.stageCount.Add(1)

	return nil
}

// Commit implements actor.Exec by folding the (simulated) lease-fenced ack into
// fn's writer transaction. When commitLeaseLost is set it models a lost lease:
// the closure never runs and nothing is consumed.
func (e *memExec) Commit(ctx context.Context,
	fn func(context.Context, unrollTx) error) error {

	if e.commitLeaseLost.Load() {
		return actor.ErrLeaseLost
	}

	e.inWriteTxn.Store(true)
	defer e.inWriteTxn.Store(false)

	if err := fn(ctx, e.tx()); err != nil {
		return err
	}

	e.commitCount.Add(1)

	return nil
}

// txExecAdapter drives a TxBehavior behind a plain in-memory actor for tests.
// It implements the classic actor.ActorBehavior surface by handing the behavior
// a memExec, so the existing ref-driven tests keep working while the behavior
// exercises its real Read/Stage/Commit code paths.
type txExecAdapter struct {
	b  *behavior
	ax *memExec
}

// Receive implements actor.ActorBehavior by delegating to the TxBehavior with
// the in-memory Exec handle.
func (a *txExecAdapter) Receive(ctx context.Context, msg Msg) fn.Result[Resp] {
	return a.b.Receive(ctx, msg, a.ax)
}

// OnStop forwards to the behavior's cleanup so subscription teardown still runs
// when the test actor stops.
func (a *txExecAdapter) OnStop(ctx context.Context) error {
	return a.b.OnStop(ctx)
}

// adaptTx wraps a TxBehavior behind the classic ActorBehavior surface for
// tests, using an Exec derived from the behavior's own config.
func adaptTx(b *behavior) *txExecAdapter {
	return &txExecAdapter{b: b, ax: newMemExecFor(b)}
}

// newActorHarness creates a new unroll actor behavior behind a regular
// in-memory actor while still persisting checkpoints to the fake store.
func newActorHarness(t *testing.T, proof *recovery.Proof,
	desc *vtxo.Descriptor) (*actor.Actor[Msg, Resp], *behavior,
	*fakeTxConfirmRef, *memCheckpointStore) {

	t.Helper()

	inst, b, ref, store, _ := newActorHarnessExec(t, proof, desc)

	return inst, b, ref, store
}

// newActorHarnessExec is newActorHarness plus the in-memory Exec the behavior
// runs on, so a test can observe Stage/Commit ordering, assert no writer
// transaction is held across the txconfirm IO, and inject a lost lease at the
// consume step.
func newActorHarnessExec(t *testing.T, proof *recovery.Proof,
	desc *vtxo.Descriptor) (*actor.Actor[Msg, Resp], *behavior,
	*fakeTxConfirmRef, *memCheckpointStore, *memExec) {

	t.Helper()

	txconfirmRef := &fakeTxConfirmRef{}
	store := newMemCheckpointStore()
	cfg := Config{
		TargetOutpoint: proof.TargetOutpoint(),
		ActorID:        "unroll-test",
		DeliveryStore:  store,
		ProofAssembler: &mockProofAssembler{
			proof: proof,
		},
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
		TxConfirmRef: txconfirmRef,
		ChainSource:  &fakeChainSourceRef{},
		Wallet:       &fakeSweepWallet{},
		Log:          fn.Some(btclog.Disabled),
	}
	behavior := &behavior{
		cfg: cfg,
		log: btclog.Disabled,
	}
	err := behavior.restoreCheckpoint(t.Context())
	require.NoError(t, err)

	exec := newMemExecFor(behavior)
	actorInstance := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "unroll-test",
		Behavior:    &txExecAdapter{b: behavior, ax: exec},
		MailboxSize: 64,
	})
	behavior.selfRef = actorInstance.TellRef()
	actorInstance.Start()
	t.Cleanup(actorInstance.Stop)

	return actorInstance, behavior, txconfirmRef, store, exec
}

// mustAsk asks the actor and unwraps the response.
func mustAsk(t *testing.T, ref actor.ActorRef[Msg, Resp], msg Msg) Resp {
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
	_, err = txscript.PayToTaprootScript(outputKey)
	require.NoError(t, err)

	// Match the test proof builders' OP_TRUE target output pkScript so
	// the production-side pkScript invariant in StandardVTXOExitSpendPolicy
	// is satisfied by the in-memory test fixtures.
	return &vtxo.Descriptor{
		Outpoint: outpoint,
		Amount:   50_000,
		PkScript: []byte{
			txscript.OP_TRUE,
		},
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
		wire.OutPoint{
			Hash:  targetTx.TxHash(),
			Index: 0,
		},
		2, &recovery.Node{
			Kind: recovery.NodeKindTree,
			Tx:   rootTx,
		}, &recovery.Node{
			Kind: recovery.NodeKindTree,
			Tx:   targetTx,
		},
	)
	require.NoError(t, err)

	return proof
}

// buildSiblingOutputProof creates a root->target proof where the target tx has
// a sibling output at index zero and the real target at index one.
func buildSiblingOutputProof(t *testing.T) *recovery.Proof {
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
		Value:    20_000,
		PkScript: []byte{txscript.OP_TRUE},
	})
	targetTx.AddTxOut(&wire.TxOut{
		Value:    50_000,
		PkScript: []byte{txscript.OP_TRUE},
	})

	proof, err := recovery.NewProof(
		wire.OutPoint{
			Hash:  targetTx.TxHash(),
			Index: 1,
		},
		2, &recovery.Node{
			Kind: recovery.NodeKindTree,
			Tx:   rootTx,
		}, &recovery.Node{
			Kind: recovery.NodeKindTree,
			Tx:   targetTx,
		},
	)
	require.NoError(t, err)

	return proof
}

// buildOORProof creates a checkpoint->ark proof.
func buildOORProof(t *testing.T) *recovery.Proof {
	t.Helper()

	checkpointTx := wire.NewMsgTx(2)
	checkpointTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 0,
		},
	})
	checkpointTx.AddTxOut(&wire.TxOut{
		Value:    70_000,
		PkScript: []byte{txscript.OP_TRUE},
	})

	arkTx := wire.NewMsgTx(2)
	arkTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  checkpointTx.TxHash(),
			Index: 0,
		},
	})
	arkTx.AddTxOut(&wire.TxOut{
		Value:    50_000,
		PkScript: []byte{txscript.OP_TRUE},
	})

	proof, err := recovery.NewProof(
		wire.OutPoint{Hash: arkTx.TxHash(), Index: 0},
		144,
		&recovery.Node{
			Kind: recovery.NodeKindCheckpoint,
			Tx:   checkpointTx,
		},
		&recovery.Node{
			Kind: recovery.NodeKindArk,
			Tx:   arkTx,
		},
	)
	require.NoError(t, err)

	return proof
}

// buildSharedArkOORProof creates two checkpoint roots feeding one ark target.
func buildSharedArkOORProof(t *testing.T) *recovery.Proof {
	t.Helper()

	leftCheckpoint := wire.NewMsgTx(2)
	leftCheckpoint.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 0,
		},
	})
	leftCheckpoint.AddTxOut(&wire.TxOut{
		Value:    40_000,
		PkScript: []byte{txscript.OP_TRUE},
	})

	rightCheckpoint := wire.NewMsgTx(2)
	rightCheckpoint.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{2},
			Index: 0,
		},
	})
	rightCheckpoint.AddTxOut(&wire.TxOut{
		Value:    45_000,
		PkScript: []byte{txscript.OP_TRUE},
	})

	arkTx := wire.NewMsgTx(2)
	arkTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  leftCheckpoint.TxHash(),
			Index: 0,
		},
	})
	arkTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  rightCheckpoint.TxHash(),
			Index: 0,
		},
	})
	arkTx.AddTxOut(&wire.TxOut{
		Value:    70_000,
		PkScript: []byte{txscript.OP_TRUE},
	})

	proof, err := recovery.NewProof(
		wire.OutPoint{Hash: arkTx.TxHash(), Index: 0},
		144,
		&recovery.Node{
			Kind: recovery.NodeKindCheckpoint,
			Tx:   leftCheckpoint,
		},
		&recovery.Node{
			Kind: recovery.NodeKindCheckpoint,
			Tx:   rightCheckpoint,
		},
		&recovery.Node{
			Kind: recovery.NodeKindArk,
			Tx:   arkTx,
		},
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
		wire.OutPoint{
			Hash:  targetTx.TxHash(),
			Index: 0,
		},
		2, &recovery.Node{
			Kind: recovery.NodeKindTree,
			Tx:   leftRootTx,
		}, &recovery.Node{
			Kind: recovery.NodeKindTree,
			Tx:   rightRootTx,
		}, &recovery.Node{
			Kind: recovery.NodeKindTree,
			Tx:   targetTx,
		},
	)
	require.NoError(t, err)

	return proof
}

// buildMultihopOORProof creates a checkpoint_AB -> arktx_AB ->
// checkpoint_BC -> arktx_BC (target) proof. The intermediate checkpoint_BC
// is the only frontier item that gets promoted from blocked to ready by an
// in-proof TxConfirmedEvent (the arktx_AB confirmation), which is the shape
// the deferral-anchor race needs to reproduce.
func buildMultihopOORProof(t *testing.T) *recovery.Proof {
	t.Helper()

	checkpointAB := wire.NewMsgTx(2)
	checkpointAB.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 0,
		},
	})
	checkpointAB.AddTxOut(&wire.TxOut{
		Value:    70_000,
		PkScript: []byte{txscript.OP_TRUE},
	})

	arkAB := wire.NewMsgTx(2)
	arkAB.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  checkpointAB.TxHash(),
			Index: 0,
		},
	})
	arkAB.AddTxOut(&wire.TxOut{
		Value:    65_000,
		PkScript: []byte{txscript.OP_TRUE},
	})

	checkpointBC := wire.NewMsgTx(2)
	checkpointBC.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  arkAB.TxHash(),
			Index: 0,
		},
	})
	checkpointBC.AddTxOut(&wire.TxOut{
		Value:    60_000,
		PkScript: []byte{txscript.OP_TRUE},
	})

	arkBC := wire.NewMsgTx(2)
	arkBC.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  checkpointBC.TxHash(),
			Index: 0,
		},
	})
	arkBC.AddTxOut(&wire.TxOut{
		Value:    55_000,
		PkScript: []byte{txscript.OP_TRUE},
	})

	proof, err := recovery.NewProof(
		wire.OutPoint{
			Hash:  arkBC.TxHash(),
			Index: 0,
		},
		144,
		&recovery.Node{
			Kind: recovery.NodeKindCheckpoint,
			Tx:   checkpointAB,
		},
		&recovery.Node{
			Kind: recovery.NodeKindArk,
			Tx:   arkAB,
		},
		&recovery.Node{
			Kind: recovery.NodeKindCheckpoint,
			Tx:   checkpointBC,
		},
		&recovery.Node{
			Kind: recovery.NodeKindArk,
			Tx:   arkBC,
		},
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

// TestFraudTriggerDefersReadyCheckpoint verifies fraud-triggered recovery
// watches a ready checkpoint before asking txconfirm to broadcast it.
func TestFraudTriggerDefersReadyCheckpoint(t *testing.T) {
	proof := buildOORProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	desc.CreatedHeight = 77
	unrollActor, behavior, txconfirmRef, store := newActorHarness(
		t, proof, desc,
	)
	chainRef, ok := behavior.cfg.ChainSource.(*fakeChainSourceRef)
	require.True(t, ok)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerFraudSpend,
	})

	require.Equal(t, 0, txconfirmRef.requestCount())
	require.Equal(t, 1, chainRef.confWatchCount())
	require.Equal(
		t, uint32(1),
		chainRef.confRequest(t, proof.RootTxids()[0]).HeightHint,
	)

	checkpoint := mustDecodeCheckpoint(t, store, "unroll-test")
	require.Len(t, checkpoint.DeferredCheckpoints, 1)
	require.Equal(
		t, proof.RootTxids()[0], checkpoint.DeferredCheckpoints[0].Txid,
	)
	require.Equal(
		t, int32(220), checkpoint.DeferredCheckpoints[0].DeadlineHeight,
	)

	mustAsk(t, unrollActor.Ref(), &HeightObservedMsg{Height: 219})
	require.Equal(t, 0, txconfirmRef.requestCount())

	mustAsk(t, unrollActor.Ref(), &HeightObservedMsg{Height: 220})
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() >= 1
	}, testTimeout, 10*time.Millisecond)
	require.Equal(
		t, proof.RootTxids()[0],
		txconfirmRef.lastRequest(t).Tx.TxHash(),
	)
	require.Equal(
		t, uint32(1), txconfirmRef.requestByTxid(
			t, proof.RootTxids()[0],
		).HeightHint,
	)

	checkpoint = mustDecodeCheckpoint(t, store, "unroll-test")
	require.Len(t, checkpoint.DeferredCheckpoints, 0)
	require.Equal(
		t, []chainhash.Hash{proof.RootTxids()[0]},
		checkpoint.State.InFlightTxids,
	)
}

// TestFraudTriggerOperatorConfirmedCheckpointUnlocksArk verifies an operator
// confirmation during the deferral window advances recovery without recipient
// checkpoint broadcast.
func TestFraudTriggerOperatorConfirmedCheckpointUnlocksArk(t *testing.T) {
	proof := buildOORProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	desc.CreatedHeight = 77
	unrollActor, behavior, txconfirmRef, _ := newActorHarness(
		t, proof, desc,
	)
	chainRef, ok := behavior.cfg.ChainSource.(*fakeChainSourceRef)
	require.True(t, ok)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerFraudSpend,
	})
	require.Equal(t, 0, txconfirmRef.requestCount())

	chainRef.emitConfirmed(t, proof.RootTxids()[0], 105)
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() >= 1
	}, testTimeout, 10*time.Millisecond)
	require.Equal(
		t, proof.TargetOutpoint().Hash,
		txconfirmRef.lastRequest(t).Tx.TxHash(),
	)
	require.Equal(
		t, uint32(1), txconfirmRef.requestByTxid(
			t, proof.TargetOutpoint().Hash,
		).HeightHint,
	)
}

// TestFraudTriggerSharedArkWaitsForAllCheckpoints verifies a fraud-triggered
// shared ark is not broadcast until every checkpoint input is confirmed.
func TestFraudTriggerSharedArkWaitsForAllCheckpoints(t *testing.T) {
	proof := buildSharedArkOORProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, behavior, txconfirmRef, _ := newActorHarness(
		t, proof, desc,
	)
	chainRef, ok := behavior.cfg.ChainSource.(*fakeChainSourceRef)
	require.True(t, ok)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerFraudSpend,
	})

	require.Eventually(t, func() bool {
		return chainRef.confWatchCount() == 2
	}, testTimeout, 10*time.Millisecond)
	require.Equal(t, 0, txconfirmRef.requestCount())

	rootTxids := proof.RootTxids()
	chainRef.emitConfirmed(t, rootTxids[0], 105)
	time.Sleep(25 * time.Millisecond)
	require.Equal(
		t, 0,
		txconfirmRef.requestCountForTxid(proof.TargetOutpoint().Hash),
	)

	chainRef.emitConfirmed(t, rootTxids[1], 106)
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(
			proof.TargetOutpoint().Hash,
		) == 1
	}, testTimeout, 10*time.Millisecond)
	require.Equal(t, 1, txconfirmRef.requestCount())
}

// TestFraudTriggerSharedCheckpointsBroadcastOnceAtDeadline verifies multiple
// deferred checkpoint roots are each broadcast once at their recipient
// deadline, and the shared ark still waits for both confirmations.
func TestFraudTriggerSharedCheckpointsBroadcastOnceAtDeadline(t *testing.T) {
	proof := buildSharedArkOORProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, _, txconfirmRef, store := newActorHarness(t, proof, desc)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerFraudSpend,
	})
	require.Equal(t, 0, txconfirmRef.requestCount())

	mustAsk(t, unrollActor.Ref(), &HeightObservedMsg{Height: 220})
	rootTxids := proof.RootTxids()
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(rootTxids[0]) == 1 &&
			txconfirmRef.requestCountForTxid(rootTxids[1]) == 1
	}, testTimeout, 10*time.Millisecond)
	require.Equal(t, 2, txconfirmRef.requestCount())
	checkpoint := mustDecodeCheckpoint(t, store, "unroll-test")
	require.ElementsMatch(t, rootTxids, checkpoint.State.InFlightTxids)

	mustAsk(t, unrollActor.Ref(), &HeightObservedMsg{Height: 221})
	time.Sleep(25 * time.Millisecond)
	require.Equal(t, 1, txconfirmRef.requestCountForTxid(rootTxids[0]))
	require.Equal(t, 1, txconfirmRef.requestCountForTxid(rootTxids[1]))

	txconfirmRef.emitConfirmedByTxid(t, rootTxids[0], 221)
	time.Sleep(25 * time.Millisecond)
	require.Equal(
		t, 0,
		txconfirmRef.requestCountForTxid(proof.TargetOutpoint().Hash),
	)

	txconfirmRef.emitConfirmedByTxid(t, rootTxids[1], 222)
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(
			proof.TargetOutpoint().Hash,
		) == 1
	}, testTimeout, 10*time.Millisecond)
	checkpoint = mustDecodeCheckpoint(t, store, "unroll-test")
	require.Contains(
		t, checkpoint.State.InFlightTxids, proof.TargetOutpoint().Hash,
	)
}

// TestFraudTriggerDeferralAnchoredToParentConfirm verifies a non-root
// checkpoint that is promoted to the ready frontier by a TxConfirmedEvent
// stamps its deferral deadline against the parent's confirm height rather
// than the FSM's current height. This is the regression case where a
// fast bulk-flush of HeightObservedMsg events arrives before a parent's
// TxConfirmedMsg: without the parent-anchored deadline the recipient's
// backstop slides arbitrarily far into the future and the test harness
// (which polls for the broadcast without mining further blocks) deadlocks.
//
// Proof shape: checkpoint_AB -> arktx_AB -> checkpoint_BC -> arktx_BC.
// checkpoint_AB is the only root; checkpoint_BC is the non-root checkpoint
// whose deferral deadline this test pins.
func TestFraudTriggerDeferralAnchoredToParentConfirm(t *testing.T) {
	proof := buildMultihopOORProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, _, txconfirmRef, store := newActorHarness(t, proof, desc)

	// Lay out the four proof nodes in topological order so we can refer
	// to them by role. Topological layers: [checkpoint_AB], [arktx_AB],
	// [checkpoint_BC], [arktx_BC=target].
	layers := proof.Layers()
	require.Len(t, layers, 4)
	require.Len(t, layers[0], 1)
	require.Len(t, layers[1], 1)
	require.Len(t, layers[2], 1)
	require.Len(t, layers[3], 1)
	checkpointAB := layers[0][0]
	arkABTxid := layers[1][0]
	checkpointBCTxid := layers[2][0]

	arkABNode, ok := proof.Node(arkABTxid)
	require.True(t, ok)
	require.Equal(t, recovery.NodeKindArk, arkABNode.Kind)
	checkpointBCNode, ok := proof.Node(checkpointBCTxid)
	require.True(t, ok)
	require.Equal(t, recovery.NodeKindCheckpoint, checkpointBCNode.Kind)

	// CSV delay 144, default safety margin 24 → deferral window = 120.
	// Start at height 100 so checkpoint_AB's first deadline lands at
	// 100 + 144 - 24 = 220, matching the conventions in the other fraud
	// trigger tests in this file.
	const startHeight int32 = 100
	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  startHeight,
		Trigger: TriggerFraudSpend,
	})

	checkpoint := mustDecodeCheckpoint(t, store, "unroll-test")
	require.Len(t, checkpoint.DeferredCheckpoints, 1)
	require.Equal(
		t, checkpointAB, checkpoint.DeferredCheckpoints[0].Txid,
	)
	require.Equal(
		t, int32(220), checkpoint.DeferredCheckpoints[0].DeadlineHeight,
	)

	// Cross checkpoint_AB's deadline so the recipient broadcasts it
	// (txconfirm submission) and then confirm it. arktx_AB then enters
	// the ready frontier and (as an ark, not a checkpoint) is submitted
	// immediately — but we deliberately do NOT confirm it yet.
	mustAsk(t, unrollActor.Ref(), &HeightObservedMsg{Height: 220})
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(checkpointAB) == 1
	}, testTimeout, 10*time.Millisecond)

	txconfirmRef.emitConfirmedByTxid(t, checkpointAB, 221)
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(arkABTxid) == 1
	}, testTimeout, 10*time.Millisecond)

	// Race setup. Advance the FSM's job.Height far ahead of where
	// arktx_AB will actually confirm. mustAsk is synchronous so the
	// HeightUpdatedEvent fully lands in the FSM before the next call
	// returns; emitConfirmedByTxid then queues the TxConfirmedMsg
	// behind it in actor-mailbox order.
	const racedHeight int32 = 400
	mustAsk(t, unrollActor.Ref(), &HeightObservedMsg{Height: racedHeight})

	// arktx_AB actually confirms at block 222 — well below racedHeight.
	// This is the block where vtxo_B (checkpoint_BC's parent) becomes
	// available. The fix anchors checkpoint_BC's deferral deadline to
	// THIS height, not racedHeight, so:
	//
	//   parent-anchored deadline = 222 + 144 - 24 = 342
	//   bug-era deadline         = racedHeight + 144 - 24 = 520
	//
	// 342 has already elapsed at racedHeight=400, so the FSM bypasses
	// the deferred set entirely and hands checkpoint_BC straight to
	// txconfirm. The bug-era deadline (520) is still in the future at
	// height 400, so without the fix checkpoint_BC would be parked in
	// DeferredCheckpoints with DeadlineHeight=520 and never broadcast.
	const arkABConfirmHeight int32 = 222
	txconfirmRef.emitConfirmedByTxid(t, arkABTxid, arkABConfirmHeight)

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(
			checkpointBCTxid,
		) == 1
	}, testTimeout, 10*time.Millisecond)

	// Persisted state should not have checkpoint_BC parked in the
	// deferred set after the immediate submission.
	c := mustDecodeCheckpoint(t, store, "unroll-test")
	for _, d := range c.DeferredCheckpoints {
		require.NotEqual(t, checkpointBCTxid, d.Txid)
	}
}

// TestResumeReissuesDeferredCheckpointWatch verifies restart restores
// deferred fraud-triggered checkpoint watches without broadcasting early.
func TestResumeReissuesDeferredCheckpointWatch(t *testing.T) {
	proof := buildOORProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	desc.CreatedHeight = 77
	store := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}
	chainRef := &fakeChainSourceRef{}
	rootTxid := proof.RootTxids()[0]

	raw, err := encodeCheckpoint(&actorCheckpoint{
		Version: checkpointVersion,
		Height:  110,
		Started: true,
		Trigger: TriggerFraudSpend,
		State:   unrollplan.State{},
		DeferredCheckpoints: []DeferredCheckpoint{{
			Txid:           rootTxid,
			DeadlineHeight: 220,
		}},
	})
	require.NoError(t, err)

	err = store.SaveCheckpoint(t.Context(), actor.CheckpointParams{
		ActorID:   "resume-deferred-test",
		StateType: checkpointStateType,
		StateData: raw,
		Version:   checkpointVersion,
	})
	require.NoError(t, err)

	cfg := Config{
		TargetOutpoint: proof.TargetOutpoint(),
		ActorID:        "resume-deferred-test",
		DeliveryStore:  store,
		ProofAssembler: &mockProofAssembler{
			proof: proof,
		},
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
		TxConfirmRef: txconfirmRef,
		ChainSource:  chainRef,
		Wallet:       &fakeSweepWallet{},
		Log:          fn.Some(btclog.Disabled),
	}
	resumeBehavior := &behavior{
		cfg: cfg,
		log: btclog.Disabled,
	}
	err = resumeBehavior.restoreCheckpoint(t.Context())
	require.NoError(t, err)

	resumedActor := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "resume-deferred-test",
		Behavior:    adaptTx(resumeBehavior),
		MailboxSize: 64,
	})
	resumeBehavior.selfRef = resumedActor.TellRef()
	resumedActor.Start()
	t.Cleanup(resumedActor.Stop)

	mustAsk(t, resumedActor.Ref(), &ResumeUnrollRequest{Height: 111})
	require.Eventually(t, func() bool {
		return chainRef.confWatchCount() == 1
	}, testTimeout, 10*time.Millisecond)
	require.Equal(
		t, uint32(1), chainRef.confRequest(t, rootTxid).HeightHint,
	)
	require.Equal(t, 0, txconfirmRef.requestCount())

	mustAsk(t, resumedActor.Ref(), &HeightObservedMsg{Height: 219})
	time.Sleep(25 * time.Millisecond)
	require.Equal(t, 0, txconfirmRef.requestCount())

	mustAsk(t, resumedActor.Ref(), &HeightObservedMsg{Height: 220})
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(rootTxid) == 1
	}, testTimeout, 10*time.Millisecond)
	require.Equal(
		t, uint32(1),
		txconfirmRef.requestByTxid(t, rootTxid).HeightHint,
	)
}

// TestResumeReissuesInFlightArk verifies a fraud-triggered restart reattaches
// to an ark transaction that had already been submitted before shutdown.
func TestResumeReissuesInFlightArk(t *testing.T) {
	proof := buildOORProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	desc.CreatedHeight = 77
	store := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}
	rootTxid := proof.RootTxids()[0]
	arkTxid := proof.TargetOutpoint().Hash

	raw, err := encodeCheckpoint(&actorCheckpoint{
		Version: checkpointVersion,
		Height:  112,
		Started: true,
		Trigger: TriggerFraudSpend,
		State: unrollplan.State{
			ConfirmedTxids: []chainhash.Hash{rootTxid},
			InFlightTxids:  []chainhash.Hash{arkTxid},
		},
	})
	require.NoError(t, err)

	err = store.SaveCheckpoint(t.Context(), actor.CheckpointParams{
		ActorID:   "resume-ark-test",
		StateType: checkpointStateType,
		StateData: raw,
		Version:   checkpointVersion,
	})
	require.NoError(t, err)

	cfg := Config{
		TargetOutpoint: proof.TargetOutpoint(),
		ActorID:        "resume-ark-test",
		DeliveryStore:  store,
		ProofAssembler: &mockProofAssembler{
			proof: proof,
		},
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
		TxConfirmRef: txconfirmRef,
		ChainSource:  &fakeChainSourceRef{},
		Wallet:       &fakeSweepWallet{},
		Log:          fn.Some(btclog.Disabled),
	}
	resumeBehavior := &behavior{
		cfg: cfg,
		log: btclog.Disabled,
	}
	err = resumeBehavior.restoreCheckpoint(t.Context())
	require.NoError(t, err)

	resumedActor := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "resume-ark-test",
		Behavior:    adaptTx(resumeBehavior),
		MailboxSize: 64,
	})
	resumeBehavior.selfRef = resumedActor.TellRef()
	resumedActor.Start()
	t.Cleanup(resumedActor.Stop)

	mustAsk(t, resumedActor.Ref(), &ResumeUnrollRequest{Height: 113})
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(arkTxid) == 1
	}, testTimeout, 10*time.Millisecond)
	require.Equal(
		t, uint32(1), txconfirmRef.requestByTxid(t, arkTxid).HeightHint,
	)
}

// TestManualTriggerSubmitsReadyCheckpointImmediately verifies the deferral
// policy is limited to fraud-triggered recovery.
func TestManualTriggerSubmitsReadyCheckpointImmediately(t *testing.T) {
	proof := buildOORProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, behavior, txconfirmRef, _ := newActorHarness(
		t, proof, desc,
	)
	chainRef, ok := behavior.cfg.ChainSource.(*fakeChainSourceRef)
	require.True(t, ok)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})

	require.Equal(t, 1, txconfirmRef.requestCount())
	require.Equal(t, 0, chainRef.confWatchCount())
	require.Equal(
		t, proof.RootTxids()[0],
		txconfirmRef.lastRequest(t).Tx.TxHash(),
	)
}

// TestProofSpendObservationAdvancesMaterialization verifies that a spend of a
// watched proof-node output is enough to mark the spent proof node confirmed.
// This is important for neutrino-backed clients because the spend watch can
// fire even if the direct confirmation notification is delayed or missed.
func TestProofSpendObservationAdvancesMaterialization(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, beh, txconfirmRef, store := newActorHarness(t, proof, desc)

	chainSource, ok := beh.cfg.ChainSource.(*fakeChainSourceRef)
	require.True(t, ok)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})
	require.Equal(t, 1, txconfirmRef.requestCount())

	rootOutpoint := wire.OutPoint{
		Hash:  proof.RootTxids()[0],
		Index: 0,
	}
	require.Eventually(t, func() bool {
		for _, outpoint := range chainSource.spendRegistrations() {
			if outpoint == rootOutpoint {
				return true
			}
		}

		return false
	}, testTimeout, 10*time.Millisecond)

	chainSource.emitSpendForOutpoint(
		t, rootOutpoint, proof.TargetOutpoint().Hash, 101,
	)

	require.Eventually(t, func() bool {
		stateResp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return stateResp.Phase == PhaseCSVPending
	}, testTimeout, 10*time.Millisecond)

	checkpoint := mustDecodeCheckpoint(t, store, "unroll-test")
	require.Contains(
		t, checkpoint.State.ConfirmedTxids, proof.RootTxids()[0],
	)
	require.Contains(
		t, checkpoint.State.ConfirmedTxids, proof.TargetOutpoint().Hash,
	)
}

// TestLegacyProofSpendObservationAdvancesKnownSpender verifies the
// backwards-compatible spend message path. Older durable mailboxes may replay
// SpendObservedMsg without the spent outpoint, so the actor still treats a
// known proof-node spender as confirmation evidence for that proof node.
func TestLegacyProofSpendObservationAdvancesKnownSpender(t *testing.T) {
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

	mustAsk(t, unrollActor.Ref(), &SpendObservedMsg{
		SpendingTxid:   proof.TargetOutpoint().Hash,
		SpendingHeight: 102,
	})

	require.Eventually(t, func() bool {
		stateResp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return stateResp.Phase == PhaseCSVPending
	}, testTimeout, 10*time.Millisecond)

	checkpoint := mustDecodeCheckpoint(t, store, "unroll-test")
	require.Contains(
		t, checkpoint.State.ConfirmedTxids, proof.TargetOutpoint().Hash,
	)
}

// TestProofSpendWatchesSkipTargetSiblings verifies the proof-spend fallback
// only watches outputs that are consumed by in-proof child nodes. An OOR tx can
// create a sibling output next to the target, and that sibling may be spent by
// another local or remote owner without affecting this target's unroll.
func TestProofSpendWatchesSkipTargetSiblings(t *testing.T) {
	proof := buildSiblingOutputProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, beh, txconfirmRef, _ := newActorHarness(t, proof, desc)

	chainSource, ok := beh.cfg.ChainSource.(*fakeChainSourceRef)
	require.True(t, ok)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})

	rootTxid := proof.RootTxids()[0]
	rootOutpoint := wire.OutPoint{
		Hash:  rootTxid,
		Index: 0,
	}
	require.Eventually(t, func() bool {
		for _, outpoint := range chainSource.spendRegistrations() {
			if outpoint == rootOutpoint {
				return true
			}
		}

		return false
	}, testTimeout, 10*time.Millisecond)

	txconfirmRef.emitConfirmed(t, 0, rootTxid, 101)
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(
			proof.TargetOutpoint().Hash,
		) == 1
	}, testTimeout, 10*time.Millisecond)

	targetSibling := wire.OutPoint{
		Hash:  proof.TargetOutpoint().Hash,
		Index: 0,
	}
	for _, outpoint := range chainSource.spendRegistrations() {
		require.NotEqual(t, targetSibling, outpoint)
	}
}

// TestExternalProofSpendTerminatesActor verifies that an unknown spend of a
// watched proof-node output fails the unroll job instead of being swallowed as
// benign materialization progress.
func TestExternalProofSpendTerminatesActor(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, beh, _, _ := newActorHarness(t, proof, desc)

	chainSource, ok := beh.cfg.ChainSource.(*fakeChainSourceRef)
	require.True(t, ok)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})

	rootOutpoint := wire.OutPoint{
		Hash:  proof.RootTxids()[0],
		Index: 0,
	}
	require.Eventually(t, func() bool {
		for _, outpoint := range chainSource.spendRegistrations() {
			if outpoint == rootOutpoint {
				return true
			}
		}

		return false
	}, testTimeout, 10*time.Millisecond)

	externalTxid := chainhash.Hash{0xee}
	chainSource.emitSpendForOutpoint(t, rootOutpoint, externalTxid, 101)

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
	require.Contains(t, stateResp.FailReason, rootOutpoint.String())
	require.Contains(t, stateResp.FailReason, "spent externally")
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
	require.Equal(
		t, proof.TargetOutpoint().Hash,
		txconfirmRef.lastRequest(t).Tx.TxHash(),
	)

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
		ProofAssembler: &mockProofAssembler{
			proof: proof,
		},
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
		TxConfirmRef: txconfirmRef,
		ChainSource:  &fakeChainSourceRef{},
		Wallet:       &fakeSweepWallet{},
		Log:          fn.Some(btclog.Disabled),
	}
	resumeBehavior := &behavior{
		cfg: cfg,
		log: btclog.Disabled,
	}
	err = resumeBehavior.restoreCheckpoint(t.Context())
	require.NoError(t, err)
	resumedActor := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "resume-test",
		Behavior:    adaptTx(resumeBehavior),
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
		t, proof.RootTxids(), txconfirmRef.requestedTxids(),
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
	require.Equal(
		t, 0,
		txconfirmRef.requestCountForTxid(proof.TargetOutpoint().Hash),
	)

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
	require.Equal(
		t, "proof tx "+rootTxid.String()+
			" failed: txconfirm returned failed state",
		checkpoint.Fail,
	)
}

// TestResumeReissuesSweepConfirmation verifies that resume reattaches
// txconfirm to an already-built sweep transaction.
func TestResumeReissuesSweepConfirmation(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	sweepTx, err := buildSweepTx(
		t.Context(), &fakeSweepWallet{}, &fakeChainSourceRef{}, proof,
		desc, 0, 110, NewStandardVTXOExitSpendPolicy(desc),
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
		ProofAssembler: &mockProofAssembler{
			proof: proof,
		},
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
		TxConfirmRef: txconfirmRef,
		ChainSource:  &fakeChainSourceRef{},
		Wallet:       &fakeSweepWallet{},
		Log:          fn.Some(btclog.Disabled),
	}
	resumeBehavior := &behavior{
		cfg: cfg,
		log: btclog.Disabled,
	}
	err = resumeBehavior.restoreCheckpoint(t.Context())
	require.NoError(t, err)

	resumedActor := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "resume-sweep-test",
		Behavior:    adaptTx(resumeBehavior),
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
		t.Context(), &fakeSweepWallet{}, &fakeChainSourceRef{}, proof,
		desc, 0, 110, NewStandardVTXOExitSpendPolicy(desc),
	)
	require.NoError(t, err)
	require.Len(t, sweepTx.TxIn, 1)
	require.Len(t, sweepTx.TxOut, 1)
	require.Equal(t, desc.RelativeExpiry, sweepTx.TxIn[0].Sequence)
	require.NotEmpty(t, sweepTx.TxIn[0].Witness)
}

// TestStandardVTXOExitSpendPolicyRejectsNilTarget verifies the policy checks
// that a materialized output is present before building an exit spend.
func TestStandardVTXOExitSpendPolicyRejectsNilTarget(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	policy := NewStandardVTXOExitSpendPolicy(desc)

	err := policy.ValidateTarget(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "target output")

	err = policy.ValidateTarget(&wire.TxOut{Value: 0})
	require.Error(t, err)
	require.Contains(t, err.Error(), "positive")
}

// TestStandardVTXOExitSpendPolicyRejectsWrongPkScript verifies the standard
// policy fails closed when the materialized output's pkScript does not match
// the descriptor's pkScript. This guards against a misrouted exit-policy kind
// silently producing a sweep against the wrong taproot output.
func TestStandardVTXOExitSpendPolicyRejectsWrongPkScript(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	desc.PkScript = []byte{txscript.OP_DROP, 0x01, 0x00}
	policy := NewStandardVTXOExitSpendPolicy(desc)

	err := policy.ValidateTarget(&wire.TxOut{
		Value:    1_000,
		PkScript: []byte{txscript.OP_TRUE},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not match descriptor pkscript")

	require.NoError(
		t,
		policy.ValidateTarget(
			&wire.TxOut{
				Value:    1_000,
				PkScript: desc.PkScript,
			},
		),
	)
}

// TestStandardExitSpendPolicyResolver verifies the default resolver maps the
// durable standard policy identity back to the standard policy implementation.
func TestStandardExitSpendPolicyResolver(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	resolver := standardExitSpendPolicyResolver{}

	policy, err := resolver.ResolveExitSpendPolicy(
		t.Context(), ExitSpendPolicyRequest{
			Kind:               StandardVTXOTimeoutExitPolicyKind,
			StandardDescriptor: desc,
		},
	)
	require.NoError(t, err)
	require.Equal(t, StandardVTXOTimeoutExitPolicyKind, policy.Kind())
	require.Equal(t, desc.RelativeExpiry, policy.CSVDelay())

	_, err = resolver.ResolveExitSpendPolicy(
		t.Context(), ExitSpendPolicyRequest{
			Kind:               StandardVTXOTimeoutExitPolicyKind,
			Ref:                "non-empty",
			StandardDescriptor: desc,
		},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ref must be empty")

	_, err = (standardExitSpendPolicyResolver{}).ResolveExitSpendPolicy(
		t.Context(), ExitSpendPolicyRequest{
			Kind: "unknown_policy",
		},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown exit policy kind")
}

// TestBuildSweepTxFallsBackWithoutFeeEstimate verifies the sweep builder uses
// the regtest fallback fee when the backend has no estimate available yet.
func TestBuildSweepTxFallsBackWithoutFeeEstimate(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())

	sweepTx, err := buildSweepTx(
		t.Context(), &fakeSweepWallet{}, &fakeChainSourceRef{
			feeErr: fmt.Errorf("no fee estimates available"),
		}, proof, desc, 0, 110, NewStandardVTXOExitSpendPolicy(desc),
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
	unrollActor, beh, txconfirmRef, store := newActorHarness(t, proof, desc)
	ledgerSink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"unroll-ledger", 2,
	)
	beh.cfg.LedgerSink = fn.Some[ledger.Sink](ledgerSink)

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

	sweepTx := txconfirmRef.lastRequest(t).Tx
	sweepTxid := sweepTx.TxHash()
	txconfirmRef.emitConfirmed(t, 2, sweepTxid, 105)

	require.Eventually(t, func() bool {
		stateResp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return stateResp.Phase == PhaseCompleted
	}, testTimeout, 10*time.Millisecond)

	checkpoint := mustDecodeCheckpoint(t, store, "unroll-test")
	require.Equal(
		t, unrollplan.SweepStatusConfirmed,
		checkpoint.State.Sweep.Status,
	)
	require.True(t, checkpoint.State.Sweep.ConfirmHeight.IsSome())

	// Reorg-safe completion treats PhaseCompleted as provisional on
	// confirmation and defers the terminal handoff (and the ExitCostMsg
	// emission) until the sweep finalizes past reorg-safety depth. Drive
	// the finalize so the exit cost lands.
	txconfirmRef.emitFinalized(t, 2, sweepTxid)

	ledgerMsg, ok := ledgerSink.AwaitMessage(testTimeout)
	require.True(t, ok)
	exitCostMsg, ok := ledgerMsg.(*ledger.ExitCostMsg)
	require.True(t, ok)

	targetOutput, err := proof.TargetOutput()
	require.NoError(t, err)

	sweepOutputValue := int64(0)
	for _, txOut := range sweepTx.TxOut {
		sweepOutputValue += txOut.Value
	}
	require.Equal(
		t, [32]byte(proof.TargetOutpoint().Hash),
		exitCostMsg.OutpointHash,
	)
	require.Equal(
		t, proof.TargetOutpoint().Index, exitCostMsg.OutpointIndex,
	)
	require.Equal(t, targetOutput.Value, exitCostMsg.AmountSat)
	require.Equal(
		t, targetOutput.Value-sweepOutputValue, exitCostMsg.ExitCostSat,
	)
	require.Equal(t, uint32(105), exitCostMsg.BlockHeight)

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

	_, ok = ledgerSink.AwaitMessage(25 * time.Millisecond)
	require.False(t, ok)
}

// TestExitCostTellFailureDefersTerminalHandoff verifies that a transient
// ledger delivery failure defers the terminal registry handoff so the
// exit-cost leg is not lost when the registry would otherwise stop the
// completed child. Once the ledger sink recovers, a later height tick
// retries: the exit cost is delivered and the registry is notified.
func TestExitCostTellFailureDefersTerminalHandoff(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, beh, txconfirmRef, _ := newActorHarness(t, proof, desc)

	// A sink resolved against an actor system with no registered ledger
	// actor returns ErrNoActorsAvailable on every Tell, modelling a
	// transient delivery failure.
	emptySystem := actor.NewActorSystem()
	t.Cleanup(func() {
		_ = emptySystem.Shutdown(context.Background())
	})
	beh.cfg.LedgerSink = fn.Some(ledger.NewSink(emptySystem))

	// Capture terminal notifications to the registry.
	registryRef := actor.NewChannelTellOnlyRef[RegistryMsg](
		"unroll-registry", 4,
	)
	beh.cfg.RegistryRef = registryRef

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

	sweepTx := txconfirmRef.lastRequest(t).Tx
	sweepTxid := sweepTx.TxHash()
	txconfirmRef.emitConfirmed(t, 2, sweepTxid, 105)

	require.Eventually(t, func() bool {
		stateResp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return stateResp.Phase == PhaseCompleted
	}, testTimeout, 10*time.Millisecond)

	// Finalize the sweep so PhaseCompleted is no longer provisional and
	// the actor becomes terminal-eligible. The terminal handoff still
	// requires a successful exit-cost emission, which fails here because
	// the sink has no ledger actor behind it.
	txconfirmRef.emitFinalized(t, 2, sweepTxid)

	// The failing ledger sink must have deferred the terminal handoff:
	// the registry sees no UnrollTerminatedMsg.
	_, ok := registryRef.AwaitMessage(50 * time.Millisecond)
	require.False(
		t, ok, "terminal handoff must be deferred while exit-cost "+
			"delivery fails",
	)

	// Recover the ledger sink and drive one more height tick. Because the
	// FSM is terminal, the event re-enters notifyRegistryIfTerminal, the
	// exit cost is delivered, and the registry is finally notified.
	ledgerSink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"unroll-ledger", 2,
	)
	beh.cfg.LedgerSink = fn.Some[ledger.Sink](ledgerSink)

	mustAsk(t, unrollActor.Ref(), &HeightObservedMsg{Height: 106})

	ledgerMsg, ok := ledgerSink.AwaitMessage(testTimeout)
	require.True(t, ok, "recovered sink must receive the exit cost")
	_, ok = ledgerMsg.(*ledger.ExitCostMsg)
	require.True(t, ok)

	terminalMsg, ok := registryRef.AwaitMessage(testTimeout)
	require.True(
		t, ok, "registry must be notified once exit cost is delivered",
	)
	_, ok = terminalMsg.(*UnrollTerminatedMsg)
	require.True(t, ok)
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
		ProofAssembler: &mockProofAssembler{
			proof: proof,
		},
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
		TxConfirmRef: txconfirmRef,
		ChainSource:  &fakeChainSourceRef{},
		Wallet:       &fakeSweepWallet{},
		Log:          fn.Some(btclog.Disabled),
	}
	resumeBehavior := &behavior{
		cfg: cfg,
		log: btclog.Disabled,
	}
	err = resumeBehavior.restoreCheckpoint(t.Context())
	require.NoError(t, err)

	resumedActor := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "resume-partial-merge",
		Behavior:    adaptTx(resumeBehavior),
		MailboxSize: 64,
	})
	resumeBehavior.selfRef = resumedActor.TellRef()
	resumedActor.Start()
	t.Cleanup(resumedActor.Stop)

	mustAsk(t, resumedActor.Ref(), &ResumeUnrollRequest{Height: 211})

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(rootTxids[1]) == 1
	}, testTimeout, 10*time.Millisecond)
	require.Equal(
		t, 0,
		txconfirmRef.requestCountForTxid(proof.TargetOutpoint().Hash),
	)

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
		ProofAssembler: &mockProofAssembler{
			proof: proof,
		},
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
		TxConfirmRef: txconfirmRef,
		ChainSource:  &fakeChainSourceRef{},
		Wallet:       &fakeSweepWallet{},
		Log:          fn.Some(btclog.Disabled),
	}
	resumeBehavior := &behavior{
		cfg: cfg,
		log: btclog.Disabled,
	}
	err = resumeBehavior.restoreCheckpoint(t.Context())
	require.NoError(t, err)

	resumedActor := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "resume-csv-test",
		Behavior:    adaptTx(resumeBehavior),
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

// TestExternalSpendTerminatesActor verifies that an external spend of
// the target VTXO is treated as a provisional block, and that
// finalization promotes it to a terminal failure. Without the
// finalization signal the actor stays in AwaitingExternalSpendFinality
// so a reorg of the spending block has a live actor to resume.
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

	// Ensure spend watch is registered for the target outpoint with
	// reorg / done callback refs wired so the actor can be driven
	// through the full reversible lifecycle.
	require.Eventually(t, func() bool {
		var targetRegistered bool
		for _, outpoint := range chainSource.spendRegistrations() {
			if outpoint == proof.TargetOutpoint() {
				targetRegistered = true
				break
			}
		}
		if !targetRegistered {
			return false
		}

		chainSource.mu.Lock()
		defer chainSource.mu.Unlock()

		return chainSource.spendReorgedRef != nil &&
			chainSource.spendFinalizedRef != nil
	}, testTimeout, 10*time.Millisecond)

	// Simulate an external party spending the target VTXO.
	externalTxid := chainhash.Hash{0xee}
	chainSource.emitSpendForOutpoint(
		t, proof.TargetOutpoint(), externalTxid, 101,
	)

	// The actor must enter the reversible
	// AwaitingExternalSpendFinality phase rather than terminating.
	require.Eventually(t, func() bool {
		stateResp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return stateResp.Phase == PhaseExternalSpendObserved
	}, testTimeout, 10*time.Millisecond)

	// Finalize the spend. The actor promotes the provisional anchor
	// to a permanent FailReason and transitions to PhaseFailed.
	chainSource.emitSpendFinalized(t)

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
		t, 1, txconfirmRef.requestCountForTxid(
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

// driveLinearToSweep drives a linear-proof unroll actor from admission through
// the proof-graph confirmations and CSV maturation until the final sweep is
// built and broadcast. It mirrors TestConfirmedNodesAdvanceToSweep and returns
// the broadcast sweep txid (read back from the durable checkpoint).
func driveLinearToSweep(t *testing.T, ref actor.ActorRef[Msg, Resp],
	txconfirmRef *fakeTxConfirmRef, store *memCheckpointStore,
	proof *recovery.Proof) chainhash.Hash {

	t.Helper()

	mustAsk(t, ref, &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})
	require.Equal(t, 1, txconfirmRef.requestCount())

	txconfirmRef.emitConfirmed(t, 0, proof.RootTxids()[0], 101)
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() >= 2
	}, testTimeout, 10*time.Millisecond)

	txconfirmRef.emitConfirmed(t, 1, proof.TargetOutpoint().Hash, 102)

	mustAsk(t, ref, &HeightObservedMsg{Height: 103})
	mustAsk(t, ref, &HeightObservedMsg{Height: 104})
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() >= 3
	}, testTimeout, 10*time.Millisecond)

	cp, err := store.LoadCheckpoint(context.Background(), "unroll-test")
	require.NoError(t, err)
	require.NotNil(t, cp)
	decoded, err := decodeCheckpoint(cp.StateData)
	require.NoError(t, err)
	require.NotNil(t, decoded.SweepTx)

	return decoded.SweepTx.TxHash()
}

// TestUnrollStageBeforeBroadcast verifies the persist-before-broadcast
// invariant survives the Read/Commit migration: the checkpoint carrying the
// sweep transaction is durably Staged BEFORE txconfirm is asked to broadcast
// it. The ordering is observed at the exact moment of the broadcast Ask.
func TestUnrollStageBeforeBroadcast(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, _, txconfirmRef, store, _ := newActorHarnessExec(
		t, proof, desc,
	)

	// At each broadcast Ask, record whether a checkpoint carrying that
	// exact tx was already durably staged.
	var mu sync.Mutex
	stagedBeforeAsk := make(map[chainhash.Hash]bool)
	txconfirmRef.onAsk = func(req *txconfirm.EnsureConfirmedReq) {
		staged := false
		cp, err := store.LoadCheckpoint(
			context.Background(),
			"unroll-test",
		)
		if err == nil && cp != nil {
			decoded, derr := decodeCheckpoint(cp.StateData)
			if derr == nil && decoded.SweepTx != nil &&
				decoded.SweepTx.TxHash() == req.Tx.TxHash() {

				staged = true
			}
		}

		mu.Lock()
		stagedBeforeAsk[req.Tx.TxHash()] = staged
		mu.Unlock()
	}

	sweepTxid := driveLinearToSweep(
		t, unrollActor.Ref(), txconfirmRef, store, proof,
	)

	// The sweep checkpoint was on disk before its broadcast crossed the
	// actor boundary.
	mu.Lock()
	defer mu.Unlock()
	require.True(
		t, stagedBeforeAsk[sweepTxid], "sweep tx must be durably "+
			"staged before the txconfirm broadcast",
	)
}

// TestUnrollNoWriterTxnHeldAcrossTxConfirmAsk verifies the contention win the
// migration targets: no SQLite writer transaction is ever held while the actor
// performs a txconfirm Ask. Under the old classic path the entire Receive --
// including this blocking cross-actor Ask -- ran inside one writer transaction.
func TestUnrollNoWriterTxnHeldAcrossTxConfirmAsk(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, _, txconfirmRef, store, exec := newActorHarnessExec(
		t, proof, desc,
	)

	var sawOpenWriteTxn atomic.Bool
	txconfirmRef.onAsk = func(_ *txconfirm.EnsureConfirmedReq) {
		if exec.inWriteTxn.Load() {
			sawOpenWriteTxn.Store(true)
		}
	}

	driveLinearToSweep(t, unrollActor.Ref(), txconfirmRef, store, proof)

	require.False(
		t, sawOpenWriteTxn.Load(),
		"no writer transaction may be held across a txconfirm Ask",
	)
	require.GreaterOrEqual(t, txconfirmRef.requestCount(), 3)
}

// TestUnrollSweepStagedSurvivesLeaseLostReplay verifies the early-durable-write
// guarantee end to end on the money path. A sweep is built, Staged, and
// broadcast, but the consuming Commit loses its lease (a crash window between
// the broadcast and the ack). The staged sweep must survive the rolled-back
// Commit, and a fresh actor that restores from the same store must reuse the
// SAME sweep txid -- never deriving a second sweep that would race the first on
// chain.
func TestUnrollSweepStagedSurvivesLeaseLostReplay(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, _, txconfirmRef, store, exec := newActorHarnessExec(
		t, proof, desc,
	)

	// Drive up to the brink of the sweep: roots and target confirmed, one
	// height tick shy of the build.
	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})
	txconfirmRef.emitConfirmed(t, 0, proof.RootTxids()[0], 101)
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() >= 2
	}, testTimeout, 10*time.Millisecond)
	txconfirmRef.emitConfirmed(t, 1, proof.TargetOutpoint().Hash, 102)
	mustAsk(t, unrollActor.Ref(), &HeightObservedMsg{Height: 103})
	require.Equal(t, 2, txconfirmRef.requestCount())

	// Lose the lease at Commit. The next height tick builds + Stages + and
	// broadcasts the sweep, then the fenced consume fails: the message is
	// not acked, but the Staged sweep is already durable.
	exec.commitLeaseLost.Store(true)

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	_, err := unrollActor.Ref().Ask(
		ctx, &HeightObservedMsg{Height: 104},
	).Await(ctx).Unpack()
	require.ErrorIs(t, err, actor.ErrLeaseLost)

	// The sweep was broadcast even though the Commit rolled back.
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() >= 3
	}, testTimeout, 10*time.Millisecond)

	// The staged sweep survived the lost-lease Commit.
	cp, err := store.LoadCheckpoint(context.Background(), "unroll-test")
	require.NoError(t, err)
	require.NotNil(t, cp)
	decoded, err := decodeCheckpoint(cp.StateData)
	require.NoError(t, err)
	require.NotNil(
		t, decoded.SweepTx,
		"staged sweep must survive the rolled-back Commit",
	)
	sweepTxid := decoded.SweepTx.TxHash()
	require.Equal(t, 1, txconfirmRef.requestCountForTxid(sweepTxid))

	// Simulate redelivery to a fresh process: a brand-new behavior restores
	// from the same store (its in-memory sweep cache starts empty) and must
	// reuse the persisted sweep rather than build a new one.
	replayCfg := Config{
		TargetOutpoint: proof.TargetOutpoint(),
		ActorID:        "unroll-test",
		DeliveryStore:  store,
		ProofAssembler: &mockProofAssembler{
			proof: proof,
		},
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
		TxConfirmRef: txconfirmRef,
		ChainSource:  &fakeChainSourceRef{},
		Wallet:       &fakeSweepWallet{},
		Log:          fn.Some(btclog.Disabled),
	}
	replayBehavior := &behavior{cfg: replayCfg, log: btclog.Disabled}
	require.NoError(t, replayBehavior.restoreCheckpoint(t.Context()))
	require.NotNil(
		t, replayBehavior.sweepTx,
		"sanity: fresh behavior loads the staged sweep from checkpoint",
	)
	require.Equal(t, sweepTxid, replayBehavior.sweepTx.TxHash())

	replayActor := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "unroll-test",
		Behavior:    adaptTx(replayBehavior),
		MailboxSize: 64,
	})
	replayBehavior.selfRef = replayActor.TellRef()
	replayActor.Start()
	t.Cleanup(replayActor.Stop)

	mustAsk(t, replayActor.Ref(), &ResumeUnrollRequest{Height: 105})

	// The reissued sweep is the SAME txid -- reused from the checkpoint --
	// so txconfirm dedups it and no second distinct sweep is ever
	// broadcast.
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(sweepTxid) == 2
	}, testTimeout, 10*time.Millisecond)

	sweepTxids := make(map[chainhash.Hash]struct{})
	for _, txid := range txconfirmRef.requestedTxids() {
		if _, ok := proof.Node(txid); ok {
			continue
		}
		if txid == proof.TargetOutpoint().Hash {
			continue
		}
		sweepTxids[txid] = struct{}{}
	}
	require.Len(
		t, sweepTxids, 1,
		"replay must not derive a second sweep transaction",
	)
}

// TestUnrollAdoptStagedSweepReusesPersisted verifies the Read-based idempotency
// guard in isolation: when a sweep was durably Staged but the in-memory cache
// is empty (the desync window the guard exists for), adoptStagedSweep reads the
// checkpoint snapshot and reuses the persisted sweep instead of leaving the
// behavior to derive a fresh one.
func TestUnrollAdoptStagedSweepReusesPersisted(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	sweepTx, err := buildSweepTx(
		t.Context(), &fakeSweepWallet{}, &fakeChainSourceRef{}, proof,
		desc, 0, 110, NewStandardVTXOExitSpendPolicy(desc),
	)
	require.NoError(t, err)
	sweepTxid := sweepTx.TxHash()

	store := newMemCheckpointStore()
	raw, err := encodeCheckpoint(&actorCheckpoint{
		Version: checkpointVersion,
		Height:  110,
		Started: true,
		State: unrollplan.State{
			Sweep: unrollplan.SweepState{
				Status: unrollplan.SweepStatusBroadcasted,
				Txid:   fn.Some(sweepTxid),
			},
		},
		SweepTx: sweepTx,
	})
	require.NoError(t, err)
	require.NoError(
		t,
		store.SaveCheckpoint(
			t.Context(), actor.CheckpointParams{
				ActorID:   "unroll-test",
				StateType: checkpointStateType,
				StateData: raw,
				Version:   checkpointVersion,
			},
		),
	)

	// A behavior with an empty in-memory sweep cache (no
	// restoreCheckpoint).
	b := &behavior{
		cfg: Config{
			ActorID:       "unroll-test",
			DeliveryStore: store,
			Log:           fn.Some(btclog.Disabled),
		},
		log: btclog.Disabled,
	}
	require.Nil(t, b.sweepTx)

	exec := newMemExec(store, "unroll-test")
	require.NoError(t, b.adoptStagedSweep(t.Context(), exec))

	// The persisted sweep is now reused in memory: a subsequent build would
	// take the reuse branch rather than derive a new address/txid.
	require.NotNil(t, b.sweepTx)
	require.Equal(t, sweepTxid, b.sweepTx.TxHash())
	require.Equal(
		t, int64(0), exec.stageCount.Load(),
		"adoptStagedSweep must only Read, never Stage",
	)
	require.Equal(t, int64(0), exec.commitCount.Load())
}

// TestUnrollStartSweepReusesStagedSweepOnLiveDesync drives a live actor to a
// broadcast sweep, then simulates the in-memory cache being lost WITHOUT a
// reboot (b.sweepTx cleared) and re-enters startSweep. The adoptStagedSweep
// Read guard inside startSweep must reload the persisted sweep so the reuse
// branch fires and no second, freshly-derived sweep txid is ever broadcast.
// This exercises the guard through startSweep on a loaded behavior,
// complementing the isolated adoptStagedSweep unit test and the reboot-path
// replay test.
func TestUnrollStartSweepReusesStagedSweepOnLiveDesync(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, behavior, txconfirmRef, store, exec := newActorHarnessExec(
		t, proof, desc,
	)

	sweepTxid := driveLinearToSweep(
		t, unrollActor.Ref(), txconfirmRef, store, proof,
	)
	requestsBefore := txconfirmRef.requestCount()

	// Simulate losing the in-memory sweep cache on a live instance (no
	// reboot, so restoreCheckpoint does not run): the durable checkpoint
	// still carries the sweep, but b.sweepTx is now nil.
	behavior.sweepTx = nil

	// Re-enter startSweep directly. With the cache empty, the only safe
	// outcome is to adopt the persisted sweep via the Read guard; deriving
	// a fresh sweep here would burn a new wallet address and a new txid.
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	_ = behavior.startSweep(ctx, exec)

	// The persisted sweep was reloaded into the in-memory cache.
	require.NotNil(
		t, behavior.sweepTx, "startSweep must adopt the staged "+
			"sweep when the cache is empty",
	)
	require.Equal(t, sweepTxid, behavior.sweepTx.TxHash())

	// Every txconfirm request that followed reused the SAME sweep txid; no
	// second, freshly-derived sweep was broadcast.
	for _, txid := range txconfirmRef.requestedTxids()[requestsBefore:] {
		require.Equal(
			t, sweepTxid, txid, "re-entered startSweep must "+
				"reuse the staged sweep txid",
		)
	}

	sweepTxids := make(map[chainhash.Hash]struct{})
	for _, txid := range txconfirmRef.requestedTxids() {
		if _, ok := proof.Node(txid); ok {
			continue
		}
		if txid == proof.TargetOutpoint().Hash {
			continue
		}
		sweepTxids[txid] = struct{}{}
	}
	require.Len(
		t, sweepTxids, 1,
		"a live-desync re-entry must not derive a second sweep",
	)
}

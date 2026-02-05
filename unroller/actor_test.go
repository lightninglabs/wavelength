package unroller

import (
	"bytes"
	"context"
	"crypto/rand"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/lndclient"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// MockUnrollStore implements UnrollStore using mock.Mock for testing.
type MockUnrollStore struct {
	mock.Mock
}

// SaveUnrollState creates a new unroll tracking record.
func (m *MockUnrollStore) SaveUnrollState(
	ctx context.Context, state *UnrollState,
) error {

	args := m.Called(ctx, state)
	return args.Error(0)
}

// UpdateUnrollState updates an existing unroll record.
func (m *MockUnrollStore) UpdateUnrollState(
	ctx context.Context, state *UnrollState,
) error {

	args := m.Called(ctx, state)
	return args.Error(0)
}

// GetUnrollState retrieves unroll state by VTXO outpoint.
//
//nolint:forcetypeassert
func (m *MockUnrollStore) GetUnrollState(
	ctx context.Context, vtxoOutpoint wire.OutPoint,
) (*UnrollState, error) {

	args := m.Called(ctx, vtxoOutpoint)
	var state *UnrollState
	if args.Get(0) != nil {
		state = args.Get(0).(*UnrollState)
	}

	return state, args.Error(1)
}

// ListActiveUnrolls returns all in-progress unrolls.
//
//nolint:forcetypeassert
func (m *MockUnrollStore) ListActiveUnrolls(
	ctx context.Context,
) ([]*UnrollState, error) {

	args := m.Called(ctx)
	var states []*UnrollState
	if args.Get(0) != nil {
		states = args.Get(0).([]*UnrollState)
	}

	return states, args.Error(1)
}

// DeleteUnrollState removes completed unroll record.
func (m *MockUnrollStore) DeleteUnrollState(
	ctx context.Context, vtxoOutpoint wire.OutPoint,
) error {

	args := m.Called(ctx, vtxoOutpoint)
	return args.Error(0)
}

// GetVTXO retrieves a VTXO by outpoint.
//
//nolint:forcetypeassert
func (m *MockUnrollStore) GetVTXO(
	ctx context.Context, outpoint wire.OutPoint,
) (*round.ClientVTXO, error) {

	args := m.Called(ctx, outpoint)
	var vtxo *round.ClientVTXO
	if args.Get(0) != nil {
		vtxo = args.Get(0).(*round.ClientVTXO)
	}

	return vtxo, args.Error(1)
}

// Compile-time check that MockUnrollStore implements UnrollStore.
var _ UnrollStore = (*MockUnrollStore)(nil)

// mockWalletKit implements the narrow WalletKit interface for testing.
// Returns a single UTXO with sufficient value, a dummy P2TR address,
// and passes through the transaction from FinalizePsbt as-is (mock
// signing).
type mockWalletKit struct {
	mu    sync.Mutex
	calls int
	err   error
}

// ListUnspent returns a single confirmed UTXO with 1 BTC of value.
func (m *mockWalletKit) ListUnspent(_ context.Context,
	_, _ int32,
	_ ...lndclient.ListUnspentOption,
) ([]*lnwallet.Utxo, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.err != nil {
		return nil, m.err
	}

	return []*lnwallet.Utxo{
		{
			OutPoint: wire.OutPoint{
				Hash:  chainhash.Hash{0x01},
				Index: 0,
			},
			Value:    btcutil.Amount(100_000_000),
			PkScript: bytes.Repeat([]byte{0xaa}, 34),
		},
	}, nil
}

// NextAddr returns a dummy P2TR-like address for change output.
func (m *mockWalletKit) NextAddr(_ context.Context,
	_ string, _ walletrpc.AddressType,
	_ bool) (btcutil.Address, error) {

	// Return a simple P2PKH address for testing. The actual
	// address type doesn't matter since we just need a valid
	// pkScript.
	return btcutil.DecodeAddress(
		"n3GNqMveyvaPvUbH469vDRadqpJMPc84JA",
		&chaincfg.RegressionNetParams,
	)
}

// FinalizePsbt returns the unsigned transaction from the PSBT packet
// as the "signed" result. This mock-signs by returning the tx as-is.
func (m *mockWalletKit) FinalizePsbt(_ context.Context,
	packet *psbt.Packet,
	_ string) (*psbt.Packet, *wire.MsgTx, error) {

	m.mu.Lock()
	m.calls++
	m.mu.Unlock()

	// Extract the unsigned tx from the PSBT.
	tx := packet.UnsignedTx

	return packet, tx, nil
}

// Compile-time check that mockWalletKit satisfies WalletKit.
var _ WalletKit = (*mockWalletKit)(nil)

// mockChainSourceRef implements a mock chain source actor ref for
// testing. Handles FeeEstimateRequest and SubmitPackageRequest (for
// broadcastLevel) and forwards Tell messages for confirmation/epoch
// subscriptions.
type mockChainSourceRef struct {
	t     *testing.T
	tells []chainsource.ChainSourceMsg
	mu    sync.Mutex

	// feeRate is the fee rate returned by FeeEstimateRequest.
	feeRate btcutil.Amount

	// submitErr is returned by SubmitPackageRequest when non-nil.
	submitErr error
}

// newMockChainSourceRef creates a new mock chain source actor ref.
func newMockChainSourceRef(t *testing.T) *mockChainSourceRef {
	return &mockChainSourceRef{
		t:       t,
		tells:   make([]chainsource.ChainSourceMsg, 0),
		feeRate: 10,
	}
}

// ID returns the actor ID.
func (m *mockChainSourceRef) ID() string {
	return "mock-chain-source"
}

// Ask sends a request and returns a future response.
func (m *mockChainSourceRef) Ask(
	_ context.Context, msg chainsource.ChainSourceMsg,
) actor.Future[chainsource.ChainSourceResp] {

	m.mu.Lock()
	defer m.mu.Unlock()

	switch msg.(type) {
	case *chainsource.FeeEstimateRequest:
		return &mockFuture[chainsource.ChainSourceResp]{
			result: fn.Ok[chainsource.ChainSourceResp](
				&chainsource.FeeEstimateResponse{
					SatPerVByte: m.feeRate,
				},
			),
		}

	case *chainsource.SubmitPackageRequest:
		if m.submitErr != nil {
			return &mockFuture[chainsource.ChainSourceResp]{
				result: fn.Err[chainsource.ChainSourceResp](
					m.submitErr,
				),
			}
		}

		return &mockFuture[chainsource.ChainSourceResp]{
			result: fn.Ok[chainsource.ChainSourceResp](
				&chainsource.SubmitPackageResponse{},
			),
		}

	default:
		m.t.Fatalf("unexpected Ask message type: %T", msg)
		return nil
	}
}

// Tell sends a message without expecting a response.
func (m *mockChainSourceRef) Tell(
	_ context.Context, msg chainsource.ChainSourceMsg,
) {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.tells = append(m.tells, msg)
}

// TellOnly returns a tell-only reference.
//
//nolint:ll
func (m *mockChainSourceRef) TellOnly() actor.TellOnlyRef[chainsource.ChainSourceMsg] {
	return m
}

// mockFuture implements actor.Future for testing.
type mockFuture[T any] struct {
	result fn.Result[T]
}

// Await returns the stored result.
func (f *mockFuture[T]) Await(_ context.Context) fn.Result[T] {
	return f.result
}

// ThenApply applies a transformation function to the result.
func (f *mockFuture[T]) ThenApply(
	ctx context.Context, transformFn func(T) T,
) actor.Future[T] {

	return &mockFuture[T]{
		result: f.result.MapOk(transformFn),
	}
}

// OnComplete registers a callback for when the result is ready.
func (f *mockFuture[T]) OnComplete(
	_ context.Context, callback func(fn.Result[T]),
) {

	callback(f.result)
}

// unrollerTestHarness provides common test utilities.
//
//nolint:containedctx
type unrollerTestHarness struct {
	t      *testing.T
	ctx    context.Context
	cancel context.CancelFunc

	store       *MockUnrollStore
	chainSource *mockChainSourceRef
	walletKit   *mockWalletKit
	actor       *UnrollerActor
}

// newUnrollerTestHarness creates a new test harness.
func newUnrollerTestHarness(t *testing.T) *unrollerTestHarness {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())

	store := &MockUnrollStore{}
	chainSource := newMockChainSourceRef(t)
	walletKit := &mockWalletKit{}

	// Create a no-op self ref for testing.
	selfRef := &mockSelfRef{t: t}

	cfg := &UnrollerConfig{
		ChainSource: chainSource,
		Store:       store,
		ChainParams: &chaincfg.RegressionNetParams,
		Logger:      btclog.Disabled,
		SelfRef:     selfRef,
		WalletKit:   walletKit,
	}

	actorInstance := NewUnrollerActor(cfg)

	h := &unrollerTestHarness{
		t:           t,
		ctx:         ctx,
		cancel:      cancel,
		store:       store,
		chainSource: chainSource,
		walletKit:   walletKit,
		actor:       actorInstance,
	}

	t.Cleanup(func() {
		cancel()
	})

	return h
}

// newTestOutpoint creates a random test outpoint.
func (h *unrollerTestHarness) newTestOutpoint() wire.OutPoint {
	h.t.Helper()

	var hash chainhash.Hash
	_, err := rand.Read(hash[:])
	require.NoError(h.t, err)

	return wire.OutPoint{Hash: hash, Index: 0}
}

// testSchnorrSignature creates a valid schnorr signature for testing.
func testSchnorrSignature(t *testing.T) *schnorr.Signature {
	t.Helper()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var msg [32]byte
	_, err = rand.Read(msg[:])
	require.NoError(t, err)

	sig, err := schnorr.Sign(privKey, msg[:])
	require.NoError(t, err)

	return sig
}

// newTestVTXO creates a test VTXO with a simple tree.
func (h *unrollerTestHarness) newTestVTXO() *round.ClientVTXO {
	h.t.Helper()

	outpoint := h.newTestOutpoint()

	// Create a simple tree with a root node that has a valid
	// signature so that ToSignedTx() succeeds. The last output
	// is a P2A ephemeral anchor (0-sat, script 0x51024e73) as
	// required by the V3 package broadcast flow.
	root := &tree.Node{
		Input: wire.OutPoint{},
		Outputs: []*wire.TxOut{
			{Value: 1000, PkScript: bytes.Repeat(
				[]byte{0xab}, 34,
			)},
			{Value: 0, PkScript: []byte{
				0x51, 0x02, 0x4e, 0x73,
			}},
		},
		Children:  make(map[uint32]*tree.Node),
		Signature: testSchnorrSignature(h.t),
	}

	treePath := &tree.Tree{
		Root: root,
	}

	return &round.ClientVTXO{
		Outpoint: outpoint,
		Amount:   btcutil.Amount(50000),
		Expiry:   144,
		TreePath: treePath,
	}
}

// mockSelfRef implements a no-op TellOnlyRef for testing.
type mockSelfRef struct {
	t *testing.T
}

// ID returns the actor ID.
func (m *mockSelfRef) ID() string {
	return "mock-self-ref"
}

// Tell is a no-op for testing.
func (m *mockSelfRef) Tell(_ context.Context, _ UnrollerMsg) {
	// No-op.
}

// TestUnrollRequest_InitiatesUnroll verifies that receiving an UnrollRequest
// starts a new unroll.
func TestUnrollRequest_InitiatesUnroll(t *testing.T) {
	t.Parallel()

	h := newUnrollerTestHarness(t)
	vtxoDesc := h.newTestVTXO()

	// Mock store to return VTXO descriptor.
	h.store.On("GetVTXO", h.ctx, vtxoDesc.Outpoint).Return(
		vtxoDesc, nil,
	)

	// Expect store to save unroll state.
	h.store.On("SaveUnrollState", h.ctx, mock.Anything).Return(nil)
	h.store.On("UpdateUnrollState", h.ctx, mock.Anything).Return(nil)

	// Send unroll request.
	msg := &UnrollRequest{
		TargetVTXOs: []wire.OutPoint{vtxoDesc.Outpoint},
	}

	result := h.actor.Receive(h.ctx, msg)

	require.True(t, result.IsOk())

	// Verify store was called to fetch VTXO.
	h.store.AssertCalled(t, "GetVTXO", h.ctx, vtxoDesc.Outpoint)

	// Verify store was called to save state.
	h.store.AssertCalled(
		t, "SaveUnrollState", h.ctx,
		mock.AnythingOfType("*unroller.UnrollState"),
	)

	// Verify the saved state has correct fields.
	// Calls: GetVTXO, SaveUnrollState, UpdateUnrollState (from broadcast).
	calls := h.store.Calls
	require.GreaterOrEqual(t, len(calls), 2)

	var savedState *UnrollState
	for _, call := range calls {
		if call.Method == "SaveUnrollState" {
			var ok bool
			savedState, ok = call.Arguments.Get(1).(*UnrollState)
			require.True(t, ok, "expected *UnrollState")

			break
		}
	}

	require.NotNil(t, savedState)
	require.Equal(t, vtxoDesc.Outpoint, savedState.VTXOOutpoint)
	// Status starts as Pending when saved, but may transition to
	// Broadcasting or AwaitingCSV immediately after in broadcastLevel
	// (AwaitingCSV if the tree has minimal levels and broadcasts complete
	// synchronously).
	require.Contains(
		t,
		[]UnrollStatus{
			UnrollStatusPending,
			UnrollStatusBroadcasting,
			UnrollStatusAwaitingCSV,
		},
		savedState.Status,
	)
}

// TestUnrollRequest_DuplicateIgnored verifies that duplicate unroll requests
// are ignored.
func TestUnrollRequest_DuplicateIgnored(t *testing.T) {
	t.Parallel()

	h := newUnrollerTestHarness(t)
	vtxoDesc := h.newTestVTXO()

	// Mock store to return VTXO descriptor.
	h.store.On("GetVTXO", h.ctx, vtxoDesc.Outpoint).Return(
		vtxoDesc, nil,
	)

	// Expect store to save unroll state once.
	h.store.On("SaveUnrollState", h.ctx, mock.Anything).Return(
		nil,
	).Once()
	h.store.On("UpdateUnrollState", h.ctx, mock.Anything).Return(nil)

	// Send first request.
	msg := &UnrollRequest{
		TargetVTXOs: []wire.OutPoint{vtxoDesc.Outpoint},
	}

	result1 := h.actor.Receive(h.ctx, msg)
	require.True(t, result1.IsOk())

	// Send duplicate request.
	result2 := h.actor.Receive(h.ctx, msg)
	require.True(t, result2.IsOk())

	// Verify GetVTXO and SaveUnrollState were only called once.
	h.store.AssertNumberOfCalls(t, "GetVTXO", 1)
	h.store.AssertNumberOfCalls(t, "SaveUnrollState", 1)
}

// TestGetUnrollStatus_ReturnsCurrentStatus verifies that GetUnrollStatusRequest
// returns the correct status.
func TestGetUnrollStatus_ReturnsCurrentStatus(t *testing.T) {
	t.Parallel()

	h := newUnrollerTestHarness(t)
	vtxoDesc := h.newTestVTXO()

	// Create unroll state directly in actor.
	state := &UnrollState{
		VTXOOutpoint: vtxoDesc.Outpoint,
		VTXO:         vtxoDesc,
		LevelOrder: []LevelTxids{
			{Level: 0, Txids: []chainhash.Hash{}},
		},
		CurrentLevel:   0,
		BroadcastTxids: make(map[chainhash.Hash]bool),
		ConfirmedTxids: make(map[chainhash.Hash]ConfirmationInfo),
		Status:         UnrollStatusBroadcasting,
	}

	h.actor.activeUnrolls[vtxoDesc.Outpoint.String()] = state

	// Query status.
	req := &GetUnrollStatusRequest{
		VTXOOutpoint: vtxoDesc.Outpoint,
	}

	result := h.actor.Receive(h.ctx, req)
	require.True(t, result.IsOk())

	respMsg, err := result.Unpack()
	require.NoError(t, err)

	resp, ok := respMsg.(*UnrollStatusResp)
	require.True(t, ok)
	require.Equal(t, UnrollStatusBroadcasting, resp.Status)
	require.Equal(t, 0, resp.CurrentLevel)
	require.Equal(t, 1, resp.TotalLevels)
}

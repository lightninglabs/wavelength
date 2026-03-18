package unroller

import (
	"context"
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/lndclient"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Mock: UnrollStore
// ---------------------------------------------------------------------------

// MockUnrollStore implements UnrollStore using testify/mock.
type MockUnrollStore struct {
	mock.Mock
}

//nolint:forcetypeassert
func (m *MockUnrollStore) GetVTXO(ctx context.Context,
	outpoint wire.OutPoint) (*round.ClientVTXO, error) {

	args := m.Called(ctx, outpoint)
	var vtxo *round.ClientVTXO
	if args.Get(0) != nil {
		vtxo = args.Get(0).(*round.ClientVTXO)
	}

	return vtxo, args.Error(1)
}

func (m *MockUnrollStore) SaveUnrollState(ctx context.Context,
	state *UnrollState) error {

	args := m.Called(ctx, state)
	return args.Error(0)
}

func (m *MockUnrollStore) UpdateUnrollState(ctx context.Context,
	state *UnrollState) error {

	args := m.Called(ctx, state)
	return args.Error(0)
}

//nolint:forcetypeassert
func (m *MockUnrollStore) GetUnrollState(ctx context.Context,
	vtxoOutpoint wire.OutPoint) (*UnrollState, error) {

	args := m.Called(ctx, vtxoOutpoint)
	var state *UnrollState
	if args.Get(0) != nil {
		state = args.Get(0).(*UnrollState)
	}

	return state, args.Error(1)
}

//nolint:forcetypeassert
func (m *MockUnrollStore) ListActiveUnrolls(ctx context.Context) (
	[]*UnrollState, error) {

	args := m.Called(ctx)
	var states []*UnrollState
	if args.Get(0) != nil {
		states = args.Get(0).([]*UnrollState)
	}

	return states, args.Error(1)
}

func (m *MockUnrollStore) DeleteUnrollState(ctx context.Context,
	vtxoOutpoint wire.OutPoint) error {

	args := m.Called(ctx, vtxoOutpoint)
	return args.Error(0)
}

var _ UnrollStore = (*MockUnrollStore)(nil)

// ---------------------------------------------------------------------------
// Mock: ChainSource ActorRef
// ---------------------------------------------------------------------------

// mockChainSourceRef is a minimal mock that satisfies
// actor.ActorRef[chainsource.ChainSourceMsg, chainsource.ChainSourceResp].
// It records Tell calls and returns pre-configured Ask responses.
type mockChainSourceRef struct {
	t *testing.T

	// askResponses maps message type name to a function that produces
	// the future result.
	askResponses map[string]func() fn.Result[chainsource.ChainSourceResp]

	// tells collects fire-and-forget messages for verification.
	tells []chainsource.ChainSourceMsg

	// asks collects request/response messages for later inspection.
	asks []chainsource.ChainSourceMsg

	// askCallCounts tracks how many times Ask was called per
	// message type. Used to verify fee bump resubmissions.
	askCallCounts map[string]int
}

// csRespFunc shortens the chain source response function type
// for 80-char line limit compliance.
type csRespFunc = func() fn.Result[chainsource.ChainSourceResp]

func newMockChainSourceRef(
	t *testing.T,
) *mockChainSourceRef {

	return &mockChainSourceRef{
		t:             t,
		askResponses:  make(map[string]csRespFunc),
		askCallCounts: make(map[string]int),
	}
}

func (m *mockChainSourceRef) ID() string {
	return "mock-chain-source"
}

func (m *mockChainSourceRef) Tell(
	_ context.Context, msg chainsource.ChainSourceMsg,
) error {

	m.tells = append(m.tells, msg)
	return nil
}

func (m *mockChainSourceRef) Ask(
	_ context.Context, msg chainsource.ChainSourceMsg,
) actor.Future[chainsource.ChainSourceResp] {

	key := msg.MessageType()
	m.askCallCounts[key]++
	m.asks = append(m.asks, msg)

	respFn, ok := m.askResponses[key]
	if !ok {
		m.t.Fatalf("unexpected Ask for message type %q", key)
	}

	return &immediateFuture[chainsource.ChainSourceResp]{
		result: respFn(),
	}
}

// onAsk registers a canned response for an Ask matching the given
// message type string.
func (m *mockChainSourceRef) onAsk(
	msgType string,
	resp func() fn.Result[chainsource.ChainSourceResp],
) {

	m.askResponses[msgType] = resp
}

var _ actor.ActorRef[
	chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
] = (*mockChainSourceRef)(nil)

// ---------------------------------------------------------------------------
// Mock: WalletKit
// ---------------------------------------------------------------------------

// mockWalletKit implements WalletKit for LND-mode tests.
type mockWalletKit struct {
	t             *testing.T
	utxos         []*lnwallet.Utxo
	nextAddr      btcutil.Address
	listCalls     int
	nextAddrCalls int
	finalizeCalls int
}

func newMockWalletKit(
	t *testing.T, utxoValue int64,
) *mockWalletKit {

	t.Helper()

	addr, err := btcutil.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	var hash chainhash.Hash
	_, err = rand.Read(hash[:])
	require.NoError(t, err)

	return &mockWalletKit{
		t:        t,
		nextAddr: addr,
		utxos: []*lnwallet.Utxo{{
			OutPoint: wire.OutPoint{
				Hash:  hash,
				Index: 1,
			},
			Value: btcutil.Amount(utxoValue),
			PkScript: []byte{
				0x00, 0x14, 0x01, 0x02, 0x03, 0x04,
				0x05, 0x06, 0x07, 0x08, 0x09, 0x0a,
				0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
				0x11, 0x12, 0x13, 0x14,
			},
		}},
	}
}

func (m *mockWalletKit) ListUnspent(
	_ context.Context, minConfs, maxConfs int32,
	_ ...lndclient.ListUnspentOption,
) ([]*lnwallet.Utxo, error) {

	require.Equal(m.t, int32(1), minConfs)
	require.Equal(m.t, maxConfsForUTXO, maxConfs)
	m.listCalls++

	return m.utxos, nil
}

func (m *mockWalletKit) NextAddr(
	_ context.Context, accountName string,
	addressType walletrpc.AddressType,
	change bool,
) (btcutil.Address, error) {

	require.Equal(m.t, "", accountName)
	require.Equal(m.t, walletrpc.AddressType_TAPROOT_PUBKEY,
		addressType)
	require.False(m.t, change)
	m.nextAddrCalls++

	return m.nextAddr, nil
}

func (m *mockWalletKit) FinalizePsbt(
	_ context.Context, packet *psbt.Packet, account string,
) (*psbt.Packet, *wire.MsgTx, error) {

	require.Equal(m.t, "", account)
	m.finalizeCalls++

	signedTx := packet.UnsignedTx.Copy()
	signedTx.TxIn[1].Witness = wire.TxWitness{
		make([]byte, 64),
	}

	return packet, signedTx, nil
}

var _ WalletKit = (*mockWalletKit)(nil)

// ---------------------------------------------------------------------------
// Mock: SelfRef (TellOnlyRef for UnrollerMsg)
// ---------------------------------------------------------------------------

type mockSelfRef struct {
	msgs []UnrollerMsg
}

func (m *mockSelfRef) ID() string { return "mock-self" }

func (m *mockSelfRef) Tell(
	_ context.Context, msg UnrollerMsg,
) error {

	m.msgs = append(m.msgs, msg)
	return nil
}

var _ actor.TellOnlyRef[UnrollerMsg] = (*mockSelfRef)(nil)

// ---------------------------------------------------------------------------
// immediateFuture: a Future that is already resolved.
// ---------------------------------------------------------------------------

type immediateFuture[T any] struct {
	result fn.Result[T]
}

func (f *immediateFuture[T]) Await(
	_ context.Context,
) fn.Result[T] {

	return f.result
}

func (f *immediateFuture[T]) ThenApply(
	ctx context.Context, apply func(T) T,
) actor.Future[T] {

	val, err := f.result.Unpack()
	if err != nil {
		return f
	}

	return &immediateFuture[T]{
		result: fn.Ok(apply(val)),
	}
}

func (f *immediateFuture[T]) OnComplete(
	_ context.Context, cb func(fn.Result[T]),
) {

	cb(f.result)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newTestOutpoint creates a random outpoint for testing.
func newTestOutpoint(t *testing.T) wire.OutPoint {
	t.Helper()

	var hash chainhash.Hash
	_, err := rand.Read(hash[:])
	require.NoError(t, err)

	return wire.OutPoint{Hash: hash, Index: 0}
}

// makeSimpleNode creates a tree.Node with a fake signed transaction
// (single input, single output + anchor, with a dummy signature).
func makeSimpleNode(t *testing.T, input wire.OutPoint,
	value int64) *tree.Node {

	t.Helper()

	// Generate a dummy 64-byte Schnorr signature.
	var sigBytes [64]byte
	_, err := rand.Read(sigBytes[:])
	require.NoError(t, err)

	sig, err := schnorr.ParseSignature(sigBytes[:])
	require.NoError(t, err)

	pkScript := make([]byte, 34)
	pkScript[0] = 0x51 // OP_1
	pkScript[1] = 0x20 // push 32 bytes
	_, err = rand.Read(pkScript[2:])
	require.NoError(t, err)

	return &tree.Node{
		Input: input,
		Outputs: []*wire.TxOut{
			{Value: value, PkScript: pkScript},
			{Value: 0, PkScript: scripts.AnchorPkScript}, // anchor
		},
		Children:  make(map[uint32]*tree.Node),
		Signature: sig,
	}
}

// newTestActor creates an UnrollerActor wired to the given mocks.
func newTestActor(t *testing.T, store *MockUnrollStore,
	cs *mockChainSourceRef) *UnrollerActor {

	t.Helper()

	selfRef := &mockSelfRef{}

	cfg := &UnrollerConfig{
		ChainSource: cs,
		Store:       store,
		ChainParams: &chaincfg.RegressionNetParams,
		Logger:      btclog.Disabled,
		SelfRef:     selfRef,
		WalletKit:   nil, // lwwallet / direct broadcast path
	}

	return NewUnrollerActor(cfg)
}

// ===========================================================================
// Tests: extractLevelOrder
// ===========================================================================

func TestExtractLevelOrder(t *testing.T) {
	t.Parallel()

	t.Run("nil tree returns error", func(t *testing.T) {
		t.Parallel()

		_, err := extractLevelOrder(nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "nil")
	})

	t.Run("nil root returns error", func(t *testing.T) {
		t.Parallel()

		_, err := extractLevelOrder(&tree.Tree{Root: nil})
		require.Error(t, err)
		require.Contains(t, err.Error(), "nil")
	})

	t.Run("single node tree", func(t *testing.T) {
		t.Parallel()

		rootInput := newTestOutpoint(t)
		root := makeSimpleNode(t, rootInput, 50000)

		tr := &tree.Tree{Root: root}
		levels, err := extractLevelOrder(tr)
		require.NoError(t, err)
		require.Len(t, levels, 1)
		require.Equal(t, 0, levels[0].Level)
		require.Len(t, levels[0].Txids, 1)
		require.Len(t, levels[0].Nodes, 1)
		require.Equal(t, root, levels[0].Nodes[0])
	})

	t.Run("multi level tree", func(t *testing.T) {
		t.Parallel()

		// Build a tree:
		//       root
		//      /    \
		//   child0  child1
		rootInput := newTestOutpoint(t)
		root := makeSimpleNode(t, rootInput, 100000)

		// Compute root txid to wire children correctly.
		rootTx, err := root.ToTx()
		require.NoError(t, err)
		rootTxid := rootTx.TxHash()

		child0Input := wire.OutPoint{Hash: rootTxid, Index: 0}
		child0 := makeSimpleNode(t, child0Input, 50000)

		child1Input := wire.OutPoint{Hash: rootTxid, Index: 1}
		child1 := makeSimpleNode(t, child1Input, 50000)

		root.Children[0] = child0
		root.Children[1] = child1

		tr := &tree.Tree{Root: root}
		levels, err := extractLevelOrder(tr)
		require.NoError(t, err)
		require.Len(t, levels, 2, "expected 2 levels")

		// Level 0: root only.
		require.Len(t, levels[0].Txids, 1)
		require.Equal(t, 0, levels[0].Level)

		// Level 1: two children.
		require.Len(t, levels[1].Txids, 2)
		require.Equal(t, 1, levels[1].Level)
	})

	t.Run("three level tree", func(t *testing.T) {
		t.Parallel()

		// root -> child -> grandchild
		rootInput := newTestOutpoint(t)
		root := makeSimpleNode(t, rootInput, 100000)

		rootTx, err := root.ToTx()
		require.NoError(t, err)

		childInput := wire.OutPoint{
			Hash: rootTx.TxHash(), Index: 0,
		}
		child := makeSimpleNode(t, childInput, 50000)

		childTx, err := child.ToTx()
		require.NoError(t, err)

		grandchildInput := wire.OutPoint{
			Hash: childTx.TxHash(), Index: 0,
		}
		grandchild := makeSimpleNode(t, grandchildInput, 25000)

		child.Children[0] = grandchild
		root.Children[0] = child

		tr := &tree.Tree{Root: root}
		levels, err := extractLevelOrder(tr)
		require.NoError(t, err)
		require.Len(t, levels, 3)

		require.Len(t, levels[0].Txids, 1) // root
		require.Len(t, levels[1].Txids, 1) // child
		require.Len(t, levels[2].Txids, 1) // grandchild
	})
}

// ===========================================================================
// Tests: UnrollStatus.String()
// ===========================================================================

func TestUnrollStatusString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status UnrollStatus
		want   string
	}{
		{UnrollStatusPending, "pending"},
		{UnrollStatusBroadcasting, "broadcasting"},
		{UnrollStatusAwaitingCSV, "awaiting_csv"},
		{UnrollStatusComplete, "complete"},
		{UnrollStatusFailed, "failed"},
		{UnrollStatus(99), "unknown"},
	}

	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, tc.status.String())
		})
	}
}

// ===========================================================================
// Tests: estimateWeight
// ===========================================================================

func TestEstimateWeight(t *testing.T) {
	t.Parallel()

	// Build a minimal V3 transaction: 1 input (with witness), 1 output.
	tx := wire.NewMsgTx(3)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: 0,
		},
		Sequence: wire.MaxTxInSequenceNum,
	})
	tx.TxIn[0].Witness = wire.TxWitness{
		make([]byte, 64), // dummy Schnorr signature
	}
	tx.AddTxOut(&wire.TxOut{
		Value:    50000,
		PkScript: make([]byte, 34),
	})

	weight := estimateWeight(tx)

	// BIP 141: weight = baseSize*3 + totalSize.
	baseSize := int64(tx.SerializeSizeStripped())
	totalSize := int64(tx.SerializeSize())
	expected := baseSize*3 + totalSize

	require.Equal(t, expected, weight)
	require.Greater(t, weight, int64(0))

	// Sanity: weight should be > base*4 because witness is non-empty
	// but lighter than non-witness data per the discount.
	require.Greater(t, weight, baseSize*4-1,
		"weight should reflect witness discount")
}

// ===========================================================================
// Tests: handleUnrollRequest
// ===========================================================================

func TestHandleUnrollRequest(t *testing.T) {
	t.Parallel()

	t.Run("empty target VTXOs returns error", func(t *testing.T) {
		t.Parallel()

		store := &MockUnrollStore{}
		cs := newMockChainSourceRef(t)
		a := newTestActor(t, store, cs)

		req := &UnrollRequest{TargetVTXOs: nil}
		result := a.Receive(t.Context(), req)
		_, err := result.Unpack()
		require.Error(t, err)
		require.Contains(t, err.Error(), "no target VTXOs")
	})

	t.Run("VTXO not found returns error", func(t *testing.T) {
		t.Parallel()

		store := &MockUnrollStore{}
		cs := newMockChainSourceRef(t)
		a := newTestActor(t, store, cs)

		outpoint := newTestOutpoint(t)
		store.On("GetVTXO", mock.Anything, outpoint).Return(
			nil, fmt.Errorf("vtxo not found"),
		)

		req := &UnrollRequest{
			TargetVTXOs: []wire.OutPoint{outpoint},
		}
		result := a.Receive(t.Context(), req)
		_, err := result.Unpack()
		require.Error(t, err)
		require.Contains(t, err.Error(), "fetch VTXO")
	})

	t.Run("OOR VTXO with nil TreePath returns error", func(t *testing.T) {
		t.Parallel()

		store := &MockUnrollStore{}
		cs := newMockChainSourceRef(t)
		a := newTestActor(t, store, cs)

		outpoint := newTestOutpoint(t)
		vtxo := &round.ClientVTXO{
			Outpoint: outpoint,
			Expiry:   144,
			TreePath: nil, // OOR VTXO
		}
		store.On("GetVTXO", mock.Anything, outpoint).Return(
			vtxo, nil,
		)

		req := &UnrollRequest{
			TargetVTXOs: []wire.OutPoint{outpoint},
		}
		result := a.Receive(t.Context(), req)
		_, err := result.Unpack()
		require.Error(t, err)
		require.Contains(t, err.Error(), "OOR VTXO")
		require.Contains(t, err.Error(), "out-of-round")
	})

	t.Run("duplicate request returns success without re-processing",
		func(t *testing.T) {
			t.Parallel()

			store := &MockUnrollStore{}
			cs := newMockChainSourceRef(t)
			a := newTestActor(t, store, cs)

			outpoint := newTestOutpoint(t)

			// Pre-populate active unrolls to simulate an
			// in-progress unroll.
			a.activeUnrolls[outpoint] = &UnrollState{
				VTXOOutpoint: outpoint,
				Status:       UnrollStatusBroadcasting,
			}

			req := &UnrollRequest{
				TargetVTXOs: []wire.OutPoint{outpoint},
			}
			result := a.Receive(t.Context(), req)
			resp, err := result.Unpack()
			require.NoError(t, err)

			_, ok := resp.(*UnrollStartedResp)
			require.True(t, ok,
				"expected UnrollStartedResp, got %T", resp)

			// Store should NOT have been called since the
			// duplicate check short-circuits.
			store.AssertNotCalled(t, "GetVTXO",
				mock.Anything, mock.Anything)
		})

	t.Run("successful unroll initiation", func(t *testing.T) {
		t.Parallel()

		store := &MockUnrollStore{}
		cs := newMockChainSourceRef(t)
		a := newTestActor(t, store, cs)

		outpoint := newTestOutpoint(t)
		rootNode := makeSimpleNode(t, newTestOutpoint(t), 50000)
		vtxoTree := &tree.Tree{Root: rootNode}

		vtxo := &round.ClientVTXO{
			Outpoint: outpoint,
			Expiry:   144,
			TreePath: vtxoTree,
		}
		store.On("GetVTXO", mock.Anything, outpoint).Return(
			vtxo, nil,
		)
		store.On("SaveUnrollState", mock.Anything,
			mock.Anything).Return(nil)
		store.On("UpdateUnrollState", mock.Anything,
			mock.Anything).Return(nil)

		// Wire up Ask responses for broadcastLevelDirect path
		// (WalletKit is nil).
		cs.onAsk(
			"BestHeightRequest",
			func() fn.Result[chainsource.ChainSourceResp] {
				return fn.Ok[chainsource.ChainSourceResp](
					&chainsource.BestHeightResponse{
						Height: 100,
					},
				)
			},
		)
		cs.onAsk(
			"SubmitPackageRequest",
			func() fn.Result[chainsource.ChainSourceResp] {
				return fn.Ok[chainsource.ChainSourceResp](
					&chainsource.SubmitPackageResponse{},
				)
			},
		)

		req := &UnrollRequest{
			TargetVTXOs: []wire.OutPoint{outpoint},
		}
		result := a.Receive(t.Context(), req)
		resp, err := result.Unpack()
		require.NoError(t, err)

		_, ok := resp.(*UnrollStartedResp)
		require.True(t, ok,
			"expected UnrollStartedResp, got %T", resp)

		// Verify state was tracked.
		require.Contains(t, a.activeUnrolls, outpoint)
		state := a.activeUnrolls[outpoint]
		require.Equal(t, UnrollStatusBroadcasting, state.Status)

		// Verify store interactions.
		store.AssertCalled(t, "SaveUnrollState",
			mock.Anything, mock.Anything)
		store.AssertCalled(t, "UpdateUnrollState",
			mock.Anything, mock.Anything)
	})
}

// ===========================================================================
// Tests: handleGetUnrollStatus
// ===========================================================================

func TestHandleGetUnrollStatus(t *testing.T) {
	t.Parallel()

	t.Run("unroll not found returns error", func(t *testing.T) {
		t.Parallel()

		store := &MockUnrollStore{}
		cs := newMockChainSourceRef(t)
		a := newTestActor(t, store, cs)

		outpoint := newTestOutpoint(t)
		req := &GetUnrollStatusRequest{VTXOOutpoint: outpoint}
		result := a.Receive(t.Context(), req)
		_, err := result.Unpack()
		require.Error(t, err)
		require.Contains(t, err.Error(), "unroll not found")
	})

	t.Run("returns correct status", func(t *testing.T) {
		t.Parallel()

		store := &MockUnrollStore{}
		cs := newMockChainSourceRef(t)
		a := newTestActor(t, store, cs)

		outpoint := newTestOutpoint(t)
		a.activeUnrolls[outpoint] = &UnrollState{
			VTXOOutpoint: outpoint,
			Status:       UnrollStatusAwaitingCSV,
			CurrentLevel: 2,
			LevelOrder:   make([]LevelTxids, 3),
			VTXO: &round.ClientVTXO{
				Expiry: 144,
			},
			LeafConfirmHeight: 100,
		}
		a.bestHeight = 150

		req := &GetUnrollStatusRequest{VTXOOutpoint: outpoint}
		result := a.Receive(t.Context(), req)
		resp, err := result.Unpack()
		require.NoError(t, err)

		statusResp, ok := resp.(*UnrollStatusResp)
		require.True(t, ok)
		require.Equal(t, UnrollStatusAwaitingCSV, statusResp.Status)
		require.Equal(t, 2, statusResp.CurrentLevel)
		require.Equal(t, 3, statusResp.TotalLevels)

		// BlocksRemaining = (100 + 144) - 150 = 94
		require.Equal(t, int32(94), statusResp.BlocksRemaining)
	})
}

// ===========================================================================
// Tests: Receive unknown message type
// ===========================================================================

// unknownMsg satisfies UnrollerMsg for testing the default case.
type unknownMsg struct {
	actor.BaseMessage
}

func (m *unknownMsg) MessageType() string { return "unknownMsg" }
func (m *unknownMsg) unrollerMsgSealed()  {}

func TestReceiveUnknownMessage(t *testing.T) {
	t.Parallel()

	store := &MockUnrollStore{}
	cs := newMockChainSourceRef(t)
	a := newTestActor(t, store, cs)

	result := a.Receive(t.Context(), &unknownMsg{})
	_, err := result.Unpack()
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown message type")
}

// ===========================================================================
// Tests: Fee Bumping
// ===========================================================================

// setupBroadcastingState creates an UnrollerActor with a single VTXO
// in Broadcasting status, pre-seeded with a valid tree so that
// feeBumpLevel can reconstruct signed transactions. The mock chain
// source is wired to accept SubmitPackageRequest calls.
func setupBroadcastingState(t *testing.T, bumpAfterBlocks int32,
	lastBroadcastHeight int32, retryCount int,
) (*UnrollerActor, *MockUnrollStore, *mockChainSourceRef, wire.OutPoint) {

	t.Helper()

	store := &MockUnrollStore{}
	cs := newMockChainSourceRef(t)

	selfRef := &mockSelfRef{}
	cfg := &UnrollerConfig{
		ChainSource:     cs,
		Store:           store,
		ChainParams:     &chaincfg.RegressionNetParams,
		Logger:          btclog.Disabled,
		SelfRef:         selfRef,
		WalletKit:       nil,
		BumpAfterBlocks: bumpAfterBlocks,
	}
	a := NewUnrollerActor(cfg)

	// Build a minimal single-node tree for the VTXO.
	outpoint := newTestOutpoint(t)
	rootInput := newTestOutpoint(t)
	rootNode := makeSimpleNode(t, rootInput, 50000)
	vtxoTree := &tree.Tree{Root: rootNode}

	levelOrder, err := extractLevelOrder(vtxoTree)
	require.NoError(t, err)

	state := &UnrollState{
		VTXOOutpoint: outpoint,
		VTXO: &round.ClientVTXO{
			Outpoint: outpoint,
			Expiry:   144,
			TreePath: vtxoTree,
		},
		LevelOrder:          levelOrder,
		CurrentLevel:        0,
		BroadcastTxids:      make(map[chainhash.Hash]bool),
		ConfirmedTxids:      make(map[chainhash.Hash]ConfirmationInfo),
		Status:              UnrollStatusBroadcasting,
		LastBroadcastHeight: lastBroadcastHeight,
		RetryCount:          retryCount,
	}

	// Mark the root txid as broadcast.
	if len(levelOrder) > 0 && len(levelOrder[0].Txids) > 0 {
		state.BroadcastTxids[levelOrder[0].Txids[0]] = true
	}

	a.activeUnrolls[outpoint] = state
	a.indexUnrollTxids(state)

	// Wire up mock responses for fee bump resubmission.
	cs.onAsk(
		"SubmitPackageRequest",
		func() fn.Result[chainsource.ChainSourceResp] {
			return fn.Ok[chainsource.ChainSourceResp](
				&chainsource.SubmitPackageResponse{},
			)
		},
	)

	// UpdateUnrollState is called after fee bump and after
	// failUnroll.
	store.On("UpdateUnrollState", mock.Anything,
		mock.Anything).Return(nil)

	return a, store, cs, outpoint
}

// setupBroadcastingStateWithWalletKit creates an UnrollerActor in LND mode
// with WalletKit enabled so fee bumps must rebuild an explicit CPFP child.
func setupBroadcastingStateWithWalletKit(t *testing.T, bumpAfterBlocks int32,
	lastBroadcastHeight int32, retryCount int, currentFeeRate int64,
) (*UnrollerActor, *MockUnrollStore, *mockChainSourceRef, *mockWalletKit,
	wire.OutPoint) {

	t.Helper()

	store := &MockUnrollStore{}
	cs := newMockChainSourceRef(t)
	walletKit := newMockWalletKit(t, 100_000)

	selfRef := &mockSelfRef{}
	cfg := &UnrollerConfig{
		ChainSource:     cs,
		Store:           store,
		ChainParams:     &chaincfg.RegressionNetParams,
		Logger:          btclog.Disabled,
		SelfRef:         selfRef,
		WalletKit:       walletKit,
		BumpAfterBlocks: bumpAfterBlocks,
	}
	a := NewUnrollerActor(cfg)

	outpoint := newTestOutpoint(t)
	rootInput := newTestOutpoint(t)
	rootNode := makeSimpleNode(t, rootInput, 50_000)
	vtxoTree := &tree.Tree{Root: rootNode}

	levelOrder, err := extractLevelOrder(vtxoTree)
	require.NoError(t, err)

	state := &UnrollState{
		VTXOOutpoint: outpoint,
		VTXO: &round.ClientVTXO{
			Outpoint: outpoint,
			Expiry:   144,
			TreePath: vtxoTree,
		},
		LevelOrder:          levelOrder,
		CurrentLevel:        0,
		BroadcastTxids:      make(map[chainhash.Hash]bool),
		ConfirmedTxids:      make(map[chainhash.Hash]ConfirmationInfo),
		Status:              UnrollStatusBroadcasting,
		LastBroadcastHeight: lastBroadcastHeight,
		RetryCount:          retryCount,
		CurrentFeeRate:      currentFeeRate,
	}

	if len(levelOrder) > 0 && len(levelOrder[0].Txids) > 0 {
		state.BroadcastTxids[levelOrder[0].Txids[0]] = true
	}

	a.activeUnrolls[outpoint] = state
	a.indexUnrollTxids(state)

	cs.onAsk(
		"FeeEstimateRequest",
		func() fn.Result[chainsource.ChainSourceResp] {
			return fn.Ok[chainsource.ChainSourceResp](
				&chainsource.FeeEstimateResponse{
					SatPerVByte: 15,
				},
			)
		},
	)
	cs.onAsk(
		"SubmitPackageRequest",
		func() fn.Result[chainsource.ChainSourceResp] {
			return fn.Ok[chainsource.ChainSourceResp](
				&chainsource.SubmitPackageResponse{},
			)
		},
	)

	store.On("UpdateUnrollState", mock.Anything,
		mock.Anything).Return(nil)

	return a, store, cs, walletKit, outpoint
}

func TestFeeBumpTriggeredAfterBlocks(t *testing.T) {
	t.Parallel()

	a, _, cs, _ := setupBroadcastingState(
		t,
		3,   // BumpAfterBlocks
		100, // LastBroadcastHeight
		0,   // RetryCount
	)

	// Send block at height 102 — only 2 blocks elapsed, should
	// NOT trigger fee bump.
	result := a.Receive(t.Context(), &BlockEpochEvent{Height: 102})
	_, err := result.Unpack()
	require.NoError(t, err)

	submitCount := cs.askCallCounts["SubmitPackageRequest"]
	require.Equal(t, 0, submitCount,
		"no fee bump expected after only 2 blocks")

	// Send block at height 103 — 3 blocks elapsed, SHOULD trigger
	// fee bump.
	result = a.Receive(t.Context(), &BlockEpochEvent{Height: 103})
	_, err = result.Unpack()
	require.NoError(t, err)

	submitCount = cs.askCallCounts["SubmitPackageRequest"]
	require.Equal(t, 1, submitCount,
		"fee bump expected after 3 blocks")
}

func TestFeeBumpMaxRetries(t *testing.T) {
	t.Parallel()

	a, _, cs, outpoint := setupBroadcastingState(
		t,
		3,                   // BumpAfterBlocks
		100,                 // LastBroadcastHeight
		maxFeeBumpRetries-1, // RetryCount = 9
	)

	// Trigger one more fee bump. Since RetryCount is 9 (< 10),
	// the bump should proceed.
	result := a.Receive(t.Context(), &BlockEpochEvent{Height: 103})
	_, err := result.Unpack()
	require.NoError(t, err)

	submitCount := cs.askCallCounts["SubmitPackageRequest"]
	require.Equal(t, 1, submitCount,
		"expected one fee bump submission")

	// The state should still be tracked (RetryCount now = 10).
	state, active := a.activeUnrolls[outpoint]
	require.True(t, active,
		"unroll should still be active after bump")
	require.Equal(t, maxFeeBumpRetries, state.RetryCount)

	// After the successful bump, LastBroadcastHeight was updated
	// to 103 (the bestHeight). Send a block at 106 to trigger
	// another bump attempt (3 blocks later).
	result = a.Receive(t.Context(), &BlockEpochEvent{Height: 106})
	_, err = result.Unpack()
	require.NoError(t, err)

	// No additional SubmitPackageRequest should have been sent
	// because RetryCount == maxFeeBumpRetries triggers failUnroll
	// before any submission.
	require.Equal(t, 1, cs.askCallCounts["SubmitPackageRequest"],
		"no additional submission after retry limit reached")

	// The unroll should now be removed from active tracking
	// (failUnroll deletes it).
	_, stillActive := a.activeUnrolls[outpoint]
	require.False(t, stillActive,
		"unroll should be removed after max retries exceeded")
}

func TestFeeBumpRebuildsChildWithWalletKit(t *testing.T) {
	t.Parallel()

	a, store, cs, walletKit, outpoint :=
		setupBroadcastingStateWithWalletKit(
			t,
			3,   // BumpAfterBlocks
			100, // LastBroadcastHeight
			0,   // RetryCount
			10,  // CurrentFeeRate
		)

	result := a.Receive(t.Context(), &BlockEpochEvent{Height: 103})
	_, err := result.Unpack()
	require.NoError(t, err)

	require.Equal(t, 1, cs.askCallCounts["FeeEstimateRequest"])
	require.Equal(t, 1, cs.askCallCounts["SubmitPackageRequest"])
	require.Equal(t, 1, walletKit.listCalls)
	require.Equal(t, 1, walletKit.nextAddrCalls)
	require.Equal(t, 1, walletKit.finalizeCalls)

	state, active := a.activeUnrolls[outpoint]
	require.True(t, active)
	require.Equal(t, int64(20), state.CurrentFeeRate)
	require.Equal(t, 1, state.RetryCount)
	require.Equal(t, int32(103), state.LastBroadcastHeight)

	var submitReq *chainsource.SubmitPackageRequest
	for _, msg := range cs.asks {
		req, ok := msg.(*chainsource.SubmitPackageRequest)
		if ok {
			submitReq = req
			break
		}
	}
	require.NotNil(t, submitReq)
	require.Len(t, submitReq.Parents, 1)
	require.NotNil(t, submitReq.Child,
		"WalletKit fee bump must submit an explicit CPFP child")

	parentWeight := estimateWeight(submitReq.Parents[0])
	parentVSize := (parentWeight + 3) / 4
	expectedFee := btcutil.Amount(20) *
		btcutil.Amount(parentVSize+childVSizeEstimate)
	require.Len(t, submitReq.Child.TxOut, 1)
	require.Equal(t,
		int64(walletKit.utxos[0].Value-expectedFee),
		submitReq.Child.TxOut[0].Value,
	)

	store.AssertCalled(t, "UpdateUnrollState",
		mock.Anything, mock.MatchedBy(func(state *UnrollState) bool {
			return state.CurrentFeeRate == 20 &&
				state.RetryCount == 1 &&
				state.LastBroadcastHeight == 103
		}))
}

// ===========================================================================
// Tests: CSV Completion and Sweep
// ===========================================================================

// setupCSVWaitState creates an UnrollerActor with a single VTXO in
// AwaitingCSV status. The leaf is confirmed at leafConfirmHeight and
// the CSV delay is set via the vtxo.Expiry field.
func setupCSVWaitState(t *testing.T, leafConfirmHeight int32,
	csvDelay uint32,
) (*UnrollerActor, *MockUnrollStore, *mockChainSourceRef, wire.OutPoint) {

	t.Helper()

	store := &MockUnrollStore{}
	cs := newMockChainSourceRef(t)

	selfRef := &mockSelfRef{}
	cfg := &UnrollerConfig{
		ChainSource: cs,
		Store:       store,
		ChainParams: &chaincfg.RegressionNetParams,
		Logger:      btclog.Disabled,
		SelfRef:     selfRef,
		WalletKit:   nil,
		// Signer and SweepAddress intentionally nil —
		// sweep is skipped.
	}
	a := NewUnrollerActor(cfg)

	outpoint := newTestOutpoint(t)
	rootNode := makeSimpleNode(t, newTestOutpoint(t), 50000)
	vtxoTree := &tree.Tree{Root: rootNode}

	levelOrder, err := extractLevelOrder(vtxoTree)
	require.NoError(t, err)

	state := &UnrollState{
		VTXOOutpoint: outpoint,
		VTXO: &round.ClientVTXO{
			Outpoint: outpoint,
			Expiry:   csvDelay,
			TreePath: vtxoTree,
		},
		LevelOrder:        levelOrder,
		CurrentLevel:      0,
		BroadcastTxids:    make(map[chainhash.Hash]bool),
		ConfirmedTxids:    make(map[chainhash.Hash]ConfirmationInfo),
		Status:            UnrollStatusAwaitingCSV,
		LeafConfirmHeight: leafConfirmHeight,
	}

	a.activeUnrolls[outpoint] = state
	a.indexUnrollTxids(state)

	store.On("UpdateUnrollState", mock.Anything,
		mock.Anything).Return(nil)

	return a, store, cs, outpoint
}

func TestCSVCompletionTransitionsToComplete(t *testing.T) {
	t.Parallel()

	// Leaf confirmed at height 100, CSV delay = 10.
	// CSV target = 100 + 10 = 110.
	a, store, _, outpoint := setupCSVWaitState(t, 100, 10)

	// Block at 109: CSV not yet satisfied.
	result := a.Receive(t.Context(), &BlockEpochEvent{Height: 109})
	_, err := result.Unpack()
	require.NoError(t, err)

	state, active := a.activeUnrolls[outpoint]
	require.True(t, active, "should still be active before CSV")
	require.Equal(t, UnrollStatusAwaitingCSV, state.Status)

	// Block at 110: CSV satisfied → state transitions to Complete.
	result = a.Receive(t.Context(), &BlockEpochEvent{Height: 110})
	_, err = result.Unpack()
	require.NoError(t, err)

	// After completion, the unroll is removed from active tracking.
	_, stillActive := a.activeUnrolls[outpoint]
	require.False(t, stillActive,
		"unroll should be removed after CSV completion")

	// UpdateUnrollState should have been called to persist the
	// Complete status.
	store.AssertCalled(t, "UpdateUnrollState",
		mock.Anything, mock.Anything)
}

func TestCSVCompletionSkipsSweepWhenUnconfigured(t *testing.T) {
	t.Parallel()

	// Signer and SweepAddress are nil in setupCSVWaitState, so
	// sweep should be skipped gracefully.
	a, _, cs, outpoint := setupCSVWaitState(t, 100, 5)

	// Trigger CSV completion.
	result := a.Receive(t.Context(), &BlockEpochEvent{Height: 105})
	_, err := result.Unpack()
	require.NoError(t, err)

	// Unroll should complete without attempting broadcast (sweep
	// not configured).
	_, active := a.activeUnrolls[outpoint]
	require.False(t, active)

	// No BroadcastTxRequest should have been issued.
	require.Equal(t, 0, cs.askCallCounts["BroadcastTxRequest"],
		"no sweep broadcast when signer is nil")
}

func TestCSVBaselineInitializedOnFirstEpoch(t *testing.T) {
	t.Parallel()

	// Set LeafConfirmHeight to 0 (not yet initialized).
	store := &MockUnrollStore{}
	cs := newMockChainSourceRef(t)

	selfRef := &mockSelfRef{}
	cfg := &UnrollerConfig{
		ChainSource: cs,
		Store:       store,
		ChainParams: &chaincfg.RegressionNetParams,
		Logger:      btclog.Disabled,
		SelfRef:     selfRef,
		WalletKit:   nil,
	}
	a := NewUnrollerActor(cfg)

	outpoint := newTestOutpoint(t)
	rootNode := makeSimpleNode(t, newTestOutpoint(t), 50000)
	vtxoTree := &tree.Tree{Root: rootNode}

	levelOrder, err := extractLevelOrder(vtxoTree)
	require.NoError(t, err)

	state := &UnrollState{
		VTXOOutpoint: outpoint,
		VTXO: &round.ClientVTXO{
			Outpoint: outpoint,
			Expiry:   10,
			TreePath: vtxoTree,
		},
		LevelOrder:        levelOrder,
		CurrentLevel:      0,
		BroadcastTxids:    make(map[chainhash.Hash]bool),
		ConfirmedTxids:    make(map[chainhash.Hash]ConfirmationInfo),
		Status:            UnrollStatusAwaitingCSV,
		LeafConfirmHeight: 0, // not yet set
	}

	a.activeUnrolls[outpoint] = state

	store.On("UpdateUnrollState", mock.Anything,
		mock.Anything).Return(nil)

	// First block epoch should initialize the baseline.
	result := a.Receive(t.Context(), &BlockEpochEvent{Height: 200})
	_, err = result.Unpack()
	require.NoError(t, err)

	require.Equal(t, int32(200), state.LeafConfirmHeight,
		"baseline should be initialized from first epoch")

	// CSV target is now 200 + 10 = 210, so still waiting.
	require.Equal(t, UnrollStatusAwaitingCSV, state.Status)
}

func TestFeeBumpNotTriggeredForCSVWait(t *testing.T) {
	t.Parallel()

	store := &MockUnrollStore{}
	cs := newMockChainSourceRef(t)

	selfRef := &mockSelfRef{}
	cfg := &UnrollerConfig{
		ChainSource:     cs,
		Store:           store,
		ChainParams:     &chaincfg.RegressionNetParams,
		Logger:          btclog.Disabled,
		SelfRef:         selfRef,
		WalletKit:       nil,
		BumpAfterBlocks: 3,
	}
	a := NewUnrollerActor(cfg)

	// Set up state in AwaitingCSV with LastBroadcastHeight=100.
	outpoint := newTestOutpoint(t)
	rootNode := makeSimpleNode(t, newTestOutpoint(t), 50000)
	vtxoTree := &tree.Tree{Root: rootNode}

	levelOrder, err := extractLevelOrder(vtxoTree)
	require.NoError(t, err)

	state := &UnrollState{
		VTXOOutpoint: outpoint,
		VTXO: &round.ClientVTXO{
			Outpoint: outpoint,
			Expiry:   144,
			TreePath: vtxoTree,
		},
		LevelOrder:          levelOrder,
		CurrentLevel:        0,
		BroadcastTxids:      make(map[chainhash.Hash]bool),
		ConfirmedTxids:      make(map[chainhash.Hash]ConfirmationInfo),
		Status:              UnrollStatusAwaitingCSV,
		LastBroadcastHeight: 100,
		LeafConfirmHeight:   95,
	}

	a.activeUnrolls[outpoint] = state

	// UpdateUnrollState may be called for CSV baseline logic.
	store.On("UpdateUnrollState", mock.Anything,
		mock.Anything).Return(nil)

	// Send block at height 110 — this is 10 blocks past
	// LastBroadcastHeight and well past BumpAfterBlocks=3, but
	// should NOT trigger fee bump because status is AwaitingCSV.
	result := a.Receive(t.Context(), &BlockEpochEvent{Height: 110})
	_, err = result.Unpack()
	require.NoError(t, err)

	// No SubmitPackageRequest should have been sent.
	require.Equal(t, 0, cs.askCallCounts["SubmitPackageRequest"],
		"no fee bump expected for AwaitingCSV status")

	// Status should still be AwaitingCSV (CSV target = 95+144 =
	// 239, height 110 is well before that).
	require.Equal(t, UnrollStatusAwaitingCSV, state.Status)
}

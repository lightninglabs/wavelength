package batchwatcher

import (
	"context"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/internal/testutils"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// testHarness provides common setup for BatchWatcher actor tests.
type testHarness struct {
	t *testing.T

	// actor is the BatchWatcher actor under test.
	actor *Actor

	// mockChainSource captures ChainSource requests.
	mockChainSource *mockChainSourceActor

	// mockFraudDetector captures FraudDetector notifications.
	mockFraudDetector *mockFraudDetectorActor

	// mockBatchSweeper captures BatchSweeper notifications.
	mockBatchSweeper *mockBatchSweeperActor

	// operatorKey is the operator's public key for building trees.
	operatorKey *btcec.PublicKey
}

// newTestHarness creates a new test harness with default configuration.
func newTestHarness(t *testing.T) *testHarness {
	t.Helper()

	// Create mock actors.
	mockCS := newMockChainSourceActor()
	mockFD := newMockFraudDetectorActor()
	mockBS := newMockBatchSweeperActor()

	// Create a mock self ref that can receive messages.
	selfRef := newMockSelfRef[BatchWatcherMsg]()

	// Generate operator key.
	operatorKey, _ := testutils.CreateKey(1)

	cfg := &ActorConfig{
		Logger:        btclog.Disabled,
		ChainSource:   mockCS.ref,
		FraudDetector: fn.Some(mockFD.ref),
		BatchSweeper:  fn.Some(mockBS.ref),
		SelfRef:       selfRef,
	}

	batchWatcher := NewActor(cfg)

	return &testHarness{
		t:                 t,
		actor:             batchWatcher,
		mockChainSource:   mockCS,
		mockFraudDetector: mockFD,
		mockBatchSweeper:  mockBS,
		operatorKey:       operatorKey,
	}
}

// createBatchID creates a new random batch ID.
func createBatchID(t *testing.T) BatchID {
	t.Helper()

	id, err := uuid.NewV7()
	require.NoError(t, err)

	return BatchID(id)
}

// createSimpleTree creates a simple tree with a single VTXO leaf for testing.
func (h *testHarness) createSimpleTree(t *testing.T) *tree.Tree {
	t.Helper()

	// Create a client key for the VTXO.
	clientKey, _ := testutils.CreateKey(100)

	// Create a mock batch outpoint.
	batchOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{1, 2, 3, 4},
		Index: 0,
	}

	// The tree builder requires leaf amounts to equal the output value.
	// Create the batch output with value matching the leaf amount.
	leafAmount := btcutil.Amount(100000)
	batchOutput := wire.NewTxOut(int64(leafAmount), []byte{0x51})

	// Create a leaf descriptor with matching amount.
	leaf := tree.LeafDescriptor{
		CoSignerKey: clientKey,
		Amount:      leafAmount,
		PkScript:    []byte{0x51, 0x20, 0x01, 0x02},
	}

	// Build a simple tree.
	sweepTapscriptRoot := []byte{0xaa, 0xbb, 0xcc}
	t1, err := tree.NewTree(
		batchOutpoint, batchOutput, []tree.LeafDescriptor{leaf},
		h.operatorKey, sweepTapscriptRoot, 2,
	)
	require.NoError(t, err)

	return t1
}

// completedFuture returns a Future that is already completed with the given
// response.
func completedFuture(
	resp chainsource.ChainSourceResp,
) actor.Future[chainsource.ChainSourceResp] {

	promise := actor.NewPromise[chainsource.ChainSourceResp]()
	promise.Complete(fn.Ok(resp))

	return promise.Future()
}

// TestRegisterBatch verifies that registering a batch creates the proper state
// and watches.
func TestRegisterBatch(t *testing.T) {
	h := newTestHarness(t)

	batchID := createBatchID(t)
	testTree := h.createSimpleTree(t)

	// Set up mock to accept spend registration with a completed future.
	h.mockChainSource.mock.On("Ask", mock.Anything, mock.Anything).
		Return(completedFuture(&chainsource.RegisterSpendResponse{})).
		Once()

	// Also mock for block subscription.
	h.mockChainSource.mock.On("Ask", mock.Anything, mock.Anything).
		Return(completedFuture(&chainsource.SubscribeBlocksResponse{})).
		Maybe()

	// Register the batch.
	req := &RegisterBatchRequest{
		BatchID:      batchID,
		Tree:         testTree,
		ExpiryHeight: 1000,
	}

	result := h.actor.Receive(h.t.Context(), req)
	require.True(t, result.IsOk(), "registration should succeed")

	// Verify batch is in state.
	state := h.actor.state.GetBatch(batchID)
	require.NotNil(t, state, "batch should be in state")
	require.Equal(t, batchID, state.BatchID)
	require.Equal(t, uint32(1000), state.ExpiryHeight)
	require.NotNil(t, state.Tree)
}

// TestGetTreeState verifies that tree state queries work correctly.
func TestGetTreeState(t *testing.T) {
	h := newTestHarness(t)

	batchID := createBatchID(t)
	testTree := h.createSimpleTree(t)

	// Manually add a batch to state for testing.
	treeState := NewBatchTreeState(batchID, testTree, 1000)
	h.actor.state.RegisterBatch(treeState)

	// Query the state.
	req := &GetTreeStateRequest{BatchID: batchID}
	result := h.actor.Receive(h.t.Context(), req)

	require.True(t, result.IsOk())

	respVal := result.UnwrapOrFail(t)
	resp, ok := respVal.(*GetTreeStateResponse)
	require.True(t, ok)
	require.True(t, resp.Found)
	require.NotNil(t, resp.TreeState)
	require.Equal(t, batchID, resp.TreeState.BatchID)
}

// TestGetTreeStateNotFound verifies that querying a non-existent batch returns
// Found=false.
func TestGetTreeStateNotFound(t *testing.T) {
	h := newTestHarness(t)

	batchID := createBatchID(t)

	req := &GetTreeStateRequest{BatchID: batchID}
	result := h.actor.Receive(h.t.Context(), req)

	require.True(t, result.IsOk())

	respVal := result.UnwrapOrFail(t)
	resp, ok := respVal.(*GetTreeStateResponse)
	require.True(t, ok)
	require.False(t, resp.Found)
}

// TestUnregisterBatch verifies that unregistering a batch removes it from
// state.
func TestUnregisterBatch(t *testing.T) {
	h := newTestHarness(t)

	batchID := createBatchID(t)
	testTree := h.createSimpleTree(t)

	// Add batch to state.
	treeState := NewBatchTreeState(batchID, testTree, 1000)
	h.actor.state.RegisterBatch(treeState)

	require.NotNil(t, h.actor.state.GetBatch(batchID))

	// Unregister.
	req := &UnregisterBatchRequest{BatchID: batchID}
	result := h.actor.Receive(h.t.Context(), req)

	require.True(t, result.IsOk())
	require.Nil(t, h.actor.state.GetBatch(batchID))
}

// TestNewBlockReceivedExpiryNotification verifies that batch expiry
// notifications are sent when a batch reaches its expiry height.
func TestNewBlockReceivedExpiryNotification(t *testing.T) {
	h := newTestHarness(t)

	batchID := createBatchID(t)
	testTree := h.createSimpleTree(t)

	// Add batch expiring at height 1000.
	treeState := NewBatchTreeState(batchID, testTree, 1000)
	h.actor.state.RegisterBatch(treeState)

	// Simulate receiving block at height 1000.
	blockMsg := &NewBlockReceived{Height: 1000}
	result := h.actor.Receive(h.t.Context(), blockMsg)

	require.True(t, result.IsOk())

	// Verify BatchSweeper was notified.
	require.Len(t, h.mockBatchSweeper.receivedMsgs, 1)

	msg := h.mockBatchSweeper.receivedMsgs[0]
	notification, ok := msg.(*BatchExpiredNotification)
	require.True(t, ok)
	require.Equal(t, batchID, notification.BatchID)
	require.Equal(t, uint32(1000), notification.ExpiryHeight)
}

// TestNewBlockReceivedNoExpiry verifies that no notifications are sent when
// no batches expire.
func TestNewBlockReceivedNoExpiry(t *testing.T) {
	h := newTestHarness(t)

	batchID := createBatchID(t)
	testTree := h.createSimpleTree(t)

	// Add batch expiring at height 2000.
	treeState := NewBatchTreeState(batchID, testTree, 2000)
	h.actor.state.RegisterBatch(treeState)

	// Simulate receiving block at height 1000 (before expiry).
	blockMsg := &NewBlockReceived{Height: 1000}
	result := h.actor.Receive(h.t.Context(), blockMsg)

	require.True(t, result.IsOk())

	// Verify no notifications sent.
	require.Len(t, h.mockBatchSweeper.receivedMsgs, 0)
}

// TestStateStoreExpiryIndex verifies the expiry index works correctly.
func TestStateStoreExpiryIndex(t *testing.T) {
	store := NewStateStore()

	// Create multiple batches with different expiry heights.
	batch1ID := createBatchID(t)
	batch2ID := createBatchID(t)
	batch3ID := createBatchID(t)

	state1 := NewBatchTreeState(batch1ID, nil, 1000)
	// Same height as batch1.
	state2 := NewBatchTreeState(batch2ID, nil, 1000)
	state3 := NewBatchTreeState(batch3ID, nil, 2000)

	store.RegisterBatch(state1)
	store.RegisterBatch(state2)
	store.RegisterBatch(state3)

	// Query expiring at 1000.
	expiring1000 := store.GetBatchesExpiringAt(1000)
	require.Len(t, expiring1000, 2)
	require.Contains(t, expiring1000, batch1ID)
	require.Contains(t, expiring1000, batch2ID)

	// Query expiring at 2000.
	expiring2000 := store.GetBatchesExpiringAt(2000)
	require.Len(t, expiring2000, 1)
	require.Contains(t, expiring2000, batch3ID)

	// Query non-existent height.
	expiring999 := store.GetBatchesExpiringAt(999)
	require.Len(t, expiring999, 0)
}

// TestStateStoreUnregisterRemovesFromExpiryIndex verifies that unregistering
// a batch removes it from the expiry index.
func TestStateStoreUnregisterRemovesFromExpiryIndex(t *testing.T) {
	store := NewStateStore()

	batch1ID := createBatchID(t)
	batch2ID := createBatchID(t)

	state1 := NewBatchTreeState(batch1ID, nil, 1000)
	state2 := NewBatchTreeState(batch2ID, nil, 1000)

	store.RegisterBatch(state1)
	store.RegisterBatch(state2)

	// Verify both in index.
	require.Len(t, store.GetBatchesExpiringAt(1000), 2)

	// Unregister one.
	store.UnregisterBatch(batch1ID)

	// Verify only one remains.
	expiring := store.GetBatchesExpiringAt(1000)
	require.Len(t, expiring, 1)
	require.Contains(t, expiring, batch2ID)
}

// TestBatchTreeStateOutputTracking verifies output tracking methods.
func TestBatchTreeStateOutputTracking(t *testing.T) {
	batchID := createBatchID(t)
	state := NewBatchTreeState(batchID, nil, 1000)

	outpoint := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 0}
	txOut := wire.NewTxOut(50000, []byte{0x51})

	// Create a regular output.
	output := &Output{
		Outpoint: outpoint,
		TxOut:    txOut,
		IsVTXO:   false,
	}

	// Add and verify.
	state.AddExistingOutput(output)
	require.NotNil(t, state.GetExistingOutput(outpoint))
	require.Len(t, state.GetUnspentOutputs(), 1)
	require.Len(t, state.GetVTXOsOnChain(), 0) // Not a VTXO.

	// Remove and verify.
	removed := state.RemoveExistingOutput(outpoint)
	require.Equal(t, output, removed)
	require.Nil(t, state.GetExistingOutput(outpoint))
	require.Len(t, state.GetUnspentOutputs(), 0)
}

// TestBatchTreeStateVTXOTracking verifies VTXO-specific tracking.
func TestBatchTreeStateVTXOTracking(t *testing.T) {
	batchID := createBatchID(t)
	state := NewBatchTreeState(batchID, nil, 1000)

	outpoint := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 0}
	txOut := wire.NewTxOut(50000, []byte{0x51})

	// Create a VTXO output.
	output := &Output{
		Outpoint: outpoint,
		TxOut:    txOut,
		IsVTXO:   true,
	}

	// Add and verify.
	state.AddExistingOutput(output)
	require.Len(t, state.GetVTXOsOnChain(), 1)

	// Remove and verify both collections updated.
	state.RemoveExistingOutput(outpoint)
	require.Len(t, state.GetUnspentOutputs(), 0)
	require.Len(t, state.GetVTXOsOnChain(), 0)
}

// TestBatchTreeStateWatchedTracking verifies watched outpoint tracking.
func TestBatchTreeStateWatchedTracking(t *testing.T) {
	batchID := createBatchID(t)
	state := NewBatchTreeState(batchID, nil, 1000)

	outpoint := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 0}

	require.False(t, state.IsWatched(outpoint))

	state.MarkWatched(outpoint)

	require.True(t, state.IsWatched(outpoint))
}

// TestBatchTreeStateSpentNodeTracking verifies spent node tracking.
func TestBatchTreeStateSpentNodeTracking(t *testing.T) {
	batchID := createBatchID(t)
	state := NewBatchTreeState(batchID, nil, 1000)

	txid := chainhash.Hash{1, 2, 3}

	require.False(t, state.IsNodeSpent(txid))

	state.MarkNodeSpent(txid)

	require.True(t, state.IsNodeSpent(txid))
}

// TestNodeSpendDetected_ProgressiveWatching tests that when a batch output is
// spent, the actor correctly updates state and registers watches on child
// outputs.
func TestNodeSpendDetected_ProgressiveWatching(t *testing.T) {
	h := newTestHarness(t)

	batchID := createBatchID(t)
	testTree := h.createSimpleTree(t)

	// Manually add a batch to state for testing.
	treeState := NewBatchTreeState(batchID, testTree, 1000)

	// Add the batch output to ExistingOutputs with TreeNode set to Root,
	// mirroring what handleRegisterBatch does.
	treeState.AddExistingOutput(&Output{
		Outpoint: testTree.BatchOutpoint,
		TxOut:    testTree.BatchOutput,
		TreeNode: testTree.Root,
	})

	// Mark the batch outpoint as watched (simulates prior registration).
	treeState.MarkWatched(testTree.BatchOutpoint)
	h.actor.state.RegisterBatch(treeState)

	// Set up mock to accept spend registration for child outputs.
	h.mockChainSource.mock.On("Ask", mock.Anything, mock.Anything).
		Return(completedFuture(&chainsource.RegisterSpendResponse{})).
		Maybe()

	// Create a spending transaction that matches the root node's structure.
	// The tree.Root node has outputs, so we simulate it being spent.
	spendingTx := wire.NewMsgTx(3)
	spendingTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: testTree.BatchOutpoint,
	})

	// Add outputs from the root node (this simulates the tree unroll).
	for _, txOut := range testTree.Root.Outputs {
		spendingTx.AddTxOut(txOut)
	}

	spendingTxHash := spendingTx.TxHash()

	// Send NodeSpendDetected message for the batch output being spent.
	msg := &NodeSpendDetected{
		BatchID:        batchID,
		SpentOutpoint:  testTree.BatchOutpoint,
		SpendingTx:     spendingTx,
		SpendingHeight: 500,
	}

	result := h.actor.Receive(h.t.Context(), msg)
	require.True(t, result.IsOk())

	// Verify the spending transaction is marked as spent.
	state := h.actor.state.GetBatch(batchID)
	require.NotNil(t, state)
	require.True(t, state.IsNodeSpent(spendingTxHash),
		"spending tx should be marked as spent")

	// Verify child outputs are tracked (at least one non-anchor output).
	require.Greater(t, len(state.ExistingOutputs), 0,
		"child outputs should be tracked")
}

// TestNodeSpendDetected_VTXONotification tests that when a VTXO leaf appears
// on-chain, the FraudDetector receives a notification.
func TestNodeSpendDetected_VTXONotification(t *testing.T) {
	h := newTestHarness(t)

	batchID := createBatchID(t)
	testTree := h.createSimpleTree(t)

	// Manually add a batch to state for testing.
	treeState := NewBatchTreeState(batchID, testTree, 1000)

	// Add the batch output to ExistingOutputs with TreeNode set to Root,
	// mirroring what handleRegisterBatch does.
	treeState.AddExistingOutput(&Output{
		Outpoint: testTree.BatchOutpoint,
		TxOut:    testTree.BatchOutput,
		TreeNode: testTree.Root,
	})

	// Mark the batch outpoint as watched.
	treeState.MarkWatched(testTree.BatchOutpoint)
	h.actor.state.RegisterBatch(treeState)

	// Set up mock to accept any ChainSource requests.
	h.mockChainSource.mock.On("Ask", mock.Anything, mock.Anything).
		Return(completedFuture(&chainsource.RegisterSpendResponse{})).
		Maybe()

	// Create a spending transaction that produces VTXO outputs.
	// For our simple tree, the root node's children are leaves (VTXOs).
	spendingTx := wire.NewMsgTx(3)
	spendingTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: testTree.BatchOutpoint,
	})

	// Add outputs from the root node.
	for _, txOut := range testTree.Root.Outputs {
		spendingTx.AddTxOut(txOut)
	}

	// Send NodeSpendDetected message.
	msg := &NodeSpendDetected{
		BatchID:        batchID,
		SpentOutpoint:  testTree.BatchOutpoint,
		SpendingTx:     spendingTx,
		SpendingHeight: 500,
	}

	result := h.actor.Receive(h.t.Context(), msg)
	require.True(t, result.IsOk())

	// Check if the tree has leaf children. If so, VTXOs should be detected.
	// Our tree has a single VTXO leaf, so the root's child is a leaf.
	state := h.actor.state.GetBatch(batchID)
	vtxos := state.GetVTXOsOnChain()

	// Verify FraudDetector was notified for each VTXO.
	if len(vtxos) > 0 {
		require.Greater(t, len(h.mockFraudDetector.receivedMsgs), 0,
			"FraudDetector should receive VTXO notification")

		fdMsg := h.mockFraudDetector.receivedMsgs[0]
		notification, ok := fdMsg.(*VTXOOnChainNotification)
		require.True(t, ok, "should be VTXOOnChainNotification")
		require.Equal(t, batchID, notification.BatchID)
		require.NotNil(t, notification.VTXOOutput)
	}
}

// TestNodeSpendDetected_TreeStateChangedNotification tests that when tree
// state changes, the BatchSweeper receives a notification.
func TestNodeSpendDetected_TreeStateChangedNotification(t *testing.T) {
	h := newTestHarness(t)

	batchID := createBatchID(t)
	testTree := h.createSimpleTree(t)

	// Manually add a batch to state for testing.
	treeState := NewBatchTreeState(batchID, testTree, 1000)

	// Add the batch output to ExistingOutputs with TreeNode set to Root,
	// mirroring what handleRegisterBatch does.
	treeState.AddExistingOutput(&Output{
		Outpoint: testTree.BatchOutpoint,
		TxOut:    testTree.BatchOutput,
		TreeNode: testTree.Root,
	})

	// Mark the batch outpoint as watched.
	treeState.MarkWatched(testTree.BatchOutpoint)
	h.actor.state.RegisterBatch(treeState)

	// Set up mock to accept any ChainSource requests.
	h.mockChainSource.mock.On("Ask", mock.Anything, mock.Anything).
		Return(completedFuture(&chainsource.RegisterSpendResponse{})).
		Maybe()

	// Create a spending transaction.
	spendingTx := wire.NewMsgTx(3)
	spendingTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: testTree.BatchOutpoint,
	})

	// Add outputs from the root node.
	for _, txOut := range testTree.Root.Outputs {
		spendingTx.AddTxOut(txOut)
	}

	// Send NodeSpendDetected message.
	msg := &NodeSpendDetected{
		BatchID:        batchID,
		SpentOutpoint:  testTree.BatchOutpoint,
		SpendingTx:     spendingTx,
		SpendingHeight: 500,
	}

	result := h.actor.Receive(h.t.Context(), msg)
	require.True(t, result.IsOk())

	// Verify BatchSweeper was notified of tree state change.
	// Look for TreeStateChangedNotification in received messages.
	var foundTreeStateChanged bool
	for _, msg := range h.mockBatchSweeper.receivedMsgs {
		if notification, ok := msg.(*TreeStateChangedNotification); ok {
			require.Equal(t, batchID, notification.BatchID)
			foundTreeStateChanged = true

			break
		}
	}

	require.True(t, foundTreeStateChanged,
		"BatchSweeper should receive TreeStateChangedNotification")
}

// ===== Mock implementations =====

// Type aliases for shorter lines.
type (
	csMsg  = chainsource.ChainSourceMsg
	csResp = chainsource.ChainSourceResp
)

// mockChainSourceActor mocks the ChainSource actor for testing.
type mockChainSourceActor struct {
	mock *mockActorRef[csMsg, csResp]
	ref  actor.ActorRef[csMsg, csResp]
}

// newMockChainSourceActor creates a new mock ChainSource actor.
func newMockChainSourceActor() *mockChainSourceActor {
	mockRef := newMockActorRef[csMsg, csResp]("mock-chainsource")

	return &mockChainSourceActor{
		mock: mockRef,
		ref:  mockRef,
	}
}

// mockFraudDetectorActor mocks the FraudDetector actor for testing.
type mockFraudDetectorActor struct {
	mu           sync.Mutex
	receivedMsgs []FraudDetectorMsg
	ref          actor.TellOnlyRef[FraudDetectorMsg]
}

// newMockFraudDetectorActor creates a new mock FraudDetector actor.
func newMockFraudDetectorActor() *mockFraudDetectorActor {
	m := &mockFraudDetectorActor{
		receivedMsgs: make([]FraudDetectorMsg, 0),
	}
	m.ref = &mockTellOnlyRef[FraudDetectorMsg]{
		id: "mock-fraud-detector",
		tellFn: func(_ context.Context, msg FraudDetectorMsg) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.receivedMsgs = append(m.receivedMsgs, msg)
		},
	}

	return m
}

// mockBatchSweeperActor mocks the BatchSweeper actor for testing.
type mockBatchSweeperActor struct {
	mu           sync.Mutex
	receivedMsgs []BatchSweeperMsg
	ref          actor.TellOnlyRef[BatchSweeperMsg]
}

// newMockBatchSweeperActor creates a new mock BatchSweeper actor.
func newMockBatchSweeperActor() *mockBatchSweeperActor {
	m := &mockBatchSweeperActor{
		receivedMsgs: make([]BatchSweeperMsg, 0),
	}
	m.ref = &mockTellOnlyRef[BatchSweeperMsg]{
		id: "mock-batch-sweeper",
		tellFn: func(_ context.Context, msg BatchSweeperMsg) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.receivedMsgs = append(m.receivedMsgs, msg)
		},
	}

	return m
}

// mockTellOnlyRef is a generic mock TellOnlyRef implementation.
type mockTellOnlyRef[M actor.Message] struct {
	id     string
	tellFn func(ctx context.Context, msg M)
}

// ID returns the mock actor's ID.
func (m *mockTellOnlyRef[M]) ID() string {
	return m.id
}

// Tell sends a message to the mock actor.
func (m *mockTellOnlyRef[M]) Tell(ctx context.Context, msg M) {
	if m.tellFn != nil {
		m.tellFn(ctx, msg)
	}
}

// mockActorRef is a generic mock ActorRef implementation using testify/mock.
type mockActorRef[M actor.Message, R any] struct {
	mock.Mock
	id string
}

// newMockActorRef creates a new mock ActorRef.
func newMockActorRef[M actor.Message, R any](id string) *mockActorRef[M, R] {
	return &mockActorRef[M, R]{id: id}
}

// ID returns the mock actor's ID.
func (m *mockActorRef[M, R]) ID() string {
	return m.id
}

// Tell sends a message without waiting for a response.
func (m *mockActorRef[M, R]) Tell(ctx context.Context, msg M) {
	m.Called(ctx, msg)
}

// Ask sends a message and returns a future for the response.
func (m *mockActorRef[M, R]) Ask(ctx context.Context, msg M) actor.Future[R] {
	args := m.Called(ctx, msg)

	return args.Get(0).(actor.Future[R]) //nolint:forcetypeassert
}

// newMockSelfRef creates a mock self reference that does nothing.
func newMockSelfRef[M actor.Message]() actor.TellOnlyRef[M] {
	return &mockTellOnlyRef[M]{
		id:     "mock-self",
		tellFn: func(_ context.Context, _ M) {},
	}
}

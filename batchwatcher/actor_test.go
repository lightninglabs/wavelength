package batchwatcher

import (
	"context"
	"fmt"
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
		Log:           fn.Some(btclog.Disabled),
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

// createFanOutTree creates a multi-level binary tree whose root fans out into
// two non-leaf branch outputs.
func (h *testHarness) createFanOutTree(t *testing.T) *tree.Tree {
	t.Helper()

	leafAmounts := []btcutil.Amount{
		40_000, 30_000, 20_000, 10_000,
	}
	leaves := make([]tree.LeafDescriptor, 0, len(leafAmounts))
	var totalAmount btcutil.Amount

	for i, amount := range leafAmounts {
		clientKey, _ := testutils.CreateKey(int32(200 + i))
		totalAmount += amount

		leaves = append(leaves, tree.LeafDescriptor{
			CoSignerKey: clientKey,
			Amount:      amount,
			PkScript: []byte{
				0x51, 0x20, byte(i + 1), byte(i + 2),
			},
		})
	}

	batchOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{9, 8, 7, 6},
		Index: 0,
	}
	batchOutput := wire.NewTxOut(int64(totalAmount), []byte{0x51})

	fanOutTree, err := tree.NewTree(
		batchOutpoint, batchOutput, leaves, h.operatorKey,
		[]byte{0xdd, 0xee, 0xff}, 2,
	)
	require.NoError(t, err)
	require.False(t, fanOutTree.Root.IsLeaf())
	require.Len(t, fanOutTree.Root.Children, 2)

	return fanOutTree
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
		BatchID:            batchID,
		Tree:               testTree,
		ConfirmationHeight: 900,
		ExpiryHeight:       1000,
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

	// Create the presigned root transaction. The BatchWatcher only
	// performs progressive unrolling if the observed spend matches the
	// presigned tree transaction for the spent output.
	spendingTx, err := testTree.Root.ToTx()
	require.NoError(t, err)

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

// TestNodeSpendDetectedFanOutRatchetsForward tests tree fan-out.
//
// The test tree has this shape:
//
//	   batch output
//	       |
//	     root tx
//	    /      \
//	 out0      out1
//	  |          |
//	branch0   branch1
//	 /   \      /   \
//	l0   l1    l2   l3
//
// The watcher starts by watching only the confirmed batch output.
//
// Step 1:
// Confirm the root tx spend of the batch output.
// Expected result:
// - the batch output is consumed
// - the root tx is marked spent
// - the watcher fans out to both branch outputs
//
// Step 2:
// Confirm the spend of branch output 0 by branch0.
// Expected result:
// - branch output 0 is consumed
// - branch0 is marked spent
// - the watcher ratchets forward to the two leaf VTXOs under branch0
// - the sibling branch output 1 remains tracked and watched.
func TestNodeSpendDetectedFanOutRatchetsForward(t *testing.T) {
	h := newTestHarness(t)

	batchID := createBatchID(t)
	testTree := h.createFanOutTree(t)

	// Seed the actor state as if RegisterBatch had already run for
	// a confirmed round output. At this point only the single batch
	// output exists on-chain and only that outpoint is being watched.
	treeState := NewBatchTreeState(batchID, testTree, 1000)
	treeState.AddExistingOutput(&Output{
		Outpoint: testTree.BatchOutpoint,
		TxOut:    testTree.BatchOutput,
		TreeNode: testTree.Root,
	})
	treeState.MarkWatched(testTree.BatchOutpoint)
	h.actor.state.RegisterBatch(treeState)

	h.mockChainSource.mock.On("Ask", mock.Anything, mock.Anything).
		Return(completedFuture(&chainsource.RegisterSpendResponse{})).
		Maybe()

	// Spend the batch output with the presigned root tx. This is the first
	// ratchet step: one watched output should turn into two watched branch
	// outputs, but no VTXO leaves should yet be on-chain.
	rootTx, err := testTree.Root.ToTx()
	require.NoError(t, err)

	rootSpend := &NodeSpendDetected{
		BatchID:        batchID,
		SpentOutpoint:  testTree.BatchOutpoint,
		SpendingTx:     rootTx,
		SpendingHeight: 500,
	}
	result := h.actor.Receive(h.t.Context(), rootSpend)
	require.True(t, result.IsOk())

	rootTxID := rootTx.TxHash()
	state := h.actor.state.GetBatch(batchID)
	require.NotNil(t, state)
	require.True(t, state.IsNodeSpent(rootTxID))
	require.Len(t, state.ExistingOutputs, 2)
	require.Len(t, state.VTXOsOnChain, 0)
	require.Len(t, state.WatchedOutpoints, 3)

	// Pick one of the two branch outputs revealed by the root
	// spend. This is still a non-leaf branch, so spending it should
	// ratchet forward again.
	branchNode, ok := testTree.Root.Children[0]
	require.True(t, ok)
	require.False(t, branchNode.IsLeaf())

	// Spend root output 0 with the matching presigned branch tx.
	branchTx, err := branchNode.ToTx()
	require.NoError(t, err)

	branchSpend := &NodeSpendDetected{
		BatchID: batchID,
		SpentOutpoint: wire.OutPoint{
			Hash:  rootTxID,
			Index: 0,
		},
		SpendingTx:     branchTx,
		SpendingHeight: 501,
	}
	result = h.actor.Receive(h.t.Context(), branchSpend)
	require.True(t, result.IsOk())

	branchTxID := branchTx.TxHash()
	state = h.actor.state.GetBatch(batchID)
	require.NotNil(t, state)
	require.True(t, state.IsNodeSpent(branchTxID))

	// After the second ratchet:
	// - the sibling root output (root:1) is still tracked
	// - the spent branch output (root:0) has been replaced by its two leaf
	//   descendants
	// - those two leaves are now recorded as on-chain VTXOs
	require.Len(t, state.ExistingOutputs, 3)
	require.Len(t, state.VTXOsOnChain, 2)

	_, siblingTracked := state.ExistingOutputs[wire.OutPoint{
		Hash:  rootTxID,
		Index: 1,
	}]
	require.True(t, siblingTracked)
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

	// Create the presigned root transaction. For our simple tree, the root
	// node's children are leaves (VTXOs).
	spendingTx, err := testTree.Root.ToTx()
	require.NoError(t, err)

	// Send NodeSpendDetected message.
	msg := &NodeSpendDetected{
		BatchID:        batchID,
		SpentOutpoint:  testTree.BatchOutpoint,
		SpendingTx:     spendingTx,
		SpendingHeight: 500,
	}

	result := h.actor.Receive(h.t.Context(), msg)
	require.True(t, result.IsOk())

	// Our simple tree has a single VTXO leaf (Root IS the leaf), so the
	// batch output spend must produce exactly one on-chain VTXO.
	state := h.actor.state.GetBatch(batchID)
	vtxos := state.GetVTXOsOnChain()
	require.Len(t, vtxos, 1,
		"single-leaf tree root spend must expose one VTXO")

	// The FraudDetector must have received a VTXOOnChainNotification for
	// the revealed leaf.
	require.Len(t, h.mockFraudDetector.receivedMsgs, 1,
		"FraudDetector must receive exactly one VTXO notification")

	fdMsg := h.mockFraudDetector.receivedMsgs[0]
	notification, ok := fdMsg.(*VTXOOnChainNotification)
	require.True(t, ok, "should be VTXOOnChainNotification")
	require.Equal(t, batchID, notification.BatchID)
	require.NotNil(t, notification.VTXOOutput)
}

// TestNodeSpendDetectedUnexpectedSpendNotifiesFraudDetector verifies that a
// confirmed spend which does not match the next presigned branch transaction
// is surfaced as a fraud-detector escalation instead of a transport-level
// error.
func TestNodeSpendDetectedUnexpectedSpendNotifiesFraudDetector(t *testing.T) {
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

	// Create a confirmed transaction that spends the watched output
	// but does not match the presigned tree tx. This exercises the
	// fraud-response boundary without ratcheting the watcher forward.
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

	// The watched output is confirmed spent, so it must be removed
	// from the tracked set even though recovery action is unknown.
	state := h.actor.state.GetBatch(batchID)
	require.NotNil(t, state)
	require.Len(t, state.ExistingOutputs, 0)

	// The watcher should hand off enough structured context for
	// fraud handling to decide whether to broadcast next.
	require.Len(t, h.mockFraudDetector.receivedMsgs, 1)

	fdMsg := h.mockFraudDetector.receivedMsgs[0]
	notification, ok := fdMsg.(*UnexpectedSpendNotification)
	require.True(t, ok, "should be UnexpectedSpendNotification")
	require.Equal(t, batchID, notification.BatchID)
	require.Equal(t, int32(500), notification.SpendingHeight)
	require.Equal(
		t, testTree.BatchOutpoint, notification.TrackedOutput.Outpoint,
	)
	require.Equal(
		t, spendingTx.TxHash(), notification.SpendingTx.TxHash(),
	)

	expectedTxID, err := testTree.Root.TXID()
	require.NoError(t, err)
	require.Equal(t, expectedTxID, notification.ResponseTxID)
	require.Equal(
		t, SpendClassificationMissedBranchTx,
		notification.Classification,
	)

	// Tree-state transition: sweeper should be nudged to refresh.
	require.Len(t, h.mockBatchSweeper.receivedMsgs, 1)
	swMsg := h.mockBatchSweeper.receivedMsgs[0]
	_, ok = swMsg.(*TreeStateChangedNotification)
	require.True(t, ok, "should be TreeStateChangedNotification")
}

// TestLeafOutputsAreWatched verifies that after a branch spend reveals leaf
// VTXO outputs, those outputs are registered with the chain source for spend
// watching — not just tracked as existing outputs.
func TestLeafOutputsAreWatched(t *testing.T) {
	h := newTestHarness(t)

	batchID := createBatchID(t)
	testTree := h.createFanOutTree(t)

	treeState := NewBatchTreeState(batchID, testTree, 1000)
	treeState.AddExistingOutput(&Output{
		Outpoint: testTree.BatchOutpoint,
		TxOut:    testTree.BatchOutput,
		TreeNode: testTree.Root,
	})
	treeState.MarkWatched(testTree.BatchOutpoint)
	h.actor.state.RegisterBatch(treeState)

	h.mockChainSource.mock.On("Ask", mock.Anything, mock.Anything).
		Return(completedFuture(&chainsource.RegisterSpendResponse{})).
		Maybe()

	// Ratchet through root → branch0 to reach the leaf layer.
	rootTx, err := testTree.Root.ToTx()
	require.NoError(t, err)

	result := h.actor.Receive(h.t.Context(), &NodeSpendDetected{
		BatchID:        batchID,
		SpentOutpoint:  testTree.BatchOutpoint,
		SpendingTx:     rootTx,
		SpendingHeight: 500,
	})
	require.True(t, result.IsOk())

	branchNode := testTree.Root.Children[0]
	branchTx, err := branchNode.ToTx()
	require.NoError(t, err)

	rootTxID := rootTx.TxHash()
	result = h.actor.Receive(h.t.Context(), &NodeSpendDetected{
		BatchID: batchID,
		SpentOutpoint: wire.OutPoint{
			Hash:  rootTxID,
			Index: 0,
		},
		SpendingTx:     branchTx,
		SpendingHeight: 501,
	})
	require.True(t, result.IsOk())

	state := h.actor.state.GetBatch(batchID)
	require.NotNil(t, state)

	// After branch0 spend: 2 leaf VTXOs + 1 sibling branch = 3 existing.
	require.Len(t, state.ExistingOutputs, 3)
	require.Len(t, state.VTXOsOnChain, 2)

	// The 2 leaf outputs should now be watched (plus batch root + 2
	// branch outputs from root + sibling branch = 5 total watched).
	require.Len(t, state.WatchedOutpoints, 5)

	// Verify the leaf outpoints specifically are in the watched set.
	branchTxHash := branchTx.TxHash()
	for op := range state.VTXOsOnChain {
		require.True(t, state.IsWatched(op),
			"leaf VTXO %s should be watched", op)
		require.Equal(t, branchTxHash, op.Hash,
			"leaf VTXO should be an output of the branch tx")
	}
}

// TestSingleLeafTreeVTXODetection verifies that when the batch tree consists
// of a single VTXO leaf (Root.IsLeaf() == true), spending the batch output
// reveals the leaf VTXO and the watcher:
//   - records it as a VTXO in ExistingOutputs,
//   - registers a spend watch so downstream leaf classification can run, and
//   - emits a VTXOOnChainNotification to the fraud detector.
//
// Previously, watchNodeOutputs looked up node.Children[i] to decide VTXO-ness.
// For a leaf root Children is empty, so the leaf fell through unmarked and
// unwatched, hiding it from the entire fraud pipeline.
func TestSingleLeafTreeVTXODetection(t *testing.T) {
	h := newTestHarness(t)

	batchID := createBatchID(t)
	testTree := h.createSimpleTree(t)

	// Sanity: createSimpleTree produces a single-leaf tree where the
	// root IS the leaf. If this ever changes the test loses its meaning.
	require.True(t, testTree.Root.IsLeaf(),
		"test premise: root of a single-leaf tree must itself be a leaf")

	treeState := NewBatchTreeState(batchID, testTree, 1000)
	treeState.AddExistingOutput(&Output{
		Outpoint: testTree.BatchOutpoint,
		TxOut:    testTree.BatchOutput,
		TreeNode: testTree.Root,
	})
	treeState.MarkWatched(testTree.BatchOutpoint)
	h.actor.state.RegisterBatch(treeState)

	h.mockChainSource.mock.On("Ask", mock.Anything, mock.Anything).
		Return(completedFuture(&chainsource.RegisterSpendResponse{})).
		Maybe()

	// Broadcast the presigned leaf tx that spends the batch output.
	spendingTx, err := testTree.Root.ToTx()
	require.NoError(t, err)

	result := h.actor.Receive(h.t.Context(), &NodeSpendDetected{
		BatchID:        batchID,
		SpentOutpoint:  testTree.BatchOutpoint,
		SpendingTx:     spendingTx,
		SpendingHeight: 500,
	})
	require.True(t, result.IsOk())

	state := h.actor.state.GetBatch(batchID)
	require.NotNil(t, state)

	// The leaf VTXO must be detected and tracked.
	require.Len(t, state.VTXOsOnChain, 1,
		"single-leaf tree should expose exactly one VTXO")

	// Find the VTXO outpoint and verify it's flagged IsVTXO=true and
	// registered as watched.
	var vtxoOp wire.OutPoint
	for op := range state.VTXOsOnChain {
		vtxoOp = op
		break
	}
	require.True(t, state.IsWatched(vtxoOp),
		"leaf VTXO outpoint must be watched for spend classification")

	output, existing := state.ExistingOutputs[vtxoOp]
	require.True(t, existing)
	require.True(t, output.IsVTXO,
		"leaf VTXO output must be flagged IsVTXO=true")
	require.NotNil(t, output.TreeNode,
		"leaf VTXO must carry a TreeNode for later classification")

	// The fraud detector must have received a VTXOOnChainNotification.
	require.Len(t, h.mockFraudDetector.receivedMsgs, 1,
		"fraud detector must receive VTXOOnChainNotification for the "+
			"revealed leaf")
	notification, ok := h.mockFraudDetector.receivedMsgs[0].(*VTXOOnChainNotification)
	require.True(t, ok, "should be VTXOOnChainNotification")
	require.Equal(t, batchID, notification.BatchID)
	require.Equal(t, vtxoOp, notification.VTXOOutpoint)
}

// leafSpendHarness extends testHarness with recovery mocks and a pre-built
// leaf VTXO output ready for spend testing.
type leafSpendHarness struct {
	*testHarness

	batchID     BatchID
	leafOutput  *Output
	recoveryMgr *mockSpendRecoveryStore
	cpLookup    *mockCheckpointLookup
}

// newLeafSpendHarness creates a harness with a watched leaf VTXO output.
func newLeafSpendHarness(t *testing.T) *leafSpendHarness {
	t.Helper()

	h := newTestHarness(t)
	batchID := createBatchID(t)

	recoveryMgr := &mockSpendRecoveryStore{}
	cpLookup := &mockCheckpointLookup{}

	h.actor.cfg.SpendRecoveryStore = fn.Some[SpendRecoveryStore](
		recoveryMgr,
	)
	h.actor.cfg.CheckpointLookup = fn.Some[CheckpointLookup](cpLookup)

	// Set up a leaf output as if the tree has already been partially
	// unrolled. The leaf TreeNode points at the node that created the
	// output (per the tree-model convention for leaves).
	leafOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{0xaa, 0xbb},
		Index: 0,
	}
	leafOutput := &Output{
		Outpoint: leafOutpoint,
		TxOut:    wire.NewTxOut(50_000, []byte{0x51, 0x20}),
		IsVTXO:   true,
		TreeNode: &tree.Node{},
	}

	treeState := NewBatchTreeState(batchID, nil, 1000)
	treeState.AddExistingOutput(leafOutput)
	treeState.MarkWatched(leafOutpoint)
	h.actor.state.RegisterBatch(treeState)

	return &leafSpendHarness{
		testHarness: h,
		batchID:     batchID,
		leafOutput:  leafOutput,
		recoveryMgr: recoveryMgr,
		cpLookup:    cpLookup,
	}
}

// spendLeaf sends a NodeSpendDetected for the harness's leaf output.
func (lh *leafSpendHarness) spendLeaf(
	t *testing.T) fn.Result[BatchWatcherResp] {

	t.Helper()

	spendingTx := wire.NewMsgTx(3)
	spendingTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: lh.leafOutput.Outpoint,
	})
	spendingTx.AddTxOut(wire.NewTxOut(49_000, []byte{0x00, 0x14}))

	return lh.actor.Receive(t.Context(), &NodeSpendDetected{
		BatchID:        lh.batchID,
		SpentOutpoint:  lh.leafOutput.Outpoint,
		SpendingTx:     spendingTx,
		SpendingHeight: 600,
	})
}

// TestLeafSpendForfeitedVTXO verifies that when a forfeited VTXO leaf is
// spent on-chain, the watcher notifies the fraud detector with the stored
// forfeit transaction.
func TestLeafSpendForfeitedVTXO(t *testing.T) {
	lh := newLeafSpendHarness(t)

	forfeitTx := wire.NewMsgTx(3)
	forfeitTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: lh.leafOutput.Outpoint,
	})
	forfeitTx.AddTxOut(wire.NewTxOut(48_000, []byte{0x00, 0x14}))

	lh.recoveryMgr.getVTXOFn = func(_ context.Context,
		op wire.OutPoint) (*RecoveryVTXO, error) {

		return &RecoveryVTXO{
			Outpoint: op,
			Status:   VTXOStatusForfeited,
		}, nil
	}
	lh.recoveryMgr.getForfeitInfoFn = func(_ context.Context,
		_ wire.OutPoint) (*RecoveryForfeitInfo, error) {

		return &RecoveryForfeitInfo{
			ForfeitTx: forfeitTx,
		}, nil
	}

	result := lh.spendLeaf(t)
	require.True(t, result.IsOk())

	state := lh.actor.state.GetBatch(lh.batchID)
	require.Len(t, state.ExistingOutputs, 0)

	require.Len(t, lh.mockFraudDetector.receivedMsgs, 1)
	fdMsg := lh.mockFraudDetector.receivedMsgs[0]
	notification, ok := fdMsg.(*UnexpectedSpendNotification)
	require.True(t, ok)
	require.Equal(t, forfeitTx.TxHash(),
		notification.ResponseTxID)
	require.Equal(
		t, SpendClassificationForfeitedLeaf,
		notification.Classification,
	)
}

// TestLeafSpendOORCheckpoint verifies that when a live VTXO leaf is spent
// on-chain and an OOR checkpoint exists, the watcher notifies the fraud
// detector with the checkpoint transaction.
func TestLeafSpendOORCheckpoint(t *testing.T) {
	lh := newLeafSpendHarness(t)

	checkpointTx := wire.NewMsgTx(3)
	checkpointTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: lh.leafOutput.Outpoint,
	})
	checkpointTx.AddTxOut(wire.NewTxOut(48_000, []byte{0x00, 0x14}))

	lh.recoveryMgr.getVTXOFn = func(_ context.Context,
		op wire.OutPoint) (*RecoveryVTXO, error) {

		return &RecoveryVTXO{
			Outpoint: op,
			Status:   VTXOStatusLive,
		}, nil
	}
	lh.cpLookup.loadFn = func(_ context.Context,
		_ wire.OutPoint) (*wire.MsgTx, bool, error) {

		return checkpointTx, true, nil
	}

	result := lh.spendLeaf(t)
	require.True(t, result.IsOk())

	require.Len(t, lh.mockFraudDetector.receivedMsgs, 1)
	fdMsg := lh.mockFraudDetector.receivedMsgs[0]
	notification, ok := fdMsg.(*UnexpectedSpendNotification)
	require.True(t, ok)
	require.Equal(t, checkpointTx.TxHash(),
		notification.ResponseTxID)
	require.Equal(
		t, SpendClassificationOORCheckpointLeaf,
		notification.Classification,
	)
}

// TestLeafSpendClientUnroll verifies that when a live VTXO leaf is spent
// and no forfeit or OOR checkpoint exists, the watcher marks the VTXO as
// unrolled_by_client.
func TestLeafSpendClientUnroll(t *testing.T) {
	lh := newLeafSpendHarness(t)

	lh.recoveryMgr.getVTXOFn = func(_ context.Context,
		op wire.OutPoint) (*RecoveryVTXO, error) {

		return &RecoveryVTXO{
			Outpoint: op,
			Status:   VTXOStatusLive,
		}, nil
	}
	lh.cpLookup.loadFn = func(_ context.Context,
		_ wire.OutPoint) (*wire.MsgTx, bool, error) {

		return nil, false, nil
	}

	var markedOutpoint wire.OutPoint
	lh.recoveryMgr.markUnrolledFn = func(_ context.Context,
		op wire.OutPoint) error {

		markedOutpoint = op

		return nil
	}

	result := lh.spendLeaf(t)
	require.True(t, result.IsOk())

	require.Equal(t, lh.leafOutput.Outpoint, markedOutpoint)

	// No fraud detector notification for a clean client unroll.
	require.Len(t, lh.mockFraudDetector.receivedMsgs, 0)

	state := lh.actor.state.GetBatch(lh.batchID)
	require.Len(t, state.ExistingOutputs, 0)
}

// TestLeafSpendAlreadyUnrolled verifies that a second spend notification for
// an already-unrolled VTXO is a no-op.
func TestLeafSpendAlreadyUnrolled(t *testing.T) {
	lh := newLeafSpendHarness(t)

	lh.recoveryMgr.getVTXOFn = func(_ context.Context,
		op wire.OutPoint) (*RecoveryVTXO, error) {

		return &RecoveryVTXO{
			Outpoint: op,
			Status:   VTXOStatusUnrolledByClient,
		}, nil
	}

	result := lh.spendLeaf(t)
	require.True(t, result.IsOk())

	require.Len(t, lh.mockFraudDetector.receivedMsgs, 0)
}

// TestLeafSpendInFlightVTXOWithCheckpointNotifiesFraudDetector verifies that
// when a VTXO in 'in_flight' status (locked by an active round or OOR
// session) is revealed on-chain AND a cosigned checkpoint exists, the
// watcher hands the checkpoint to the fraud detector for broadcast. This
// covers the case where the OOR session reached cosigned state but not yet
// finalized when the client raced with a unilateral exit.
func TestLeafSpendInFlightVTXOWithCheckpointNotifiesFraudDetector(t *testing.T) {
	lh := newLeafSpendHarness(t)

	checkpointTx := wire.NewMsgTx(3)
	checkpointTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: lh.leafOutput.Outpoint,
	})
	checkpointTx.AddTxOut(wire.NewTxOut(47_000, []byte{0x00, 0x14}))

	lh.recoveryMgr.getVTXOFn = func(_ context.Context,
		op wire.OutPoint) (*RecoveryVTXO, error) {

		return &RecoveryVTXO{
			Outpoint: op,
			Status:   VTXOStatusInFlight,
		}, nil
	}
	lh.cpLookup.loadFn = func(_ context.Context,
		_ wire.OutPoint) (*wire.MsgTx, bool, error) {

		return checkpointTx, true, nil
	}

	result := lh.spendLeaf(t)
	require.True(t, result.IsOk(),
		"in_flight leaf spend with checkpoint must classify "+
			"successfully, not hard-error")

	require.Len(t, lh.mockFraudDetector.receivedMsgs, 1)
	notification, ok := lh.mockFraudDetector.receivedMsgs[0].(*UnexpectedSpendNotification)
	require.True(t, ok)
	require.Equal(
		t, SpendClassificationInFlightLeaf,
		notification.Classification,
	)
	require.Equal(t, checkpointTx.TxHash(), notification.ResponseTxID)
}

// TestLeafSpendInFlightVTXOWithoutCheckpointMarksUnrolled verifies that when
// an in_flight VTXO is revealed on-chain and no checkpoint exists (the lock
// was held by a round rather than a cosigned OOR session), the watcher
// marks the VTXO unrolled_by_client so downstream cooperative paths reject
// it. The SQL precondition must accept 'in_flight' as well as 'live'.
func TestLeafSpendInFlightVTXOWithoutCheckpointMarksUnrolled(t *testing.T) {
	lh := newLeafSpendHarness(t)

	var markCalled bool
	lh.recoveryMgr.getVTXOFn = func(_ context.Context,
		op wire.OutPoint) (*RecoveryVTXO, error) {

		return &RecoveryVTXO{
			Outpoint: op,
			Status:   VTXOStatusInFlight,
		}, nil
	}
	lh.cpLookup.loadFn = func(_ context.Context,
		_ wire.OutPoint) (*wire.MsgTx, bool, error) {

		return nil, false, nil
	}
	lh.recoveryMgr.markUnrolledFn = func(_ context.Context,
		_ wire.OutPoint) error {

		markCalled = true

		return nil
	}

	result := lh.spendLeaf(t)
	require.True(t, result.IsOk(),
		"in_flight leaf spend without checkpoint must not hard-error")

	require.True(t, markCalled,
		"MarkVTXOUnrolledByClient must be called so future "+
			"cooperative paths reject the consumed VTXO")
	require.Len(t, lh.mockFraudDetector.receivedMsgs, 0,
		"no checkpoint means no fraud response is required")
}

// TestLeafSpendSpentVTXONotifiesFraudDetector verifies the primary OOR fraud
// response path specified by ARK-04 §"Response to Spent VTXO Unroll":
// when a VTXO whose rounds-DB status is "spent" (because an OOR session
// finalized and wrote status='spent' to the shared vtxos table) is revealed
// on-chain, the watcher MUST forward the stored checkpoint tx to the fraud
// detector so it can race the CSV delay.
func TestLeafSpendSpentVTXONotifiesFraudDetector(t *testing.T) {
	lh := newLeafSpendHarness(t)

	checkpointTx := wire.NewMsgTx(3)
	checkpointTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: lh.leafOutput.Outpoint,
	})
	checkpointTx.AddTxOut(wire.NewTxOut(48_000, []byte{0x00, 0x14}))

	lh.recoveryMgr.getVTXOFn = func(_ context.Context,
		op wire.OutPoint) (*RecoveryVTXO, error) {

		return &RecoveryVTXO{
			Outpoint: op,
			Status:   VTXOStatusSpent,
		}, nil
	}
	lh.cpLookup.loadFn = func(_ context.Context,
		_ wire.OutPoint) (*wire.MsgTx, bool, error) {

		return checkpointTx, true, nil
	}

	result := lh.spendLeaf(t)
	require.True(t, result.IsOk(),
		"spent-VTXO leaf spend with checkpoint must classify "+
			"successfully, not hard-error")

	require.Len(t, lh.mockFraudDetector.receivedMsgs, 1,
		"fraud detector must be notified so it can race the CSV")
	fdMsg := lh.mockFraudDetector.receivedMsgs[0]
	notification, ok := fdMsg.(*UnexpectedSpendNotification)
	require.True(t, ok)
	require.Equal(t, checkpointTx.TxHash(), notification.ResponseTxID,
		"response tx must be the checkpoint that races the client's "+
			"unilateral exit")
	require.Equal(
		t, SpendClassificationSpentLeaf, notification.Classification,
		"classification must be SpentLeaf so the fraud detector "+
			"knows to broadcast the checkpoint",
	)
}

// TestLeafSpendSpentVTXOWithoutCheckpointIsFatal verifies that if the rounds
// VTXO status is 'spent' but no broadcastable checkpoint exists, the watcher
// surfaces this as a hard error: the pairing of "OOR-finalized" (spent) with
// "no checkpoint" is a data-integrity violation the operator MUST know about.
func TestLeafSpendSpentVTXOWithoutCheckpointIsFatal(t *testing.T) {
	lh := newLeafSpendHarness(t)

	lh.recoveryMgr.getVTXOFn = func(_ context.Context,
		op wire.OutPoint) (*RecoveryVTXO, error) {

		return &RecoveryVTXO{
			Outpoint: op,
			Status:   VTXOStatusSpent,
		}, nil
	}
	lh.cpLookup.loadFn = func(_ context.Context,
		_ wire.OutPoint) (*wire.MsgTx, bool, error) {

		return nil, false, nil
	}

	result := lh.spendLeaf(t)
	require.True(t, result.IsErr(),
		"spent-VTXO leaf spend without a stored checkpoint is an "+
			"invariant violation and must surface as an error")

	// No fraud-detector notification: we have nothing actionable to
	// hand off.
	require.Len(t, lh.mockFraudDetector.receivedMsgs, 0)
}

// TestLeafSpendExpiredVTXOIsLegitimate verifies that a leaf spend for a VTXO
// whose rounds-DB status is "expired" is classified as a legitimate race
// outcome per ARK-04 Expired→Unrolled: no fraud response, no error. This
// case occurs when the client wins the race against the operator's sweep
// after the batch reaches expiry.
func TestLeafSpendExpiredVTXOIsLegitimate(t *testing.T) {
	lh := newLeafSpendHarness(t)

	lh.recoveryMgr.getVTXOFn = func(_ context.Context,
		op wire.OutPoint) (*RecoveryVTXO, error) {

		return &RecoveryVTXO{
			Outpoint: op,
			Status:   VTXOStatusExpired,
		}, nil
	}

	result := lh.spendLeaf(t)
	require.True(t, result.IsOk(),
		"expired status is a legitimate race outcome, not an error")

	// No fraud-detector notification: the spec explicitly classifies
	// this as a valid outcome (client unrolled after expiry, before
	// operator sweep).
	require.Len(t, lh.mockFraudDetector.receivedMsgs, 0,
		"expired-leaf spend must NOT notify fraud detector")

	// The output must be removed from tracking since the event was
	// fully handled.
	state := lh.actor.state.GetBatch(lh.batchID)
	require.NotNil(t, state)
	_, stillTracked := state.ExistingOutputs[lh.leafOutput.Outpoint]
	require.False(t, stillTracked,
		"expired-leaf spend must remove the tracked output after "+
			"successful classification")
}

// TestLeafSpendClassificationErrorPreservesTracking verifies that a transient
// error during leaf-spend classification (e.g. a DB lookup failure) does NOT
// silently lose the spend event: the tracked output must remain in the
// in-memory set so the event can be retried or re-classified on restart, and
// no spurious notifications may be emitted.
//
// This guards against the fail-open pattern in which RemoveExistingOutput
// runs before handleLeafSpend — if classification then errors, the event is
// gone from tracking with no downstream signal.
func TestLeafSpendClassificationErrorPreservesTracking(t *testing.T) {
	lh := newLeafSpendHarness(t)

	// Make GetVTXO fail with a transient error. This models a DB
	// connection drop during classification.
	lh.recoveryMgr.getVTXOFn = func(_ context.Context,
		_ wire.OutPoint) (*RecoveryVTXO, error) {

		return nil, fmt.Errorf("transient db error")
	}

	result := lh.spendLeaf(t)
	require.True(t, result.IsErr(),
		"classification failure must surface as an error")

	state := lh.actor.state.GetBatch(lh.batchID)
	require.NotNil(t, state)

	// The output MUST still be tracked so the event can be retried.
	_, stillTracked := state.ExistingOutputs[lh.leafOutput.Outpoint]
	require.True(t, stillTracked,
		"tracked output must NOT be removed when classification "+
			"errors; the spend event would otherwise be lost")

	// No fraud-detector notification: we do not yet know the
	// classification, so we must not hand off bogus data.
	require.Len(t, lh.mockFraudDetector.receivedMsgs, 0,
		"fraud detector must not be notified on classification error")

	// No sweeper nudge either: the tree state has not actually changed.
	require.Len(t, lh.mockBatchSweeper.receivedMsgs, 0,
		"sweeper must not be nudged when classification errors")
}

// TestNodeSpendDetected_ExpiredRootSweepNotification tests that an expired
// batch root spent by a non-branch transaction is treated as the terminal
// whole-batch sweep case and notifies the BatchSweeper.
func TestNodeSpendDetected_ExpiredRootSweepNotification(t *testing.T) {
	h := newTestHarness(t)

	batchID := createBatchID(t)
	testTree := h.createSimpleTree(t)

	treeState := NewBatchTreeState(batchID, testTree, 1000)
	treeState.AddExistingOutput(&Output{
		Outpoint: testTree.BatchOutpoint,
		TxOut:    testTree.BatchOutput,
		TreeNode: testTree.Root,
	})
	treeState.MarkWatched(testTree.BatchOutpoint)
	h.actor.state.RegisterBatch(treeState)

	spendingTx := wire.NewMsgTx(3)
	spendingTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: testTree.BatchOutpoint,
	})
	for _, txOut := range testTree.Root.Outputs {
		spendingTx.AddTxOut(txOut)
	}

	msg := &NodeSpendDetected{
		BatchID:        batchID,
		SpentOutpoint:  testTree.BatchOutpoint,
		SpendingTx:     spendingTx,
		SpendingHeight: 1000,
	}

	result := h.actor.Receive(h.t.Context(), msg)
	require.True(t, result.IsOk())

	var foundBatchSwept bool
	for _, msg := range h.mockBatchSweeper.receivedMsgs {
		if notification, ok := msg.(*BatchSweptNotification); ok {
			require.Equal(t, batchID, notification.BatchID)
			require.NotNil(t, notification.Tree)
			foundBatchSwept = true

			break
		}
	}

	require.True(t, foundBatchSwept,
		"BatchSweeper should receive BatchSweptNotification")
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
func (m *mockTellOnlyRef[M]) Tell(ctx context.Context, msg M) error {
	if m.tellFn != nil {
		m.tellFn(ctx, msg)
	}

	return nil
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
func (m *mockActorRef[M, R]) Tell(ctx context.Context, msg M) error {
	m.Called(ctx, msg)
	return nil
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

// mockSpendRecoveryStore is a test double for the SpendRecoveryStore
// interface used by leaf-spend classification.
type mockSpendRecoveryStore struct {
	getVTXOFn func(
		context.Context, wire.OutPoint,
	) (*RecoveryVTXO, error)

	getForfeitInfoFn func(
		context.Context, wire.OutPoint,
	) (*RecoveryForfeitInfo, error)

	markUnrolledFn func(
		context.Context, wire.OutPoint,
	) error
}

// GetVTXO returns the configured lookup result.
func (m *mockSpendRecoveryStore) GetVTXO(ctx context.Context,
	op wire.OutPoint) (*RecoveryVTXO, error) {

	if m.getVTXOFn != nil {
		return m.getVTXOFn(ctx, op)
	}

	return nil, nil
}

// GetForfeitInfo returns the configured forfeit lookup result.
func (m *mockSpendRecoveryStore) GetForfeitInfo(ctx context.Context,
	op wire.OutPoint) (*RecoveryForfeitInfo, error) {

	if m.getForfeitInfoFn != nil {
		return m.getForfeitInfoFn(ctx, op)
	}

	return nil, nil
}

// MarkVTXOUnrolledByClient records the configured transition call.
func (m *mockSpendRecoveryStore) MarkVTXOUnrolledByClient(
	ctx context.Context, op wire.OutPoint) error {

	if m.markUnrolledFn != nil {
		return m.markUnrolledFn(ctx, op)
	}

	return nil
}

// mockCheckpointLookup is a test double for the CheckpointLookup interface
// used by leaf-spend classification.
type mockCheckpointLookup struct {
	loadFn func(context.Context, wire.OutPoint) (*wire.MsgTx, bool, error)
}

// LoadCheckpointTxByInput returns the configured checkpoint lookup result.
func (m *mockCheckpointLookup) LoadCheckpointTxByInput(
	ctx context.Context, input wire.OutPoint) (*wire.MsgTx, bool, error) {

	if m.loadFn != nil {
		return m.loadFn(ctx, input)
	}

	return nil, false, nil
}

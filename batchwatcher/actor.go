package batchwatcher

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// ActorConfig contains the configuration for creating a new BatchWatcherActor.
type ActorConfig struct {
	// Logger is used for structured logging.
	Logger btclog.Logger

	// ChainSource is a reference to the ChainSource actor for registering
	// spend and block watches.
	ChainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]

	// FraudDetector is a reference to the FraudDetector actor for sending
	// VTXO on-chain notifications. May be None if fraud detection is not
	// enabled.
	FraudDetector fn.Option[actor.TellOnlyRef[FraudDetectorMsg]]

	// BatchSweeper is a reference to the BatchSweeper actor for sending
	// expiry and tree state change notifications. May be None if sweeping
	// is not enabled.
	BatchSweeper fn.Option[actor.TellOnlyRef[BatchSweeperMsg]]

	// SelfRef is a reference to this actor for receiving async callbacks
	// from ChainSource (spend events, block events).
	SelfRef actor.TellOnlyRef[BatchWatcherMsg]
}

// Actor is the BatchWatcherActor that monitors on-chain tree state for all
// registered batches. It uses progressive spend watching to efficiently track
// tree unrolls and notifies child actors when relevant events occur.
type Actor struct {
	cfg *ActorConfig
	log btclog.Logger

	// state holds all batch tree states and expiry indices.
	state *StateStore

	// blockSubscriptionActive indicates whether we have an active block
	// subscription.
	blockSubscriptionActive bool
}

// NewActor creates a new BatchWatcherActor with the provided configuration.
func NewActor(cfg *ActorConfig) *Actor {
	return &Actor{
		cfg:   cfg,
		log:   cfg.Logger,
		state: NewStateStore(),
	}
}

// Receive processes incoming messages for the BatchWatcherActor.
func (a *Actor) Receive(ctx context.Context,
	msg BatchWatcherMsg) fn.Result[BatchWatcherResp] {

	switch m := msg.(type) {
	case *RegisterBatchRequest:
		return a.handleRegisterBatch(ctx, m)

	case *GetTreeStateRequest:
		return a.handleGetTreeState(ctx, m)

	case *NodeSpendDetected:
		return a.handleNodeSpendDetected(ctx, m)

	case *NewBlockReceived:
		return a.handleNewBlockReceived(ctx, m)

	case *UnregisterBatchRequest:
		return a.handleUnregisterBatch(ctx, m)

	default:
		return fn.Err[BatchWatcherResp](
			fmt.Errorf("unknown message type: %T", m),
		)
	}
}

// handleRegisterBatch processes a request to register a new batch for
// monitoring.
func (a *Actor) handleRegisterBatch(ctx context.Context,
	req *RegisterBatchRequest) fn.Result[BatchWatcherResp] {

	a.log.InfoS(ctx, "Registering batch for monitoring",
		"batch_id", req.BatchID,
		"confirmation_height", req.ConfirmationHeight,
		"expiry_height", req.ExpiryHeight)

	// Create the tree state for this batch.
	treeState := NewBatchTreeState(
		req.BatchID, req.Tree, req.ExpiryHeight,
	)

	// Record the batch output as an existing output. This ensures that a
	// batch that never unrolls still has a sweepable operator-controlled
	// output (the commitment transaction output) in its tree state.
	if req.Tree != nil && req.Tree.BatchOutput != nil {
		treeState.AddExistingOutput(&Output{
			Outpoint: req.Tree.BatchOutpoint,
			TxOut:    req.Tree.BatchOutput,

			ConfirmedHeight: req.ConfirmationHeight,

			// The batch output is spent by the root transaction in the
			// presigned tree, so we store Root as the expected spending
			// node for progressive watching and sweeping.
			TreeNode: req.Tree.Root,
		})
	}

	// Register the batch in our state store.
	a.state.RegisterBatch(treeState)

	// Start watching the batch output. This is the root of progressive
	// watching - when this output is spent, we'll watch its children.
	err := a.watchBatchOutput(ctx, req.BatchID, req.Tree)
	if err != nil {
		return fn.Err[BatchWatcherResp](
			fmt.Errorf("failed to watch batch output: %w", err),
		)
	}

	// Ensure we have block subscription for expiry tracking.
	if !a.blockSubscriptionActive {
		err := a.subscribeToBlocks(ctx)
		if err != nil {
			a.log.WarnS(ctx, "Failed to subscribe to blocks",
				err, "batch_id", req.BatchID)
		}
	}

	return fn.Ok[BatchWatcherResp](&RegisterBatchResponse{})
}

// watchBatchOutput registers a spend watch on the batch output (the root of
// the tree).
func (a *Actor) watchBatchOutput(ctx context.Context, batchID BatchID,
	t *tree.Tree) error {

	batchOutpoint := t.BatchOutpoint
	batchState := a.state.GetBatch(batchID)
	if batchState == nil {
		return fmt.Errorf("batch %s not found in state", batchID)
	}

	// Check if already watching.
	if batchState.IsWatched(batchOutpoint) {
		return nil
	}

	// Create a mapped reference that transforms SpendEvent to
	// NodeSpendDetected. We capture the batchID in the closure so we know
	// which batch the spend belongs to.
	mappedRef := chainsource.MapSpendEvent(
		a.cfg.SelfRef,
		func(event chainsource.SpendEvent) BatchWatcherMsg {
			return &NodeSpendDetected{
				BatchID:        batchID,
				SpentOutpoint:  event.Outpoint,
				SpendingTx:     event.SpendingTx,
				SpendingHeight: event.SpendingHeight,
			}
		},
	)

	// Register the spend watch with ChainSource.
	req := &chainsource.RegisterSpendRequest{
		CallerID:    fmt.Sprintf("batchwatcher-%s-batch", batchID),
		Outpoint:    &batchOutpoint,
		PkScript:    t.BatchOutput.PkScript,
		HeightHint:  0,
		NotifyActor: fn.Some(mappedRef),
	}

	future := a.cfg.ChainSource.Ask(ctx, req)
	result := future.Await(ctx)
	if result.IsErr() {
		return fmt.Errorf("failed to register spend watch: %w",
			result.Err())
	}

	// Mark as watched.
	batchState.MarkWatched(batchOutpoint)

	a.log.DebugS(ctx, "Watching batch output",
		"batch_id", batchID,
		"outpoint", batchOutpoint)

	return nil
}

// watchNodeOutputs registers spend watches on the outputs of a tree node.
// This is called when a parent node is spent to continue progressive watching.
func (a *Actor) watchNodeOutputs(ctx context.Context, batchID BatchID,
	node *tree.Node, spendingTx *wire.MsgTx, spendingHeight int32) error {

	batchState := a.state.GetBatch(batchID)
	if batchState == nil {
		return fmt.Errorf("batch %s not found in state", batchID)
	}

	txHash := spendingTx.TxHash()

	// Get anchor script to identify anchor outputs.
	anchorScript := scripts.AnchorOutput().PkScript

	// Iterate through the spending transaction's outputs and register
	// watches for each non-anchor output.
	for i, txOut := range spendingTx.TxOut {
		// Skip anchor outputs (zero value outputs with anchor script).
		if isAnchorOutput(txOut, anchorScript) {
			continue
		}

		outpoint := wire.OutPoint{
			Hash:  txHash,
			Index: uint32(i),
		}

		// Check if already watching.
		if batchState.IsWatched(outpoint) {
			continue
		}

		// Determine if this is a VTXO output (leaf node). If the child
		// doesn't exist in the tree (e.g., anchor outputs), childNode
		// will be nil and isVTXO will be false.
		childNode := node.Children[uint32(i)]
		isVTXO := childNode != nil && childNode.IsLeaf()

		confirmedHeight := uint32(0)
		if spendingHeight >= 0 {
			confirmedHeight = uint32(spendingHeight)
		}

		// Create the output record.
		output := &Output{
			Outpoint:        outpoint,
			TxOut:           txOut,
			ConfirmedHeight: confirmedHeight,
			IsVTXO:          isVTXO,
			TreeNode:        childNode,
			OutputIndex:     uint32(i),
		}

		// Add to existing outputs.
		batchState.AddExistingOutput(output)

		// If this is a VTXO, notify FraudDetector.
		if isVTXO {
			a.notifyVTXOOnChain(ctx, batchID, output)
		}

		// Register spend watch for non-VTXO outputs. VTXOs are
		// terminal - we don't need to watch them further since there
		// are no more children to unroll.
		if !isVTXO && childNode != nil {
			err := a.watchOutput(ctx, batchID, outpoint, txOut)
			if err != nil {
				a.log.WarnS(ctx, "Failed to watch output",
					err,
					"batch_id", batchID,
					"outpoint", outpoint)
			}
		}
	}

	// Notify BatchSweeper that tree state has changed.
	a.notifyTreeStateChanged(ctx, batchID)

	return nil
}

// watchOutput registers a spend watch for a single output.
func (a *Actor) watchOutput(ctx context.Context, batchID BatchID,
	outpoint wire.OutPoint, txOut *wire.TxOut) error {

	batchState := a.state.GetBatch(batchID)
	if batchState == nil {
		return fmt.Errorf("batch %s not found in state", batchID)
	}

	// Create a mapped reference for this specific output.
	mappedRef := chainsource.MapSpendEvent(
		a.cfg.SelfRef,
		func(event chainsource.SpendEvent) BatchWatcherMsg {
			return &NodeSpendDetected{
				BatchID:        batchID,
				SpentOutpoint:  event.Outpoint,
				SpendingTx:     event.SpendingTx,
				SpendingHeight: event.SpendingHeight,
			}
		},
	)

	// Use a unique caller ID for each output.
	callerID := fmt.Sprintf("batchwatcher-%s-%s:%d",
		batchID, outpoint.Hash, outpoint.Index)

	req := &chainsource.RegisterSpendRequest{
		CallerID:    callerID,
		Outpoint:    &outpoint,
		PkScript:    txOut.PkScript,
		HeightHint:  0,
		NotifyActor: fn.Some(mappedRef),
	}

	future := a.cfg.ChainSource.Ask(ctx, req)
	result := future.Await(ctx)
	if result.IsErr() {
		return fmt.Errorf("failed to register spend watch: %w",
			result.Err())
	}

	// Mark as watched.
	batchState.MarkWatched(outpoint)

	a.log.TraceS(ctx, "Watching tree output",
		"batch_id", batchID,
		"outpoint", outpoint)

	return nil
}

// handleGetTreeState returns the current tree state for a batch.
func (a *Actor) handleGetTreeState(_ context.Context,
	req *GetTreeStateRequest) fn.Result[BatchWatcherResp] {

	state := a.state.GetBatch(req.BatchID)
	if state == nil {
		return fn.Ok[BatchWatcherResp](&GetTreeStateResponse{
			Found: false,
		})
	}

	// Return a clone to prevent external modification.
	return fn.Ok[BatchWatcherResp](&GetTreeStateResponse{
		Found:     true,
		TreeState: state.Clone(),
	})
}

// handleNodeSpendDetected processes a notification that a watched output was
// spent. This updates tree state and registers watches on child outputs.
func (a *Actor) handleNodeSpendDetected(ctx context.Context,
	msg *NodeSpendDetected) fn.Result[BatchWatcherResp] {

	a.log.DebugS(ctx, "Node spend detected",
		"batch_id", msg.BatchID,
		"outpoint", msg.SpentOutpoint,
		"spending_tx", msg.SpendingTx.TxHash(),
		"height", msg.SpendingHeight)

	batchState := a.state.GetBatch(msg.BatchID)
	if batchState == nil {
		a.log.WarnS(ctx, "Spend detected for unknown batch", nil,
			"batch_id", msg.BatchID)

		return fn.Ok[BatchWatcherResp](nil)
	}

	// Get the output that was spent.
	spentOutput := batchState.RemoveExistingOutput(msg.SpentOutpoint)

	spendingTxHash := msg.SpendingTx.TxHash()

	// If this was the batch output (root), watch the root node's outputs.
	if batchState.Tree != nil &&
		msg.SpentOutpoint == batchState.Tree.BatchOutpoint {

		// Only treat this as a tree unroll if the spend matches the
		// presigned root transaction. Batch sweeps will spend the batch
		// output with a non-tree transaction, and must not trigger
		// progressive watching.
		expectedTxid, err := batchState.Tree.Root.TXID()
		if err != nil {
			a.log.WarnS(ctx, "Failed to compute root txid", err,
				"batch_id", msg.BatchID)

			return fn.Ok[BatchWatcherResp](nil)
		}

		if spendingTxHash != expectedTxid {
			a.log.InfoS(ctx, "Batch output spent by non-tree tx",
				"batch_id", msg.BatchID,
				"outpoint", msg.SpentOutpoint,
				"spending_tx", spendingTxHash)

			a.notifyTreeStateChanged(ctx, msg.BatchID)

			return fn.Ok[BatchWatcherResp](nil)
		}

		// Mark the presigned node transaction as spent (tree progression).
		batchState.MarkNodeSpent(spendingTxHash)

		err = a.watchNodeOutputs(
			ctx, msg.BatchID, batchState.Tree.Root, msg.SpendingTx,
			msg.SpendingHeight,
		)
		if err != nil {
			a.log.WarnS(ctx, "Failed to watch root outputs",
				err, "batch_id", msg.BatchID)
		}

		return fn.Ok[BatchWatcherResp](nil)
	}

	if spentOutput == nil {
		a.log.WarnS(ctx, "Spend detected for unknown output", nil,
			"batch_id", msg.BatchID,
			"outpoint", msg.SpentOutpoint,
			"spending_tx", spendingTxHash)

		return fn.Ok[BatchWatcherResp](nil)
	}

	if spentOutput.TreeNode == nil {
		a.log.WarnS(ctx, "Spent output missing tree node", nil,
			"batch_id", msg.BatchID,
			"outpoint", msg.SpentOutpoint,
			"spending_tx", spendingTxHash)

		a.notifyTreeStateChanged(ctx, msg.BatchID)

		return fn.Ok[BatchWatcherResp](nil)
	}

	// Otherwise, this was an internal node being spent. Find the
	// corresponding tree node and watch its children.
	expectedTxid, err := spentOutput.TreeNode.TXID()
	if err != nil {
		a.log.WarnS(ctx, "Failed to compute tree node txid", err,
			"batch_id", msg.BatchID,
			"outpoint", msg.SpentOutpoint)

		a.notifyTreeStateChanged(ctx, msg.BatchID)

		return fn.Ok[BatchWatcherResp](nil)
	}

	// If this spend is not the presigned transaction for this output, it
	// is a non-tree spend (typically an operator sweep). Do not unroll.
	if spendingTxHash != expectedTxid {
		a.log.InfoS(ctx, "Tree output spent by non-tree tx",
			"batch_id", msg.BatchID,
			"outpoint", msg.SpentOutpoint,
			"spending_tx", spendingTxHash)

		a.notifyTreeStateChanged(ctx, msg.BatchID)

		return fn.Ok[BatchWatcherResp](nil)
	}

	// Mark the presigned node transaction as spent (tree progression).
	batchState.MarkNodeSpent(spendingTxHash)

	err = a.watchNodeOutputs(
		ctx, msg.BatchID, spentOutput.TreeNode, msg.SpendingTx,
		msg.SpendingHeight,
	)
	if err != nil {
		a.log.WarnS(ctx, "Failed to watch child outputs",
			err, "batch_id", msg.BatchID)
	}

	return fn.Ok[BatchWatcherResp](nil)
}

// handleNewBlockReceived processes a new block notification and checks for
// expired batches.
func (a *Actor) handleNewBlockReceived(ctx context.Context,
	msg *NewBlockReceived) fn.Result[BatchWatcherResp] {

	// Check for batches expiring at this height.
	expiringBatches := a.state.GetBatchesExpiringAt(uint32(msg.Height))
	for _, batchID := range expiringBatches {
		a.log.InfoS(ctx, "Batch expired",
			"batch_id", batchID,
			"height", msg.Height)

		a.notifyBatchExpired(ctx, batchID, uint32(msg.Height))
	}

	return fn.Ok[BatchWatcherResp](nil)
}

// handleUnregisterBatch removes a batch from monitoring.
func (a *Actor) handleUnregisterBatch(ctx context.Context,
	req *UnregisterBatchRequest) fn.Result[BatchWatcherResp] {

	a.log.InfoS(ctx, "Unregistering batch",
		"batch_id", req.BatchID)

	a.state.UnregisterBatch(req.BatchID)

	return fn.Ok[BatchWatcherResp](&UnregisterBatchResponse{})
}

// subscribeToBlocks subscribes to new block notifications from ChainSource.
func (a *Actor) subscribeToBlocks(ctx context.Context) error {
	// Create a mapped reference that transforms BlockEpoch to
	// NewBlockReceived.
	mappedRef := chainsource.MapBlockEpoch(
		a.cfg.SelfRef,
		func(epoch chainsource.BlockEpoch) BatchWatcherMsg {
			return &NewBlockReceived{
				Height: epoch.Height,
			}
		},
	)

	req := &chainsource.SubscribeBlocksRequest{
		CallerID:    "batchwatcher-blocks",
		NotifyActor: fn.Some(mappedRef),
	}

	future := a.cfg.ChainSource.Ask(ctx, req)
	result := future.Await(ctx)
	if result.IsErr() {
		return fmt.Errorf("failed to subscribe to blocks: %w",
			result.Err())
	}

	a.blockSubscriptionActive = true
	a.log.DebugS(ctx, "Subscribed to block notifications")

	return nil
}

// notifyVTXOOnChain sends a notification to the FraudDetector that a VTXO
// has appeared on-chain.
func (a *Actor) notifyVTXOOnChain(ctx context.Context, batchID BatchID,
	output *Output) {

	a.cfg.FraudDetector.WhenSome(func(
		ref actor.TellOnlyRef[FraudDetectorMsg],
	) {

		notification := &VTXOOnChainNotification{
			BatchID:      batchID,
			VTXOOutpoint: output.Outpoint,
			VTXOOutput:   output.TxOut,
		}

		ref.Tell(ctx, notification)

		a.log.DebugS(ctx, "Notified FraudDetector of VTXO on-chain",
			"batch_id", batchID,
			"outpoint", output.Outpoint)
	})
}

// notifyBatchExpired sends a notification to the BatchSweeper that a batch
// has reached its expiry height.
func (a *Actor) notifyBatchExpired(ctx context.Context, batchID BatchID,
	expiryHeight uint32) {

	a.cfg.BatchSweeper.WhenSome(func(
		ref actor.TellOnlyRef[BatchSweeperMsg],
	) {

		notification := &BatchExpiredNotification{
			BatchID:      batchID,
			ExpiryHeight: expiryHeight,
		}

		ref.Tell(ctx, notification)

		a.log.DebugS(ctx, "Notified BatchSweeper of batch expiry",
			"batch_id", batchID,
			"expiry_height", expiryHeight)
	})
}

// notifyTreeStateChanged sends a notification to the BatchSweeper that the
// tree state has changed.
func (a *Actor) notifyTreeStateChanged(ctx context.Context, batchID BatchID) {
	a.cfg.BatchSweeper.WhenSome(func(
		ref actor.TellOnlyRef[BatchSweeperMsg],
	) {

		notification := &TreeStateChangedNotification{
			BatchID: batchID,
		}

		ref.Tell(ctx, notification)

		a.log.TraceS(ctx, "Notified BatchSweeper of tree state change",
			"batch_id", batchID)
	})
}

// isAnchorOutput returns true if the output is an anchor output (zero value
// with the ephemeral anchor script).
func isAnchorOutput(txOut *wire.TxOut, anchorScript []byte) bool {
	if txOut.Value != 0 {
		return false
	}

	return bytes.Equal(txOut.PkScript, anchorScript)
}

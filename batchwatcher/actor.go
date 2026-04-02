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
	// Log is an optional logger. When None, logging is disabled.
	Log fn.Option[btclog.Logger]

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
		log:   cfg.Log.UnwrapOr(btclog.Disabled),
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

			// The batch output is spent by the root transaction in
			// the presigned tree, so we store Root as the expected
			// spending node for progressive watching and sweeping.
			TreeNode: req.Tree.Root,
		})
	}

	// Register the batch in our state store.
	a.state.RegisterBatch(treeState)

	// Start watching the batch output. This is the root of progressive
	// watching - when this output is spent, we'll watch its children.
	err := a.watchBatchOutput(
		ctx, req.BatchID, req.Tree, req.ConfirmationHeight,
	)
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
// the tree). This delegates to watchOutput after checking if already watched.
func (a *Actor) watchBatchOutput(ctx context.Context, batchID BatchID,
	t *tree.Tree, heightHint uint32) error {

	batchState := a.state.GetBatch(batchID)
	if batchState == nil {
		return fmt.Errorf("batch %s not found in state", batchID)
	}

	// Check if already watching.
	if batchState.IsWatched(t.BatchOutpoint) {
		return nil
	}

	err := a.watchOutput(
		ctx, batchID, t.BatchOutpoint, t.BatchOutput, heightHint,
	)
	if err != nil {
		return err
	}

	// Log at Debug level for batch root (watchOutput uses Trace).
	a.log.DebugS(ctx, "Watching batch output",
		"batch_id", batchID,
		"outpoint", t.BatchOutpoint)

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

		// Determine if this is a VTXO output (leaf node) by looking up
		// the corresponding child node in the presigned tree. If the
		// child node is missing, we still track the output on-chain,
		// but we cannot continue progressive watching from it.
		childNode, ok := node.Children[uint32(i)]
		if !ok {
			a.log.WarnS(ctx, "Missing tree child for output", nil,
				"batch_id", batchID,
				"outpoint", outpoint,
				"spending_tx", txHash,
				"output_index", i)
		}
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
			err := a.watchOutput(
				ctx, batchID, outpoint, txOut, confirmedHeight,
			)
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
	outpoint wire.OutPoint, txOut *wire.TxOut, heightHint uint32) error {

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
		HeightHint:  heightHint,
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

	// Get the output that was spent. For the batch root output, this was
	// registered with TreeNode set to Tree.Root in handleRegisterBatch.
	spentOutput := batchState.RemoveExistingOutput(msg.SpentOutpoint)

	spendingTxHash := msg.SpendingTx.TxHash()

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

	// Compute the expected txid for this node's presigned transaction.
	// Only treat this as a tree unroll if the spend matches the presigned
	// transaction. Non-matching spends (e.g., operator sweeps) must not
	// trigger progressive watching.
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
		a.log.InfoS(ctx, "Output spent by non-tree tx",
			"batch_id", msg.BatchID,
			"outpoint", msg.SpentOutpoint,
			"spending_tx", spendingTxHash)

		// If no unspent outputs remain, the batch is fully consumed
		// (the operator swept the root whole). Notify the sweeper
		// with the tree so it can extract VTXO outpoints, then
		// self-unregister so expiry notifications stop.
		if len(batchState.ExistingOutputs) == 0 {
			a.notifyBatchSwept(ctx, msg.BatchID, batchState.Tree)
			a.state.UnregisterBatch(msg.BatchID)

			a.log.InfoS(ctx, "Batch fully swept, unregistered",
				"batch_id", msg.BatchID)

			return fn.Ok[BatchWatcherResp](nil)
		}

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

// handleNewBlockReceived processes a new block notification. It notifies the
// BatchSweeper for batches newly expiring at this height, and re-notifies for
// all already-expired batches to trigger per-block sweep retries with fresh
// fee rates.
func (a *Actor) handleNewBlockReceived(ctx context.Context,
	msg *NewBlockReceived) fn.Result[BatchWatcherResp] {

	// Ignore invalid heights. This can happen during tests or if the chain
	// backend reports an unexpected height.
	if msg.Height < 0 {
		return fn.Ok[BatchWatcherResp](nil)
	}

	currentHeight := uint32(msg.Height)

	// Check for batches expiring at this exact height (new expiries).
	newlyExpiring := a.state.GetBatchesExpiringAt(currentHeight)
	for _, batchID := range newlyExpiring {
		a.log.InfoS(ctx, "Batch expired",
			"batch_id", batchID,
			"height", msg.Height)

		a.notifyBatchExpired(ctx, batchID, currentHeight)
	}

	// Re-notify for all already-expired batches to trigger sweep retries
	// with fresh fee rates. Skip batches that just expired (already
	// notified above).
	alreadyExpired := a.state.GetExpiredBatches(currentHeight)
	for _, batchID := range alreadyExpired {
		// Skip batches that just expired at this height.
		batch := a.state.GetBatch(batchID)
		if batch != nil && batch.ExpiryHeight == currentHeight {
			continue
		}

		a.notifyBatchExpired(ctx, batchID, batch.ExpiryHeight)
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

		if err := ref.Tell(ctx, notification); err != nil {
			a.log.WarnS(ctx, "Failed to notify FraudDetector of VTXO",
				err,
				"batch_id", batchID,
				"outpoint", output.Outpoint,
			)

			return
		}

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

		if err := ref.Tell(ctx, notification); err != nil {
			a.log.WarnS(ctx, "Failed to notify BatchSweeper of expiry",
				err,
				"batch_id", batchID,
				"expiry_height", expiryHeight,
			)

			return
		}

		a.log.DebugS(ctx, "Notified BatchSweeper of batch expiry",
			"batch_id", batchID,
			"expiry_height", expiryHeight)
	})
}

// notifyTreeStateChanged sends a notification to the BatchSweeper that the
// tree state has changed. This allows the sweeper to re-attempt sweeping for
// expired batches when additional operator-controlled outputs appear on-chain
// due to progressive unrolls.
func (a *Actor) notifyTreeStateChanged(ctx context.Context, batchID BatchID) {
	a.cfg.BatchSweeper.WhenSome(func(
		ref actor.TellOnlyRef[BatchSweeperMsg],
	) {

		notification := &TreeStateChangedNotification{
			BatchID: batchID,
		}

		if err := ref.Tell(ctx, notification); err != nil {
			a.log.WarnS(ctx, "Failed to notify BatchSweeper of state",
				err,
				"batch_id", batchID,
			)

			return
		}

		a.log.TraceS(ctx, "Notified BatchSweeper of tree state change",
			"batch_id", batchID)
	})
}

// notifyBatchSwept sends a notification to the BatchSweeper that a batch has
// been fully swept by a non-tree transaction. The tree is included so the
// sweeper can extract VTXO leaf outpoints without querying back.
func (a *Actor) notifyBatchSwept(ctx context.Context, batchID BatchID,
	t *tree.Tree) {

	a.cfg.BatchSweeper.WhenSome(func(
		ref actor.TellOnlyRef[BatchSweeperMsg],
	) {

		notification := &BatchSweptNotification{
			BatchID: batchID,
			Tree:    t,
		}

		if err := ref.Tell(ctx, notification); err != nil {
			a.log.WarnS(ctx, "Failed to notify BatchSweeper of sweep",
				err,
				"batch_id", batchID,
			)

			return
		}

		a.log.DebugS(ctx, "Notified BatchSweeper of batch sweep",
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

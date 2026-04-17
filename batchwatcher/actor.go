package batchwatcher

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
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

	// SpendRecoveryStore provides persisted VTXO and forfeit lookups for
	// recognized client-owned spend handling. May be None until the
	// surrounding subsystem wires recovery support.
	SpendRecoveryStore fn.Option[SpendRecoveryStore]

	// CheckpointLookup provides OOR checkpoint lookup by spent VTXO input.
	// May be None until the OOR subsystem is initialized.
	CheckpointLookup fn.Option[CheckpointLookup]

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

// spendDisposition classifies how the BatchWatcher should react to a confirmed
// spend of a watched output.
type spendDisposition uint8

const (
	spendDispositionBranchTx spendDisposition = iota
	spendDispositionExpiredRootSweep
	spendDispositionLeafSpend
	spendDispositionUnexpected
)

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
	anchorScript := arkscript.AnchorOutput().PkScript

	// A node that is itself a leaf has an empty Children map. In that
	// degenerate (single-leaf tree) or terminal-branch case, every
	// non-anchor output of the spending tx is a VTXO created by this
	// leaf tx. The TreeNode for those outputs is the leaf node itself,
	// so downstream leaf-spend classification can recover the parent
	// context.
	nodeIsLeaf := node.IsLeaf()

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

		// Determine the child-node context for this output. For a
		// non-leaf parent we consult the Children map; for a leaf
		// parent we bind the TreeNode to the leaf itself so the
		// output is still recognizable to leaf-spend classification.
		var (
			childNode *tree.Node
			isVTXO    bool
		)
		switch {
		case nodeIsLeaf:
			childNode = node
			isVTXO = true

		default:
			var ok bool
			childNode, ok = node.Children[uint32(i)]
			if !ok {
				a.log.WarnS(ctx,
					"Missing tree child for output", nil,
					"batch_id", batchID,
					"outpoint", outpoint,
					"spending_tx", txHash,
					"output_index", i)
			}
			isVTXO = childNode != nil && childNode.IsLeaf()
		}

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

		// Register a spend watch for every output that has a tree
		// node. Non-leaf (branch) outputs are watched for tree
		// progression. Leaf VTXO outputs are watched so that
		// client-owned spends (forfeit, OOR, CSV timeout) can be
		// detected and classified by the recovery path.
		if childNode != nil {
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

	spendingTxHash := msg.SpendingTx.TxHash()

	// Get the output that was spent. For the batch root output, this was
	// registered with TreeNode set to Tree.Root in handleRegisterBatch.
	spentOutput := batchState.GetExistingOutput(msg.SpentOutpoint)
	if spentOutput == nil {
		a.log.WarnS(ctx, "Spend detected for unknown output", nil,
			"batch_id", msg.BatchID,
			"outpoint", msg.SpentOutpoint,
			"spending_tx", spendingTxHash)

		return fn.Ok[BatchWatcherResp](nil)
	}

	disposition, expectedTxid, err := a.classifySpend(
		batchState, spentOutput, spendingTxHash, msg.SpendingHeight,
	)
	if err != nil {
		return fn.Err[BatchWatcherResp](err)
	}

	switch disposition {
	case spendDispositionBranchTx:
		// Mark the spent output as consumed before ratcheting forward.
		batchState.RemoveExistingOutput(msg.SpentOutpoint)

		// Mark the presigned node transaction as spent to record
		// normal tree progression.
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

	case spendDispositionExpiredRootSweep:
		// The confirmed batch root was swept after expiry. Remove the
		// tracked output, notify the sweeper, and stop monitoring.
		batchState.RemoveExistingOutput(msg.SpentOutpoint)
		a.notifyBatchSwept(ctx, msg.BatchID, batchState.Tree)
		a.state.UnregisterBatch(msg.BatchID)

		a.log.InfoS(ctx, "Expired batch root swept, unregistered",
			"batch_id", msg.BatchID,
			"spending_tx", spendingTxHash)

		return fn.Ok[BatchWatcherResp](nil)

	case spendDispositionLeafSpend:
		// A watched VTXO leaf was spent on-chain. Classify via the
		// recovery seams FIRST, then mutate in-memory state only if
		// classification succeeds.
		//
		// Rationale: handleLeafSpend performs DB lookups that may
		// fail transiently. If we removed the output first and the
		// classification then errored, the tracked entry would be
		// gone from ExistingOutputs with no downstream notification
		// sent — the spend would be silently lost. Leaving the
		// tracked output in place on error lets the actor retry or
		// re-classify on restart (the batchwatcher StateStore is
		// rebuilt from persistent rounds state on restart anyway).
		err = a.handleLeafSpend(
			ctx, msg.BatchID, spentOutput, msg.SpendingTx,
			msg.SpendingHeight,
		)
		if err != nil {
			return fn.Err[BatchWatcherResp](err)
		}

		batchState.RemoveExistingOutput(msg.SpentOutpoint)
		a.notifyTreeStateChanged(ctx, msg.BatchID)

		return fn.Ok[BatchWatcherResp](nil)

	default:
		// The spend is confirmed but does not match the expected
		// presigned branch transaction. Remove the tracked output
		// and hand off to the fraud detector for classification.
		batchState.RemoveExistingOutput(msg.SpentOutpoint)

		a.notifyUnexpectedSpend(
			ctx, msg.BatchID, spentOutput,
			SpendClassificationMissedBranchTx, expectedTxid,
			msg.SpendingTx, msg.SpendingHeight,
		)
		a.notifyTreeStateChanged(ctx, msg.BatchID)

		a.log.InfoS(ctx, "Unexpected spend handed to fraud detector",
			"batch_id", msg.BatchID,
			"outpoint", msg.SpentOutpoint,
			"expected_tx", expectedTxid,
			"spending_tx", spendingTxHash)

		return fn.Ok[BatchWatcherResp](nil)
	}
}

// classifySpend decides whether a confirmed spend advances the expected tree
// path, is the terminal expired root sweep case, is a leaf VTXO spend, or is
// an unexpected non-branch spend.
//
// The returned hash is the expected presigned tree txid. It is only
// meaningful when the disposition is not an error.
func (a *Actor) classifySpend(batchState *BatchTreeState,
	spentOutput *Output, spendingTxHash chainhash.Hash,
	spendingHeight int32) (spendDisposition, chainhash.Hash, error) {

	if spentOutput == nil {
		return spendDispositionUnexpected, chainhash.Hash{},
			fmt.Errorf("spent output must be provided")
	}

	if spentOutput.TreeNode == nil {
		return spendDispositionUnexpected, chainhash.Hash{},
			fmt.Errorf(
				"tracked output %s has no tree node",
				spentOutput.Outpoint,
			)
	}

	// Leaf VTXO outputs are a special case. The TreeNode txid is
	// the tx that created the VTXO, not the tx that spends it, so
	// the branch-txid comparison is not applicable. Route these to
	// the leaf classification path instead.
	if spentOutput.IsVTXO {
		return spendDispositionLeafSpend, chainhash.Hash{}, nil
	}

	expectedTxid, err := spentOutput.TreeNode.TXID()
	if err != nil {
		return spendDispositionUnexpected, chainhash.Hash{},
			fmt.Errorf(
				"compute expected tree txid for %s: %w",
				spentOutput.Outpoint, err,
			)
	}

	if spendingTxHash == expectedTxid {
		return spendDispositionBranchTx, expectedTxid, nil
	}

	// Expired-root sweep: only the original batch output qualifies.
	// Mid-tree branch outputs exposed by partial unrolls are not
	// checked here; the sweeper handles those through a separate
	// path, so they fall through to spendDispositionUnexpected.
	if batchState.Tree != nil &&
		spentOutput.Outpoint == batchState.Tree.BatchOutpoint &&
		spendingHeight >= int32(batchState.ExpiryHeight) {

		return spendDispositionExpiredRootSweep, expectedTxid, nil
	}

	return spendDispositionUnexpected, expectedTxid, nil
}

// handleLeafSpend classifies a confirmed spend of a watched VTXO leaf output
// using the persisted recovery state. The classification is:
//
//   - forfeit: the VTXO was already forfeited → notify fraud detector with
//     the stored forfeit tx so it can be broadcast.
//   - OOR checkpoint: the VTXO was spent via OOR → notify fraud detector
//     with the stored checkpoint tx so it can be broadcast.
//   - live, no forfeit/OOR: the client revealed the VTXO on-chain via its
//     CSV timeout path → mark the VTXO as unrolled_by_client.
//   - already unrolled: no-op (idempotent).
//   - unknown VTXO or unexpected status: error.
func (a *Actor) handleLeafSpend(ctx context.Context, batchID BatchID,
	spentOutput *Output, spendingTx *wire.MsgTx,
	spendingHeight int32) error {

	store, err := a.cfg.SpendRecoveryStore.UnwrapOrErr(
		fmt.Errorf("spend recovery store not configured"),
	)
	if err != nil {
		return err
	}

	vtxo, err := store.GetVTXO(ctx, spentOutput.Outpoint)
	if err != nil {
		return fmt.Errorf("leaf spend lookup: %w", err)
	}
	if vtxo == nil {
		return fmt.Errorf(
			"leaf spend for unknown VTXO %s in batch %s",
			spentOutput.Outpoint, batchID,
		)
	}

	spendingTxHash := spendingTx.TxHash()

	switch vtxo.Status {
	case VTXOStatusForfeited:
		info, err := store.GetForfeitInfo(ctx, spentOutput.Outpoint)
		if err != nil {
			return fmt.Errorf("load forfeit info: %w", err)
		}
		if info == nil || info.ForfeitTx == nil {
			return fmt.Errorf(
				"VTXO %s marked forfeited but no forfeit "+
					"tx found", spentOutput.Outpoint,
			)
		}

		a.notifyUnexpectedSpend(
			ctx, batchID, spentOutput,
			SpendClassificationForfeitedLeaf,
			info.ForfeitTx.TxHash(), spendingTx,
			spendingHeight,
		)

		a.log.InfoS(ctx,
			"Leaf VTXO spent, forfeit tx available",
			"batch_id", batchID,
			"outpoint", spentOutput.Outpoint,
			"forfeit_tx", info.ForfeitTx.TxHash(),
			"spending_tx", spendingTxHash)

		return nil

	case VTXOStatusLive:
		// Check for an OOR checkpoint before marking as client-
		// unrolled: the VTXO might still be live in the rounds DB
		// even though OOR finalization has completed.
		checkpoint, err := a.cfg.CheckpointLookup.UnwrapOrErr(
			fmt.Errorf("checkpoint lookup not configured"),
		)
		if err != nil {
			return err
		}

		cpTx, found, err := checkpoint.LoadCheckpointTxByInput(
			ctx, spentOutput.Outpoint,
		)
		if err != nil {
			return fmt.Errorf("load checkpoint: %w", err)
		}
		if found {
			a.notifyUnexpectedSpend(
				ctx, batchID, spentOutput,
				SpendClassificationOORCheckpointLeaf,
				cpTx.TxHash(), spendingTx,
				spendingHeight,
			)

			a.log.InfoS(ctx,
				"Leaf VTXO spent, OOR checkpoint available",
				"batch_id", batchID,
				"outpoint", spentOutput.Outpoint,
				"checkpoint_tx", cpTx.TxHash(),
				"spending_tx", spendingTxHash)

			return nil
		}

		// No forfeit, no OOR: the client used its CSV timeout
		// path. Mark the VTXO so future cooperative paths reject
		// it.
		err = store.MarkVTXOUnrolledByClient(
			ctx, spentOutput.Outpoint,
		)
		if err != nil {
			return fmt.Errorf(
				"mark vtxo unrolled_by_client: %w", err,
			)
		}

		a.log.InfoS(ctx, "Leaf VTXO unrolled by client",
			"batch_id", batchID,
			"outpoint", spentOutput.Outpoint,
			"spending_tx", spendingTxHash)

		return nil

	case VTXOStatusUnrolledByClient:
		a.log.DebugS(ctx,
			"Leaf VTXO already marked unrolled_by_client",
			"batch_id", batchID,
			"outpoint", spentOutput.Outpoint)

		return nil

	case VTXOStatusExpired:
		// Per ARK-04 Expired → Unrolled: the client won the race
		// against the operator's sweep after expiry. This is a
		// legitimate outcome, not fraud. Log and return cleanly.
		a.log.InfoS(ctx,
			"Leaf VTXO spent after expiry — legitimate race win",
			"batch_id", batchID,
			"outpoint", spentOutput.Outpoint,
			"spending_tx", spendingTxHash)

		return nil

	case VTXOStatusSpent:
		// Per ARK-04 §"Response to Spent VTXO Unroll": an OOR session
		// finalized, so the shared vtxos row is 'spent'. The client
		// then revealed the same VTXO on-chain via unilateral exit.
		// The operator MUST broadcast the stored checkpoint before
		// the CSV delay expires; otherwise the OOR recipient loses
		// their preconfirmed VTXO and the operator loses the
		// transferred value.
		checkpoint, err := a.cfg.CheckpointLookup.UnwrapOrErr(
			fmt.Errorf("checkpoint lookup not configured"),
		)
		if err != nil {
			return err
		}

		cpTx, found, err := checkpoint.LoadCheckpointTxByInput(
			ctx, spentOutput.Outpoint,
		)
		if err != nil {
			return fmt.Errorf("load checkpoint: %w", err)
		}
		if !found {
			// Invariant violation: status='spent' means an OOR
			// session reached awaiting_notify or finalized, so a
			// checkpoint MUST exist. Surfacing this loudly helps
			// the operator catch DB corruption or a missed
			// OOR.ApplyFinalizeAndMaterialize write.
			return fmt.Errorf(
				"VTXO %s marked spent but no broadcastable "+
					"checkpoint found", spentOutput.Outpoint,
			)
		}

		a.notifyUnexpectedSpend(
			ctx, batchID, spentOutput,
			SpendClassificationSpentLeaf,
			cpTx.TxHash(), spendingTx,
			spendingHeight,
		)

		a.log.InfoS(ctx,
			"Spent VTXO revealed on-chain — broadcasting "+
				"checkpoint for fraud response",
			"batch_id", batchID,
			"outpoint", spentOutput.Outpoint,
			"checkpoint_tx", cpTx.TxHash(),
			"spending_tx", spendingTxHash)

		return nil

	default:
		return fmt.Errorf(
			"leaf VTXO %s has unexpected status %q in batch %s",
			spentOutput.Outpoint, vtxo.Status, batchID,
		)
	}
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

// notifyUnexpectedSpend sends a notification to the FraudDetector that a
// watched output was spent by a transaction other than the expected presigned
// branch. The fraud detector uses classification to choose the response flow.
func (a *Actor) notifyUnexpectedSpend(ctx context.Context, batchID BatchID,
	trackedOutput *Output, classification SpendClassification,
	responseTxID chainhash.Hash, spendingTx *wire.MsgTx,
	spendingHeight int32) {

	a.cfg.FraudDetector.WhenSome(func(
		ref actor.TellOnlyRef[FraudDetectorMsg],
	) {

		notification := &UnexpectedSpendNotification{
			BatchID:        batchID,
			TrackedOutput:  trackedOutput,
			Classification: classification,
			ResponseTxID:   responseTxID,
			SpendingTx:     spendingTx,
			SpendingHeight: spendingHeight,
		}

		if err := ref.Tell(ctx, notification); err != nil {
			a.log.WarnS(ctx,
				"Failed to notify FraudDetector of "+
					"unexpected spend",
				err,
				"batch_id", batchID,
				"outpoint", trackedOutput.Outpoint,
			)

			return
		}

		a.log.DebugS(ctx,
			"Notified FraudDetector of unexpected spend",
			"batch_id", batchID,
			"outpoint", trackedOutput.Outpoint,
			"spending_tx", spendingTx.TxHash())
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

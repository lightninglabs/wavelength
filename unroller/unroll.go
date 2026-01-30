package unroller

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// handleUnrollRequest initiates unroll for the requested VTXOs.
func (a *UnrollerActor) handleUnrollRequest(
	ctx context.Context, req *UnrollRequest,
) fn.Result[UnrollerResp] {

	a.mu.Lock()
	defer a.mu.Unlock()

	// For now, handle single VTXO unroll. Multi-VTXO support can be added
	// later.
	if len(req.TargetVTXOs) == 0 {
		return fn.Err[UnrollerResp](
			fmt.Errorf("no target VTXOs specified"),
		)
	}

	outpoint := req.TargetVTXOs[0]
	outpointKey := outpoint.String()

	// Check if already unrolling this VTXO.
	if _, exists := a.activeUnrolls[outpointKey]; exists {
		a.cfg.Logger.WarnS(ctx, "Unroll already in progress", nil,
			slog.String("outpoint", outpointKey))

		return fn.Ok[UnrollerResp](&UnrollStartedResp{})
	}

	// Fetch VTXO descriptor from store.
	vtxoDesc, err := a.cfg.Store.GetVTXO(ctx, outpoint)
	if err != nil {
		a.cfg.Logger.ErrorS(ctx, "Failed to fetch VTXO", err,
			slog.String("outpoint", outpointKey))

		return fn.Err[UnrollerResp](
			fmt.Errorf("fetch VTXO: %w", err),
		)
	}

	a.cfg.Logger.InfoS(ctx, "Starting VTXO unroll",
		slog.String("outpoint", outpointKey))

	// Extract level-ordered transactions from tree.
	levelOrder := extractLevelOrder(vtxoDesc.TreePath)

	// Create unroll state.
	state := &UnrollState{
		VTXOOutpoint:   outpoint,
		VTXO:           vtxoDesc,
		LevelOrder:     levelOrder,
		CurrentLevel:   0,
		BroadcastTxids: make(map[chainhash.Hash]bool),
		ConfirmedTxids: make(map[chainhash.Hash]ConfirmationInfo),
		Status:         UnrollStatusPending,
	}

	// Persist state.
	if err := a.cfg.Store.SaveUnrollState(ctx, state); err != nil {
		a.cfg.Logger.ErrorS(ctx, "Failed to save unroll state", err,
			slog.String("outpoint", outpointKey))

		return fn.Err[UnrollerResp](err)
	}

	a.activeUnrolls[outpointKey] = state

	// Start broadcasting from level 1. Level 0 is the root/pool transaction
	// which is already confirmed on-chain.
	a.broadcastLevel(ctx, state, 1)

	return fn.Ok[UnrollerResp](&UnrollStartedResp{})
}

// broadcastLevel broadcasts all transactions at the specified level.
func (a *UnrollerActor) broadcastLevel(
	ctx context.Context, state *UnrollState, level int,
) {

	if level >= len(state.LevelOrder) {
		// All levels complete, transition to CSV wait or completion.
		a.handleAllLevelsComplete(ctx, state)
		return
	}

	levelTxids := state.LevelOrder[level]

	a.cfg.Logger.InfoS(ctx, "Broadcasting tree level",
		slog.String("vtxo", state.VTXOOutpoint.String()),
		slog.Int("level", level),
		slog.Int("tx_count", len(levelTxids.Txids)))

	state.CurrentLevel = level
	state.Status = UnrollStatusBroadcasting

	for i, node := range levelTxids.Nodes {
		if i >= len(levelTxids.Txids) {
			break
		}

		txid := levelTxids.Txids[i]

		// Convert to signed transaction.
		signedTx, err := node.ToSignedTx()
		if err != nil {
			a.cfg.Logger.ErrorS(ctx, "Failed to construct tx", err,
				slog.String("txid", txid.String()))

			continue
		}

		// Broadcast transaction.
		broadReq := &chainsource.BroadcastTxRequest{
			Tx: signedTx,
			Label: fmt.Sprintf(
				"unroll-%s-L%d", state.VTXOOutpoint, level,
			),
		}

		future := a.cfg.ChainSource.Ask(ctx, broadReq)
		result := future.Await(ctx)

		if result.IsErr() {
			a.cfg.Logger.WarnS(
				ctx, "Broadcast failed", result.Err(),
				slog.String("txid", txid.String()),
				slog.Int("level", level))

			// Continue with other transactions, will retry later.
			continue
		}

		// Mark as broadcast.
		state.BroadcastTxids[txid] = true

		// Register for confirmation monitoring.
		a.registerConfirmation(ctx, state, txid)
	}

	// Subscribe to block epochs for CSV tracking.
	a.subscribeBlockEpochs(ctx, state)

	// Persist updated state.
	if err := a.cfg.Store.UpdateUnrollState(ctx, state); err != nil {
		a.cfg.Logger.ErrorS(ctx, "Failed to update unroll state", err,
			slog.String("vtxo", state.VTXOOutpoint.String()))
	}
}

// registerConfirmation subscribes to confirmation events for a transaction.
func (a *UnrollerActor) registerConfirmation(
	ctx context.Context, state *UnrollState, txid chainhash.Hash,
) {

	// Create mapped ref to convert confirmation events to our message type.
	mappedRef := actor.NewMapInputRef(
		a.cfg.SelfRef,
		func(evt chainsource.ConfirmationEvent) UnrollerMsg {
			return &ConfirmationEvent{
				Txid:        evt.Txid,
				BlockHeight: evt.BlockHeight,
				BlockHash:   evt.BlockHash,
			}
		},
	)

	// Cast mapped ref to TellOnlyRef for registration.
	notifyRef := actor.TellOnlyRef[chainsource.ConfirmationEvent](mappedRef)

	callerID := fmt.Sprintf("unroll-%s-%s", state.VTXOOutpoint, txid)
	confReq := &chainsource.RegisterConfRequest{
		CallerID:    callerID,
		Txid:        &txid,
		TargetConfs: 1, // V3 transactions need only 1 conf.
		NotifyActor: fn.Some(notifyRef),
	}

	// Use background context for long-lived registration.
	a.cfg.ChainSource.Tell(context.Background(), confReq)
}

// subscribeBlockEpochs subscribes to block epoch events for CSV tracking.
func (a *UnrollerActor) subscribeBlockEpochs(
	ctx context.Context, state *UnrollState,
) {

	// Create mapped ref to convert block epochs to our message type.
	mappedRef := actor.NewMapInputRef(
		a.cfg.SelfRef,
		func(epoch chainsource.BlockEpoch) UnrollerMsg {
			return &BlockEpochEvent{
				Height: epoch.Height,
				Hash:   epoch.Hash,
			}
		},
	)

	// Cast mapped ref to TellOnlyRef for subscription.
	notifyRef := actor.TellOnlyRef[chainsource.BlockEpoch](mappedRef)

	subReq := &chainsource.SubscribeBlocksRequest{
		CallerID: fmt.Sprintf(
			"csv-%s-L%d", state.VTXOOutpoint, state.CurrentLevel,
		),
		NotifyActor: fn.Some(notifyRef),
	}

	a.cfg.ChainSource.Tell(context.Background(), subReq)
}

// handleConfirmation processes confirmation events.
func (a *UnrollerActor) handleConfirmation(
	ctx context.Context, evt *ConfirmationEvent,
) fn.Result[UnrollerResp] {

	a.mu.Lock()
	defer a.mu.Unlock()

	// Find which unroll this confirmation belongs to.
	state := a.findUnrollByTxid(evt.Txid)
	if state == nil {
		// Might be from a previous unroll that completed.
		return fn.Ok[UnrollerResp](&UnrollStartedResp{})
	}

	// Mark as confirmed.
	state.ConfirmedTxids[evt.Txid] = ConfirmationInfo{
		Height:    evt.BlockHeight,
		BlockHash: evt.BlockHash,
	}

	a.cfg.Logger.InfoS(ctx, "Transaction confirmed",
		slog.String("txid", evt.Txid.String()),
		slog.Int("height", int(evt.BlockHeight)),
		slog.Int("level", state.CurrentLevel))

	// Check if entire level is confirmed.
	if a.isLevelConfirmed(state, state.CurrentLevel) {
		a.cfg.Logger.InfoS(ctx, "Level fully confirmed",
			slog.Int("level", state.CurrentLevel))

		// Check if CSV delay required before next level.
		if state.CurrentLevel < len(state.LevelOrder)-1 {
			// Not final level, CSV delay will be checked in block
			// epoch handler.
			a.cfg.Logger.InfoS(ctx, "Waiting for CSV delay",
				slog.Int("level", state.CurrentLevel),
				slog.Int(
					"csv_blocks",
					int(state.VTXO.Expiry),
				))
		} else {
			// Final level confirmed, handle completion.
			a.handleAllLevelsComplete(ctx, state)
		}
	}

	// Persist updated state.
	if err := a.cfg.Store.UpdateUnrollState(ctx, state); err != nil {
		a.cfg.Logger.ErrorS(ctx, "Failed to update unroll state", err,
			slog.String("vtxo", state.VTXOOutpoint.String()))
	}

	return fn.Ok[UnrollerResp](&UnrollStartedResp{})
}

// isLevelConfirmed checks if all transactions at a level are confirmed.
func (a *UnrollerActor) isLevelConfirmed(
	state *UnrollState, level int,
) bool {

	levelTxids := state.LevelOrder[level]

	for _, txid := range levelTxids.Txids {
		if _, confirmed := state.ConfirmedTxids[txid]; !confirmed {
			return false
		}
	}

	return true
}

// handleBlockEpoch checks if CSV delay satisfied for pending levels or sweep.
func (a *UnrollerActor) handleBlockEpoch(
	ctx context.Context, evt *BlockEpochEvent,
) fn.Result[UnrollerResp] {

	a.mu.Lock()
	defer a.mu.Unlock()

	for _, state := range a.activeUnrolls {
		csvDelay := int32(state.VTXO.Expiry)

		switch state.Status {
		case UnrollStatusBroadcasting:
			// Check if level confirmed and CSV delay passed.
			if !a.isLevelConfirmed(state, state.CurrentLevel) {
				continue
			}

			// Get confirmation height of current level.
			currentLevelHeight := a.getLevelConfirmHeight(
				state, state.CurrentLevel,
			)

			if evt.Height >= currentLevelHeight+csvDelay {
				// CSV delay satisfied, broadcast next level.
				nextLevel := state.CurrentLevel + 1

				a.cfg.Logger.InfoS(
					ctx, "CSV delay satisfied for level",
					slog.String("vtxo", state.VTXOOutpoint.String()),
					slog.Int("level", state.CurrentLevel),
					slog.Int("current_height", int(evt.Height)),
					slog.Int(
						"confirm_height",
						int(currentLevelHeight),
					))

				a.broadcastLevel(ctx, state, nextLevel)
			}

		case UnrollStatusAwaitingCSV:
			// Check if CSV delay satisfied to complete unroll.
			if evt.Height >= state.LeafConfirmHeight+csvDelay {
				a.cfg.Logger.InfoS(
					ctx, "CSV satisfied, completing unroll",
					slog.String("vtxo", state.VTXOOutpoint.String()),
					slog.Int("height", int(evt.Height)),
					slog.Int("leaf_height", int(state.LeafConfirmHeight)))

				a.handleCSVComplete(ctx, state)
			}
		}
	}

	return fn.Ok[UnrollerResp](&UnrollStartedResp{})
}

// getLevelConfirmHeight returns the confirmation height of a level.
func (a *UnrollerActor) getLevelConfirmHeight(
	state *UnrollState, level int,
) int32 {

	levelTxids := state.LevelOrder[level]
	if len(levelTxids.Txids) == 0 {
		return 0
	}

	// Use first txid's confirmation height (all should be similar).
	firstTxid := levelTxids.Txids[0]
	if info, exists := state.ConfirmedTxids[firstTxid]; exists {
		return info.Height
	}

	return 0
}

// handleAllLevelsComplete transitions to CSV wait state.
func (a *UnrollerActor) handleAllLevelsComplete(
	ctx context.Context, state *UnrollState,
) {

	// Final leaf transaction has confirmed.
	finalLevel := len(state.LevelOrder) - 1
	state.LeafConfirmHeight = a.getLevelConfirmHeight(state, finalLevel)

	// Transition to awaiting CSV state.
	state.Status = UnrollStatusAwaitingCSV

	a.cfg.Logger.InfoS(ctx, "All tree levels confirmed, awaiting CSV",
		slog.String("vtxo", state.VTXOOutpoint.String()),
		slog.Int("leaf_height", int(state.LeafConfirmHeight)),
		slog.Int("csv_delay", int(state.VTXO.Expiry)))

	if err := a.cfg.Store.UpdateUnrollState(ctx, state); err != nil {
		a.cfg.Logger.ErrorS(ctx, "Failed to update unroll state", err,
			slog.String("vtxo", state.VTXOOutpoint.String()))
	}
}

// handleCSVComplete marks the unroll as complete after CSV delay is satisfied.
// The VTXO output is now spendable on-chain. A separate sweeper actor will
// handle actually spending the output.
func (a *UnrollerActor) handleCSVComplete(
	ctx context.Context, state *UnrollState,
) {

	a.cfg.Logger.InfoS(ctx, "CSV delay satisfied, unroll complete",
		slog.String("vtxo", state.VTXOOutpoint.String()))

	state.Status = UnrollStatusComplete

	if err := a.cfg.Store.UpdateUnrollState(ctx, state); err != nil {
		a.cfg.Logger.ErrorS(ctx, "Failed to update unroll state", err,
			slog.String("vtxo", state.VTXOOutpoint.String()))
	}

	// Remove from active tracking. The VTXO is now ready for sweeping
	// by a dedicated sweeper actor.
	delete(a.activeUnrolls, state.VTXOOutpoint.String())
}

// findUnrollByTxid finds which unroll a txid belongs to.
func (a *UnrollerActor) findUnrollByTxid(
	txid chainhash.Hash,
) *UnrollState {

	for _, state := range a.activeUnrolls {
		for _, level := range state.LevelOrder {
			for _, levelTxid := range level.Txids {
				if levelTxid == txid {
					return state
				}
			}
		}
	}

	return nil
}

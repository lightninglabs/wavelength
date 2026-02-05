package unroller

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
)

// handleUnrollRequest initiates unroll for the requested VTXOs.
func (a *UnrollerActor) handleUnrollRequest(
	ctx context.Context, req *UnrollRequest,
) fn.Result[UnrollerResp] {

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
		a.cfg.Logger.InfoS(ctx, "Unroll already in progress",
			slog.String("outpoint", outpointKey))

		return fn.Ok[UnrollerResp](&UnrollStartedResp{})
	}

	// Fetch VTXO descriptor from store.
	vtxoDesc, err := a.cfg.Store.GetVTXO(ctx, outpoint)
	if err != nil {
		a.cfg.Logger.WarnS(ctx, "Failed to fetch VTXO", err,
			slog.String("outpoint", outpointKey))

		return fn.Err[UnrollerResp](
			fmt.Errorf("fetch VTXO: %w", err),
		)
	}

	a.cfg.Logger.InfoS(ctx, "Starting VTXO unroll",
		slog.String("outpoint", outpointKey))

	// Extract level-ordered transactions from tree.
	levelOrder, err := extractLevelOrder(vtxoDesc.TreePath)
	if err != nil {
		a.cfg.Logger.WarnS(ctx, "Failed to extract tree levels", err,
			slog.String("outpoint", outpointKey))

		return fn.Err[UnrollerResp](
			fmt.Errorf("extract tree levels: %w", err),
		)
	}

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
		a.cfg.Logger.WarnS(ctx, "Failed to save unroll state", err,
			slog.String("outpoint", outpointKey))

		return fn.Err[UnrollerResp](err)
	}

	a.activeUnrolls[outpointKey] = state

	// Index all txids for O(1) confirmation lookup.
	a.indexUnrollTxids(state)

	// Start broadcasting from level 0. The tree root (level 0) is a
	// virtual transaction that spends the batch outpoint and needs to
	// be broadcast to initiate the unilateral exit.
	a.broadcastLevel(ctx, state, 0)

	return fn.Ok[UnrollerResp](&UnrollStartedResp{})
}

// broadcastLevel broadcasts all transactions at the specified level
// using 1P1C package relay. V3 transactions with ephemeral anchors
// (0-sat P2A outputs) cannot be broadcast standalone — they must be
// submitted as parent+child packages via bitcoind's submitpackage
// RPC with a fee-paying CPFP child that spends the anchor output.
func (a *UnrollerActor) broadcastLevel(
	ctx context.Context, state *UnrollState, level int,
) {

	if level >= len(state.LevelOrder) {
		// All levels complete, transition to CSV wait or
		// completion.
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

	// Query the current best block height for use as height
	// hint when registering confirmation notifications. lnd
	// requires a height hint > 0.
	heightHint, err := a.getBestHeight(ctx)
	if err != nil {
		a.failUnroll(
			ctx, state,
			fmt.Errorf("get best height: %w", err),
		)

		return
	}

	// Get fee rate once per level so all transactions in the
	// same level use a consistent fee rate.
	feeRate, err := a.getFeeRate(ctx)
	if err != nil {
		a.failUnroll(
			ctx, state,
			fmt.Errorf("get fee rate: %w", err),
		)

		return
	}

	for i, node := range levelTxids.Nodes {
		if i >= len(levelTxids.Txids) {
			break
		}

		txid := levelTxids.Txids[i]

		// Convert to signed transaction. A failure here means
		// the tree data is corrupt or incomplete (missing
		// signature), which cannot be retried.
		signedTx, err := node.ToSignedTx()
		if err != nil {
			a.failUnroll(
				ctx, state,
				fmt.Errorf("construct signed tx %s: %w",
					txid, err),
			)

			return
		}

		// The anchor output is always the last output in the
		// node's output list (branch nodes: [child1, child2,
		// ..., anchor], leaf nodes: [vtxo, anchor]).
		anchorIdx := uint32(len(signedTx.TxOut) - 1)
		anchorOut := signedTx.TxOut[anchorIdx]

		// Compute fee for entire package: parent vsize + child
		// vsize estimate, multiplied by the per-vbyte fee rate.
		parentWeight := estimateWeight(signedTx)
		parentVSize := (parentWeight + 3) / 4
		totalFee := feeRate *
			btcutil.Amount(parentVSize+childVSizeEstimate)

		if totalFee < 1 {
			totalFee = 1
		}

		// Select a confirmed wallet UTXO for the fee input.
		// V3 rules require the child to have at most one
		// unconfirmed input (the anchor), so the fee input
		// must be confirmed.
		feeInput, err := selectFeeUTXO(
			ctx, a.cfg.WalletKit, totalFee,
		)
		if err != nil {
			a.failUnroll(
				ctx, state,
				fmt.Errorf("select fee UTXO for %s: %w",
					txid, err),
			)

			return
		}

		// Get a change address from the wallet.
		changeAddr, err := a.cfg.WalletKit.NextAddr(
			ctx, "",
			walletrpc.AddressType_TAPROOT_PUBKEY, false,
		)
		if err != nil {
			a.failUnroll(
				ctx, state,
				fmt.Errorf("get change address: %w", err),
			)

			return
		}

		changePkScript, err := txscript.PayToAddrScript(
			changeAddr,
		)
		if err != nil {
			a.failUnroll(
				ctx, state,
				fmt.Errorf("encode change script: %w",
					err),
			)

			return
		}

		// Build the unsigned CPFP child transaction.
		parentTxid := signedTx.TxHash()
		childTx, err := buildCPFPChild(
			parentTxid, anchorIdx, feeInput,
			changePkScript, totalFee,
		)
		if err != nil {
			a.failUnroll(
				ctx, state,
				fmt.Errorf("build CPFP child for %s: %w",
					txid, err),
			)

			return
		}

		// Sign the child via PSBT. The P2A anchor input gets
		// an empty witness (anyone-can-spend). The wallet
		// input is signed by LND.
		signedChild, err := signCPFPChild(
			ctx, a.cfg.WalletKit, childTx, anchorOut,
			feeInput,
		)
		if err != nil {
			a.failUnroll(
				ctx, state,
				fmt.Errorf("sign CPFP child for %s: %w",
					txid, err),
			)

			return
		}

		a.cfg.Logger.InfoS(ctx, "Submitting package",
			slog.String("parent_txid", parentTxid.String()),
			slog.Int("parent_inputs", len(signedTx.TxIn)),
			slog.Int("parent_outputs", len(signedTx.TxOut)),
			slog.Int64("parent_weight", parentWeight),
			slog.Int("child_inputs", len(signedChild.TxIn)),
			slog.Int("child_outputs", len(signedChild.TxOut)),
			slog.Int64("total_fee", int64(totalFee)),
			slog.Int("parent_version", int(signedTx.Version)),
			slog.Int64("fee_rate", int64(feeRate)),
			slog.Int64("anchor_value", anchorOut.Value),
			slog.Int("anchor_pkscript_len", len(anchorOut.PkScript)))

		// Submit via ChainSource atomically.
		submitReq := &chainsource.SubmitPackageRequest{
			Parents: []*wire.MsgTx{signedTx},
			Child:   signedChild,
		}

		submitResult := a.cfg.ChainSource.Ask(
			ctx, submitReq,
		).Await(ctx)
		_, err = submitResult.Unpack()
		if err != nil {
			// Package submission failure is fatal — the
			// CPFP child is valid and funded, so rejection
			// indicates a fundamental problem (e.g. invalid
			// parent, conflicting transaction).
			a.failUnroll(
				ctx, state,
				fmt.Errorf("package broadcast %s: %w",
					txid, err),
			)

			return
		}

		a.cfg.Logger.InfoS(ctx, "Package broadcast successful",
			slog.String("txid", txid.String()),
			slog.Int("level", level))

		// Mark as broadcast.
		state.BroadcastTxids[txid] = true

		// Register for confirmation monitoring using the
		// first output's pkScript so lnd's notifier can
		// match the transaction in new blocks.
		a.registerConfirmation(
			ctx, state, txid,
			signedTx.TxOut[0].PkScript,
			heightHint,
		)
	}

	// Persist updated state.
	if err := a.cfg.Store.UpdateUnrollState(ctx, state); err != nil {
		a.cfg.Logger.WarnS(
			ctx, "Failed to update unroll state", err,
			slog.String("vtxo", state.VTXOOutpoint.String()),
		)
	}
}

// getFeeRate queries the chain source for the current fee rate estimate.
// Uses a target of 6 blocks which is a reasonable default for unroll
// transactions that are not time-critical within a single level.
func (a *UnrollerActor) getFeeRate(
	ctx context.Context,
) (btcutil.Amount, error) {

	feeReq := &chainsource.FeeEstimateRequest{TargetConf: 6}
	result := a.cfg.ChainSource.Ask(ctx, feeReq).Await(ctx)

	resp, err := result.Unpack()
	if err != nil {
		return 0, fmt.Errorf("fee estimate: %w", err)
	}

	feeResp, ok := resp.(*chainsource.FeeEstimateResponse)
	if !ok {
		return 0, fmt.Errorf(
			"unexpected fee response type: %T", resp,
		)
	}

	return feeResp.SatPerVByte, nil
}

// getBestHeight queries the chain source for the current best block
// height. This is used as the height hint when registering for
// confirmation notifications, since lnd requires a hint > 0.
func (a *UnrollerActor) getBestHeight(
	ctx context.Context,
) (uint32, error) {

	heightReq := &chainsource.BestHeightRequest{}
	result := a.cfg.ChainSource.Ask(
		ctx, heightReq,
	).Await(ctx)

	resp, err := result.Unpack()
	if err != nil {
		return 0, fmt.Errorf("best height: %w", err)
	}

	heightResp, ok := resp.(*chainsource.BestHeightResponse)
	if !ok {
		return 0, fmt.Errorf(
			"unexpected height response type: %T", resp,
		)
	}

	return uint32(heightResp.Height), nil
}

// failUnroll transitions the unroll to a failed state, persists it, and
// removes it from active tracking.
func (a *UnrollerActor) failUnroll(
	ctx context.Context, state *UnrollState, err error,
) {

	a.cfg.Logger.WarnS(ctx, "Unroll failed", err,
		slog.String("vtxo", state.VTXOOutpoint.String()))

	state.Status = UnrollStatusFailed
	state.Error = err

	if storeErr := a.cfg.Store.UpdateUnrollState(ctx, state); storeErr != nil {
		a.cfg.Logger.WarnS(
			ctx, "Failed to persist failed unroll state",
			storeErr,
			slog.String("vtxo", state.VTXOOutpoint.String()),
		)
	}

	// Clean up reverse-lookup entries and remove from active tracking.
	a.cleanupUnrollTxids(state)

	delete(a.activeUnrolls, state.VTXOOutpoint.String())
}

// registerConfirmation subscribes to confirmation events for a
// transaction. The pkScript of the first output is required by lnd's
// chain notifier for matching confirmations in new blocks.
func (a *UnrollerActor) registerConfirmation(
	ctx context.Context, state *UnrollState, txid chainhash.Hash,
	pkScript []byte, heightHint uint32,
) {

	// Create mapped ref to convert confirmation events to our
	// message type.
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
	notifyRef := actor.TellOnlyRef[
		chainsource.ConfirmationEvent,
	](mappedRef)

	callerID := fmt.Sprintf(
		"unroll-%s-%s", state.VTXOOutpoint, txid,
	)
	confReq := &chainsource.RegisterConfRequest{
		CallerID:    callerID,
		Txid:        &txid,
		PkScript:    pkScript,
		HeightHint:  heightHint,
		TargetConfs: 1, // V3 transactions need only 1 conf.
		NotifyActor: fn.Some(notifyRef),
	}

	a.cfg.Logger.DebugS(ctx, "Registering for confirmation",
		slog.String("txid", txid.String()),
		slog.Int("pkscript_len", len(pkScript)),
		slog.Int("height_hint", int(heightHint)))

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

	// Use a stable per-unroll CallerID to avoid accumulating
	// subscriptions across levels.
	subReq := &chainsource.SubscribeBlocksRequest{
		CallerID: fmt.Sprintf(
			"csv-%s", state.VTXOOutpoint,
		),
		NotifyActor: fn.Some(notifyRef),
	}

	a.cfg.ChainSource.Tell(context.Background(), subReq)
}

// handleConfirmation processes confirmation events.
func (a *UnrollerActor) handleConfirmation(
	ctx context.Context, evt *ConfirmationEvent,
) fn.Result[UnrollerResp] {

	// Find which unroll this confirmation belongs to via O(1) lookup.
	outpointKey, known := a.txidToUnroll[evt.Txid]
	if !known {
		// Might be from a previous unroll that completed.
		return fn.Ok[UnrollerResp](&UnrollStartedResp{})
	}

	state, active := a.activeUnrolls[outpointKey]
	if !active {
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

		// Intermediate branch nodes have no CSV lock (only the
		// final VTXO leaf has OP_CHECKSEQUENCEVERIFY). So we
		// broadcast the next level immediately on confirmation.
		if state.CurrentLevel < len(state.LevelOrder)-1 {
			nextLevel := state.CurrentLevel + 1
			a.broadcastLevel(ctx, state, nextLevel)
		} else {
			// Final level confirmed, transition to CSV wait.
			a.handleAllLevelsComplete(ctx, state)
		}
	}

	// Persist updated state.
	if err := a.cfg.Store.UpdateUnrollState(ctx, state); err != nil {
		a.cfg.Logger.WarnS(ctx, "Failed to update unroll state", err,
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

	// Track latest block height for status reporting.
	a.bestHeight = evt.Height

	for _, state := range a.activeUnrolls {
		csvDelay := int32(state.VTXO.Expiry)

		switch state.Status {
		case UnrollStatusAwaitingCSV:
			// If no leaf confirmation height was recorded, initialize the
			// baseline at the first epoch we observe.
			if state.LeafConfirmHeight == 0 {
				state.LeafConfirmHeight = evt.Height

				a.cfg.Logger.InfoS(ctx, "Initialized CSV baseline",
					slog.String("vtxo", state.VTXOOutpoint.String()),
					slog.Int(
						"leaf_height",
						int(state.LeafConfirmHeight),
					))

				if err := a.cfg.Store.UpdateUnrollState(
					ctx, state,
				); err != nil {
					a.cfg.Logger.WarnS(
						ctx, "Failed to update unroll state", err,
						slog.String(
							"vtxo",
							state.VTXOOutpoint.String(),
						),
					)
				}
			}

			// Check if CSV delay satisfied to complete unroll.
			if evt.Height >= state.LeafConfirmHeight+csvDelay {
				a.cfg.Logger.InfoS(
					ctx, "CSV satisfied, completing unroll",
					slog.String("vtxo", state.VTXOOutpoint.String()),
					slog.Int("height", int(evt.Height)),
					slog.Int(
						"leaf_height",
						int(state.LeafConfirmHeight),
					))

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
		a.cfg.Logger.WarnS(ctx, "Failed to update unroll state", err,
			slog.String("vtxo", state.VTXOOutpoint.String()))
	}

	// Ensure we track block epochs even when no broadcasts happen.
	a.subscribeBlockEpochs(ctx, state)
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
		a.cfg.Logger.WarnS(ctx, "Failed to update unroll state", err,
			slog.String("vtxo", state.VTXOOutpoint.String()))
	}

	// Clean up reverse-lookup entries.
	a.cleanupUnrollTxids(state)

	// Unsubscribe from block epoch notifications since CSV tracking
	// is no longer needed.
	unsubReq := &chainsource.UnsubscribeBlocksRequest{
		CallerID: fmt.Sprintf(
			"csv-%s", state.VTXOOutpoint,
		),
	}
	a.cfg.ChainSource.Tell(context.Background(), unsubReq)

	// Remove from active tracking. The VTXO is now ready for sweeping
	// by a dedicated sweeper actor.
	delete(a.activeUnrolls, state.VTXOOutpoint.String())
}

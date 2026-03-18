package unroller

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
)

// defaultMaxFeeRate is the maximum fee rate (sat/vB) used when
// UnrollerConfig.MaxFeeRate is zero. This protects against fee
// spikes that could drain wallet funds.
const defaultMaxFeeRate = btcutil.Amount(500)

// isBroadcastNonFatal returns true if the error from SubmitPackage
// indicates the transaction is already known (in mempool or confirmed
// on-chain). These are safe to ignore during broadcast and fee bump
// since they mean the transaction has already progressed.
func isBroadcastNonFatal(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())

	// bitcoind submitpackage / sendrawtransaction error strings.
	nonFatalSubstrings := []string{
		"txn-already-in-mempool",
		"txn-already-known",
		"already in block chain",
		"transaction already in block chain",
		"already known",
	}

	for _, s := range nonFatalSubstrings {
		if strings.Contains(msg, s) {
			return true
		}
	}

	return false
}

// clampFeeRate caps the fee rate to the configured maximum.
func (a *UnrollerActor) clampFeeRate(
	ctx context.Context, feeRate btcutil.Amount,
) btcutil.Amount {

	maxRate := a.cfg.MaxFeeRate
	if maxRate == 0 {
		maxRate = defaultMaxFeeRate
	}
	if feeRate > maxRate {
		a.cfg.Logger.WarnS(ctx,
			"Fee rate exceeds cap, using max", nil,
			slog.Int64("estimated", int64(feeRate)),
			slog.Int64("max", int64(maxRate)))

		return maxRate
	}

	return feeRate
}

// buildAndSubmitPackageWithWalletKit builds and submits an explicit CPFP child
// for the given parent transaction using the configured WalletKit.
func (a *UnrollerActor) buildAndSubmitPackageWithWalletKit(
	ctx context.Context, txid chainhash.Hash, signedTx *wire.MsgTx,
	feeRate btcutil.Amount, usedOutpoints map[wire.OutPoint]bool,
) error {

	anchorIdx, anchorOut := findAnchorOutput(signedTx)
	if anchorIdx < 0 {
		return fmt.Errorf("no P2A anchor in tx %s", txid)
	}

	// Compute fee for entire package: parent vsize + child vsize estimate,
	// multiplied by the per-vbyte fee rate.
	parentWeight := estimateWeight(signedTx)
	parentVSize := (parentWeight + 3) / 4
	totalFee := feeRate *
		btcutil.Amount(parentVSize+childVSizeEstimate)
	if totalFee < 1 {
		totalFee = 1
	}

	// Select a confirmed wallet UTXO for the fee input. V3 rules
	// require the child to have at most one unconfirmed input (the
	// anchor), so the fee input must be confirmed.
	feeInput, err := selectFeeUTXO(
		ctx, a.cfg.WalletKit, totalFee, usedOutpoints,
	)
	if err != nil {
		return fmt.Errorf("select fee UTXO for %s: %w", txid, err)
	}

	// Get a change address from the wallet.
	changeAddr, err := a.cfg.WalletKit.NextAddr(
		ctx, "",
		walletrpc.AddressType_TAPROOT_PUBKEY, false,
	)
	if err != nil {
		return fmt.Errorf("get change address: %w", err)
	}

	changePkScript, err := txscript.PayToAddrScript(changeAddr)
	if err != nil {
		return fmt.Errorf("encode change script: %w", err)
	}

	// Mark this fee UTXO as used so sibling nodes in this level select
	// different UTXOs.
	usedOutpoints[feeInput.outpoint] = true

	// Build the unsigned CPFP child transaction.
	parentTxid := signedTx.TxHash()
	childTx, err := buildCPFPChild(
		parentTxid, uint32(anchorIdx), feeInput,
		changePkScript, totalFee,
	)
	if err != nil {
		return fmt.Errorf("build CPFP child for %s: %w", txid, err)
	}

	// Sign the child via PSBT. The P2A anchor input gets an empty witness
	// (anyone-can-spend). The wallet input is signed by LND.
	signedChild, err := signCPFPChild(
		ctx, a.cfg.WalletKit, childTx, anchorOut, feeInput,
	)
	if err != nil {
		return fmt.Errorf("sign CPFP child for %s: %w", txid, err)
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
		// If the transaction is already in the mempool or confirmed,
		// this is not a failure — it means a previous broadcast
		// succeeded.
		if isBroadcastNonFatal(err) {
			a.cfg.Logger.InfoS(ctx,
				"Tx already known, skipping",
				slog.String("txid", txid.String()),
				slog.String("reason", err.Error()))

			return nil
		}

		return fmt.Errorf("package broadcast %s: %w", txid, err)
	}

	a.cfg.Logger.InfoS(ctx, "Package broadcast successful",
		slog.String("txid", txid.String()))

	return nil
}

// handleUnrollRequest initiates unroll for the requested VTXOs.
func (a *UnrollerActor) handleUnrollRequest(
	ctx context.Context, req *UnrollRequest,
) fn.Result[UnrollerResp] {

	if len(req.TargetVTXOs) == 0 {
		return fn.Err[UnrollerResp](
			fmt.Errorf("no target VTXOs specified"),
		)
	}

	// Process each requested VTXO. The RPC accepts multiple
	// outpoints, so we initiate unrolls for all of them.
	for _, outpoint := range req.TargetVTXOs {
		if err := a.startSingleUnroll(
			ctx, outpoint,
		); err != nil {
			return fn.Err[UnrollerResp](err)
		}
	}

	return fn.Ok[UnrollerResp](&UnrollStartedResp{})
}

// startSingleUnroll initiates unroll for a single VTXO outpoint.
func (a *UnrollerActor) startSingleUnroll(
	ctx context.Context, outpoint wire.OutPoint,
) error {

	// Check if already unrolling this VTXO.
	if _, exists := a.activeUnrolls[outpoint]; exists {
		a.cfg.Logger.InfoS(ctx, "Unroll already in progress",
			slog.String("outpoint", outpoint.String()))

		return nil
	}

	// Fetch VTXO descriptor from store.
	vtxoDesc, err := a.cfg.Store.GetVTXO(ctx, outpoint)
	if err != nil {
		a.cfg.Logger.WarnS(ctx, "Failed to fetch VTXO", err,
			slog.String("outpoint", outpoint.String()))

		return fmt.Errorf("fetch VTXO %s: %w", outpoint, err)
	}

	a.cfg.Logger.InfoS(ctx, "Starting VTXO unroll",
		slog.String("outpoint", outpoint.String()))

	// Guard: OOR VTXOs have nil TreePath and require a
	// different unroll path (checkpoint chain broadcast).
	// TODO: Implement checkpoint-based unrolling for OOR VTXOs.
	if vtxoDesc.TreePath == nil {
		return fmt.Errorf("OOR VTXO unrolling not yet "+
			"supported (outpoint %s has no tree "+
			"path — received via out-of-round "+
			"transfer)", outpoint)
	}

	// Extract level-ordered transactions from tree.
	levelOrder, err := extractLevelOrder(vtxoDesc.TreePath)
	if err != nil {
		a.cfg.Logger.WarnS(ctx, "Failed to extract tree levels",
			err, slog.String("outpoint", outpoint.String()))

		return fmt.Errorf("extract tree levels for %s: %w",
			outpoint, err)
	}

	// Sanity check: the extracted level order must contain at
	// least one level with at least one transaction.
	if len(levelOrder) == 0 || len(levelOrder[0].Txids) == 0 {
		return fmt.Errorf("extracted empty level order for "+
			"VTXO %s: got %d levels", outpoint,
			len(levelOrder))
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
		a.cfg.Logger.WarnS(ctx, "Failed to save unroll state",
			err, slog.String("outpoint", outpoint.String()))

		return err
	}

	a.activeUnrolls[outpoint] = state

	// Index all txids for O(1) confirmation lookup.
	a.indexUnrollTxids(state)

	// Start broadcasting from level 0. The tree root (level 0) is a
	// virtual transaction that spends the batch outpoint and needs to
	// be broadcast to initiate the unilateral exit.
	a.broadcastLevel(ctx, state, 0)

	return nil
}

// broadcastLevel broadcasts all transactions at the specified level.
// When WalletKit is available, uses 1P1C package relay (CPFP) for
// V3 transactions with ephemeral anchors. When WalletKit is nil
// (e.g. lwwallet mode), falls back to the chain backend's
// SubmitPackage which auto-constructs a CPFP child using the
// lwwallet for fee payment via the esplora package relay API.
func (a *UnrollerActor) broadcastLevel(
	ctx context.Context, state *UnrollState, level int,
) {

	if level >= len(state.LevelOrder) {
		// All levels complete, transition to CSV wait or
		// completion.
		a.handleAllLevelsComplete(ctx, state)
		return
	}

	// Use chain backend's auto-CPFP when WalletKit is unavailable.
	if a.cfg.WalletKit == nil {
		a.broadcastLevelDirect(ctx, state, level)
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

	feeRate = a.clampFeeRate(ctx, feeRate)

	// Track fee UTXOs selected within this level to prevent
	// double-spending the same wallet UTXO across sibling nodes.
	usedOutpoints := make(map[wire.OutPoint]bool)

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

		err = a.buildAndSubmitPackageWithWalletKit(
			ctx, txid, signedTx, feeRate, usedOutpoints,
		)
		if err != nil {
			a.failUnroll(ctx, state, err)
			return
		}

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

	// Record broadcast height and fee rate for fee bumping.
	state.LastBroadcastHeight = int32(heightHint)
	state.CurrentFeeRate = int64(feeRate)

	// Persist updated state.
	if err := a.cfg.Store.UpdateUnrollState(ctx, state); err != nil {
		a.cfg.Logger.WarnS(
			ctx, "Failed to update unroll state", err,
			slog.String("vtxo", state.VTXOOutpoint.String()),
		)
	}

	// Subscribe to block epochs so fee bumping can trigger
	// when transactions don't confirm within BumpAfterBlocks.
	a.subscribeBlockEpochs(ctx, state)
}

// broadcastLevelDirect broadcasts transactions via SubmitPackage with
// automatic CPFP child construction. This is used in lwwallet mode
// where WalletKit is unavailable. V3 transactions with ephemeral
// anchors cannot be broadcast individually; they require package
// relay. The chain backend's SubmitPackage automatically builds a
// fee-paying CPFP child using wallet UTXOs when child is nil.
func (a *UnrollerActor) broadcastLevelDirect(
	ctx context.Context, state *UnrollState, level int,
) {

	levelTxids := state.LevelOrder[level]

	a.cfg.Logger.InfoS(ctx, "Broadcasting tree level (CPFP package)",
		slog.String("vtxo", state.VTXOOutpoint.String()),
		slog.Int("level", level),
		slog.Int("tx_count", len(levelTxids.Txids)))

	state.CurrentLevel = level
	state.Status = UnrollStatusBroadcasting

	heightHint, err := a.getBestHeight(ctx)
	if err != nil {
		a.failUnroll(ctx, state,
			fmt.Errorf("get best height: %w", err))
		return
	}

	for i, node := range levelTxids.Nodes {
		if i >= len(levelTxids.Txids) {
			break
		}

		txid := levelTxids.Txids[i]

		signedTx, err := node.ToSignedTx()
		if err != nil {
			a.failUnroll(ctx, state,
				fmt.Errorf("signed tx %s: %w",
					txid, err))

			return
		}

		a.cfg.Logger.InfoS(ctx, "Submitting package (auto-CPFP)",
			slog.String("parent_txid", txid.String()),
			slog.Int("level", level),
			slog.Int("parent_version",
				int(signedTx.Version)),
			slog.Int("parent_outputs",
				len(signedTx.TxOut)))

		// Submit the V3 parent as-is with no child. The chain
		// backend's SubmitPackage will auto-construct a CPFP
		// child using the wallet when child is nil.
		submitReq := &chainsource.SubmitPackageRequest{
			Parents: []*wire.MsgTx{signedTx},
			Child:   nil,
		}

		submitResult := a.cfg.ChainSource.Ask(
			ctx, submitReq,
		).Await(ctx)
		_, submitErr := submitResult.Unpack()
		if submitErr != nil {
			if isBroadcastNonFatal(submitErr) {
				a.cfg.Logger.InfoS(ctx,
					"Tx already known, skipping",
					slog.String("txid",
						txid.String()),
					slog.String("reason",
						submitErr.Error()))
			} else {
				a.failUnroll(ctx, state,
					fmt.Errorf("package broadcast "+
						"%s: %w", txid,
						submitErr))

				return
			}
		}

		a.cfg.Logger.InfoS(ctx, "Package broadcast OK",
			slog.String("txid", txid.String()),
			slog.Int("level", level))

		state.BroadcastTxids[txid] = true

		a.registerConfirmation(ctx, state, txid,
			signedTx.TxOut[0].PkScript, heightHint)
	}

	// Record broadcast height for fee bumping. Fee rate is
	// managed by the chain backend in direct mode, so we leave
	// CurrentFeeRate at zero.
	state.LastBroadcastHeight = int32(heightHint)

	if err := a.cfg.Store.UpdateUnrollState(
		ctx, state,
	); err != nil {
		a.cfg.Logger.WarnS(ctx,
			"Failed to update unroll state", err,
			slog.String("vtxo",
				state.VTXOOutpoint.String()))
	}

	// Subscribe to block epochs so fee bumping can trigger
	// when transactions don't confirm within BumpAfterBlocks.
	a.subscribeBlockEpochs(ctx, state)
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

	storeErr := a.cfg.Store.UpdateUnrollState(
		ctx, state,
	)
	if storeErr != nil {
		a.cfg.Logger.WarnS(
			ctx, "Failed to persist failed unroll state",
			storeErr,
			slog.String("vtxo", state.VTXOOutpoint.String()),
		)
	}

	// Unsubscribe from block epoch notifications to avoid
	// leaking subscriptions on failed unrolls.
	unsubReq := &chainsource.UnsubscribeBlocksRequest{
		CallerID: fmt.Sprintf(
			"csv-%s", state.VTXOOutpoint,
		),
	}
	bgCtx := context.Background()
	if tellErr := a.cfg.ChainSource.Tell(
		bgCtx, unsubReq,
	); tellErr != nil {
		a.cfg.Logger.WarnS(ctx,
			"Failed to unsubscribe epochs",
			tellErr)
	}

	// Clean up reverse-lookup entries and remove from
	// active tracking.
	a.cleanupUnrollTxids(state)

	delete(a.activeUnrolls, state.VTXOOutpoint)
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
	notifyRef := actor.TellOnlyRef[chainsource.ConfirmationEvent](mappedRef)

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
	bgCtx := context.Background()
	if err := a.cfg.ChainSource.Tell(bgCtx, confReq); err != nil {
		a.cfg.Logger.WarnS(ctx, "Failed to register confirmation", err,
			slog.String("txid", txid.String()),
			slog.String("vtxo", state.VTXOOutpoint.String()))
	}
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

	bgCtx := context.Background()
	if err := a.cfg.ChainSource.Tell(bgCtx, subReq); err != nil {
		a.cfg.Logger.WarnS(ctx,
			"Failed to subscribe to block epochs",
			err,
			slog.String("vtxo", state.VTXOOutpoint.String()))
	}
}

// handleConfirmation processes confirmation events.
func (a *UnrollerActor) handleConfirmation(
	ctx context.Context, evt *ConfirmationEvent,
) fn.Result[UnrollerResp] {

	// Find which unroll this confirmation belongs to via O(1) lookup.
	outpoint, known := a.txidToUnroll[evt.Txid]
	if !known {
		// Might be from a previous unroll that completed.
		return fn.Ok[UnrollerResp](&UnrollStartedResp{})
	}

	state, active := a.activeUnrolls[outpoint]
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
		// Check if we should fee-bump unconfirmed transactions.
		if state.Status == UnrollStatusBroadcasting {
			bumpAfter := a.cfg.BumpAfterBlocks
			if bumpAfter == 0 {
				bumpAfter = 6 // default
			}

			sinceLastBroadcast := evt.Height -
				state.LastBroadcastHeight
			if state.LastBroadcastHeight > 0 &&
				sinceLastBroadcast >= bumpAfter {

				a.feeBumpLevel(ctx, state)
			}

			continue
		}

		csvDelay := int32(state.VTXO.Expiry)

		if state.Status != UnrollStatusAwaitingCSV {
			continue
		}

		// If no leaf confirmation height was recorded,
		// initialize the baseline at the first epoch.
		if state.LeafConfirmHeight == 0 {
			state.LeafConfirmHeight = evt.Height

			a.cfg.Logger.InfoS(ctx,
				"Initialized CSV baseline",
				slog.String("vtxo",
					state.VTXOOutpoint.String()),
				slog.Int("leaf_height",
					int(state.LeafConfirmHeight)))

			if err := a.cfg.Store.UpdateUnrollState(
				ctx, state,
			); err != nil {
				a.cfg.Logger.WarnS(ctx,
					"Failed to update state",
					err, slog.String("vtxo",
						state.VTXOOutpoint.String()))
			}
		}

		// Check if CSV delay satisfied.
		csvTarget := state.LeafConfirmHeight + csvDelay
		if evt.Height >= csvTarget {
			a.cfg.Logger.InfoS(ctx,
				"CSV satisfied, completing",
				slog.String("vtxo",
					state.VTXOOutpoint.String()),
				slog.Int("height",
					int(evt.Height)))

			a.handleCSVComplete(ctx, state)
		}
	}

	return fn.Ok[UnrollerResp](&UnrollStartedResp{})
}

// maxFeeBumpRetries is the maximum number of fee bump attempts per
// level before giving up. This prevents infinite bumping.
const maxFeeBumpRetries = 10

// feeBumpLevel attempts to rebroadcast the current level's
// transactions with a higher fee. This is a best-effort operation:
// errors are logged but do not fail the unroll. The next block epoch
// will trigger another attempt if needed.
func (a *UnrollerActor) feeBumpLevel(
	ctx context.Context, state *UnrollState,
) {

	level := state.CurrentLevel
	if level >= len(state.LevelOrder) {
		return
	}

	if state.RetryCount >= maxFeeBumpRetries {
		a.cfg.Logger.WarnS(ctx,
			"Fee bump retry limit reached, giving up",
			nil,
			slog.String("vtxo",
				state.VTXOOutpoint.String()),
			slog.Int("retries", state.RetryCount))

		a.failUnroll(ctx, state,
			fmt.Errorf("fee bump retry limit (%d) "+
				"reached for level %d",
				maxFeeBumpRetries, level))

		return
	}

	a.cfg.Logger.InfoS(ctx, "Attempting fee bump",
		slog.String("vtxo", state.VTXOOutpoint.String()),
		slog.Int("level", level),
		slog.Int("retry", state.RetryCount+1),
		slog.Int64("old_fee_rate", state.CurrentFeeRate))

	levelTxids := state.LevelOrder[level]

	var (
		err     error
		feeRate btcutil.Amount
	)
	if a.cfg.WalletKit != nil {
		feeRate, err = a.getFeeRate(ctx)
		if err != nil {
			a.cfg.Logger.WarnS(ctx,
				"Fee bump: failed to estimate fee rate", err,
				slog.String("vtxo",
					state.VTXOOutpoint.String()))

			return
		}

		multiplier := a.cfg.FeeMultiplier
		if multiplier == 0 {
			multiplier = 2
		}

		if state.CurrentFeeRate > 0 {
			bumpedRate := btcutil.Amount(state.CurrentFeeRate) *
				btcutil.Amount(multiplier)
			if bumpedRate > feeRate {
				feeRate = bumpedRate
			}
		}

		feeRate = a.clampFeeRate(ctx, feeRate)
	}

	usedOutpoints := make(map[wire.OutPoint]bool)
	for i, node := range levelTxids.Nodes {
		if i >= len(levelTxids.Txids) {
			break
		}

		txid := levelTxids.Txids[i]

		signedTx, err := node.ToSignedTx()
		if err != nil {
			a.cfg.Logger.WarnS(ctx,
				"Fee bump: failed to construct tx",
				err,
				slog.String("txid", txid.String()))

			return
		}

		if a.cfg.WalletKit == nil {
			// In lwwallet mode the chain backend auto-constructs a
			// fresh CPFP child when Child=nil. In WalletKit mode we
			// rebuild and sign an explicit child locally below.
			submitReq := &chainsource.SubmitPackageRequest{
				Parents: []*wire.MsgTx{signedTx},
				Child:   nil,
			}

			submitResult := a.cfg.ChainSource.Ask(
				ctx, submitReq,
			).Await(ctx)
			_, submitErr := submitResult.Unpack()
			if submitErr != nil {
				// If the tx already confirmed, fee bump is
				// unnecessary — the confirmation handler will
				// advance the state.
				if isBroadcastNonFatal(submitErr) {
					a.cfg.Logger.InfoS(ctx,
						"Fee bump: tx already known",
						slog.String("txid",
							txid.String()))

					continue
				}

				// Best-effort: log and wait for next epoch.
				a.cfg.Logger.WarnS(ctx,
					"Fee bump broadcast failed",
					submitErr,
					slog.String("txid", txid.String()),
					slog.Int("level", level))

				return
			}
		} else {
			err = a.buildAndSubmitPackageWithWalletKit(
				ctx, txid, signedTx, feeRate, usedOutpoints,
			)
			if err != nil {
				// Best-effort: log and wait for next epoch.
				a.cfg.Logger.WarnS(ctx,
					"Fee bump broadcast failed",
					err,
					slog.String("txid", txid.String()),
					slog.Int("level", level))

				return
			}
		}

		a.cfg.Logger.InfoS(ctx, "Fee bump broadcast OK",
			slog.String("txid", txid.String()),
			slog.Int("level", level))
	}

	// Update state after successful rebroadcast.
	state.LastBroadcastHeight = a.bestHeight
	if a.cfg.WalletKit != nil {
		state.CurrentFeeRate = int64(feeRate)
	}
	state.RetryCount++

	if err := a.cfg.Store.UpdateUnrollState(
		ctx, state,
	); err != nil {
		a.cfg.Logger.WarnS(ctx,
			"Failed to update state after fee bump",
			err,
			slog.String("vtxo",
				state.VTXOOutpoint.String()))
	}
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

// sweepVTXO constructs and broadcasts a transaction that spends the
// VTXO via the timeout path back to the user's wallet. The VTXO is
// on-chain as an output of the final leaf transaction after unrolling.
//
// Per ARK-05 Step 5, the sweep transaction is:
//
//	Version: 2, Locktime: 0
//	Input: leaf tx VTXO outpoint, sequence: csv_delay
//	Witness: <signature> <timeout_script> <control_block>
//	Output: destination (client wallet address)
func (a *UnrollerActor) sweepVTXO(
	ctx context.Context, state *UnrollState,
) error {

	vtxo := state.VTXO

	// Find the on-chain outpoint of the VTXO. After unrolling, the
	// VTXO lives as the non-anchor output of the last level's leaf
	// transaction. The anchor output uses the P2A script; the VTXO
	// output is the other one.
	finalLevel := state.LevelOrder[len(state.LevelOrder)-1]
	if len(finalLevel.Nodes) == 0 {
		return fmt.Errorf("final level has no nodes")
	}

	leafNode := finalLevel.Nodes[0]
	leafTx, err := leafNode.ToSignedTx()
	if err != nil {
		return fmt.Errorf("construct leaf tx: %w", err)
	}

	leafTxid := leafTx.TxHash()

	// Find the VTXO output index (non-anchor output).
	vtxoIdx := -1
	for i, out := range leafTx.TxOut {
		if !isAnchorOutput(out) {
			vtxoIdx = i
			break
		}
	}
	if vtxoIdx < 0 {
		return fmt.Errorf("no VTXO output found in leaf tx %s",
			leafTxid)
	}

	vtxoOut := leafTx.TxOut[vtxoIdx]
	onChainOutpoint := wire.OutPoint{
		Hash:  leafTxid,
		Index: uint32(vtxoIdx),
	}

	// Reconstruct the tapscript from VTXO keys.
	tapscript, err := scripts.VTXOTapScript(
		vtxo.ClientKey.PubKey, vtxo.OperatorKey, vtxo.Expiry,
	)
	if err != nil {
		return fmt.Errorf("build VTXO tapscript: %w", err)
	}

	// Get spend info for the timeout leaf.
	spendInfo, err := scripts.NewVTXOSpendInfo(
		tapscript, scripts.VTXOTimeoutPathLeaf,
	)
	if err != nil {
		return fmt.Errorf("VTXO spend info: %w", err)
	}

	// Get a sweep destination address.
	destAddr, err := a.cfg.SweepAddress(ctx)
	if err != nil {
		return fmt.Errorf("get sweep address: %w", err)
	}

	destScript, err := txscript.PayToAddrScript(destAddr)
	if err != nil {
		return fmt.Errorf("encode dest script: %w", err)
	}

	// Build the sweep transaction (version 2, no anchor needed).
	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: onChainOutpoint,
		Sequence:         vtxo.Expiry,
	})

	// Estimate fee: sweep tx has 1 taproot script-path input and
	// 1 P2TR output.
	//
	// Witness: ~1+64 (sig) + ~1+N (script) + ~1+M (control block)
	// Typical total witness ~200 bytes.
	// Non-witness: ~10 (overhead) + 41 (input) + 43 (P2TR output) = ~94.
	// Weight = 94*4 + 200 = 576. VSize = 144.
	const sweepVSizeEstimate = 144

	feeRate, err := a.getFeeRate(ctx)
	if err != nil {
		// On regtest or fresh nodes, fee estimation may fail
		// because there is no fee history. Fall back to the
		// minimum relay fee of 1 sat/vB.
		a.cfg.Logger.WarnS(ctx,
			"Fee estimation failed, using 1 sat/vB fallback",
			err)
		feeRate = 1
	}

	maxRate := a.cfg.MaxFeeRate
	if maxRate == 0 {
		maxRate = defaultMaxFeeRate
	}
	if feeRate > maxRate {
		feeRate = maxRate
	}

	fee := feeRate * btcutil.Amount(sweepVSizeEstimate)
	if fee < 1 {
		fee = 1
	}

	outputValue := btcutil.Amount(vtxoOut.Value) - fee
	if outputValue <= 0 {
		return fmt.Errorf("VTXO value %d too small to cover "+
			"fee %d", vtxoOut.Value, int64(fee))
	}

	sweepTx.AddTxOut(&wire.TxOut{
		Value:    int64(outputValue),
		PkScript: destScript,
	})

	// Build the sign descriptor for the timeout path.
	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		vtxoOut.PkScript, vtxoOut.Value,
	)
	sigHashes := txscript.NewTxSigHashes(sweepTx, prevFetcher)

	signDesc := scripts.VTXOSignDesc(
		vtxo.ClientKey, vtxoOut, sigHashes,
		prevFetcher, 0, spendInfo,
	)

	// Sign and construct witness via the timeout spend path.
	witness, err := scripts.VTXOTimeoutSpendWitness(
		a.cfg.Signer, signDesc, sweepTx,
	)
	if err != nil {
		return fmt.Errorf("sign sweep tx: %w", err)
	}

	sweepTx.TxIn[0].Witness = witness

	// Broadcast the sweep transaction.
	sweepTxid := sweepTx.TxHash()

	a.cfg.Logger.InfoS(ctx, "Broadcasting VTXO sweep",
		slog.String("sweep_txid", sweepTxid.String()),
		slog.String("vtxo_outpoint", onChainOutpoint.String()),
		slog.Int64("value", int64(outputValue)),
		slog.Int64("fee", int64(fee)),
		slog.String("dest", destAddr.EncodeAddress()))

	broadcastReq := &chainsource.BroadcastTxRequest{
		Tx:    sweepTx,
		Label: fmt.Sprintf("vtxo-sweep-%s", state.VTXOOutpoint),
	}

	broadcastResult := a.cfg.ChainSource.Ask(
		ctx, broadcastReq,
	).Await(ctx)
	_, err = broadcastResult.Unpack()
	if err != nil {
		return fmt.Errorf("broadcast sweep tx: %w", err)
	}

	a.cfg.Logger.InfoS(ctx, "VTXO sweep broadcast successful",
		slog.String("sweep_txid", sweepTxid.String()),
		slog.String("vtxo", state.VTXOOutpoint.String()))

	return nil
}

// isAnchorOutput returns true if the output is a P2A ephemeral anchor.
func isAnchorOutput(out *wire.TxOut) bool {
	return bytes.Equal(out.PkScript, scripts.AnchorPkScript)
}

// handleCSVComplete marks the unroll as complete after CSV delay is satisfied.
// The VTXO output is now spendable on-chain. If a signer and sweep address
// are configured, the VTXO is automatically swept to the wallet.
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

	// Sweep the VTXO to the user's wallet if configured.
	if a.cfg.Signer != nil && a.cfg.SweepAddress != nil {
		if err := a.sweepVTXO(ctx, state); err != nil {
			a.cfg.Logger.WarnS(ctx, "Failed to sweep VTXO",
				err,
				slog.String("vtxo",
					state.VTXOOutpoint.String()))
			// Don't fail the unroll -- the VTXO is on-chain
			// and can be swept manually later.
		}
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
	bgCtx := context.Background()
	if err := a.cfg.ChainSource.Tell(bgCtx, unsubReq); err != nil {
		a.cfg.Logger.WarnS(ctx,
			"Failed to unsubscribe from blocks",
			err,
			slog.String("vtxo", state.VTXOOutpoint.String()))
	}

	// Remove from active tracking.
	delete(a.activeUnrolls, state.VTXOOutpoint)
}

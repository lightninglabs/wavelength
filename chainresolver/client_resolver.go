package chainresolver

import (
	"context"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// ClientResolverActor is the main actor for client-side chain resolution.
// It coordinates VTXO monitoring, unroll initiation, and CSV timeout recovery.
type ClientResolverActor struct {
	// chainSource is the actor reference for blockchain operations.
	chainSource actor.ActorRef[
		chainsource.ChainSourceMsg,
		chainsource.ChainSourceResp,
	]

	// system is the actor system for spawning sub-actors.
	system *actor.ActorSystem

	// signer provides signing operations for recovery transactions.
	signer Signer

	// log is the logger for this actor.
	log btclog.Logger

	// mu protects the monitors maps.
	mu sync.RWMutex

	// vtxoMonitors maps VTXO outpoint to the active monitoring handle.
	vtxoMonitors map[wire.OutPoint]*VTXOMonitorHandle

	// notifyActors maps VTXO outpoint to the registered notification actor.
	notifyActors map[wire.OutPoint]actor.TellOnlyRef[ClientResolverEvent]

	// boardingMonitors maps boarding address string to monitor handle.
	boardingMonitors map[string]*BoardingMonitorHandle

	// pendingUnrolls tracks VTXOs we've initiated unroll for.
	pendingUnrolls map[wire.OutPoint]bool

	// ctx is the actor's context.
	//nolint:containedctx
	ctx context.Context

	// cancel cancels the actor's context.
	cancel context.CancelFunc
}

// NewClientResolverActor creates a new ClientResolverActor instance.
func NewClientResolverActor(
	chainSource actor.ActorRef[
		chainsource.ChainSourceMsg,
		chainsource.ChainSourceResp,
	],
	system *actor.ActorSystem,
	signer Signer,
	log btclog.Logger,
) *ClientResolverActor {

	ctx, cancel := context.WithCancel(context.Background())

	notifyMap := make(
		map[wire.OutPoint]actor.TellOnlyRef[ClientResolverEvent],
	)

	return &ClientResolverActor{
		chainSource:      chainSource,
		system:           system,
		signer:           signer,
		log:              log,
		vtxoMonitors:     make(map[wire.OutPoint]*VTXOMonitorHandle),
		notifyActors:     notifyMap,
		boardingMonitors: make(map[string]*BoardingMonitorHandle),
		pendingUnrolls:   make(map[wire.OutPoint]bool),
		ctx:              ctx,
		cancel:           cancel,
	}
}

// Receive processes incoming messages for the ClientResolverActor.
func (a *ClientResolverActor) Receive(actorCtx context.Context,
	msg ClientResolverMsg) fn.Result[ClientResolverResp] {

	switch m := msg.(type) {
	case *MonitorVTXORequest:
		return a.handleMonitorVTXO(actorCtx, m)

	case *StopMonitorVTXORequest:
		return a.handleStopMonitorVTXO(actorCtx, m)

	case *InitiateUnrollRequest:
		return a.handleInitiateUnroll(actorCtx, m)

	case *RecoverVTXORequest:
		return a.handleRecoverVTXO(actorCtx, m)

	case *MonitorBoardingRequest:
		return a.handleMonitorBoarding(actorCtx, m)

	case *internalVTXOSpentNotification:
		return a.handleVTXOSpent(actorCtx, m)

	case *internalCSVTimeoutNotification:
		return a.handleCSVTimeout(actorCtx, m)

	default:
		return fn.Err[ClientResolverResp](
			fmt.Errorf("unknown message type: %T", msg),
		)
	}
}

// handleMonitorVTXO processes a request to start monitoring a VTXO.
func (a *ClientResolverActor) handleMonitorVTXO(ctx context.Context,
	req *MonitorVTXORequest) fn.Result[ClientResolverResp] {

	a.log.InfoS(ctx, "Starting VTXO monitoring",
		"outpoint", req.VTXOOutpoint.String(),
		"exit_delay", req.ExitDelay)

	// Check if already monitoring.
	a.mu.RLock()
	_, exists := a.vtxoMonitors[req.VTXOOutpoint]
	a.mu.RUnlock()

	if exists {
		return fn.Err[ClientResolverResp](
			fmt.Errorf("VTXO %s is already being monitored",
				req.VTXOOutpoint.String()),
		)
	}

	// Create service key for sub-actor.
	serviceKeyName := fmt.Sprintf(
		"vtxo-monitor.%s", req.VTXOOutpoint.String(),
	)
	serviceKey := actor.NewServiceKey[vtxoMonitorMsg, vtxoMonitorResp](
		serviceKeyName,
	)

	// Create and spawn the VTXOMonitorActor.
	vtxoMonitor := NewVTXOMonitorActor(
		a.chainSource, a.log.SubSystem("vtxo-"+req.VTXOOutpoint.String()),
	)

	monitorRef := serviceKey.Spawn(a.system, serviceKeyName, vtxoMonitor)

	// Get refs for callbacks.
	selfRef := serviceKey.Ref(a.system)
	parentRef := ClientResolverKey.Ref(a.system)

	// Send start monitoring request.
	startReq := &startVTXOMonitorRequest{
		Config:  req,
		Parent:  parentRef,
		SelfRef: selfRef,
	}

	future := monitorRef.Ask(ctx, startReq)
	result := future.Await(ctx)

	if result.IsErr() {
		return fn.Err[ClientResolverResp](
			fmt.Errorf("failed to start VTXO monitor: %w",
				result.Err()),
		)
	}

	// Store the handle.
	a.mu.Lock()
	a.vtxoMonitors[req.VTXOOutpoint] = &VTXOMonitorHandle{
		VTXOOutpoint:       req.VTXOOutpoint,
		ServiceKeyName:     serviceKeyName,
		Config:             req,
		ConfirmationHeight: 0,
		CSVTimeoutHeight:   0,
		TimeoutNotified:    false,
		Spent:              false,
		ExpectedSpend:      false,
	}

	// Store notification actor if provided.
	req.NotifyActor.WhenSome(
		func(ref actor.TellOnlyRef[ClientResolverEvent]) {
			a.notifyActors[req.VTXOOutpoint] = ref
		},
	)
	a.mu.Unlock()

	return fn.Ok[ClientResolverResp](&MonitorVTXOResponse{
		MonitorID: serviceKeyName,
	})
}

// handleStopMonitorVTXO processes a request to stop monitoring a VTXO.
func (a *ClientResolverActor) handleStopMonitorVTXO(ctx context.Context,
	req *StopMonitorVTXORequest) fn.Result[ClientResolverResp] {

	a.log.InfoS(ctx, "Stopping VTXO monitoring",
		"outpoint", req.VTXOOutpoint.String())

	a.mu.Lock()
	handle, exists := a.vtxoMonitors[req.VTXOOutpoint]
	if !exists {
		a.mu.Unlock()

		return fn.Ok[ClientResolverResp](&StopMonitorVTXOResponse{
			Stopped: false,
		})
	}

	delete(a.vtxoMonitors, req.VTXOOutpoint)
	delete(a.notifyActors, req.VTXOOutpoint)
	a.mu.Unlock()

	// Unregister the sub-actor.
	serviceKey := actor.NewServiceKey[vtxoMonitorMsg, vtxoMonitorResp](
		handle.ServiceKeyName,
	)
	serviceKey.UnregisterAll(a.system)

	return fn.Ok[ClientResolverResp](&StopMonitorVTXOResponse{
		Stopped: true,
	})
}

// handleInitiateUnroll processes a request to initiate an unroll.
func (a *ClientResolverActor) handleInitiateUnroll(ctx context.Context,
	req *InitiateUnrollRequest) fn.Result[ClientResolverResp] {

	a.log.InfoS(ctx, "Initiating unroll")

	// Extract the path for this cosigner.
	extractedTree, err := req.TreePath.ExtractPathForCoSigner(
		req.CoSignerKey,
	)
	if err != nil {
		return fn.Err[ClientResolverResp](
			fmt.Errorf("failed to extract path: %w", err),
		)
	}

	if extractedTree == nil {
		return fn.Err[ClientResolverResp](
			fmt.Errorf("no path found for cosigner"),
		)
	}

	// Broadcast each transaction from root to leaf.
	var broadcastTxids []chainhash.Hash
	var leafOutpoint wire.OutPoint

	for node := range extractedTree.Root.NodesIter() {
		signedTx, signErr := node.ToSignedTx()
		if signErr != nil {
			return fn.Err[ClientResolverResp](
				fmt.Errorf("signed tx: %w", signErr),
			)
		}

		broadcastReq := &chainsource.BroadcastTxRequest{
			Tx:    signedTx,
			Label: "client-unroll",
		}
		future := a.chainSource.Ask(ctx, broadcastReq)

		result := future.Await(ctx)
		if result.IsErr() {
			return fn.Err[ClientResolverResp](fmt.Errorf(
				"failed to broadcast unroll tx: %w",
				result.Err(),
			))
		}

		resp, unpackErr := result.Unpack()
		if unpackErr != nil {
			return fn.Err[ClientResolverResp](fmt.Errorf(
				"failed to unpack broadcast response: %w",
				unpackErr,
			))
		}

		txResp, ok := resp.(*chainsource.BroadcastTxResponse)
		if !ok {
			return fn.Err[ClientResolverResp](
				fmt.Errorf("unexpected response type"),
			)
		}

		broadcastTxids = append(broadcastTxids, txResp.Txid)

		// Track the leaf outpoint.
		if node.IsLeaf() {
			leafOutpoint = wire.OutPoint{
				Hash:  txResp.Txid,
				Index: 0,
			}

			// Mark this as a pending unroll (expected spend).
			a.mu.Lock()
			a.pendingUnrolls[leafOutpoint] = true
			a.mu.Unlock()
		}

		a.log.DebugS(ctx, "Broadcast unroll transaction",
			"txid", txResp.Txid.String())
	}

	return fn.Ok[ClientResolverResp](&InitiateUnrollResponse{
		BroadcastTxids: broadcastTxids,
		LeafOutpoint:   leafOutpoint,
	})
}

// handleRecoverVTXO processes a request to recover a VTXO via CSV timeout.
func (a *ClientResolverActor) handleRecoverVTXO(ctx context.Context,
	req *RecoverVTXORequest) fn.Result[ClientResolverResp] {

	a.log.InfoS(ctx, "Processing VTXO recovery request",
		"outpoint", req.VTXOOutpoint.String(),
		"csv_timeout", req.CSVTimeout)

	// Create the recovery transaction.
	recoveryTx := wire.NewMsgTx(3)

	// Add input with CSV sequence.
	recoveryTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: req.VTXOOutpoint,
		Sequence:         req.CSVTimeout,
	})

	// Get fee estimate.
	feeFuture := a.chainSource.Ask(ctx, &chainsource.FeeEstimateRequest{
		TargetConf: 6,
	})

	feeResult := feeFuture.Await(ctx)
	if feeResult.IsErr() {
		feeErr := feeResult.Err()

		return fn.Err[ClientResolverResp](
			fmt.Errorf("failed to estimate fee: %w", feeErr),
		)
	}

	feeResp, err := feeResult.Unpack()
	if err != nil {
		return fn.Err[ClientResolverResp](
			fmt.Errorf("failed to unpack fee resp: %w", err),
		)
	}

	feeEstimate, ok := feeResp.(*chainsource.FeeEstimateResponse)
	if !ok {
		return fn.Err[ClientResolverResp](
			fmt.Errorf("unexpected fee response type"),
		)
	}

	// Estimate size for CSV spend.
	estimatedVSize := int64(153)
	fee := btcutil.Amount(estimatedVSize) * feeEstimate.SatPerVByte

	inputAmount := btcutil.Amount(req.VTXOOutput.Value)
	if inputAmount <= fee {
		return fn.Err[ClientResolverResp](
			fmt.Errorf("insufficient funds: input %d, fee %d",
				inputAmount, fee),
		)
	}

	// Create output to destination.
	destScript, err := txscript.PayToAddrScript(req.Destination)
	if err != nil {
		return fn.Err[ClientResolverResp](
			fmt.Errorf("failed to create output script: %w", err),
		)
	}

	recoveryTx.AddTxOut(&wire.TxOut{
		Value:    int64(inputAmount - fee),
		PkScript: destScript,
	})

	// Sign with CSV timeout path.
	signedTx, err := a.signer.SignTimeoutPath(
		ctx, recoveryTx, req.VTXOOutput, req.CSVTimeout,
	)
	if err != nil {
		return fn.Err[ClientResolverResp](
			fmt.Errorf("failed to sign recovery tx: %w", err),
		)
	}

	// Broadcast.
	broadcastReq := &chainsource.BroadcastTxRequest{
		Tx:    signedTx,
		Label: "vtxo-recovery",
	}
	broadcastFuture := a.chainSource.Ask(ctx, broadcastReq)

	broadcastResult := broadcastFuture.Await(ctx)
	if broadcastResult.IsErr() {
		return fn.Err[ClientResolverResp](
			fmt.Errorf("failed to broadcast recovery tx: %w",
				broadcastResult.Err()),
		)
	}

	broadcastResp, err := broadcastResult.Unpack()
	if err != nil {
		return fn.Err[ClientResolverResp](
			fmt.Errorf("failed to unpack broadcast resp: %w", err),
		)
	}

	txResp, ok := broadcastResp.(*chainsource.BroadcastTxResponse)
	if !ok {
		return fn.Err[ClientResolverResp](
			fmt.Errorf("unexpected broadcast response type"),
		)
	}

	a.log.InfoS(ctx, "VTXO recovery transaction broadcast",
		"recovery_txid", txResp.Txid.String(),
		"amount", inputAmount)

	return fn.Ok[ClientResolverResp](&RecoverVTXOResponse{
		RecoveryTxid: txResp.Txid,
		Amount:       inputAmount,
	})
}

// handleMonitorBoarding processes a request to monitor a boarding address.
func (a *ClientResolverActor) handleMonitorBoarding(ctx context.Context,
	req *MonitorBoardingRequest) fn.Result[ClientResolverResp] {

	addrStr := req.BoardingAddress.String()

	a.log.InfoS(ctx, "Starting boarding address monitoring",
		"address", addrStr,
		"exit_delay", req.ExitDelay)

	// Check if already monitoring.
	a.mu.RLock()
	_, exists := a.boardingMonitors[addrStr]
	a.mu.RUnlock()

	if exists {
		return fn.Err[ClientResolverResp](
			fmt.Errorf("boarding address %s is already monitored",
				addrStr),
		)
	}

	// Store the monitor handle.
	a.mu.Lock()
	a.boardingMonitors[addrStr] = &BoardingMonitorHandle{
		Address:   req.BoardingAddress,
		PkScript:  req.PkScript,
		ExitDelay: req.ExitDelay,
		Deposits:  nil,
	}
	a.mu.Unlock()

	// TODO: Register for address notifications with chainsource.
	// This would require chainsource to support address monitoring.

	return fn.Ok[ClientResolverResp](&MonitorBoardingResponse{
		MonitorID: addrStr,
	})
}

// handleVTXOSpent processes a VTXO spend notification from a sub-actor.
func (a *ClientResolverActor) handleVTXOSpent(ctx context.Context,
	notification *internalVTXOSpentNotification,
) fn.Result[ClientResolverResp] {

	event := notification.Event

	a.log.InfoS(ctx, "VTXO spent",
		"outpoint", event.VTXOOutpoint.String(),
		"expected", event.ExpectedSpend)

	// Update monitor state.
	a.mu.Lock()
	if handle, exists := a.vtxoMonitors[event.VTXOOutpoint]; exists {
		handle.Spent = true
		handle.ExpectedSpend = event.ExpectedSpend
	}
	a.mu.Unlock()

	// Forward to notification actor.
	a.mu.RLock()
	notifyRef, exists := a.notifyActors[event.VTXOOutpoint]
	a.mu.RUnlock()

	if exists {
		notifyRef.Tell(ctx, event)
	}

	return fn.Ok[ClientResolverResp](nil)
}

// handleCSVTimeout processes a CSV timeout notification.
func (a *ClientResolverActor) handleCSVTimeout(ctx context.Context,
	notification *internalCSVTimeoutNotification,
) fn.Result[ClientResolverResp] {

	event := notification.Event

	a.log.InfoS(ctx, "CSV timeout reached",
		"outpoint", event.VTXOOutpoint.String(),
		"timeout_height", event.TimeoutHeight)

	// Update monitor state.
	a.mu.Lock()
	if handle, exists := a.vtxoMonitors[event.VTXOOutpoint]; exists {
		handle.TimeoutNotified = true
	}
	a.mu.Unlock()

	// Forward to notification actor.
	a.mu.RLock()
	notifyRef, exists := a.notifyActors[event.VTXOOutpoint]
	a.mu.RUnlock()

	if exists {
		notifyRef.Tell(ctx, event)
	}

	return fn.Ok[ClientResolverResp](nil)
}

// IsExpectedSpend checks if a spend was user-initiated.
func (a *ClientResolverActor) IsExpectedSpend(outpoint wire.OutPoint) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.pendingUnrolls[outpoint]
}

// Stop gracefully shuts down the ClientResolverActor.
func (a *ClientResolverActor) Stop() {
	a.cancel()

	// Stop all VTXO monitors.
	a.mu.Lock()
	for _, handle := range a.vtxoMonitors {
		key := actor.NewServiceKey[vtxoMonitorMsg, vtxoMonitorResp](
			handle.ServiceKeyName,
		)
		key.UnregisterAll(a.system)
	}
	a.vtxoMonitors = make(map[wire.OutPoint]*VTXOMonitorHandle)
	a.notifyActors = make(
		map[wire.OutPoint]actor.TellOnlyRef[ClientResolverEvent],
	)
	a.boardingMonitors = make(map[string]*BoardingMonitorHandle)
	a.pendingUnrolls = make(map[wire.OutPoint]bool)
	a.mu.Unlock()
}

package chainresolver

import (
	"context"
	"fmt"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// VTXOMonitorActor monitors a single VTXO for spends and CSV timeout.
type VTXOMonitorActor struct {
	// chainSource is the actor reference for blockchain operations.
	chainSource actor.ActorRef[
		chainsource.ChainSourceMsg,
		chainsource.ChainSourceResp,
	]

	// log is the logger for this actor.
	log btclog.Logger

	// config is the monitoring configuration.
	config *MonitorVTXORequest

	// parent is the reference to the ClientResolverActor.
	parent actor.TellOnlyRef[ClientResolverMsg]

	// selfRef is this actor's reference for callbacks.
	selfRef actor.TellOnlyRef[vtxoMonitorMsg]

	// confirmationHeight is when the VTXO was confirmed (0 if unconfirmed).
	confirmationHeight int32

	// csvTimeoutHeight is when CSV timeout will be reached.
	csvTimeoutHeight int32

	// spent indicates whether the VTXO has been spent.
	spent bool
}

// NewVTXOMonitorActor creates a new VTXOMonitorActor instance.
func NewVTXOMonitorActor(
	chainSource actor.ActorRef[
		chainsource.ChainSourceMsg,
		chainsource.ChainSourceResp,
	],
	log btclog.Logger,
) *VTXOMonitorActor {

	return &VTXOMonitorActor{
		chainSource: chainSource,
		log:         log,
	}
}

// Receive processes incoming messages for the VTXOMonitorActor.
func (a *VTXOMonitorActor) Receive(ctx context.Context,
	msg vtxoMonitorMsg) fn.Result[vtxoMonitorResp] {

	switch m := msg.(type) {
	case *startVTXOMonitorRequest:
		return a.handleStart(ctx, m)

	case *stopVTXOMonitorRequest:
		return a.handleStop(ctx, m)

	case *internalVTXOSpendEvent:
		return a.handleSpendEvent(ctx, m)

	case *internalVTXOBlockEpoch:
		return a.handleBlockEpoch(ctx, m)

	default:
		return fn.Err[vtxoMonitorResp](
			fmt.Errorf("unknown message type: %T", msg),
		)
	}
}

// handleStart initializes monitoring for the VTXO.
func (a *VTXOMonitorActor) handleStart(ctx context.Context,
	req *startVTXOMonitorRequest) fn.Result[vtxoMonitorResp] {

	a.config = req.Config
	a.parent = req.Parent
	a.selfRef = req.SelfRef

	a.log.InfoS(ctx, "Starting VTXO monitoring",
		"outpoint", a.config.VTXOOutpoint.String(),
		"exit_delay", a.config.ExitDelay)

	// Register for spend notifications.
	spendNotifyRef := chainsource.MapSpendEvent(
		a.selfRef, func(event chainsource.SpendEvent) vtxoMonitorMsg {
			return &internalVTXOSpendEvent{Event: event}
		},
	)

	spendReq := &chainsource.RegisterSpendRequest{
		CallerID: fmt.Sprintf(
			"vtxo-monitor.%s", a.config.VTXOOutpoint.String(),
		),
		Outpoint:    &a.config.VTXOOutpoint,
		PkScript:    a.config.VTXOOutput.PkScript,
		HeightHint:  a.config.HeightHint,
		NotifyActor: fn.Some(spendNotifyRef),
	}

	spendFuture := a.chainSource.Ask(ctx, spendReq)
	spendResult := spendFuture.Await(ctx)

	if spendResult.IsErr() {
		return fn.Err[vtxoMonitorResp](
			fmt.Errorf("failed to register spend watch: %w",
				spendResult.Err()),
		)
	}

	// Register for block epoch notifications to track CSV timeout.
	epochNotifyRef := chainsource.MapBlockEpoch(
		a.selfRef, func(epoch chainsource.BlockEpoch) vtxoMonitorMsg {
			return &internalVTXOBlockEpoch{Epoch: epoch}
		},
	)

	epochReq := &chainsource.SubscribeBlocksRequest{
		CallerID: fmt.Sprintf(
			"vtxo-monitor.%s", a.config.VTXOOutpoint.String(),
		),
		NotifyActor: fn.Some(epochNotifyRef),
	}

	epochFuture := a.chainSource.Ask(ctx, epochReq)
	epochResult := epochFuture.Await(ctx)

	if epochResult.IsErr() {
		return fn.Err[vtxoMonitorResp](
			fmt.Errorf("failed to subscribe to blocks: %w",
				epochResult.Err()),
		)
	}

	return fn.Ok[vtxoMonitorResp](&startVTXOMonitorResponse{})
}

// handleStop stops monitoring and cleans up.
func (a *VTXOMonitorActor) handleStop(ctx context.Context,
	_ *stopVTXOMonitorRequest) fn.Result[vtxoMonitorResp] {

	a.log.InfoS(ctx, "Stopping VTXO monitoring",
		"outpoint", a.config.VTXOOutpoint.String())

	// Unregister spend watch.
	callerID := fmt.Sprintf(
		"vtxo-monitor.%s", a.config.VTXOOutpoint.String(),
	)

	unregSpendReq := &chainsource.UnregisterSpendRequest{
		CallerID: callerID,
		Outpoint: &a.config.VTXOOutpoint,
		PkScript: a.config.VTXOOutput.PkScript,
	}
	a.chainSource.Ask(ctx, unregSpendReq).Await(ctx)

	// Unregister block subscription.
	unsubReq := &chainsource.UnsubscribeBlocksRequest{
		CallerID: callerID,
	}
	a.chainSource.Ask(ctx, unsubReq).Await(ctx)

	return fn.Ok[vtxoMonitorResp](&stopVTXOMonitorResponse{})
}

// handleSpendEvent processes a spend event from chainsource.
func (a *VTXOMonitorActor) handleSpendEvent(ctx context.Context,
	event *internalVTXOSpendEvent) fn.Result[vtxoMonitorResp] {

	spendEvent := event.Event

	a.log.InfoS(ctx, "VTXO spend detected",
		"outpoint", a.config.VTXOOutpoint.String(),
		"spending_tx", spendEvent.SpendingTxid.String(),
		"height", spendEvent.SpendingHeight)

	a.spent = true

	// Determine if this was an expected spend (user-initiated unroll).
	// For now, we mark all spends as unexpected unless the parent tells
	// us otherwise. The parent tracks pending unrolls.
	expectedSpend := false

	// Create the spent notification.
	notification := &internalVTXOSpentNotification{
		Event: VTXOSpentEvent{
			VTXOOutpoint:   a.config.VTXOOutpoint,
			SpendingTx:     spendEvent.SpendingTx,
			SpendingHeight: spendEvent.SpendingHeight,
			ExpectedSpend:  expectedSpend,
		},
	}

	// Notify parent.
	a.parent.Tell(ctx, notification)

	return fn.Ok[vtxoMonitorResp](nil)
}

// handleBlockEpoch processes a block epoch event to track CSV timeout.
func (a *VTXOMonitorActor) handleBlockEpoch(ctx context.Context,
	event *internalVTXOBlockEpoch) fn.Result[vtxoMonitorResp] {

	epoch := event.Epoch

	// If we haven't seen the VTXO confirmed yet, check if we now have
	// a confirmation height we can use.
	if a.confirmationHeight == 0 {
		// In a full implementation, we would check if the VTXO is
		// confirmed at or before this height. For now, assume confirmed
		// if we're monitoring it.
		a.confirmationHeight = epoch.Height
		a.csvTimeoutHeight = a.confirmationHeight +
			int32(a.config.ExitDelay)

		a.log.DebugS(ctx, "VTXO confirmation assumed",
			"outpoint", a.config.VTXOOutpoint.String(),
			"confirmation_height", a.confirmationHeight,
			"timeout_height", a.csvTimeoutHeight)
	}

	// Check if CSV timeout has been reached.
	if a.csvTimeoutHeight > 0 && epoch.Height >= a.csvTimeoutHeight {
		if !a.spent {
			a.log.InfoS(ctx, "CSV timeout reached for VTXO",
				"outpoint", a.config.VTXOOutpoint.String(),
				"timeout_height", a.csvTimeoutHeight)

			// Notify parent about timeout.
			notification := &internalCSVTimeoutNotification{
				Event: CSVTimeoutReachedEvent{
					VTXOOutpoint:  a.config.VTXOOutpoint,
					TimeoutHeight: epoch.Height,
				},
			}

			a.parent.Tell(ctx, notification)
		}
	}

	return fn.Ok[vtxoMonitorResp](nil)
}

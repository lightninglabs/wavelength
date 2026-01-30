package unroller

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// UnrollerConfig holds configuration for the unroller actor.
type UnrollerConfig struct {
	// ChainSource for broadcasting and confirmation monitoring.
	ChainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]

	// Store for persisting unroll state.
	Store UnrollStore

	// ChainParams for network configuration.
	ChainParams *chaincfg.Params

	// Logger for structured logging.
	Logger btclog.Logger

	// SelfRef for receiving confirmation events.
	SelfRef actor.TellOnlyRef[UnrollerMsg]
}

// UnrollerActor manages on-chain unrolling of VTXO trees.
type UnrollerActor struct {
	cfg *UnrollerConfig

	// activeUnrolls tracks in-progress unrolls by VTXO outpoint.
	activeUnrolls map[string]*UnrollState
	mu            sync.RWMutex
}

// NewUnrollerActor creates a new unroller actor.
func NewUnrollerActor(cfg *UnrollerConfig) *UnrollerActor {
	return &UnrollerActor{
		cfg:           cfg,
		activeUnrolls: make(map[string]*UnrollState),
	}
}

// Start initializes the actor and recovers any in-progress unrolls.
func (a *UnrollerActor) Start(ctx context.Context) error {
	// Load persisted unroll states.
	states, err := a.cfg.Store.ListActiveUnrolls(ctx)
	if err != nil {
		return fmt.Errorf("list active unrolls: %w", err)
	}

	for _, state := range states {
		a.activeUnrolls[state.VTXOOutpoint.String()] = state

		// Resume unroll from where it left off.
		a.resumeUnroll(ctx, state)
	}

	a.cfg.Logger.InfoS(ctx, "Unroller started",
		slog.Int("recovered_unrolls", len(states)))

	return nil
}

// OnStop performs cleanup when actor is stopped.
func (a *UnrollerActor) OnStop(ctx context.Context) error {
	a.cfg.Logger.InfoS(ctx, "Unroller stopped")
	return nil
}

// Receive processes incoming messages.
func (a *UnrollerActor) Receive(
	ctx context.Context, msg UnrollerMsg,
) fn.Result[UnrollerResp] {

	switch m := msg.(type) {
	case *UnrollRequest:
		return a.handleUnrollRequest(ctx, m)

	case *ConfirmationEvent:
		return a.handleConfirmation(ctx, m)

	case *BlockEpochEvent:
		return a.handleBlockEpoch(ctx, m)

	case *GetUnrollStatusRequest:
		return a.handleGetUnrollStatus(ctx, m)

	default:
		return fn.Err[UnrollerResp](
			fmt.Errorf("unknown message type: %T", msg),
		)
	}
}

// resumeUnroll resumes an unroll that was interrupted.
func (a *UnrollerActor) resumeUnroll(
	ctx context.Context, state *UnrollState,
) {

	a.cfg.Logger.InfoS(ctx, "Resuming unroll",
		slog.String("vtxo", state.VTXOOutpoint.String()),
		slog.String("status", state.Status.String()),
		slog.Int("level", state.CurrentLevel))

	switch state.Status {
	case UnrollStatusBroadcasting:
		// Re-register confirmations for broadcast transactions.
		for txid := range state.BroadcastTxids {
			_, confirmed := state.ConfirmedTxids[txid]
			if !confirmed {
				a.registerConfirmation(ctx, state, txid)
			}
		}

		// Subscribe to block epochs for CSV tracking.
		a.subscribeBlockEpochs(ctx, state)

	case UnrollStatusAwaitingCSV:
		// Subscribe to block epochs to monitor CSV completion.
		a.subscribeBlockEpochs(ctx, state)

	case UnrollStatusComplete, UnrollStatusFailed:
		// Nothing to resume, cleanup will happen naturally.

	default:
		// Unknown or pending status, re-initiate broadcast.
		a.broadcastLevel(ctx, state, state.CurrentLevel)
	}
}

// handleGetUnrollStatus returns the current status of an unroll.
func (a *UnrollerActor) handleGetUnrollStatus(
	ctx context.Context, req *GetUnrollStatusRequest,
) fn.Result[UnrollerResp] {

	a.mu.RLock()
	defer a.mu.RUnlock()

	state, exists := a.activeUnrolls[req.VTXOOutpoint.String()]
	if !exists {
		return fn.Err[UnrollerResp](
			fmt.Errorf("unroll not found: %v", req.VTXOOutpoint),
		)
	}

	resp := &UnrollStatusResp{
		Status:       state.Status,
		CurrentLevel: state.CurrentLevel,
		TotalLevels:  len(state.LevelOrder),
	}

	return fn.Ok[UnrollerResp](resp)
}

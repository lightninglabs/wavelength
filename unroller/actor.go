package unroller

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
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

	// WalletKit provides wallet operations for CPFP child
	// construction: UTXO selection, change address generation,
	// and PSBT signing.
	WalletKit WalletKit
}

// UnrollerActor manages on-chain unrolling of VTXO trees.
//
// All state access is serialized through the actor framework's Receive
// method which runs in a single goroutine, so no mutex is needed.
type UnrollerActor struct {
	cfg *UnrollerConfig

	// activeUnrolls tracks in-progress unrolls by VTXO outpoint.
	activeUnrolls map[string]*UnrollState

	// txidToUnroll provides O(1) reverse lookup from transaction hash
	// to the VTXO outpoint key of the owning unroll. Populated during
	// broadcastLevel and cleaned up on completion or failure.
	txidToUnroll map[chainhash.Hash]string

	// bestHeight tracks the latest observed block height, updated
	// from BlockEpochEvent messages. Used to compute BlocksRemaining
	// in status responses.
	bestHeight int32
}

// NewUnrollerActor creates a new unroller actor.
func NewUnrollerActor(cfg *UnrollerConfig) *UnrollerActor {
	return &UnrollerActor{
		cfg:           cfg,
		activeUnrolls: make(map[string]*UnrollState),
		txidToUnroll:  make(map[chainhash.Hash]string),
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
		outpointKey := state.VTXOOutpoint.String()
		a.activeUnrolls[outpointKey] = state

		// Index all known txids for O(1) lookup in
		// handleConfirmation.
		a.indexUnrollTxids(state)

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
		// Re-register confirmations for broadcast transactions
		// that haven't confirmed yet. When they confirm,
		// handleConfirmation will trigger the next level
		// broadcast directly (no inter-level CSV).
		for txid := range state.BroadcastTxids {
			_, confirmed := state.ConfirmedTxids[txid]
			if !confirmed {
				a.registerConfirmation(ctx, state, txid)
			}
		}

	case UnrollStatusAwaitingCSV:
		// Subscribe to block epochs to monitor CSV completion.
		a.subscribeBlockEpochs(ctx, state)

	case UnrollStatusComplete, UnrollStatusFailed:
		// Nothing to resume, cleanup will happen naturally.

	case UnrollStatusPending:
		// Pending means broadcast never started, begin from the
		// current level.
		a.broadcastLevel(ctx, state, state.CurrentLevel)

	default:
		a.cfg.Logger.WarnS(ctx, "Unknown unroll status on resume",
			nil,
			slog.String("vtxo", state.VTXOOutpoint.String()),
			slog.String("status", state.Status.String()))
	}
}

// indexUnrollTxids populates the txid reverse-lookup map for all
// transactions in the given unroll's level order.
func (a *UnrollerActor) indexUnrollTxids(state *UnrollState) {
	outpointKey := state.VTXOOutpoint.String()

	for _, level := range state.LevelOrder {
		for _, txid := range level.Txids {
			a.txidToUnroll[txid] = outpointKey
		}
	}
}

// cleanupUnrollTxids removes all txid reverse-lookup entries for the
// given unroll.
func (a *UnrollerActor) cleanupUnrollTxids(state *UnrollState) {
	for _, level := range state.LevelOrder {
		for _, txid := range level.Txids {
			delete(a.txidToUnroll, txid)
		}
	}
}

// handleGetUnrollStatus returns the current status of an unroll.
func (a *UnrollerActor) handleGetUnrollStatus(
	ctx context.Context, req *GetUnrollStatusRequest,
) fn.Result[UnrollerResp] {

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

	// Compute remaining blocks when awaiting CSV.
	if state.Status == UnrollStatusAwaitingCSV &&
		state.LeafConfirmHeight > 0 && a.bestHeight > 0 {

		csvTarget := state.LeafConfirmHeight +
			int32(state.VTXO.Expiry)

		remaining := csvTarget - a.bestHeight
		if remaining < 0 {
			remaining = 0
		}

		resp.BlocksRemaining = remaining
	}

	return fn.Ok[UnrollerResp](resp)
}

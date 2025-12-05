package vtxo

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/round"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// VTXOActorServiceKey constructs a service key for a VTXO actor based on its
// outpoint. This enables the round actor to send messages directly to specific
// VTXO actors without routing through the manager.
func VTXOActorServiceKey(outpoint wire.OutPoint) actor.ServiceKey[
	VTXOEvent, VTXOActorResponse,
] {
	return actor.NewServiceKey[VTXOEvent, VTXOActorResponse](
		fmt.Sprintf("vtxo.%s", outpoint.String()),
	)
}

// VTXOActorConfig holds configuration for a single VTXO actor.
type VTXOActorConfig struct {
	VTXO        *Descriptor
	Store       VTXOStore
	Wallet      VTXOWallet
	ChainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]
	ChainParams  *chaincfg.Params
	ExpiryConfig *ExpiryConfig
	Logger       btclog.Logger

	// RoundActor receives refresh requests and forfeit signatures from this
	// VTXO actor.
	RoundActor actor.TellOnlyRef[round.ClientMsg]

	// ChainResolver receives expiring notifications for unilateral exit.
	ChainResolver actor.TellOnlyRef[ExpiringNotification]

	// Manager receives termination notifications for cleanup.
	Manager actor.TellOnlyRef[ManagerMsg]
}

// VTXOActor manages the lifecycle of a single VTXO. It processes events using
// the FSM state machine pattern, subscribes to block epochs for expiry
// monitoring, and returns outbox messages for the caller to dispatch.
type VTXOActor struct {
	cfg   *VTXOActorConfig
	state VTXOState
	env   *VTXOEnvironment

	selfRef actor.TellOnlyRef[VTXOEvent]
}

// NewVTXOActor creates a new VTXO actor with the given configuration.
func NewVTXOActor(cfg *VTXOActorConfig) *VTXOActor {
	actorID := fmt.Sprintf("vtxo.%s", cfg.VTXO.Outpoint.String())
	env := NewVTXOEnvironment(
		actorID, cfg.Store, cfg.Wallet, cfg.ExpiryConfig,
		cfg.ChainParams,
	)

	return &VTXOActor{
		cfg:   cfg,
		state: statusToState(cfg.VTXO),
		env:   env,
	}
}

// Start initializes the actor and subscribes to block epochs.
func (a *VTXOActor) Start(ctx context.Context,
	selfRef actor.TellOnlyRef[VTXOEvent]) error {

	a.selfRef = selfRef

	// Don't subscribe to epochs if already in a terminal state.
	if a.state.IsTerminal() {
		return nil
	}

	return a.subscribeBlockEpochs(ctx)
}

// Stop unsubscribes from block epochs.
func (a *VTXOActor) Stop(ctx context.Context) {
	a.unsubscribeBlockEpochs(ctx)
}

// Receive processes incoming events and returns outbox messages for dispatch.
func (a *VTXOActor) Receive(ctx context.Context,
	event VTXOEvent) fn.Result[VTXOActorResponse] {

	transition, err := a.state.ProcessEvent(ctx, event, a.env)
	if err != nil {
		return fn.Err[VTXOActorResponse](fmt.Errorf("process event: %w", err))
	}

	priorState := a.state

	// Type assert the next state to VTXOState.
	nextState, ok := transition.NextState.(VTXOState)
	if !ok {
		return fn.Err[VTXOActorResponse](fmt.Errorf(
			"unexpected state type: %T", transition.NextState,
		))
	}
	a.state = nextState

	// Unsubscribe from block epochs when reaching terminal state.
	if a.state.IsTerminal() && !priorState.IsTerminal() {
		a.unsubscribeBlockEpochs(ctx)
	}

	// Extract outbox messages for caller to dispatch.
	var outbox []VTXOOutMsg
	transition.NewEvents.WhenSome(func(emitted VTXOEmittedEvent) {
		outbox = emitted.Outbox
	})

	// Process persistence updates immediately.
	a.processOutbox(ctx, outbox)

	return fn.Ok(VTXOActorResponse{
		PriorState: priorState,
		NewState:   a.state,
		Outbox:     outbox,
	})
}

// processOutbox routes outbox messages to their destinations. This includes
// persistence updates, messages to the round actor, chain resolver, and
// manager for cleanup.
func (a *VTXOActor) processOutbox(ctx context.Context, outbox []VTXOOutMsg) {
	for _, msg := range outbox {
		switch m := msg.(type) {
		case *VTXOStatusUpdate:
			err := a.cfg.Store.UpdateVTXOStatus(ctx, m.Outpoint, m.NewStatus)
			if err != nil {
				a.cfg.Logger.ErrorS(ctx,
					"Failed to update VTXO status", err,
					slog.String("outpoint", m.Outpoint.String()),
					slog.String("status", m.NewStatus.String()),
				)
			}

		case *RefreshRequest:
			// Route refresh request to round actor.
			if a.cfg.RoundActor != nil {
				a.cfg.RoundActor.Tell(ctx, &round.RefreshVTXORequest{
					VTXOOutpoint: m.VTXOOutpoint,
					Amount:       m.Amount,
				})
				a.cfg.Logger.InfoS(ctx, "Sent refresh request",
					slog.String("outpoint", m.VTXOOutpoint.String()),
					slog.String("urgency", m.Urgency.String()),
				)
			}

		case *ForfeitSignatureSubmission:
			// Route forfeit signature to round actor.
			if a.cfg.RoundActor != nil {
				resp := &round.ForfeitSignatureResponse{
					VTXOOutpoint: m.VTXOOutpoint,
					RoundID:      m.RoundID,
					ForfeitTx:    m.ForfeitTx,
					Signature:    m.Signature,
				}
				a.cfg.RoundActor.Tell(ctx, resp)
				a.cfg.Logger.InfoS(
					ctx, "Sent forfeit signature",
					slog.String("outpoint", m.VTXOOutpoint.String()),
					slog.String("round_id", m.RoundID),
				)
			}

		case *ExpiringNotification:
			// Route to chain resolver for unilateral exit handling.
			if a.cfg.ChainResolver != nil {
				a.cfg.ChainResolver.Tell(ctx, *m)
				a.cfg.Logger.WarnS(
					ctx, "VTXO sent to chain resolver", nil,
					slog.String("outpoint", m.VTXO.Outpoint.String()),
					slog.Int("blocks_remaining", int(m.BlocksRemaining)),
				)
			}

		case *VTXOTerminatedNotification:
			// Notify manager to remove this actor from tracking.
			if a.cfg.Manager != nil {
				a.cfg.Manager.Tell(ctx, &VTXOTerminatedMsg{
					Outpoint:   m.VTXOOutpoint,
					FinalState: m.FinalState,
					Reason:     m.Reason,
				})
			}
		}
	}
}

// subscribeBlockEpochs registers for block notifications with chainsource.
func (a *VTXOActor) subscribeBlockEpochs(ctx context.Context) error {
	callerID := fmt.Sprintf("vtxo.%s", a.cfg.VTXO.Outpoint.String())

	epochRef := chainsource.MapBlockEpoch(a.selfRef,
		func(epoch chainsource.BlockEpoch) VTXOEvent {
			return &BlockEpochEvent{
				Height:    epoch.Height,
				Hash:      epoch.Hash,
				Timestamp: epoch.Timestamp,
			}
		},
	)

	req := &chainsource.SubscribeBlocksRequest{
		CallerID:    callerID,
		NotifyActor: fn.Some(epochRef),
	}

	future := a.cfg.ChainSource.Ask(ctx, req)
	result := future.Await(ctx)
	if result.IsErr() {
		return fmt.Errorf("subscribe blocks: %w", result.Err())
	}

	a.cfg.Logger.DebugS(ctx, "Subscribed to block epochs",
		slog.String("vtxo", a.cfg.VTXO.Outpoint.String()))

	return nil
}

// unsubscribeBlockEpochs cancels the block epoch subscription.
func (a *VTXOActor) unsubscribeBlockEpochs(ctx context.Context) {
	callerID := fmt.Sprintf("vtxo.%s", a.cfg.VTXO.Outpoint.String())

	a.cfg.ChainSource.Tell(ctx, &chainsource.UnsubscribeBlocksRequest{
		CallerID: callerID,
	})

	a.cfg.Logger.DebugS(ctx, "Unsubscribed from block epochs",
		slog.String("vtxo", a.cfg.VTXO.Outpoint.String()))
}

// CurrentState returns the actor's current FSM state.
func (a *VTXOActor) CurrentState() VTXOState {
	return a.state
}

// VTXOActorResponse is returned from processing an event.
type VTXOActorResponse struct {
	PriorState VTXOState
	NewState   VTXOState
	Outbox     []VTXOOutMsg
}

// statusToState converts a persisted VTXOStatus to the appropriate FSM state.
func statusToState(vtxo *Descriptor) VTXOState {
	switch vtxo.Status {
	case VTXOStatusLive:
		return &LiveState{
			VTXO:              vtxo,
			LastCheckedHeight: vtxo.CreatedHeight,
		}

	case VTXOStatusRefreshRequested:
		return &RefreshRequestedState{VTXO: vtxo, RequestedAtHeight: 0}

	case VTXOStatusForfeiting:
		return &ForfeitingState{VTXO: vtxo, NewRoundID: vtxo.RoundID}

	case VTXOStatusForfeited:
		return &ForfeitedState{VTXO: vtxo, NewRoundID: vtxo.RoundID}

	case VTXOStatusExpiring:
		return &ExpiringState{
			VTXO:   vtxo,
			Reason: "recovered from storage",
		}

	case VTXOStatusFailed:
		return &FailedState{
			VTXO:   vtxo,
			Reason: "recovered from storage",
		}

	default:
		return &LiveState{
			VTXO:              vtxo,
			LastCheckedHeight: vtxo.CreatedHeight,
		}
	}
}

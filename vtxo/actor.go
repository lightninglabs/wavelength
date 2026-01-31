package vtxo

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/round"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// VTXOActorServiceKey returns the service key for looking up a VTXO actor.
// This delegates to actormsg.VTXOActorServiceKey to ensure both packages use
// the same key for registration and lookup, avoiding type mismatches.
func VTXOActorServiceKey(outpoint wire.OutPoint) actor.ServiceKey[
	actormsg.VTXOActorMsg, actormsg.VTXOActorResp,
] {

	return actormsg.VTXOActorServiceKey(outpoint)
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

	selfRef actor.TellOnlyRef[actormsg.VTXOActorMsg]
}

// NewVTXOActor creates a new VTXO actor with the given configuration. For
// actors being recovered from storage (e.g., in forfeiting state), this
// fetches persisted data like the forfeit tx.
func NewVTXOActor(ctx context.Context, cfg *VTXOActorConfig) *VTXOActor {
	actorID := fmt.Sprintf("vtxo.%s", cfg.VTXO.Outpoint.String())
	env := NewVTXOEnvironment(
		actorID, cfg.Store, cfg.Wallet, cfg.ExpiryConfig,
		cfg.ChainParams,
	)

	return &VTXOActor{
		cfg:   cfg,
		state: statusToState(ctx, cfg.VTXO, cfg.Store, cfg.Logger),
		env:   env,
	}
}

// Start initializes the actor and subscribes to block epochs.
func (a *VTXOActor) Start(ctx context.Context,
	selfRef actor.TellOnlyRef[actormsg.VTXOActorMsg]) error {

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
// The return type is actormsg.VTXOActorResp (interface) which VTXOActorResponse
// implements via the VTXOActorResp() marker method.
func (a *VTXOActor) Receive(ctx context.Context,
	event actormsg.VTXOActorMsg) fn.Result[actormsg.VTXOActorResp] {

	a.cfg.Logger.DebugS(ctx, "VTXO actor received event",
		slog.String("event_type", fmt.Sprintf("%T", event)),
		slog.String("outpoint", a.cfg.VTXO.Outpoint.String()),
		slog.String("current_state", fmt.Sprintf("%T", a.state)))

	vtxoEvent, ok := event.(VTXOEvent)
	if !ok {
		return fn.Err[actormsg.VTXOActorResp](
			fmt.Errorf("unexpected event type: %T", event),
		)
	}

	transition, err := a.state.ProcessEvent(ctx, vtxoEvent, a.env)
	if err != nil {
		a.cfg.Logger.ErrorS(ctx, "VTXO FSM ProcessEvent failed", err,
			slog.String("event_type", fmt.Sprintf("%T", vtxoEvent)),
			slog.String("outpoint", a.cfg.VTXO.Outpoint.String()))

		return fn.Err[actormsg.VTXOActorResp](
			fmt.Errorf("process event: %w", err),
		)
	}

	// Log transition details.
	var outboxLen int
	transition.NewEvents.WhenSome(func(emitted VTXOEmittedEvent) {
		outboxLen = len(emitted.Outbox)
	})
	a.cfg.Logger.DebugS(ctx, "VTXO FSM transition completed",
		slog.String("next_state", fmt.Sprintf("%T", transition.NextState)),
		slog.Int("outbox_len", outboxLen))

	priorState := a.state

	// Type assert the next state to VTXOState.
	nextState, ok := transition.NextState.(VTXOState)
	if !ok {
		return fn.Err[actormsg.VTXOActorResp](fmt.Errorf(
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

	return fn.Ok[actormsg.VTXOActorResp](VTXOActorResponse{
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
			// For forfeiting status with a forfeit tx, use
			// MarkForfeiting to persist both status and the signed
			// tx for crash recovery.
			var err error
			isForfeitingWithTx :=
				m.NewStatus == VTXOStatusForfeiting &&
					m.ForfeitTx != nil

			a.cfg.Logger.DebugS(ctx, "Processing VTXOStatusUpdate",
				slog.String("outpoint", m.Outpoint.String()),
				slog.String("new_status", m.NewStatus.String()),
				slog.Bool("has_forfeit_tx", m.ForfeitTx != nil),
				slog.Bool("is_forfeiting_with_tx", isForfeitingWithTx))

			if isForfeitingWithTx {
				err = a.cfg.Store.MarkForfeiting(
					ctx, m.Outpoint, m.RoundID, m.ForfeitTx,
				)
				a.cfg.Logger.DebugS(
					ctx, "Called MarkForfeiting",
					slog.String("outpoint", m.Outpoint.String()),
					slog.String("round_id", m.RoundID),
					slog.Bool("error", err != nil),
				)
			} else {
				err = a.cfg.Store.UpdateVTXOStatus(
					ctx, m.Outpoint, m.NewStatus,
				)
			}
			if err != nil {
				a.cfg.Logger.ErrorS(ctx,
					"Failed to update VTXO status", err,
					slog.String("outpoint", m.Outpoint.String()),
					slog.String("status", m.NewStatus.String()),
				)
			}

		case *RefreshRequest:
			// Route refresh request to round actor. This tells
			// the round to include this VTXO in the forfeit flow.
			// We also send a separate VTXORequest to explicitly
			// request a new VTXO for the refreshed value (refresh
			// no longer automatically creates a replacement VTXO).
			if a.cfg.RoundActor != nil {
				vtxo := a.cfg.VTXO

				// Send the refresh request for forfeit flow.
				refreshReq := &round.RefreshVTXORequest{
					VTXOOutpoint: m.VTXOOutpoint,
					Amount:       m.Amount,
					NewVTXOKey:   vtxo.ClientKey.PubKey,
					PkScript:     vtxo.PkScript,
					OperatorKey:  vtxo.OperatorKey,
					Expiry:       vtxo.RelativeExpiry,
					SigningKey:   vtxo.ClientKey,
				}
				a.cfg.RoundActor.Tell(ctx, refreshReq)

				// Also send a VTXORequest to request the new
				// VTXO output for this refresh.
				amt := btcutil.Amount(m.Amount)
				vtxoReq := &round.VTXORequestsReceived{
					Requests: []types.VTXORequest{{
						Amount:   amt,
						PkScript: vtxo.PkScript,
						Expiry:   vtxo.RelativeExpiry,
						ClientKey: vtxo.ClientKey.
							PubKey,
						OperatorKey: vtxo.OperatorKey,
						SigningKey:  vtxo.ClientKey,
					}},
				}
				a.cfg.RoundActor.Tell(ctx, vtxoReq)

				a.cfg.Logger.InfoS(
					ctx, "Sent refresh and VTXO request",
					slog.String(
						"outpoint",
						m.VTXOOutpoint.String(),
					),
					slog.String(
						"urgency", m.Urgency.String(),
					),
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
		func(epoch chainsource.BlockEpoch) actormsg.VTXOActorMsg {
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

// VTXOActorResp implements actormsg.VTXOActorResp marker interface.
func (VTXOActorResponse) VTXOActorResp() {}

// statusToState converts a persisted VTXOStatus to the appropriate FSM state.
// For forfeiting state, it fetches the persisted forfeit tx for crash recovery.
func statusToState(
	ctx context.Context, vtxo *Descriptor, store VTXOStore, logger btclog.Logger,
) VTXOState {

	switch vtxo.Status {
	case VTXOStatusLive:
		return &LiveState{
			VTXO:              vtxo,
			LastCheckedHeight: vtxo.CreatedHeight,
		}

	case VTXOStatusRefreshRequested:
		return &RefreshRequestedState{VTXO: vtxo, RequestedAtHeight: 0}

	case VTXOStatusForfeiting:
		// Fetch the persisted forfeit tx for crash recovery.
		var forfeitTx *wire.MsgTx
		if store != nil {
			tx, err := store.GetForfeitTx(ctx, vtxo.Outpoint)
			if err != nil && logger != nil {
				logger.WarnS(
					ctx, "Could not get forfeit tx", err,
					slog.String("outpoint", vtxo.Outpoint.String()),
				)
			}
			forfeitTx = tx
		}

		return &ForfeitingState{
			VTXO:       vtxo,
			NewRoundID: vtxo.RoundID,
			ForfeitTx:  forfeitTx,
		}

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

package vtxo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/build"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/round"
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

// RefreshFeeQuoter is the VTXO actor's hook into the operator's
// EstimateFee RPC. Under the seal-time fee handshake (#270) the
// quoter is advisory only: its return value is carried on
// RefreshVTXORequest.OperatorFee as a hint for observability and
// later decision-making (e.g. "is the projected fee acceptable
// right now, or should I defer this refresh until non-critical"),
// but it is NOT subtracted from the new VTXO output or otherwise
// persisted into the intent. The server computes the authoritative
// per-input fee at seal time via computeSealTimeQuotes and the
// client's refresh VTXO is marked IsChange=true so the resulting
// residual lands on the new output automatically.
//
// A zero return is valid and means "I don't have a live quote" —
// the refresh still goes through; the server decides the fee at
// seal time and the client's MaxOperatorFee cap in
// QuoteReceivedState is the authoritative upper bound, not this
// value.
type RefreshFeeQuoter func(ctx context.Context,
	amount btcutil.Amount, remainingBlocks uint32) btcutil.Amount

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

	// Log is an optional logger for this actor instance. If None, the
	// actor falls back to extracting a logger from context via
	// LoggerFromContext, or uses btclog.Disabled if no logger is found.
	Log fn.Option[btclog.Logger]

	// ChainResolver receives expiring notifications for unilateral exit.
	ChainResolver actor.TellOnlyRef[ExpiringNotification]

	// Manager receives relay messages and termination notifications.
	// The VTXO actor routes round-bound signals through the manager
	// rather than holding a direct round actor reference.
	Manager actor.TellOnlyRef[ManagerMsg]

	// LedgerSink is an optional reference to the client-side
	// ledger accounting actor. The VTXO actor does not know the
	// confirmed exit miner fee; unroll emits ExitCostMsg once the
	// final sweep confirms.
	LedgerSink fn.Option[ledger.Sink]

	// RefreshFeeQuoter, when set, is invoked on every auto-refresh
	// emission so the relayed RefreshVTXORequest.OperatorFee field
	// carries an advisory hint about the server's projected fee.
	// Under the seal-time fee handshake (#270) this value is
	// observability only — the server decides the authoritative
	// fee at seal time and the client accepts or rejects via the
	// QuoteReceivedState MaxOperatorFee cap. When nil, the actor
	// emits RefreshVTXORequest with OperatorFee=0, which is fine:
	// the seal-time quote is still the source of truth.
	RefreshFeeQuoter RefreshFeeQuoter

	// FetchOperatorKey, when set, returns the operator's current
	// long-term public key by issuing a fresh GetInfo round-trip to
	// the operator at the moment of an auto-refresh emission. The
	// fetched key is used to build the NEW VTXO output's policy
	// template; the input VTXO's stored operator key is intentionally
	// not reused for the new output because VTXOs commit to their
	// operator key for their entire lifetime, and the new output is a
	// fresh VTXO whose operator key is chosen at join time.
	//
	// A nil callback causes refreshOutputTemplate to fall back to the
	// descriptor's stored bytes (harness paths and pre-fix behavior).
	// A non-nil callback that errors propagates the error so the
	// refresh fails loudly rather than silently emitting against a
	// stale key; the next expiry tick will retry.
	FetchOperatorKey func(context.Context) (*btcec.PublicKey, error)

	// ForfeitParticipantSigner, when set, obtains keyed signatures
	// from non-local participants for custom VTXO policies. The hook is
	// called after connector assignment, so signatures bind the exact
	// forfeit transaction that will be submitted to the operator.
	ForfeitParticipantSigner ForfeitParticipantSigner
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
		cfg.ChainParams, cfg.ForfeitParticipantSigner,
	)

	logger := cfg.Log.UnwrapOr(build.LoggerFromContext(ctx))

	return &VTXOActor{
		cfg:   cfg,
		state: statusToState(ctx, cfg.VTXO, cfg.Store, logger),
		env:   env,
	}
}

// logger returns the configured logger or falls back to extracting from
// context. If no logger is found in either location, returns btclog.Disabled.
func (a *VTXOActor) logger(ctx context.Context) btclog.Logger {
	return a.cfg.Log.UnwrapOr(build.LoggerFromContext(ctx))
}

// refreshOutputTemplate returns the policy template the auto-refresh emission
// should attach to the relayed RefreshVTXORequest for the NEW VTXO output.
//
// The new output is a freshly-minted VTXO whose operator key is chosen at
// join time — VTXOs commit to their operator key for life, so the input
// VTXO's stored key must not leak into the new output. When the
// FetchOperatorKey seam is wired, the actor issues a fresh GetInfo and uses
// the returned key to rebuild the standard template. For non-standard shapes
// (vHTLC etc.) the rebuild surface is unavailable and the actor falls back
// to the descriptor's stored bytes, accepting that a key rotation across
// those VTXOs will still be rejected by the rounds validator. When the seam
// is unset the actor also falls back, which keeps harness paths working
// unchanged.
func (a *VTXOActor) refreshOutputTemplate(ctx context.Context,
	vtxo *Descriptor) ([]byte, error) {

	if a.cfg.FetchOperatorKey == nil {
		return vtxo.EffectivePolicyTemplate()
	}

	currentKey, err := a.cfg.FetchOperatorKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch current operator key: %w", err)
	}
	if currentKey == nil {
		return nil, fmt.Errorf("fetch current operator key: nil key " +
			"returned")
	}

	rebuilt, err := vtxo.RefreshOutputTemplate(currentKey)
	if err != nil {
		// Non-standard policy: keep the stored bytes. The server may
		// still accept the request if the operator key happens to
		// match; if not, the rounds validator will reject it and the
		// caller will see the same error surface as before the fix.
		if errors.Is(err, ErrRefreshOperatorKeyUnsupported) {
			return vtxo.EffectivePolicyTemplate()
		}

		return nil, err
	}

	return rebuilt, nil
}

// emitExitCost is the VTXO-actor entry point for unilateral-exit accounting.
// It is intentionally empty: the VTXO actor hands off to unroll before the
// final sweep is built, so it never sees the confirmed miner fee or height.
// Unroll emits ExitCostMsg after the final sweep confirms.
func (a *VTXOActor) emitExitCost(ctx context.Context,
	notif *ExpiringNotification) {

	// Intentionally empty. See docstring.
	_ = ctx
	_ = notif
}

// tellManager sends a message to the manager. All outbound signals from
// the VTXO actor are routed through this single point.
func (a *VTXOActor) tellManager(ctx context.Context, msg ManagerMsg) {
	if a.cfg.Manager == nil {
		return
	}

	if err := a.cfg.Manager.Tell(ctx, msg); err != nil {
		a.logger(ctx).WarnS(ctx, "Failed to tell manager",
			err,
			slog.String("msg_type", fmt.Sprintf("%T", msg)),
			slog.String("outpoint", a.cfg.VTXO.Outpoint.String()),
		)
	}
}

// quoteRefreshFee asks the configured RefreshFeeQuoter for this
// VTXO's projected operator fee at the observed block height, so
// the auto-refresh RefreshVTXORequest carries an advisory hint for
// observability. Under the seal-time fee handshake (#270) the
// returned value is NOT persisted into the intent or subtracted
// from the new VTXO's amount — the server decides the authoritative
// fee at seal time via computeSealTimeQuotes, and the client's
// MaxOperatorFee cap in QuoteReceivedState is the upper bound the
// FSM enforces against that value.
//
// lastCheckedHeight is the height observed by the FSM when it
// emitted the ForfeitRequest outbox message, carried on the
// message itself so this helper does not have to look at
// a.state — by the time processOutbox runs a.state has already
// transitioned past LiveState.
//
// Returns zero when no quoter is configured (tests, or operators
// running a zero fee schedule) or when the quoter itself returns
// zero. A zero advisory does not suppress the refresh — the
// refresh still goes through and the server fills in the residual
// at seal time.
func (a *VTXOActor) quoteRefreshFee(ctx context.Context, vtxo *Descriptor,
	lastCheckedHeight int32) btcutil.Amount {

	if a.cfg.RefreshFeeQuoter == nil {
		return 0
	}

	// Compute remaining blocks until batch expiry from the height
	// the FSM observed when it emitted the ForfeitRequest. Clamp
	// non-positive differences to zero; the server's EstimateFee
	// treats zero as the SweepDelay default, which slightly
	// over-quotes but still validates because implicit_fee >=
	// expected.
	var remainingBlocks uint32
	if lastCheckedHeight > 0 &&
		vtxo.BatchExpiry > lastCheckedHeight {

		remainingBlocks = uint32(
			vtxo.BatchExpiry - lastCheckedHeight,
		)
	}

	return a.cfg.RefreshFeeQuoter(
		ctx, vtxo.Amount, remainingBlocks,
	)
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

	a.logger(ctx).DebugS(ctx, "VTXO actor received event",
		slog.String("event_type", fmt.Sprintf("%T", event)),
		slog.String("outpoint", a.cfg.VTXO.Outpoint.String()),
		slog.String("current_state", fmt.Sprintf("%T", a.state)),
	)

	vtxoEvent, ok := event.(VTXOEvent)
	if !ok {
		return fn.Err[actormsg.VTXOActorResp](
			fmt.Errorf("unexpected event type: %T", event),
		)
	}

	transition, err := a.state.ProcessEvent(ctx, vtxoEvent, a.env)
	if err != nil {
		a.logger(ctx).ErrorS(ctx, "VTXO FSM ProcessEvent failed",
			err,
			slog.String("event_type", fmt.Sprintf("%T", vtxoEvent)),
			slog.String("outpoint", a.cfg.VTXO.Outpoint.String()),
		)

		return fn.Err[actormsg.VTXOActorResp](
			fmt.Errorf("process event: %w", err),
		)
	}

	// Log transition details.
	var outboxLen int
	transition.NewEvents.WhenSome(func(emitted VTXOEmittedEvent) {
		outboxLen = len(emitted.Outbox)
	})
	a.logger(ctx).DebugS(ctx, "VTXO FSM transition completed",
		slog.String(
			"next_state", fmt.Sprintf("%T", transition.NextState),
		),
		slog.Int("outbox_len", outboxLen),
	)

	priorState := a.state

	// Type assert the next state to VTXOState.
	nextState, ok := transition.NextState.(VTXOState)
	if !ok {
		return fn.Err[actormsg.VTXOActorResp](
			fmt.Errorf("unexpected state type: %T",
				transition.NextState),
		)
	}
	// Extract outbox messages for caller to dispatch.
	var outbox []VTXOOutMsg
	transition.NewEvents.WhenSome(func(emitted VTXOEmittedEvent) {
		outbox = emitted.Outbox
	})

	// Process persistence updates before publishing the in-memory state
	// transition. If the durable write fails, callers must see an error
	// and retries must re-drive the original state rather than observing
	// a terminal state that never made it to disk.
	if err := a.processOutbox(ctx, outbox); err != nil {
		return fn.Err[actormsg.VTXOActorResp](err)
	}

	a.state = nextState

	// Unsubscribe from block epochs when reaching terminal state.
	if a.state.IsTerminal() && !priorState.IsTerminal() {
		a.unsubscribeBlockEpochs(ctx)
	}

	return fn.Ok[actormsg.VTXOActorResp](VTXOActorResponse{
		PriorState: priorState,
		NewState:   a.state,
		Outbox:     outbox,
	})
}

// processOutbox routes outbox messages to their destinations. This includes
// persistence updates, messages to the round actor, chain resolver, and
// manager for cleanup. Status updates are always persisted before any
// notification side effects so persistence failures leave the actor state
// unchanged and retryable.
func (a *VTXOActor) processOutbox(ctx context.Context,
	outbox []VTXOOutMsg) error {

	// Persist status updates first so a DB failure causes Receive to
	// return an error before applying the in-memory state transition.
	for _, msg := range outbox {
		statusUpdate, ok := msg.(*VTXOStatusUpdate)
		if !ok {
			continue
		}

		if err := a.processStatusUpdate(ctx, statusUpdate); err != nil {
			return err
		}
	}

	for _, msg := range outbox {
		switch m := msg.(type) {
		case *VTXOStatusUpdate:
			continue

		case *ForfeitRequest:
			// Relay forfeit request through the manager. The
			// manager forwards it to the round actor. We build
			// the round-specific message here since the VTXO
			// actor has the descriptor data needed.
			vtxo := a.cfg.VTXO
			policyTemplate, err := a.refreshOutputTemplate(
				ctx, vtxo,
			)
			if err != nil {
				// WarnS, not ErrorS: this can fail because
				// the FetchOperatorKey callback returned an
				// error (operator unreachable, fresh GetInfo
				// timed out) — an external trigger, not an
				// internal bug. The next expiry tick will
				// retry; skipping this emission is the right
				// local behavior.
				a.logger(ctx).WarnS(
					ctx,
					"Failed to build refresh output "+
						"template",
					err,
					slog.String(
						"outpoint",
						vtxo.Outpoint.String(),
					),
				)

				continue
			}

			// Quote the operator fee for this VTXO so the
			// RefreshVTXORequest.OperatorFee field carries an
			// advisory hint to downstream emitters under the
			// seal-time fee handshake (#270). The server is the
			// authoritative fee source — it fills the new VTXO
			// amount at seal time via computeSealTimeQuotes —
			// so the value returned here is observability only
			// and is not persisted into the intent shape.
			operatorFee := a.quoteRefreshFee(
				ctx, vtxo, m.LastCheckedHeight,
			)

			refreshReq := &round.RefreshVTXORequest{
				VTXOOutpoint:        m.VTXOOutpoint,
				Amount:              int64(vtxo.Amount),
				TriggerRegistration: true,
				OperatorFee:         int64(operatorFee),
				PolicyTemplate:      policyTemplate,
				OwnerKey:            vtxo.ClientKey,
				SigningKey:          vtxo.ClientKey,
			}
			a.tellManager(ctx, &RelayToRoundMsg{
				Payload: refreshReq,
			})

		case *ForfeitSignatureSubmission:
			resp := &round.ForfeitSignatureResponse{
				VTXOOutpoint:        m.VTXOOutpoint,
				RoundID:             m.RoundID,
				ForfeitTx:           m.ForfeitTx,
				Signature:           m.Signature,
				ParticipantVTXOSigs: m.ParticipantVTXOSigs,
				SpendPath:           m.SpendPath,
			}

			// Forfeit signatures are produced after an async
			// round->VTXO handoff. Detach the enqueue context so
			// the manager relay does not depend on the original
			// server-message handler still being live.
			notifyCtx := context.WithoutCancel(ctx)
			a.tellManager(notifyCtx, &RelayToRoundMsg{
				Payload: resp,
			})

		case *ExpiringNotification:
			// Route directly to chain resolver for unilateral
			// exit handling. We strip the FSM transition's
			// per-message processCtx via context.WithoutCancel
			// so the registry handoff outlives this Receive
			// invocation: the processCtx fires its cancel as
			// soon as Receive returns, and a manual
			// ForceUnrollEvent path triggered by an RPC Ask
			// would otherwise have its admission context
			// canceled mid-enqueue and the unroll job
			// silently dropped.
			if a.cfg.ChainResolver != nil {
				notifyCtx := context.WithoutCancel(ctx)
				err := a.cfg.ChainResolver.Tell(notifyCtx, *m)
				if err != nil {
					a.logger(ctx).WarnS(
						ctx,
						"Failed to tell chain resolver",
						err,
						slog.String(
							"outpoint",
							m.VTXO.Outpoint.
								String(),
						))
				}
			}

			// Post the unilateral exit to the ledger. We only
			// know the VTXO value at this point: the on-chain
			// miner fee and confirmation height are determined
			// by the chain resolver later, so ExitCostSat /
			// BlockHeight are posted as 0 here and the chain
			// resolver is expected to book a refining entry
			// (future wiring) when the exit actually confirms.
			// Emitting now gives accounting an immediate
			// "VTXO left off-chain custody via exit" record
			// and keeps vtxo_balance in sync with reality even
			// if the confirmation flow is delayed.
			a.emitExitCost(ctx, m)

		case *VTXOTerminatedNotification:
			// Notify manager to remove this actor from tracking.
			a.tellManager(ctx, &VTXOTerminatedMsg{
				Outpoint:   m.VTXOOutpoint,
				FinalState: m.FinalState,
				Reason:     m.Reason,
			})
		}
	}

	return nil
}

// processStatusUpdate persists a VTXO status transition and returns any write
// error to the actor caller so higher-level durable workflows can retry.
func (a *VTXOActor) processStatusUpdate(ctx context.Context,
	m *VTXOStatusUpdate) error {

	// For forfeiting status with a forfeit tx, use MarkForfeiting to
	// persist both status and the signed tx for crash recovery.
	var err error
	isForfeitingWithTx := m.NewStatus == VTXOStatusForfeiting &&
		m.ForfeitTx != nil

	a.logger(ctx).DebugS(ctx, "Processing VTXOStatusUpdate",
		slog.String("outpoint", m.Outpoint.String()),
		slog.String("new_status", m.NewStatus.String()),
		slog.Bool("has_forfeit_tx", m.ForfeitTx != nil),
		slog.Bool("is_forfeiting_with_tx", isForfeitingWithTx),
	)

	switch {
	case isForfeitingWithTx:
		err = a.cfg.Store.MarkForfeiting(
			ctx, m.Outpoint, m.RoundID, m.ForfeitTx,
		)
		a.logger(ctx).DebugS(ctx, "Called MarkForfeiting",
			slog.String("outpoint", m.Outpoint.String()),
			slog.String("round_id", m.RoundID),
			slog.Bool("error", err != nil),
		)

	case m.ReleaseSpendReservation:
		// Leaving SpendingState: drop the durable reservation row in
		// the same transaction as the status change so a stale row can
		// never mask a future orphan on this outpoint.
		err = a.cfg.Store.UpdateVTXOStatusReleasingReservation(
			ctx, m.Outpoint, m.NewStatus,
		)

	default:
		err = a.cfg.Store.UpdateVTXOStatus(
			ctx, m.Outpoint, m.NewStatus,
		)
	}
	if err != nil {
		a.logger(ctx).ErrorS(ctx, "Failed to update VTXO status",
			err,
			slog.String("outpoint", m.Outpoint.String()),
			slog.String("status", m.NewStatus.String()),
		)

		return fmt.Errorf("persist vtxo status %s to %s: %w",
			m.Outpoint, m.NewStatus, err)
	}

	return nil
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

	a.logger(ctx).DebugS(ctx, "Subscribed to block epochs",
		slog.String("vtxo", a.cfg.VTXO.Outpoint.String()),
	)

	return nil
}

// unsubscribeBlockEpochs cancels the block epoch subscription.
func (a *VTXOActor) unsubscribeBlockEpochs(ctx context.Context) {
	callerID := fmt.Sprintf("vtxo.%s", a.cfg.VTXO.Outpoint.String())

	err := a.cfg.ChainSource.Tell(
		ctx, &chainsource.UnsubscribeBlocksRequest{
			CallerID: callerID,
		},
	)
	if err != nil {
		a.logger(ctx).WarnS(ctx,
			"Failed to unsubscribe from blocks",
			err,
			slog.String(
				"vtxo",
				a.cfg.VTXO.Outpoint.String(),
			))
	}

	a.logger(ctx).DebugS(ctx, "Unsubscribed from block epochs",
		slog.String("vtxo", a.cfg.VTXO.Outpoint.String()),
	)
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
func statusToState(ctx context.Context, vtxo *Descriptor, store VTXOStore,
	logger btclog.Logger) VTXOState {

	switch vtxo.Status {
	case VTXOStatusLive:
		return &LiveState{
			VTXO:              vtxo,
			LastCheckedHeight: vtxo.CreatedHeight,
		}

	case VTXOStatusSpending:
		return &SpendingState{
			VTXO:              vtxo,
			LastCheckedHeight: vtxo.CreatedHeight,
		}

	case VTXOStatusPendingForfeit:
		return &PendingForfeitState{VTXO: vtxo, RequestedAtHeight: 0}

	case VTXOStatusForfeiting:
		// Fetch the persisted forfeit tx for crash recovery.
		var forfeitTx *wire.MsgTx
		if store != nil {
			tx, err := store.GetForfeitTx(ctx, vtxo.Outpoint)
			if err != nil && logger != nil {
				logger.WarnS(ctx, "Could not get forfeit tx",
					err,
					slog.String(
						"outpoint",
						vtxo.Outpoint.String(),
					),
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

	case VTXOStatusSpent:
		return &SpentState{VTXO: vtxo}

	case VTXOStatusUnilateralExit:
		return &UnilateralExitState{
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

package vtxo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/round"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// VTXOActorRef is the actor reference type for VTXO actors. Uses
// actormsg.VTXOActorMsg as the message type to enable both round and vtxo
// packages to use the same service key for registration and lookup.
type VTXOActorRef = actor.ActorRef[
	actormsg.VTXOActorMsg, actormsg.VTXOActorResp,
]

// ManagerConfig holds configuration for the VTXO Manager.
type ManagerConfig struct {
	Store       VTXOStore
	Wallet      VTXOWallet
	ChainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]
	ActorSystem  *actor.ActorSystem
	ChainParams  *chaincfg.Params
	ExpiryConfig *ExpiryConfig

	// Log is an optional logger for this manager instance. If None, the
	// manager falls back to extracting a logger from context via
	// LoggerFromContext, or uses btclog.Disabled if no logger is found.
	Log fn.Option[btclog.Logger]

	// RoundActor receives forfeit requests and signatures relayed from VTXO
	// actors through the manager. Uses actormsg.RoundReceivable to avoid
	// import cycles.
	RoundActor actor.TellOnlyRef[actormsg.RoundReceivable]

	// ChainResolver receives expiring notifications for unilateral exit.
	// Passed through to spawned VTXO actors.
	ChainResolver actor.TellOnlyRef[ExpiringNotification]

	// LedgerSink is an optional reference to the client-side
	// ledger accounting actor, propagated to each spawned
	// VTXOActor so unilateral exits can record their on-chain
	// fee + send-leg pair. When None, accounting emission is
	// silently skipped.
	LedgerSink fn.Option[ledger.Sink]

	// RefreshFeeQuoter is propagated to each spawned VTXOActor so
	// auto-refresh emissions stamp an advisory hint on the relayed
	// RefreshVTXORequest.OperatorFee field for observability. Under
	// the seal-time fee handshake (#270) the server is the
	// authoritative fee source at seal time; this value is not
	// subtracted from the new VTXO amount or otherwise persisted
	// into the intent. When nil, spawned actors emit with
	// OperatorFee=0, which is harmless because the server still
	// fills in the residual via the JoinRoundQuote.
	RefreshFeeQuoter RefreshFeeQuoter
}

// Manager coordinates VTXO actor lifecycle - spawning new actors when VTXOs
// are created and recovering persisted actors on startup. Each VTXO actor
// manages its own block epoch subscription and communicates directly with the
// round actor via service keys.
type Manager struct {
	cfg *ManagerConfig

	// managerRef is the manager's own actor ref, used for creating mapped
	// refs that VTXO actors can use to send termination notifications.
	managerRef actor.TellOnlyRef[ManagerMsg]

	// actors tracks active VTXO actors by outpoint.
	actors map[wire.OutPoint]VTXOActorRef
}

// NewManager creates a new VTXO Manager.
func NewManager(cfg *ManagerConfig) *Manager {
	if cfg.ExpiryConfig == nil {
		cfg.ExpiryConfig = DefaultExpiryConfig()
	}

	return &Manager{
		cfg:    cfg,
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}
}

// logger returns the configured logger or falls back to extracting from
// context. If no logger is found in either location, returns btclog.Disabled.
func (m *Manager) logger(ctx context.Context) btclog.Logger {
	return m.cfg.Log.UnwrapOr(build.LoggerFromContext(ctx))
}

// Start initializes the manager by recovering persisted VTXOs. The selfRef
// parameter is the manager's own actor reference, used for VTXO actors to
// send termination notifications.
func (m *Manager) Start(ctx context.Context,
	selfRef actor.TellOnlyRef[ManagerMsg]) error {

	m.managerRef = selfRef

	vtxos, err := m.cfg.Store.ListLiveVTXOs(ctx)
	if err != nil {
		return fmt.Errorf("list live vtxos: %w", err)
	}

	for _, vtxo := range vtxos {
		ref, err := m.spawnVTXOActor(ctx, vtxo)
		if err != nil {
			m.logger(ctx).ErrorS(
				ctx, "Failed to recover VTXO actor", err,
				slog.String("outpoint", vtxo.Outpoint.String()),
			)

			continue
		}

		m.actors[vtxo.Outpoint] = ref

		m.logger(ctx).InfoS(ctx, "Recovered VTXO actor",
			slog.String("outpoint", vtxo.Outpoint.String()),
			slog.String("status", vtxo.Status.String()))
	}

	m.logger(ctx).InfoS(ctx, "VTXO manager started",
		slog.Int("recovered", len(m.actors)))

	return nil
}

// Stop gracefully shuts down the manager.
func (m *Manager) Stop(ctx context.Context) {
	m.logger(ctx).InfoS(ctx, "VTXO manager stopped")
}

// Receive processes incoming messages.
func (m *Manager) Receive(ctx context.Context,
	msg ManagerMsg) fn.Result[ManagerResp] {

	switch req := msg.(type) {
	case *round.VTXOCreatedNotification:
		return m.handleVTXOCreated(ctx, req)

	case *VTXOsMaterializedNotification:
		return m.handleVTXOsMaterialized(ctx, req)

	case *round.VTXOTerminatedMsg:
		return m.handleVTXOTerminated(ctx, req)

	case *RelayToRoundMsg:
		return m.handleRelayToRound(ctx, req)

	case *SelectAndReserveSpendRequest:
		return m.handleSelectAndReserveSpend(ctx, req)

	case *ReleaseSpendRequest:
		return m.handleReleaseSpend(ctx, req)

	case *CompleteSpendRequest:
		return m.handleCompleteSpend(ctx, req)

	case *ReserveForfeitRequest:
		return m.handleReserveForfeit(ctx, req)

	case *ReleaseForfeitRequest:
		return m.handleReleaseForfeit(ctx, req)

	case *SelectAndReserveForfeitRequest:
		return m.handleSelectAndReserveForfeit(ctx, req)

	case *GetActiveVTXOCountRequest:
		return fn.Ok[ManagerResp](&GetActiveVTXOCountResponse{
			Count: len(m.actors),
		})

	case *ForceUnrollRequest:
		return m.handleForceUnroll(ctx, req)

	default:
		return fn.Err[ManagerResp](
			fmt.Errorf("unknown message: %T", msg),
		)
	}
}

// handleVTXOCreated spawns a new VTXO actor for each created VTXO.
func (m *Manager) handleVTXOCreated(ctx context.Context,
	msg *round.VTXOCreatedNotification) fn.Result[ManagerResp] {

	for _, clientVTXO := range msg.VTXOs {
		outpoint := clientVTXO.Outpoint

		if _, exists := m.actors[outpoint]; exists {
			m.logger(ctx).WarnS(ctx,
				"VTXO actor already exists", nil,
				slog.String("outpoint", outpoint.String()),
			)

			continue
		}

		result := clientVTXOToDescriptor(clientVTXO, msg)
		descriptor, err := result.Unpack()
		if err != nil {
			m.logger(ctx).ErrorS(ctx,
				"Failed to build descriptor", err,
				slog.String("outpoint", outpoint.String()),
			)

			continue
		}

		if err := m.cfg.Store.SaveVTXO(ctx, descriptor); err != nil {
			m.logger(ctx).ErrorS(ctx, "Failed to save VTXO", err,
				slog.String("outpoint", outpoint.String()),
			)

			continue
		}

		ref, err := m.spawnVTXOActor(ctx, descriptor)
		if err != nil {
			m.logger(ctx).ErrorS(ctx,
				"Failed to spawn VTXO actor", err,
				slog.String("outpoint", outpoint.String()),
			)

			continue
		}

		m.actors[outpoint] = ref

		m.logger(ctx).InfoS(ctx, "Spawned VTXO actor",
			slog.String("outpoint", outpoint.String()),
			slog.Int64("amount", int64(clientVTXO.Amount)),
			slog.Int("batch_expiry", int(msg.BatchExpiry)))
	}

	return fn.Ok[ManagerResp](&VTXOCreatedResp{})
}

// handleVTXOsMaterialized spawns VTXO actors for descriptors that were already
// persisted by another actor, such as the OOR receive flow.
func (m *Manager) handleVTXOsMaterialized(ctx context.Context,
	msg *VTXOsMaterializedNotification) fn.Result[ManagerResp] {

	for _, descriptor := range msg.VTXOs {
		if descriptor == nil {
			continue
		}

		outpoint := descriptor.Outpoint
		if _, exists := m.actors[outpoint]; exists {
			m.logger(ctx).WarnS(ctx,
				"VTXO actor already exists", nil,
				slog.String("outpoint", outpoint.String()),
			)

			continue
		}

		ref, err := m.spawnVTXOActor(ctx, descriptor)
		if err != nil {
			m.logger(ctx).ErrorS(ctx,
				"Failed to spawn VTXO actor", err,
				slog.String("outpoint", outpoint.String()),
			)

			continue
		}

		m.actors[outpoint] = ref

		m.logger(ctx).InfoS(ctx, "Spawned VTXO actor",
			slog.String("outpoint", outpoint.String()),
			slog.Int64("amount", int64(descriptor.Amount)),
			slog.Int("batch_expiry", int(descriptor.BatchExpiry)))
	}

	return fn.Ok[ManagerResp](&VTXOsMaterializedResp{})
}

// handleForceUnroll transitions a VTXO into UnilateralExitState via the
// VTXO actor's FSM, then lets the outbox handler emit
// ExpiringNotification through the chain resolver seam. This ensures
// manual and automatic unroll converge on the same ownership/state
// transition path. The actor is driven via Ask (not Tell) so the caller
// can distinguish "accepted and transitioning", "already terminal", and
// "no such vtxo" rather than observing a uniform Accepted:true even when
// the FSM silently self-looped on a terminal state.
func (m *Manager) handleForceUnroll(ctx context.Context,
	req *ForceUnrollRequest) fn.Result[ManagerResp] {

	actorRef, ok := m.actors[req.Outpoint]
	if !ok {
		// The VTXO actor is already gone (likely already terminal
		// and cleaned up via handleVTXOTerminated). Report a
		// specific reason so the caller can tell this apart from
		// "event accepted but actor self-looped".
		return fn.Ok[ManagerResp](&ForceUnrollResponse{
			Accepted: false,
			Reason:   "no such vtxo",
		})
	}

	reason := req.Reason
	if reason == "" {
		reason = "manual unroll"
	}

	resp, err := actorRef.Ask(ctx, &ForceUnrollEvent{
		Reason: reason,
	}).Await(ctx).Unpack()
	if err != nil {
		return fn.Err[ManagerResp](fmt.Errorf(
			"ask force-unroll: %w", err,
		))
	}

	actorResp, ok := resp.(VTXOActorResponse)
	if !ok {
		return fn.Err[ManagerResp](fmt.Errorf(
			"unexpected force-unroll response type: %T", resp,
		))
	}

	// Terminal states self-loop on ForceUnrollEvent. Detect the
	// PriorState == NewState case on a terminal state and report a
	// clear Reason so the caller sees a no-op explicitly rather than
	// Accepted:true on work that was never scheduled.
	priorTerminal := actorResp.PriorState != nil &&
		actorResp.PriorState.IsTerminal()
	newTerminal := actorResp.NewState != nil &&
		actorResp.NewState.IsTerminal()

	if priorTerminal && newTerminal {
		m.logger(ctx).InfoS(ctx, "Force-unroll no-op on terminal VTXO",
			slog.String("outpoint", req.Outpoint.String()),
			slog.String("state", fmt.Sprintf(
				"%T", actorResp.NewState,
			)),
		)

		return fn.Ok[ManagerResp](&ForceUnrollResponse{
			Accepted: false,
			Reason:   "already terminal",
		})
	}

	m.logger(ctx).InfoS(ctx, "Force-unroll accepted by VTXO actor",
		slog.String("outpoint", req.Outpoint.String()),
		slog.String("reason", reason),
		slog.String("new_state", fmt.Sprintf(
			"%T", actorResp.NewState,
		)),
	)

	return fn.Ok[ManagerResp](&ForceUnrollResponse{
		Accepted: true,
	})
}

// handleVTXOTerminated removes a VTXO actor from tracking when it reaches
// a terminal state (Forfeited, Failed, etc.).
func (m *Manager) handleVTXOTerminated(ctx context.Context,
	msg *round.VTXOTerminatedMsg) fn.Result[ManagerResp] {

	delete(m.actors, msg.Outpoint)

	m.logger(ctx).InfoS(ctx, "VTXO actor terminated",
		slog.String("outpoint", msg.Outpoint.String()),
		slog.String("final_state", msg.FinalState),
		slog.String("reason", msg.Reason))

	return fn.Ok[ManagerResp](&VTXOTerminatedResp{})
}

// handleRelayToRound forwards a VTXO actor's message to the round actor.
// The VTXO actor pre-builds the round-specific message
// (RefreshVTXORequest for the auto-expiry path, or
// ForfeitSignatureResponse during forfeit signing) and wraps it in
// RelayToRoundMsg. The manager just unwraps and forwards.
//
// Liveness guarantee: when a VTXO approaches expiry, the VTXO actor
// autonomously emits a ForfeitRequest (wrapped in RelayToRoundMsg) without
// requiring wallet input. The manager relays this immediately, ensuring
// cooperative action is always attempted before critical expiry. This
// default policy means safety does not depend on wallet reaction time.
// Auto-expiry forfeits intentionally bypass admission gating because
// the VTXO actor has already determined that cooperative action is
// urgent. Delaying relay would risk missing the expiry window.
func (m *Manager) handleRelayToRound(ctx context.Context,
	msg *RelayToRoundMsg) fn.Result[ManagerResp] {

	if err := m.cfg.RoundActor.Tell(ctx, msg.Payload); err != nil {
		m.logger(ctx).WarnS(ctx, "Failed to relay to round", err,
			slog.String("payload_type", fmt.Sprintf("%T", msg.Payload)))

		return fn.Err[ManagerResp](
			fmt.Errorf("relay to round: %w", err),
		)
	}

	return fn.Ok[ManagerResp](&RelayToRoundResp{})
}

// spawnVTXOActor creates a new VTXO FSM actor.
func (m *Manager) spawnVTXOActor(ctx context.Context,
	vtxo *Descriptor) (VTXOActorRef, error) {

	actorID := fmt.Sprintf("vtxo.%s", vtxo.Outpoint.String())
	serviceKey := VTXOActorServiceKey(vtxo.Outpoint)

	actorCfg := &VTXOActorConfig{
		VTXO:             vtxo,
		Store:            m.cfg.Store,
		Wallet:           m.cfg.Wallet,
		ChainSource:      m.cfg.ChainSource,
		ChainParams:      m.cfg.ChainParams,
		ExpiryConfig:     m.cfg.ExpiryConfig,
		Log:              m.cfg.Log,
		ChainResolver:    m.cfg.ChainResolver,
		Manager:          m.managerRef,
		LedgerSink:       m.cfg.LedgerSink,
		RefreshFeeQuoter: m.cfg.RefreshFeeQuoter,
	}

	vtxoActor := NewVTXOActor(ctx, actorCfg)
	ref := serviceKey.Spawn(m.cfg.ActorSystem, actorID, vtxoActor)

	// Start the actor to subscribe to block epochs. ActorRef embeds
	// TellOnlyRef so we can use it directly.
	if err := vtxoActor.Start(ctx, ref); err != nil {
		// Stop the spawned actor to prevent a zombie actor in the
		// system.
		m.cfg.ActorSystem.StopAndRemoveActor(actorID)

		return ref, fmt.Errorf("start vtxo actor: %w", err)
	}

	return ref, nil
}

// =============================================================================
// Spend admission handlers
// =============================================================================

// reserveParams bundles the per-caller differences for
// selectAndReserveVTXOs so the shared coin-selection + reservation
// logic does not need to be duplicated.
type reserveParams struct {
	targetAmount btcutil.Amount
	reserveEvent actormsg.VTXOActorMsg
	rollback     func(ctx context.Context, ops []wire.OutPoint)
	label        string
}

// selectAndReserveVTXOs performs largest-first coin selection and
// atomically reserves each selected VTXO by sending reserveEvent to
// its actor. On partial failure the rollback function is called for
// already-reserved outpoints. Returns the selected VTXO details and
// total amount on success.
func (m *Manager) selectAndReserveVTXOs(ctx context.Context,
	p reserveParams) ([]SelectedVTXO, btcutil.Amount, error) {

	if p.targetAmount <= 0 {
		return nil, 0, fmt.Errorf(
			"target amount must be positive",
		)
	}

	// List live candidates from the store.
	candidates, err := m.cfg.Store.ListVTXOsByStatus(
		ctx, VTXOStatusLive,
	)
	if err != nil {
		return nil, 0, fmt.Errorf(
			"list live vtxos: %w", err,
		)
	}

	// Run largest-first selection.
	selected := selectLargestFirst(candidates, p.targetAmount)
	if selected == nil {
		return nil, 0, fmt.Errorf(
			"insufficient funds: need %d", p.targetAmount,
		)
	}

	// Reserve each selected VTXO via its actor. Track successfully
	// reserved outpoints so we can roll back on partial failure.
	var reserved []wire.OutPoint
	for _, vtxo := range selected {
		ref, ok := m.actors[vtxo.Outpoint]
		if !ok {
			p.rollback(ctx, reserved)

			return nil, 0, fmt.Errorf(
				"no actor for outpoint %s",
				vtxo.Outpoint,
			)
		}

		result := ref.Ask(
			ctx, p.reserveEvent,
		).Await(ctx)
		if _, err := result.Unpack(); err != nil {
			m.logger(ctx).WarnS(
				ctx, p.label+" reserve failed", err,
				slog.String(
					"outpoint",
					vtxo.Outpoint.String(),
				),
			)
			p.rollback(ctx, reserved)

			return nil, 0, fmt.Errorf(
				"reserve %s %s: %w", p.label,
				vtxo.Outpoint, err,
			)
		}

		reserved = append(reserved, vtxo.Outpoint)
	}

	// Build the result with selected VTXO details.
	var (
		selectedVTXOs []SelectedVTXO
		totalSelected btcutil.Amount
	)
	for _, vtxo := range selected {
		selectedVTXOs = append(selectedVTXOs, SelectedVTXO{
			Outpoint: vtxo.Outpoint,
			Amount:   vtxo.Amount,
			PkScript: vtxo.PkScript,
		})
		totalSelected += vtxo.Amount
	}

	m.logger(ctx).InfoS(
		ctx, "Reserved VTXOs for "+p.label,
		slog.Int("count", len(selected)),
		slog.Int64("total", int64(totalSelected)),
		slog.Int64("target", int64(p.targetAmount)),
	)

	return selectedVTXOs, totalSelected, nil
}

// handleSelectAndReserveSpend selects VTXOs covering the target amount using
// largest-first coin selection, then atomically reserves them for an OOR
// spend by Asking each VTXO actor to process SpendReserveEvent. If any
// reservation fails, already-reserved VTXOs are rolled back.
func (m *Manager) handleSelectAndReserveSpend(ctx context.Context,
	req *SelectAndReserveSpendRequest) fn.Result[ManagerResp] {

	vtxos, total, err := m.selectAndReserveVTXOs(ctx, reserveParams{
		targetAmount: req.TargetAmount,
		reserveEvent: &SpendReserveEvent{},
		rollback:     m.rollbackSpend,
		label:        "spend",
	})
	if err != nil {
		return fn.Err[ManagerResp](err)
	}

	return fn.Ok[ManagerResp](&SelectAndReserveSpendResponse{
		SelectedVTXOs: vtxos,
		TotalSelected: total,
	})
}

// rollbackSpend sends SpendReleasedEvent to all previously reserved VTXOs.
// This is best-effort: errors are logged but do not propagate.
func (m *Manager) rollbackSpend(ctx context.Context,
	outpoints []wire.OutPoint) {

	for _, op := range outpoints {
		ref, ok := m.actors[op]
		if !ok {
			continue
		}

		result := ref.Ask(
			ctx, &SpendReleasedEvent{},
		).Await(ctx)
		if _, err := result.Unpack(); err != nil {
			m.logger(ctx).WarnS(
				ctx, "Spend rollback failed", err,
				slog.String("outpoint", op.String()),
			)
		}
	}
}

// handleReleaseSpend releases VTXOs from spend reservation back to
// LiveState. Release is best-effort: all outpoints are attempted even
// if some fail, and errors are aggregated. This prevents a single
// failure from leaving the remaining VTXOs permanently locked.
func (m *Manager) handleReleaseSpend(ctx context.Context,
	req *ReleaseSpendRequest) fn.Result[ManagerResp] {

	outpoints := dedupOutpoints(req.Outpoints)

	var (
		released int
		errs     []error
	)
	for _, op := range outpoints {
		ref, ok := m.actors[op]
		if !ok {
			errs = append(errs, fmt.Errorf(
				"no actor for outpoint %s", op,
			))

			continue
		}

		result := ref.Ask(
			ctx, &SpendReleasedEvent{},
		).Await(ctx)
		if _, err := result.Unpack(); err != nil {
			m.logger(ctx).WarnS(
				ctx, "Spend release failed", err,
				slog.String("outpoint", op.String()),
			)
			errs = append(errs, fmt.Errorf(
				"release %s: %w", op, err,
			))

			continue
		}

		released++
	}

	if len(errs) > 0 {
		return fn.Err[ManagerResp](fmt.Errorf(
			"release spend: %d/%d failed: %w",
			len(errs), len(outpoints), errors.Join(errs...),
		))
	}

	return fn.Ok[ManagerResp](&ReleaseSpendResponse{
		ReleasedCount: released,
	})
}

// handleCompleteSpend marks VTXOs as fully spent via SpendCompletedEvent.
func (m *Manager) handleCompleteSpend(ctx context.Context,
	req *CompleteSpendRequest) fn.Result[ManagerResp] {

	outpoints := dedupOutpoints(req.Outpoints)

	var completed int
	for _, op := range outpoints {
		ref, ok := m.actors[op]
		if !ok {
			spent, err := m.isPersistedSpent(ctx, op)
			if err != nil {
				return fn.Err[ManagerResp](err)
			}
			if spent {
				completed++

				continue
			}

			return fn.Err[ManagerResp](fmt.Errorf(
				"no actor for outpoint %s", op,
			))
		}

		result := ref.Ask(
			ctx, &SpendCompletedEvent{},
		).Await(ctx)
		if _, err := result.Unpack(); err != nil {
			return fn.Err[ManagerResp](fmt.Errorf(
				"complete %s: %w", op, err,
			))
		}

		completed++
	}

	m.logger(ctx).InfoS(ctx, "Completed OOR spend",
		slog.Int("count", completed))

	return fn.Ok[ManagerResp](&CompleteSpendResponse{
		CompletedCount: completed,
	})
}

// isPersistedSpent returns true when an actor was already cleaned up after
// the spend status reached durable storage. It returns false with no error
// when the VTXO is absent, and returns an error when the store cannot give a
// definitive answer.
//
// This makes CompleteSpend idempotent across crashes that happen after the
// VTXO status commit but before the OOR session checkpoints Completed.
func (m *Manager) isPersistedSpent(ctx context.Context,
	op wire.OutPoint) (bool, error) {

	if m.cfg.Store == nil {
		return false, nil
	}

	desc, err := m.cfg.Store.GetVTXO(ctx, op)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}

		return false, fmt.Errorf(
			"load vtxo for spent check %s: %w", op, err,
		)
	}

	if desc == nil {
		return false, nil
	}

	return desc.Status == VTXOStatusSpent, nil
}

// =============================================================================
// Forfeit admission handlers
// =============================================================================

// handleReserveForfeit reserves specific VTXOs for cooperative consumption
// by Asking each actor to process PendingForfeitEvent. If any reservation
// fails, already-claimed VTXOs are rolled back via ForfeitReleasedEvent.
func (m *Manager) handleReserveForfeit(ctx context.Context,
	req *ReserveForfeitRequest) fn.Result[ManagerResp] {

	outpoints := dedupOutpoints(req.Outpoints)

	// Validate all outpoints are known before attempting reservation.
	for _, op := range outpoints {
		if _, ok := m.actors[op]; !ok {
			return fn.Err[ManagerResp](fmt.Errorf(
				"no actor for outpoint %s", op,
			))
		}
	}

	// Reserve each VTXO. Track successes for rollback on failure.
	var reserved []wire.OutPoint
	for _, op := range outpoints {
		ref := m.actors[op]
		result := ref.Ask(
			ctx, &PendingForfeitEvent{},
		).Await(ctx)
		if _, err := result.Unpack(); err != nil {
			m.logger(ctx).WarnS(
				ctx, "Forfeit reserve failed", err,
				slog.String("outpoint", op.String()),
			)
			m.rollbackForfeit(ctx, reserved)

			return fn.Err[ManagerResp](fmt.Errorf(
				"reserve forfeit %s: %w", op, err,
			))
		}

		reserved = append(reserved, op)
	}

	m.logger(ctx).InfoS(ctx, "Reserved VTXOs for forfeit",
		slog.Int("count", len(reserved)))

	return fn.Ok[ManagerResp](&ReserveForfeitResponse{})
}

// rollbackForfeit sends ForfeitReleasedEvent to previously reserved VTXOs.
// Best-effort: errors are logged but do not propagate.
func (m *Manager) rollbackForfeit(ctx context.Context,
	outpoints []wire.OutPoint) {

	for _, op := range outpoints {
		ref, ok := m.actors[op]
		if !ok {
			continue
		}

		result := ref.Ask(
			ctx, &ForfeitReleasedEvent{},
		).Await(ctx)
		if _, err := result.Unpack(); err != nil {
			m.logger(ctx).WarnS(
				ctx, "Forfeit rollback failed", err,
				slog.String("outpoint", op.String()),
			)
		}
	}
}

// handleReleaseForfeit releases VTXOs from pending forfeit back to
// LiveState. Release is best-effort: all outpoints are attempted even
// if some fail, and errors are aggregated. This prevents a single
// failure from leaving the remaining VTXOs permanently locked.
func (m *Manager) handleReleaseForfeit(ctx context.Context,
	req *ReleaseForfeitRequest) fn.Result[ManagerResp] {

	outpoints := dedupOutpoints(req.Outpoints)

	var (
		released int
		errs     []error
	)
	for _, op := range outpoints {
		ref, ok := m.actors[op]
		if !ok {
			errs = append(errs, fmt.Errorf(
				"no actor for outpoint %s", op,
			))

			continue
		}

		result := ref.Ask(
			ctx, &ForfeitReleasedEvent{},
		).Await(ctx)
		if _, err := result.Unpack(); err != nil {
			m.logger(ctx).WarnS(
				ctx, "Forfeit release failed", err,
				slog.String("outpoint", op.String()),
			)
			errs = append(errs, fmt.Errorf(
				"release forfeit %s: %w", op, err,
			))

			continue
		}

		released++
	}

	if len(errs) > 0 {
		return fn.Err[ManagerResp](fmt.Errorf(
			"release forfeit: %d/%d failed: %w",
			len(errs), len(outpoints), errors.Join(errs...),
		))
	}

	return fn.Ok[ManagerResp](&ReleaseForfeitResponse{
		ReleasedCount: released,
	})
}

// handleSelectAndReserveForfeit selects VTXOs covering the target amount
// using largest-first coin selection, then atomically reserves them for
// cooperative consumption by Asking each VTXO actor to process
// PendingForfeitEvent. If any reservation fails, already-reserved VTXOs
// are rolled back. This is the directed-send counterpart of
// handleSelectAndReserveSpend.
func (m *Manager) handleSelectAndReserveForfeit(ctx context.Context,
	req *SelectAndReserveForfeitRequest) fn.Result[ManagerResp] {

	vtxos, total, err := m.selectAndReserveVTXOs(ctx, reserveParams{
		targetAmount: req.TargetAmount,
		reserveEvent: &PendingForfeitEvent{},
		rollback:     m.rollbackForfeit,
		label:        "forfeit",
	})
	if err != nil {
		return fn.Err[ManagerResp](err)
	}

	return fn.Ok[ManagerResp](
		&SelectAndReserveForfeitResponse{
			SelectedVTXOs: vtxos,
			TotalSelected: total,
		},
	)
}

// =============================================================================
// Coin selection
// =============================================================================

// selectLargestFirst implements largest-first coin selection. It sorts
// candidates by amount descending and picks VTXOs until the target is met.
// Returns nil if the candidates cannot cover the target.
func selectLargestFirst(candidates []*Descriptor,
	target btcutil.Amount) []*Descriptor {

	// Sort by amount descending.
	sorted := make([]*Descriptor, len(candidates))
	copy(sorted, candidates)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Amount > sorted[j].Amount
	})

	var (
		selected []*Descriptor
		total    btcutil.Amount
	)
	for _, vtxo := range sorted {
		selected = append(selected, vtxo)
		total += vtxo.Amount

		if total >= target {
			return selected
		}
	}

	// Not enough funds.
	return nil
}

// dedupOutpoints returns a copy of the slice with duplicate outpoints
// removed, preserving the order of first occurrence. This prevents the same
// VTXO actor from receiving the same event twice in a single request, which
// would cause an invalid state-transition error on the second pass.
func dedupOutpoints(ops []wire.OutPoint) []wire.OutPoint {
	seen := make(map[wire.OutPoint]struct{}, len(ops))
	out := make([]wire.OutPoint, 0, len(ops))

	for _, op := range ops {
		if _, dup := seen[op]; dup {
			continue
		}

		seen[op] = struct{}{}
		out = append(out, op)
	}

	return out
}

// clientVTXOToDescriptor converts a round.ClientVTXO to a Descriptor using
// metadata from the VTXOCreatedNotification. TreeDepth and TapScript are
// computed from the VTXO data since each VTXO may be at a different depth.
func clientVTXOToDescriptor(cv *round.ClientVTXO,
	msg *round.VTXOCreatedNotification) fn.Result[*Descriptor] {

	// Compute tree depth from the VTXO's path. Each VTXO may be at a
	// different depth in the commitment tree.
	var treeDepth int
	if cv.TreePath != nil {
		treeDepth = cv.TreePath.Depth()
	}

	// Reconstruct the standard tapscript only when the round
	// output uses the default Ark policy shape. Custom policies
	// carry their semantic template and explicit spend paths
	// instead of a derived standard tapscript.
	var tapscript *waddrmgr.Tapscript
	if len(cv.PolicyTemplate) > 0 {
		desc := &Descriptor{PolicyTemplate: cv.PolicyTemplate}
		ts, err := desc.StandardTapScript()
		if err == nil {
			tapscript = ts
		}
	}

	return fn.Ok(&Descriptor{
		Outpoint:       cv.Outpoint,
		Amount:         cv.Amount,
		PolicyTemplate: cv.PolicyTemplate,
		PkScript:       cv.PkScript,
		ClientKey:      cv.OwnerKey,
		OperatorKey:    cv.OperatorKey,
		TapScript:      tapscript,
		TreePath:       cv.TreePath,
		RoundID:        msg.RoundID,
		CommitmentTxID: msg.CommitmentTxID,
		BatchExpiry:    msg.BatchExpiry,
		RelativeExpiry: cv.Expiry,
		TreeDepth:      treeDepth,
		ChainDepth:     0,
		CreatedHeight:  msg.CreatedHeight,
		Status:         VTXOStatusLive,
	})
}

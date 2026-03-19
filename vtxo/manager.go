package vtxo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/lib/scripts"
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

	case *GetActiveVTXOCountRequest:
		return fn.Ok[ManagerResp](&GetActiveVTXOCountResponse{
			Count: len(m.actors),
		})

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
		VTXO:          vtxo,
		Store:         m.cfg.Store,
		Wallet:        m.cfg.Wallet,
		ChainSource:   m.cfg.ChainSource,
		ChainParams:   m.cfg.ChainParams,
		ExpiryConfig:  m.cfg.ExpiryConfig,
		Log:           m.cfg.Log,
		ChainResolver: m.cfg.ChainResolver,
		Manager:       m.managerRef,
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

// handleSelectAndReserveSpend selects VTXOs covering the target amount using
// largest-first coin selection, then atomically reserves them for an OOR
// spend by Asking each VTXO actor to process SpendReserveEvent. If any
// reservation fails, already-reserved VTXOs are rolled back.
func (m *Manager) handleSelectAndReserveSpend(ctx context.Context,
	req *SelectAndReserveSpendRequest) fn.Result[ManagerResp] {

	if req.TargetAmount <= 0 {
		return fn.Err[ManagerResp](
			fmt.Errorf("target amount must be positive"),
		)
	}

	// List live candidates from the store.
	candidates, err := m.cfg.Store.ListVTXOsByStatus(
		ctx, VTXOStatusLive,
	)
	if err != nil {
		return fn.Err[ManagerResp](
			fmt.Errorf("list live vtxos: %w", err),
		)
	}

	// Run largest-first selection.
	selected := selectLargestFirst(candidates, req.TargetAmount)
	if selected == nil {
		return fn.Err[ManagerResp](
			fmt.Errorf("insufficient funds: need %d",
				req.TargetAmount),
		)
	}

	// Reserve each selected VTXO via its actor. Track successfully
	// reserved outpoints so we can roll back on partial failure.
	var reserved []wire.OutPoint
	for _, vtxo := range selected {
		ref, ok := m.actors[vtxo.Outpoint]
		if !ok {
			m.rollbackSpend(ctx, reserved)

			return fn.Err[ManagerResp](fmt.Errorf(
				"no actor for outpoint %s",
				vtxo.Outpoint,
			))
		}

		result := ref.Ask(ctx, &SpendReserveEvent{}).Await(ctx)
		if _, err := result.Unpack(); err != nil {
			m.logger(ctx).WarnS(
				ctx, "Spend reserve failed", err,
				slog.String(
					"outpoint",
					vtxo.Outpoint.String(),
				),
			)
			m.rollbackSpend(ctx, reserved)

			return fn.Err[ManagerResp](fmt.Errorf(
				"reserve %s: %w", vtxo.Outpoint, err,
			))
		}

		reserved = append(reserved, vtxo.Outpoint)
	}

	// Build the response with selected VTXO details.
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

	m.logger(ctx).InfoS(ctx, "Reserved VTXOs for spend",
		slog.Int("count", len(selected)),
		slog.Int64("total", int64(totalSelected)),
		slog.Int64("target", int64(req.TargetAmount)))

	return fn.Ok[ManagerResp](&SelectAndReserveSpendResponse{
		SelectedVTXOs: selectedVTXOs,
		TotalSelected: totalSelected,
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

	// Construct the TapScript from the client and operator keys. This is
	// the standard VTXO tapscript with collaborative and timeout paths.
	tapscript, err := scripts.VTXOTapScript(
		cv.OwnerKey.PubKey, cv.OperatorKey, cv.Expiry,
	)
	if err != nil {
		return fn.Err[*Descriptor](
			fmt.Errorf("build tapscript: %w", err),
		)
	}

	return fn.Ok(&Descriptor{
		Outpoint:       cv.Outpoint,
		Amount:         cv.Amount,
		PkScript:       cv.PkScript,
		OwnerKey:       cv.OwnerKey,
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

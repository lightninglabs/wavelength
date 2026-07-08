package vtxo

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/batchcanon"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/coinselect"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/round"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// defaultForfeitVTXOActorAskTimeout bounds one stalled refresh/forfeit child
// actor turn without making healthy child work race an overly short deadline.
const defaultForfeitVTXOActorAskTimeout = 5 * time.Second

// ExitOutcomeResolution is the persisted terminal result for an exiting VTXO,
// as resolved from the subsystem that owns unilateral-exit jobs.
type ExitOutcomeResolution struct {
	// Outcome classifies the terminal exit result to apply.
	Outcome ExitOutcome

	// Reason carries the terminal failure reason when Outcome is
	// ExitOutcomeRecoverable.
	Reason string
}

// ExitOutcomeResolver resolves the terminal unilateral-exit outcome, if any,
// for a VTXO that is still persisted as VTXOStatusUnilateralExit.
type ExitOutcomeResolver func(
	context.Context, wire.OutPoint,
) (fn.Option[ExitOutcomeResolution], error)

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

	// ForfeitVTXOActorAskTimeout bounds manager-to-VTXO Ask calls on
	// forfeit admission paths. The manager actor is a shared admission
	// point, so a slow or blocked refresh/forfeit child actor must fail
	// that request instead of monopolizing the manager until the outer
	// RPC deadline. Zero uses the default timeout; negative disables the
	// timeout.
	ForfeitVTXOActorAskTimeout time.Duration

	// LedgerSink is an optional reference to the client-side
	// ledger accounting actor, propagated to each spawned VTXOActor.
	// Confirmed unilateral-exit costs are emitted by unroll once the
	// final sweep confirms.
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

	// FetchOperatorKey is propagated to each spawned VTXOActor so
	// the auto-refresh emission can fetch the operator's current
	// long-term key at join time and rebuild the NEW VTXO output's
	// policy template against it. A fresh fetch is used — rather
	// than a daemon-startup cache — because VTXOs commit to their
	// operator key for life and the new output's key is chosen at
	// join time. Nil leaves the spawned actors falling back to the
	// descriptor's stored bytes (the pre-fix behavior).
	FetchOperatorKey func(context.Context) (*btcec.PublicKey, error)

	// ForfeitParticipantSigner is propagated to each spawned VTXOActor
	// so custom VTXO policies can collect non-local participant
	// signatures after the round assigns connector outputs.
	ForfeitParticipantSigner ForfeitParticipantSigner

	// TerminalVTXOObserver receives the outpoint of VTXOs that leave the
	// manager's active set so daemon-local observers can clean up related
	// actor-owned work.
	TerminalVTXOObserver func(context.Context, wire.OutPoint) error

	// ExitOutcomeResolver resolves terminal unilateral-exit job outcomes
	// for VTXOs that remain persisted in VTXOStatusUnilateralExit at
	// manager startup. When set, Start uses it to re-converge VTXO status
	// with the exit job's terminal result.
	ExitOutcomeResolver ExitOutcomeResolver

	// ReservationStore is the durable spending-reservation index. When set,
	// the manager runs a startup sweep that releases orphaned Spending
	// VTXOs (those with no reservation row) and deletes reservations as
	// VTXOs leave SpendingState. When nil, the reservation index is not
	// maintained and the startup sweep is skipped.
	ReservationStore SpendingReservationStore

	// BatchCanonicality, when set, gates coin selection on batch lineage
	// canonicality: a VTXO whose batch reorged out (limbo) or was
	// conflict-invalidated is excluded from selection so it is never spent
	// or forfeited while its lineage is not on the canonical chain
	// (darepo#454). Nil disables the gate, which is the default until the
	// batch producers (round, OOR) register their batches with the
	// canonicality manager; the gate is permissive for unregistered or
	// unseen lineage either way.
	BatchCanonicality batchcanon.Store
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

	// customForfeitSynthetic marks custom forfeit signer actors whose VTXO
	// descriptor was created only for the temporary signer. Rollback
	// deletes those descriptors, while signer actors for already-known swap
	// VTXOs only need to be stopped.
	customForfeitSynthetic map[wire.OutPoint]bool

	// reserved is the manager-goroutine-owned admission gate for spend
	// reservations. An entry means the outpoint was handed to a spend
	// session this process lifetime and must not be selected again, even
	// though the VTXO actor's durable Spending status write may still be
	// in flight: the spend reserve is delivered tell-style (an Ask whose
	// future is observed via OnComplete rather than awaited), so the
	// manager turn no longer blocks on the per-input FSM write
	// transaction. Entries are dropped on release, completion, terminal
	// notification, or an asynchronous reservation failure. Only the
	// manager goroutine touches the map.
	//
	// The value is a monotonic reservation epoch stamped by markReserved.
	// The asynchronous failure hop-back carries the epoch it observed and
	// only drops the mark when it still matches, so a stale failure from a
	// released-then-re-reserved outpoint (ABA) cannot un-gate a mark a
	// newer reservation owns.
	reserved map[wire.OutPoint]uint64

	// reserveEpoch is the monotonic counter stamped into the reserved map
	// on each markReserved. Manager-goroutine-owned, like the map.
	reserveEpoch uint64

	// liveDescriptors snapshots the live VTXO descriptors recovered
	// from the store during Start. The list is the source of truth for
	// daemon-local subsystems that need to re-arm per-VTXO state on
	// restart (notably the recipient fraud watcher) and is not kept
	// in sync after Start; runtime updates flow through the materialize
	// and terminate hooks instead.
	liveDescriptors []*Descriptor
}

// NewManager creates a new VTXO Manager.
func NewManager(cfg *ManagerConfig) *Manager {
	if cfg.ExpiryConfig == nil {
		cfg.ExpiryConfig = DefaultExpiryConfig()
	}

	return &Manager{
		cfg:                    cfg,
		actors:                 make(map[wire.OutPoint]VTXOActorRef),
		customForfeitSynthetic: make(map[wire.OutPoint]bool),
		reserved:               make(map[wire.OutPoint]uint64),
	}
}

// markReserved records an in-memory spend reservation and returns the
// monotonic epoch stamped for it. The epoch lets an asynchronous failure
// hop-back distinguish the reservation it observed from a later one on the
// same outpoint (see dropReservedEpoch). Nil-safe so test fixtures that build
// a Manager literal without NewManager keep working.
func (m *Manager) markReserved(op wire.OutPoint) uint64 {
	if m.reserved == nil {
		m.reserved = make(map[wire.OutPoint]uint64)
	}

	m.reserveEpoch++
	m.reserved[op] = m.reserveEpoch

	return m.reserveEpoch
}

// dropReserved clears an in-memory spend reservation, if present. Used by the
// synchronous, in-turn paths (rollback, release, completion, terminal
// notification) where no concurrent re-reservation can have intervened.
func (m *Manager) dropReserved(op wire.OutPoint) {
	delete(m.reserved, op)
}

// dropReservedEpoch clears an in-memory spend reservation only if its current
// epoch still matches the one the caller observed. The asynchronous reserve
// failure hop-back uses this so a stale failure (the outpoint was released and
// re-reserved by a newer session before the failure landed) cannot drop the
// newer reservation's mark.
func (m *Manager) dropReservedEpoch(op wire.OutPoint, epoch uint64) bool {
	if cur, ok := m.reserved[op]; !ok || cur != epoch {
		return false
	}

	delete(m.reserved, op)

	return true
}

// isReserved reports whether the outpoint holds an in-memory spend
// reservation.
func (m *Manager) isReserved(op wire.OutPoint) bool {
	_, ok := m.reserved[op]

	return ok
}

// logger returns the configured logger or falls back to extracting from
// context. If no logger is found in either location, returns btclog.Disabled.
func (m *Manager) logger(ctx context.Context) btclog.Logger {
	return m.cfg.Log.UnwrapOr(build.LoggerFromContext(ctx))
}

// forfeitVTXOActorAskTimeout returns the timeout used for refresh/forfeit
// manager-to-child VTXO actor asks. Spend paths keep the caller's context so
// healthy OOR payments are not failed by an artificial shorter deadline.
func (m *Manager) forfeitVTXOActorAskTimeout() time.Duration {
	timeout := m.cfg.ForfeitVTXOActorAskTimeout
	switch {
	case timeout < 0:
		return 0

	case timeout == 0:
		return defaultForfeitVTXOActorAskTimeout

	default:
		return timeout
	}
}

// askVTXOActor asks a child VTXO actor with the caller's context.
func (m *Manager) askVTXOActor(ctx context.Context, ref VTXOActorRef,
	msg actormsg.VTXOActorMsg) fn.Result[actormsg.VTXOActorResp] {

	return ref.Ask(ctx, msg).Await(ctx)
}

// detachedReserveTimeout bounds the asynchronous observation of a detached
// spend reservation's outcome. Generous on purpose: it only has to outlive a
// loaded child actor's FSM turn plus its write transaction.
const detachedReserveTimeout = 30 * time.Second

// detachedReserve hands a spend reservation to a child VTXO actor without
// blocking the manager turn on the child's FSM write transaction. The Ask
// enqueues and returns a future immediately; the outcome is observed on a
// detached goroutine via OnComplete. A failure (the candidate raced out of
// LiveState, or the child died) hops back to the manager goroutine as a
// spendReservationFailedMsg so the in-memory reservation mark is dropped
// where the map is owned. The observation context is daemon-owned: the turn
// context must not cancel the outcome watch. The epoch is the reservation
// generation observed at mark time; it rides the failure hop-back so the
// manager only drops a mark this reservation still owns.
func (m *Manager) detachedReserve(ctx context.Context, ref VTXOActorRef,
	op wire.OutPoint, epoch uint64, event actormsg.VTXOActorMsg,
	label string) {

	log := m.logger(ctx)
	managerRef := m.managerRef

	// The observation context is daemon-owned: the turn context must not
	// cancel the outcome watch. askCtx bounds how long the OnComplete
	// goroutine waits on the child's outcome so a wedged child cannot leak
	// the watcher. The failure report, however, runs on a fresh bounded
	// context derived from the same detached root rather than askCtx:
	// reporting on askCtx would drop the spendReservationFailedMsg the
	// instant the wait exhausted askCtx's budget, stranding the in-memory
	// reservation mark until restart.
	detachedCtx := context.WithoutCancel(ctx)
	askCtx, cancel := context.WithTimeout(
		detachedCtx, detachedReserveTimeout,
	)

	future := ref.Ask(askCtx, event)
	future.OnComplete(
		askCtx, func(res fn.Result[actormsg.VTXOActorResp]) {
			defer cancel()

			_, err := res.Unpack()
			if err == nil {
				return
			}

			// A watch timeout is ambiguous: the child's FSM write
			// may still be in flight (and about to commit Spending)
			// rather than having failed. Reporting a failure here
			// would drop a mark whose durable Spending write then
			// lands, briefly re-opening the in-flight window the
			// mark exists to cover. Re-confirm the durable status
			// before treating a timeout as a failure: only a
			// still-Live row means the reserve never took effect. A
			// read error or a still-Live row falls through to
			// report the failure so the mark cannot leak.
			if errors.Is(err, context.DeadlineExceeded) ||
				errors.Is(err, context.Canceled) {

				desc, gErr := m.cfg.Store.GetVTXO(
					detachedCtx, op,
				)
				if gErr == nil &&
					desc.Status != VTXOStatusLive {

					log.DebugS(detachedCtx, "Detached reserve "+
						"watch timed out but VTXO advanced "+
						"past Live; keeping reservation",
						slog.String(
							"outpoint", op.String(),
						),
						slog.String(
							"status",
							desc.Status.String(),
						),
					)

					return
				}
			}

			log.WarnS(
				detachedCtx,
				label+" detached reserve failed",
				err,
				slog.String("outpoint", op.String()),
			)

			if managerRef == nil {
				return
			}

			reportCtx, reportCancel := context.WithTimeout(
				detachedCtx, detachedReserveTimeout,
			)
			defer reportCancel()

			tellErr := managerRef.Tell(
				reportCtx, &spendReservationFailedMsg{
					Outpoint: op,
					Epoch:    epoch,
				},
			)
			if tellErr != nil {
				log.WarnS(detachedCtx, "Failed to report "+
					"detached reserve failure", tellErr,
					slog.String("outpoint", op.String()),
				)
			}
		},
	)
}

// askForfeitVTXOActor asks a child VTXO actor with the manager's bounded
// forfeit timeout. The parent context is still honored, but a blocked
// refresh/forfeit child actor can only hold the shared manager admission point
// for a short window.
func (m *Manager) askForfeitVTXOActor(ctx context.Context, ref VTXOActorRef,
	msg actormsg.VTXOActorMsg) fn.Result[actormsg.VTXOActorResp] {

	askCtx := ctx
	cancel := func() {}
	if timeout := m.forfeitVTXOActorAskTimeout(); timeout > 0 {
		askCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	return ref.Ask(askCtx, msg).Await(askCtx)
}

// rollbackContext returns a bounded context for best-effort local rollback.
// Rollback should not inherit a canceled RPC context, because admission may
// have partially reserved child VTXOs before the caller timed out.
//
// A negative ForfeitVTXOActorAskTimeout intentionally disables both the
// admission ask deadline and this rollback deadline for tests/diagnostics.
func (m *Manager) rollbackContext(ctx context.Context) (context.Context,
	context.CancelFunc) {

	rollbackCtx := context.WithoutCancel(ctx)
	if timeout := m.forfeitVTXOActorAskTimeout(); timeout > 0 {
		return context.WithTimeout(rollbackCtx, timeout)
	}

	return rollbackCtx, func() {}
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
				ctx,
				"Failed to recover VTXO actor",
				err,
				slog.String("outpoint", vtxo.Outpoint.String()),
			)

			continue
		}

		m.actors[vtxo.Outpoint] = ref
		m.liveDescriptors = append(m.liveDescriptors, vtxo)

		m.logger(ctx).InfoS(ctx, "Recovered VTXO actor",
			slog.String("outpoint", vtxo.Outpoint.String()),
			slog.String("status", vtxo.Status.String()),
		)
	}

	m.logger(ctx).InfoS(ctx, "VTXO manager started",
		slog.Int("recovered", len(m.actors)),
	)

	m.reconcileUnilateralExits(ctx)

	// With all actors recovered, release any Spending VTXOs whose owning
	// spend died before its reservation was durably recorded (orphans).
	m.sweepOrphanedReservations(ctx)

	// Likewise, return any VTXOs stranded in PendingForfeitState to Live:
	// their owning round FSM is in-memory only and cannot have survived the
	// restart to release the reservation itself.
	m.releaseOrphanedForfeits(ctx)

	return nil
}

// reconcileUnilateralExits re-converges VTXOs still persisted in
// unilateral-exit with their terminal exit job outcome. A recoverable
// no-footprint failure rolls the VTXO back to live; a completed exit retires
// it to spent. Non-terminal jobs and footprint-bearing failures are left in
// unilateral-exit.
func (m *Manager) reconcileUnilateralExits(ctx context.Context) {
	if m.cfg.ExitOutcomeResolver == nil {
		return
	}

	log := m.logger(ctx)
	exiting, err := m.cfg.Store.ListVTXOsByStatus(
		ctx, VTXOStatusUnilateralExit,
	)
	if err != nil {
		log.WarnS(ctx, "List unilateral-exit VTXOs failed", err)

		return
	}

	for _, desc := range exiting {
		resolution, err := m.cfg.ExitOutcomeResolver(
			ctx, desc.Outpoint,
		)
		if err != nil {
			log.WarnS(ctx, "Resolve exit outcome failed",
				err,
				slog.String("outpoint", desc.Outpoint.String()),
			)

			continue
		}
		if resolution.IsNone() {
			continue
		}

		outcome := resolution.UnsafeFromSome()
		req := &ExitOutcomeNotification{
			Outpoint: desc.Outpoint,
			Outcome:  outcome.Outcome,
			Reason:   outcome.Reason,
		}

		_, err = m.handleExitOutcome(ctx, req).Unpack()
		if err != nil {
			log.WarnS(ctx, "Reconcile exit VTXO failed",
				err,
				slog.String("outpoint", desc.Outpoint.String()),
				slog.String("outcome",
					outcome.Outcome.String()),
			)

			continue
		}

		log.InfoS(ctx, "Reconciled unilateral-exit VTXO",
			slog.String("outpoint", desc.Outpoint.String()),
			slog.String("outcome", outcome.Outcome.String()),
		)
	}
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

	case *spendReservationFailedMsg:
		// The detached reserve's outcome watcher reports a failed
		// reservation; drop the in-memory mark on the goroutine that
		// owns the map so the liquidity becomes selectable again. The
		// drop is epoch-guarded: a stale failure whose outpoint was
		// released and re-reserved by a newer session before the report
		// landed must not un-gate the newer reservation's mark.
		dropped := m.dropReservedEpoch(req.Outpoint, req.Epoch)

		m.logger(ctx).InfoS(ctx, "Released failed spend reservation",
			slog.String("outpoint", req.Outpoint.String()),
			slog.Bool("dropped", dropped),
		)

		return fn.Ok[ManagerResp](&ReleaseSpendResponse{})

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

	case *RestoreForfeitedVTXORequest:
		return m.handleRestoreForfeitedVTXO(ctx, req)

	case *ActivateCustomForfeitInputsRequest:
		return m.handleActivateCustomForfeitInputs(ctx, req)

	case *DropCustomForfeitInputsRequest:
		return m.handleDropCustomForfeitInputs(ctx, req)

	case *SelectAndReserveForfeitRequest:
		return m.handleSelectAndReserveForfeit(ctx, req)

	case *GetActiveVTXOCountRequest:
		return fn.Ok[ManagerResp](&GetActiveVTXOCountResponse{
			Count: len(m.actors),
		})

	case *ListLiveDescriptorsRequest:
		descs := make([]*Descriptor, len(m.liveDescriptors))
		copy(descs, m.liveDescriptors)

		return fn.Ok[ManagerResp](&ListLiveDescriptorsResponse{
			Descriptors: descs,
		})

	case *ForceUnrollRequest:
		return m.handleForceUnroll(ctx, req)

	case *ExitOutcomeNotification:
		return m.handleExitOutcome(ctx, req)

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
			m.logger(ctx).WarnS(ctx, "VTXO actor already exists",
				nil,
				slog.String("outpoint", outpoint.String()),
			)

			continue
		}

		result := clientVTXOToDescriptor(clientVTXO, msg)
		descriptor, err := result.Unpack()
		if err != nil {
			m.logger(ctx).ErrorS(ctx, "Failed to build descriptor",
				err,
				slog.String("outpoint", outpoint.String()),
			)

			continue
		}

		if err := m.cfg.Store.SaveVTXO(ctx, descriptor); err != nil {
			m.logger(ctx).ErrorS(ctx, "Failed to save VTXO",
				err,
				slog.String("outpoint", outpoint.String()),
			)

			continue
		}

		ref, err := m.spawnVTXOActor(ctx, descriptor)
		if err != nil {
			m.logger(ctx).ErrorS(ctx, "Failed to spawn VTXO actor",
				err,
				slog.String("outpoint", outpoint.String()),
			)

			continue
		}

		m.actors[outpoint] = ref

		m.logger(ctx).InfoS(ctx, "Spawned VTXO actor",
			slog.String("outpoint", outpoint.String()),
			slog.Int64("amount", int64(clientVTXO.Amount)),
			slog.Int("batch_expiry", int(msg.BatchExpiry)),
		)
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
			m.logger(ctx).WarnS(ctx, "VTXO actor already exists",
				nil,
				slog.String("outpoint", outpoint.String()),
			)

			continue
		}

		ref, err := m.spawnVTXOActor(ctx, descriptor)
		if err != nil {
			m.logger(ctx).ErrorS(ctx, "Failed to spawn VTXO actor",
				err,
				slog.String("outpoint", outpoint.String()),
			)

			continue
		}

		m.actors[outpoint] = ref

		m.logger(ctx).InfoS(ctx, "Spawned VTXO actor",
			slog.String("outpoint", outpoint.String()),
			slog.Int64("amount", int64(descriptor.Amount)),
			slog.Int("batch_expiry", int(descriptor.BatchExpiry)),
		)
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
		return fn.Err[ManagerResp](
			fmt.Errorf("ask force-unroll: %w", err),
		)
	}

	actorResp, ok := resp.(VTXOActorResponse)
	if !ok {
		return fn.Err[ManagerResp](
			fmt.Errorf("unexpected force-unroll response type: %T",
				resp),
		)
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
			slog.String(
				"state", fmt.Sprintf("%T", actorResp.NewState),
			),
		)

		return fn.Ok[ManagerResp](&ForceUnrollResponse{
			Accepted: false,
			Reason:   "already terminal",
		})
	}

	m.logger(ctx).InfoS(ctx, "Force-unroll accepted by VTXO actor",
		slog.String("outpoint", req.Outpoint.String()),
		slog.String("reason", reason),
		slog.String("new_state", fmt.Sprintf("%T", actorResp.NewState)),
	)

	return fn.Ok[ManagerResp](&ForceUnrollResponse{
		Accepted: true,
	})
}

// handleExitOutcome applies the terminal outcome of a unilateral-exit job
// reported by the unroll subsystem. A recoverable failure (no on-chain
// footprint) rolls the VTXO back to LiveState; a confirmed exit retires it
// to the terminal SpentState. Both are driven through the VTXO actor's FSM
// when the actor is still alive, with a store-level fallback when it is not
// (e.g. the actor was never restored after a daemon restart, since exiting
// VTXOs are excluded from the live-recovery set).
func (m *Manager) handleExitOutcome(ctx context.Context,
	req *ExitOutcomeNotification) fn.Result[ManagerResp] {

	m.logger(ctx).InfoS(ctx, "Applying unroll exit outcome",
		slog.String("outpoint", req.Outpoint.String()),
		slog.String("outcome", req.Outcome.String()),
		slog.String("reason", req.Reason),
	)

	switch req.Outcome {
	case ExitOutcomeRecoverable:
		return m.recoverExitedVTXO(ctx, req)

	case ExitOutcomeConfirmed:
		return m.confirmExitedVTXO(ctx, req)

	default:
		return fn.Err[ManagerResp](
			fmt.Errorf("unknown exit outcome: %d", req.Outcome),
		)
	}
}

// recoverExitedVTXO rolls a VTXO back to LiveState after a failed unroll that
// left no on-chain footprint. When the actor is still alive it drives the
// ExitFailedEvent through the FSM; otherwise it re-materializes a live actor
// from the persisted descriptor so a daemon that restarted mid-exit still
// recovers the coin.
func (m *Manager) recoverExitedVTXO(ctx context.Context,
	req *ExitOutcomeNotification) fn.Result[ManagerResp] {

	if actorRef, ok := m.actors[req.Outpoint]; ok {
		_, err := m.askVTXOActor(ctx, actorRef, &ExitFailedEvent{
			Reason: req.Reason,
		}).Unpack()
		if err != nil {
			return fn.Err[ManagerResp](
				fmt.Errorf("ask exit-failed: %w", err),
			)
		}

		return fn.Ok[ManagerResp](&ExitOutcomeResp{})
	}

	// No live actor: re-materialize one in LiveState from the persisted
	// descriptor. This covers the restart case where an exiting VTXO was
	// not part of the live-recovery set, so no actor was spawned at Start.
	descriptor, err := m.cfg.Store.GetVTXO(ctx, req.Outpoint)
	if err != nil {
		return fn.Err[ManagerResp](
			fmt.Errorf("load vtxo for recovery: %w", err),
		)
	}
	if descriptor == nil {
		m.logger(ctx).WarnS(ctx, "No descriptor to recover exited VTXO",
			nil,
			slog.String("outpoint", req.Outpoint.String()),
		)

		return fn.Ok[ManagerResp](&ExitOutcomeResp{})
	}

	// Idempotency guard: only relive a VTXO that is still in the exit
	// state. Boot reconciliation can re-deliver a recovery whose previous
	// attempt already succeeded (status now Live) or whose VTXO has since
	// moved on; reliving again would spawn a duplicate actor or clobber a
	// later state.
	if descriptor.Status != VTXOStatusUnilateralExit {
		m.logger(ctx).DebugS(ctx, "Skipping exit recovery for "+
			"non-exiting VTXO",
			slog.String("outpoint", req.Outpoint.String()),
			slog.String("status", descriptor.Status.String()),
		)

		return fn.Ok[ManagerResp](&ExitOutcomeResp{})
	}

	// Spawn the live actor BEFORE persisting the status flip. The actor is
	// what monitors the coin (expiry, re-exit); if we persisted Live first
	// and the spawn then failed, the VTXO would be live in the DB with
	// nothing watching it until the next restart. Spawning first guarantees
	// the coin is always monitored the moment its status is recoverable,
	// and a failed status write is re-converged by boot reconciliation (the
	// VTXO stays in unilateral-exit on disk, so the next boot re-drives
	// recovery).
	descriptor.Status = VTXOStatusLive

	ref, err := m.spawnVTXOActor(ctx, descriptor)
	if err != nil {
		return fn.Err[ManagerResp](
			fmt.Errorf("respawn recovered vtxo actor: %w", err),
		)
	}
	m.actors[req.Outpoint] = ref

	if err := m.cfg.Store.UpdateVTXOStatus(
		ctx, req.Outpoint, VTXOStatusLive,
	); err != nil {
		return fn.Err[ManagerResp](
			fmt.Errorf("restore vtxo status: %w", err),
		)
	}

	m.logger(ctx).InfoS(ctx, "Recovered exited VTXO to live",
		slog.String("outpoint", req.Outpoint.String()),
	)

	return fn.Ok[ManagerResp](&ExitOutcomeResp{})
}

// handleRestoreForfeitedVTXO rolls a forfeited VTXO back to LiveState because
// the batch that consumed it via forfeit has been invalidated (its forfeit
// reversed by a finalized reorg / conflict). The forfeit transition reaped the
// VTXO actor and persisted VTXOStatusForfeited, so this re-materializes a live
// actor from the persisted descriptor, mirroring recoverExitedVTXO. It is
// idempotent: a VTXO that is not currently forfeited is left untouched.
func (m *Manager) handleRestoreForfeitedVTXO(ctx context.Context,
	req *RestoreForfeitedVTXORequest) fn.Result[ManagerResp] {

	descriptor, err := m.cfg.Store.GetVTXO(ctx, req.Outpoint)
	if err != nil {
		return fn.Err[ManagerResp](
			fmt.Errorf("load vtxo for forfeit restore: %w", err),
		)
	}
	if descriptor == nil {
		m.logger(ctx).WarnS(ctx, "No descriptor to restore forfeited "+
			"VTXO", nil,
			slog.String("outpoint", req.Outpoint.String()))

		return fn.Ok[ManagerResp](&RestoreForfeitedVTXOResponse{})
	}

	// Idempotency guard: only relive a VTXO that is actually forfeited. A
	// re-delivered restore must not clobber a VTXO that already moved on or
	// was never forfeited.
	if descriptor.Status != VTXOStatusForfeited {
		m.logger(ctx).DebugS(ctx, "Skipping forfeit restore for "+
			"non-forfeited VTXO",
			slog.String("outpoint", req.Outpoint.String()),
			slog.String("status", descriptor.Status.String()))

		return fn.Ok[ManagerResp](&RestoreForfeitedVTXOResponse{})
	}

	// The forfeit transition reaps the actor, so none should be resident.
	// If one somehow is, do not spawn a duplicate; treat it as already
	// restored.
	if _, ok := m.actors[req.Outpoint]; ok {
		m.logger(ctx).DebugS(ctx, "Forfeited VTXO already has a live "+
			"actor; skipping restore",
			slog.String("outpoint", req.Outpoint.String()))

		return fn.Ok[ManagerResp](&RestoreForfeitedVTXOResponse{})
	}

	// Spawn the live actor BEFORE persisting the status flip (same ordering
	// rationale as recoverExitedVTXO: the coin must be monitored the moment
	// its status becomes recoverable, and a failed status write is
	// re-driven by the persisted Forfeited status on the next restore
	// attempt).
	descriptor.Status = VTXOStatusLive

	ref, err := m.spawnVTXOActor(ctx, descriptor)
	if err != nil {
		return fn.Err[ManagerResp](
			fmt.Errorf("respawn restored vtxo actor: %w", err),
		)
	}
	m.actors[req.Outpoint] = ref

	if err := m.cfg.Store.UpdateVTXOStatus(
		ctx, req.Outpoint, VTXOStatusLive,
	); err != nil {
		return fn.Err[ManagerResp](
			fmt.Errorf("restore forfeited vtxo status: %w", err),
		)
	}

	m.logger(ctx).InfoS(ctx, "Restored forfeited VTXO to live after "+
		"batch invalidation",
		slog.String("outpoint", req.Outpoint.String()))

	return fn.Ok[ManagerResp](&RestoreForfeitedVTXOResponse{Restored: true})
}

// confirmExitedVTXO retires a VTXO to the terminal SpentState after its
// unilateral exit confirmed on-chain. When the actor is alive it drives the
// ExitConfirmedEvent through the FSM (which emits the terminated
// notification that reaps the actor); otherwise it persists the terminal
// status directly.
func (m *Manager) confirmExitedVTXO(ctx context.Context,
	req *ExitOutcomeNotification) fn.Result[ManagerResp] {

	if actorRef, ok := m.actors[req.Outpoint]; ok {
		_, err := m.askVTXOActor(
			ctx, actorRef, &ExitConfirmedEvent{},
		).Unpack()
		if err != nil {
			return fn.Err[ManagerResp](
				fmt.Errorf("ask exit-confirmed: %w", err),
			)
		}

		return fn.Ok[ManagerResp](&ExitOutcomeResp{})
	}

	// No live actor: persist the terminal spent status directly so a
	// restarted daemon still records the on-chain spend. Only act on a VTXO
	// still in the exit state so a re-delivered confirmation cannot stomp a
	// VTXO that has since been reissued or recovered to live.
	descriptor, err := m.cfg.Store.GetVTXO(ctx, req.Outpoint)
	if err != nil {
		return fn.Err[ManagerResp](
			fmt.Errorf("load vtxo for confirm: %w", err),
		)
	}
	if descriptor == nil || descriptor.Status != VTXOStatusUnilateralExit {
		return fn.Ok[ManagerResp](&ExitOutcomeResp{})
	}

	if err := m.cfg.Store.UpdateVTXOStatus(
		ctx, req.Outpoint, VTXOStatusSpent,
	); err != nil {
		return fn.Err[ManagerResp](
			fmt.Errorf("persist spent status: %w", err),
		)
	}

	return fn.Ok[ManagerResp](&ExitOutcomeResp{})
}

// handleVTXOTerminated removes a VTXO actor from tracking when it reaches
// a terminal state (Forfeited, Failed, etc.).
func (m *Manager) handleVTXOTerminated(ctx context.Context,
	msg *round.VTXOTerminatedMsg) fn.Result[ManagerResp] {

	delete(m.actors, msg.Outpoint)
	m.dropReserved(msg.Outpoint)

	m.logger(ctx).InfoS(ctx, "VTXO actor terminated",
		slog.String("outpoint", msg.Outpoint.String()),
		slog.String("final_state", msg.FinalState),
		slog.String("reason", msg.Reason),
	)

	if m.cfg.TerminalVTXOObserver != nil {
		err := m.cfg.TerminalVTXOObserver(ctx, msg.Outpoint)
		if err != nil {
			m.logger(ctx).WarnS(
				ctx,
				"Failed to notify terminal VTXO observer",
				err,
				slog.String("outpoint", msg.Outpoint.String()),
			)
		}
	}

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

	// Relay messages can originate from async VTXO outbox work. The caller
	// context is only useful for enqueue cancellation, so detach before the
	// final round-actor handoff and let the destination actor lifecycle
	// decide whether the message can be accepted.
	notifyCtx := context.WithoutCancel(ctx)
	if err := m.cfg.RoundActor.Tell(notifyCtx, msg.Payload); err != nil {
		m.logger(ctx).WarnS(ctx, "Failed to relay to round",
			err,
			slog.String(
				"payload_type", fmt.Sprintf("%T", msg.Payload),
			),
		)

		return fn.Err[ManagerResp](
			fmt.Errorf("relay to round: %w", err),
		)
	}

	return fn.Ok[ManagerResp](&RelayToRoundResp{})
}

// spawnVTXOActor creates a new VTXO FSM actor.
// respawnActorFromStore self-heals a live-in-DB but actorless VTXO by
// spawning its actor from the persisted descriptor. The materialized-VTXO
// notification that normally spawns the actor is delivered asynchronously
// after the producing session's commit, so a coin selection racing that
// window (or a notification lost to a crash before the next restart) can
// observe the committed row before the actor exists. The row is the source
// of truth; a missing actor must never make the liquidity unusable. Runs on
// the manager goroutine, so the actors map mutation is safe.
func (m *Manager) respawnActorFromStore(ctx context.Context,
	outpoint wire.OutPoint) (VTXOActorRef, error) {

	desc, err := m.cfg.Store.GetVTXO(ctx, outpoint)
	if err != nil {
		return nil, fmt.Errorf("load descriptor: %w", err)
	}

	// Only a Live row is a valid selection candidate; anything else means
	// the candidate list went stale between the listing and this reserve,
	// which the caller handles as a normal reservation failure.
	if desc.Status != VTXOStatusLive {
		return nil, fmt.Errorf("descriptor status %v is not live",
			desc.Status)
	}

	ref, err := m.spawnVTXOActor(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("respawn actor: %w", err)
	}

	m.actors[outpoint] = ref

	m.logger(ctx).InfoS(ctx, "Respawned VTXO actor from store during "+
		"selection",
		slog.String("outpoint", outpoint.String()),
		slog.Int64("amount", int64(desc.Amount)),
	)

	return ref, nil
}

func (m *Manager) spawnVTXOActor(ctx context.Context, vtxo *Descriptor) (
	VTXOActorRef, error) {

	actorID := fmt.Sprintf("vtxo.%s", vtxo.Outpoint.String())
	serviceKey := VTXOActorServiceKey(vtxo.Outpoint)

	actorCfg := &VTXOActorConfig{
		VTXO:                     vtxo,
		Store:                    m.cfg.Store,
		Wallet:                   m.cfg.Wallet,
		ChainSource:              m.cfg.ChainSource,
		ChainParams:              m.cfg.ChainParams,
		ExpiryConfig:             m.cfg.ExpiryConfig,
		Log:                      m.cfg.Log,
		ChainResolver:            m.cfg.ChainResolver,
		Manager:                  m.managerRef,
		LedgerSink:               m.cfg.LedgerSink,
		RefreshFeeQuoter:         m.cfg.RefreshFeeQuoter,
		FetchOperatorKey:         m.cfg.FetchOperatorKey,
		ForfeitParticipantSigner: m.cfg.ForfeitParticipantSigner,
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
	targetAmount    btcutil.Amount
	minChangeAmount btcutil.Amount
	reserveEvent    actormsg.VTXOActorMsg
	rollback        func(ctx context.Context, ops []wire.OutPoint)
	ask             func(context.Context, VTXOActorRef,
		actormsg.VTXOActorMsg) fn.Result[actormsg.VTXOActorResp]
	label string

	// detached delivers the reservation tell-style: the manager marks
	// the outpoint in its in-memory reservation map, issues the Ask, and
	// observes the child's future via OnComplete instead of awaiting it,
	// so the manager turn never blocks on the per-input FSM write
	// transaction. The spend path enables this; the forfeit path keeps
	// the synchronous ask because round participation wants the durable
	// state settled before it proceeds.
	detached bool
}

// selectAndReserveVTXOs performs largest-first coin selection and
// atomically reserves each selected VTXO by sending reserveEvent to
// its actor. On partial failure the rollback function is called for
// already-reserved outpoints. Returns the selected VTXO details and
// total amount on success.
// gateUnavailableLineage drops candidates whose batch lineage is in a limbo
// (reorged-out) or invalidated (conflict-finalized) canonicality state, so a
// VTXO is never selected while any batch in its lineage is off the canonical
// chain. It is a no-op when no canonicality store is configured (the gate
// stays dormant until the batch producers register their batches). It gates on
// the FULL lineage: a VTXO's direct commitment txid plus every cross-commitment
// ancestor batch (a multi-input OOR VTXO descends from more than one batch, and
// any single reorged-out/invalidated parent makes the leaf unspendable). The
// gate is permissive: an unregistered or unseen batch does not block selection.
// lineageCommitmentTxids returns the deduped set of commitment txids in a
// VTXO's lineage: its direct commitment tx plus every distinct ancestor
// commitment tx recorded in its ancestry. A round-direct or same-commitment
// OOR VTXO yields one txid; a cross-commitment multi-input OOR VTXO yields one
// per contributing batch. The direct commitment txid is included even when the
// ancestry slice is empty (e.g. incoming VTXOs materialized without their
// commitment tree) so the gate still governs the leaf by its batch.
func lineageCommitmentTxids(desc *Descriptor) []chainhash.Hash {
	seen := make(map[chainhash.Hash]struct{}, len(desc.Ancestry)+1)
	txids := make([]chainhash.Hash, 0, len(desc.Ancestry)+1)

	add := func(txid chainhash.Hash) {
		if txid == (chainhash.Hash{}) {
			return
		}
		if _, ok := seen[txid]; ok {
			return
		}
		seen[txid] = struct{}{}
		txids = append(txids, txid)
	}

	add(desc.CommitmentTxID)
	for i := range desc.Ancestry {
		add(desc.Ancestry[i].CommitmentTxID)
	}

	return txids
}

func (m *Manager) gateUnavailableLineage(ctx context.Context,
	candidates []*Descriptor) ([]*Descriptor, error) {

	if m.cfg.BatchCanonicality == nil {
		return candidates, nil
	}

	kept := make([]*Descriptor, 0, len(candidates))
	for _, c := range candidates {
		desc, err := m.cfg.Store.GetVTXO(ctx, c.Outpoint)
		if err != nil {
			return nil, fmt.Errorf("load vtxo for lineage gate "+
				"%s: %w", c.Outpoint, err)
		}

		blocked, avail, err := batchcanon.LineageBlocked(
			ctx, m.cfg.BatchCanonicality,
			lineageCommitmentTxids(desc)...,
		)
		if err != nil {
			return nil, fmt.Errorf("lineage gate %s: %w",
				c.Outpoint, err)
		}
		if blocked {
			m.logger(ctx).DebugS(ctx, "Excluding VTXO with "+
				"unavailable batch lineage from selection",
				slog.String("outpoint", c.Outpoint.String()),
				slog.String("availability", avail.String()))

			continue
		}

		kept = append(kept, c)
	}

	return kept, nil
}

func (m *Manager) selectAndReserveVTXOs(ctx context.Context, p reserveParams) (
	[]SelectedVTXO, btcutil.Amount, error) {

	if p.targetAmount <= 0 {
		return nil, 0, fmt.Errorf("target amount must be positive")
	}

	// List live candidates from the store via the lightweight selection
	// projection: selection only consumes outpoint, amount, and pkScript,
	// so there is no reason to decode full descriptors (taproot script
	// reconstruction, policy decode, ancestry load) on this per-payment
	// path. The projection rows are wrapped as minimal descriptors so the
	// shared largest-first machinery below stays unchanged; these partial
	// descriptors never escape this function (the response carries
	// SelectedVTXO projections built from the same three fields).
	rows, err := m.cfg.Store.ListSelectionCandidatesByStatus(
		ctx, VTXOStatusLive,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("list live vtxos: %w", err)
	}

	// Wrap the lightweight projection rows as minimal descriptors so the
	// shared largest-first selector can consume them without decoding full
	// descriptors. These partial descriptors never escape this function.
	//
	// The in-memory reservation map gates admission ahead of the durable
	// status: a detached spend reservation's Spending write may still be
	// in flight, so a row can read Live here while the outpoint is
	// already owned by a spend session. Both the spend and the forfeit
	// selection paths funnel through this filter.
	candidates := make([]*Descriptor, 0, len(rows))
	for _, row := range rows {
		if m.isReserved(row.Outpoint) {
			continue
		}

		candidates = append(candidates, &Descriptor{
			Outpoint: row.Outpoint,
			Amount:   row.Amount,
			PkScript: row.PkScript,
			Status:   VTXOStatusLive,
		})
	}

	// Drop any candidate whose batch lineage is in limbo or invalidated, so
	// a VTXO whose batch reorged out or was conflict-invalidated is never
	// selected while its lineage is off the canonical chain. No-op when no
	// canonicality store is configured.
	candidates, err = m.gateUnavailableLineage(ctx, candidates)
	if err != nil {
		return nil, 0, err
	}

	// Run largest-first selection through the shared selector. Map its
	// typed outcomes back onto the manager's liquidity diagnostics: a
	// dust-change rejection is reported verbatim, while any shortfall
	// (including an empty candidate set) is refined into the
	// locked-vs-absent distinction.
	res, err := coinselect.LargestFirst(
		candidates, func(d *Descriptor) btcutil.Amount {
			return d.Amount
		}, coinselect.Request{
			Target:    p.targetAmount,
			MinChange: p.minChangeAmount,
		},
	)
	switch {
	case errors.Is(err, coinselect.ErrChangeBelowMin):
		change := res.Total - p.targetAmount

		return nil, 0, fmt.Errorf("change %d is below minimum change "+
			"amount %d", change, p.minChangeAmount)

	case errors.Is(err, coinselect.ErrSelectionShortfall),
		errors.Is(err, coinselect.ErrNoCandidates):
		return nil, 0, m.insufficientLiquidityError(ctx, candidates, p)

	case err != nil:
		return nil, 0, err
	}
	selected := res.Selected

	// Reserve each selected VTXO via its actor. Track successfully
	// reserved outpoints so we can roll back on partial failure.
	var reserved []wire.OutPoint
	for _, vtxo := range selected {
		ref, ok := m.actors[vtxo.Outpoint]
		if !ok {
			// The row is committed Live but the actor is not
			// resident yet: the materialized-VTXO notification is
			// delivered asynchronously after the OOR session's
			// commit, so a selection racing that window (or a
			// notification lost to a crash) sees the row before
			// the actor. The store row is the source of truth, so
			// self-heal by spawning the actor from it instead of
			// failing the payment.
			lazyRef, err := m.respawnActorFromStore(
				ctx, vtxo.Outpoint,
			)
			if err != nil {
				p.rollback(ctx, reserved)

				return nil, 0, fmt.Errorf("no actor for "+
					"outpoint %s: %w", vtxo.Outpoint, err)
			}

			ref = lazyRef
		}

		// Detached (spend) path: mark the in-memory reservation and
		// hand the FSM event to the child without awaiting its write
		// transaction. The outcome is observed asynchronously; a
		// failed reservation hops back as a manager message that
		// drops the mark, and the owning session's spend then fails
		// at signing/submit and retries through the normal machinery.
		if p.detached {
			epoch := m.markReserved(vtxo.Outpoint)
			m.detachedReserve(
				ctx, ref, vtxo.Outpoint, epoch, p.reserveEvent,
				p.label,
			)

			reserved = append(reserved, vtxo.Outpoint)

			continue
		}

		result := p.ask(ctx, ref, p.reserveEvent)
		if _, err := result.Unpack(); err != nil {
			m.logger(ctx).WarnS(
				ctx, p.label+" reserve failed", err,
				slog.String(
					"outpoint", vtxo.Outpoint.String(),
				),
			)
			p.rollback(ctx, reserved)

			// The common post-selection failure is that another
			// actor turn already moved this candidate out of
			// LiveState while the store view was stale. Treat it as
			// locked liquidity so callers can retry; unexpected
			// actor/infrastructure errors remain wrapped inside the
			// returned error for logs.
			return nil, 0, fmt.Errorf("%w: reserve %s %s: %w",
				ErrVTXOLiquidityLocked, p.label, vtxo.Outpoint,
				err)
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

	m.logger(ctx).InfoS(ctx, "Reserved VTXOs for "+p.label,
		slog.Int("count", len(selected)),
		slog.Int64("total", int64(totalSelected)),
		slog.Int64("target", int64(p.targetAmount)),
	)

	return selectedVTXOs, totalSelected, nil
}

// insufficientLiquidityError distinguishes a true spendable-funds shortfall
// from liquidity that is present but unavailable because another operation has
// already moved it out of LiveState.
func (m *Manager) insufficientLiquidityError(ctx context.Context,
	liveCandidates []*Descriptor, p reserveParams) error {

	liveTotal := SumBalance(liveCandidates)

	nonTerminal, err := m.cfg.Store.ListLiveVTXOs(ctx)
	if err != nil {
		return fmt.Errorf("classify liquidity: %w", err)
	}

	var lockedTotal btcutil.Amount
	for _, desc := range nonTerminal {
		if desc == nil {
			continue
		}

		if desc.Status == VTXOStatusLive {
			continue
		}

		lockedTotal += desc.Amount
	}

	if lockedTotal > 0 && liveTotal+lockedTotal >= p.targetAmount {
		return fmt.Errorf("%w: need %d, spendable %d, locked %d",
			ErrVTXOLiquidityLocked, p.targetAmount, liveTotal,
			lockedTotal)
	}

	return fmt.Errorf("%w: need %d, spendable %d",
		ErrInsufficientSpendableFunds, p.targetAmount, liveTotal)
}

// handleSelectAndReserveSpend selects VTXOs covering the target amount using
// largest-first coin selection, then atomically reserves them for an OOR
// spend by Asking each VTXO actor to process SpendReserveEvent. If any
// reservation fails, already-reserved VTXOs are rolled back.
func (m *Manager) handleSelectAndReserveSpend(ctx context.Context,
	req *SelectAndReserveSpendRequest) fn.Result[ManagerResp] {

	vtxos, total, err := m.selectAndReserveVTXOs(ctx, reserveParams{
		targetAmount:    req.TargetAmount,
		minChangeAmount: req.MinChangeAmount,
		reserveEvent:    &SpendReserveEvent{},
		rollback:        m.rollbackSpend,
		ask:             m.askVTXOActor,
		label:           "spend",
		detached:        true,
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

	rollbackCtx, cancel := m.rollbackContext(ctx)
	defer cancel()

	for _, op := range outpoints {
		// Per-actor mailbox FIFO guarantees the release lands after
		// any detached reserve already queued for the same actor, so
		// dropping the in-memory mark here cannot resurrect a
		// half-reserved outpoint.
		m.dropReserved(op)

		ref, ok := m.actors[op]
		if !ok {
			continue
		}

		result := m.askVTXOActor(
			rollbackCtx, ref, &SpendReleasedEvent{},
		)
		if _, err := result.Unpack(); err != nil {
			m.logger(ctx).WarnS(ctx, "Spend rollback failed",
				err,
				slog.String("outpoint", op.String()),
			)
		}
	}
}

// sweepOrphanedReservations releases Spending VTXOs that have no live
// reservation row. A reservation row exists IFF the owning spend session was
// durably checkpointed, and a checkpointed session is always restored+resumed
// on restart; so a Spending VTXO with no row is provably orphaned (its spend
// died before checkpointing) and is safe to release back to LiveState.
//
// The sweep is conservative: if the reservation list cannot be read it aborts
// without releasing anything, because releasing on incomplete information could
// free a VTXO that an in-flight spend still owns.
func (m *Manager) sweepOrphanedReservations(ctx context.Context) {
	if m.cfg.ReservationStore == nil {
		return
	}

	spending, err := m.cfg.Store.ListVTXOsByStatus(
		ctx, VTXOStatusSpending,
	)
	if err != nil {
		m.logger(ctx).ErrorS(
			ctx,
			"Reservation sweep: list Spending VTXOs failed",
			err,
		)

		return
	}

	// Note: do not early-return when there are no Spending VTXOs. The
	// reverse-direction recovery below re-drives reservation rows whose
	// VTXO is still Live (the owning session checkpointed its reservation
	// but the detached SpendingState write never landed before the
	// shutdown). In exactly that case the spending set is empty, so a
	// len(spending) == 0 short-circuit here would skip recovery and leave
	// the live input selectable by another spend.
	reserved, err := m.cfg.ReservationStore.ListReservedOutpoints(ctx)
	if err != nil {
		// Never release on incomplete info: an unreadable reservation
		// list could otherwise free a VTXO an in-flight spend owns.
		m.logger(ctx).ErrorS(
			ctx,
			"Reservation sweep aborted: list reservations failed",
			err,
		)

		return
	}

	reservedSet := fn.NewSet(reserved...)

	var released int
	for _, desc := range spending {
		op := desc.Outpoint
		if reservedSet.Contains(op) {
			continue
		}

		ref, ok := m.actors[op]
		if !ok {
			m.logger(ctx).WarnS(ctx,
				"Reservation sweep: no actor for orphaned "+
					"Spending VTXO", nil,
				slog.String("outpoint", op.String()),
			)

			continue
		}

		result := m.askVTXOActor(ctx, ref, &SpendReleasedEvent{})
		if _, err := result.Unpack(); err != nil {
			m.logger(ctx).WarnS(
				ctx,
				"Reservation sweep: release failed",
				err,
				slog.String("outpoint", op.String()),
			)

			continue
		}

		// Refresh the recovery-time liveDescriptors snapshot so the
		// released VTXO no longer reads as Spending (see
		// markDescriptorLive).
		m.markDescriptorLive(op)

		released++
	}

	// Reverse direction: a reservation row whose VTXO row is NOT in
	// SpendingState means the owning session checkpointed but the
	// detached Spending status write never landed before the shutdown.
	// The session resumes on this boot and still owns the input, so
	// re-mark the in-memory reservation and re-drive the reserve event
	// to converge the durable status. Without this, a restarted daemon
	// could select an input an in-flight session owns.
	spendingSet := fn.NewSet[wire.OutPoint]()
	for _, desc := range spending {
		spendingSet.Add(desc.Outpoint)
	}

	var redriven int
	for _, op := range reserved {
		if spendingSet.Contains(op) {
			continue
		}

		ref, ok := m.actors[op]
		if !ok {
			// Terminal row (the spend completed) or unknown; the
			// owning session's completion path reconciles it.
			continue
		}

		m.markReserved(op)

		if err := ref.Tell(ctx, &SpendReserveEvent{}); err != nil {
			m.logger(ctx).WarnS(
				ctx,
				"Reservation sweep: re-drive reserve failed",
				err,
				slog.String("outpoint", op.String()),
			)
		}

		redriven++
	}

	m.logger(ctx).InfoS(ctx, "Reservation sweep complete",
		slog.Int("spending", len(spending)),
		slog.Int("reserved", len(reserved)),
		slog.Int("released", released),
		slog.Int("redriven", redriven),
	)
}

// releaseOrphanedForfeits returns VTXOs stranded in PendingForfeitState back to
// LiveState at startup. A VTXO enters PendingForfeitState when an in-flight
// cooperative round (refresh, leave, or directed send) reserves it as a forfeit
// input. That reservation is owned by the round FSM, which is in-memory only:
// temp rounds are never persisted, and the rounds table is not written until
// InputSigSentState, the point of no return. A daemon that restarts while a
// round sits in any pre-signing state therefore loses the FSM that would have
// released the reservation on failure (round.releaseForfeitsOnFailure), leaving
// the VTXO orphaned in PendingForfeitState with no path back to Live.
//
// Any VTXO found in PendingForfeitState at startup is provably orphaned.
// Forfeit signatures are only submitted on the PendingForfeit -> Forfeiting
// transition, so a still-PendingForfeit VTXO has leaked no signature and the
// operator cannot broadcast a forfeit; returning it to LiveState is safe and
// cannot double-spend. VTXOs already in Forfeiting or Forfeited are past the
// point of no return and are deliberately left untouched for chain-confirmation
// reconciliation.
func (m *Manager) releaseOrphanedForfeits(ctx context.Context) {
	pending, err := m.cfg.Store.ListVTXOsByStatus(
		ctx, VTXOStatusPendingForfeit,
	)
	if err != nil {
		m.logger(ctx).ErrorS(
			ctx,
			"Forfeit sweep: list PendingForfeit VTXOs failed",
			err,
		)

		return
	}

	var released int
	for _, desc := range pending {
		op := desc.Outpoint

		ref, ok := m.actors[op]
		if !ok {
			m.logger(ctx).WarnS(ctx,
				"Forfeit sweep: no actor for orphaned "+
					"PendingForfeit VTXO", nil,
				slog.String("outpoint", op.String()),
			)

			continue
		}

		// Bound the ask so a single wedged child actor cannot stall the
		// daemon's startup critical path indefinitely; this matches
		// every other forfeit-path ask in the manager.
		result := m.askForfeitVTXOActor(
			ctx, ref, &ForfeitReleasedEvent{},
		)
		if _, err := result.Unpack(); err != nil {
			m.logger(ctx).WarnS(
				ctx,
				"Forfeit sweep: release failed",
				err,
				slog.String("outpoint", op.String()),
			)

			continue
		}

		// The release flipped the actor and DB status to Live, but the
		// liveDescriptors snapshot was captured during actor recovery
		// before this sweep ran. Refresh it so downstream consumers
		// such as the fraud-watch restore do not see the stale
		// PendingForfeit status and skip the now-live VTXO.
		m.markDescriptorLive(op)

		released++
	}

	m.logger(ctx).InfoS(ctx, "Forfeit sweep complete",
		slog.Int("pending_forfeit", len(pending)),
		slog.Int("released", released),
	)
}

// markDescriptorLive updates the cached liveDescriptors snapshot so a VTXO that
// a startup sweep returned to LiveState is reported with its true status. The
// snapshot is captured during actor recovery, before the sweeps run, so without
// this refresh a released VTXO would still read with its pre-sweep status
// (PendingForfeit or Spending). Consumers that key on status would then act on
// stale data: the fraud-watch restore, for one, skips any descriptor whose
// status is not Live and would leave a swept OOR VTXO's ancestor spend watches
// un-armed. The entries are pointers into the same descriptors the snapshot
// shares, so mutating Status in place is visible to every reader.
func (m *Manager) markDescriptorLive(op wire.OutPoint) {
	for _, desc := range m.liveDescriptors {
		if desc.Outpoint == op {
			desc.Status = VTXOStatusLive

			return
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
			errs = append(
				errs, fmt.Errorf("no actor for outpoint %s",
					op),
			)

			continue
		}

		result := m.askVTXOActor(ctx, ref, &SpendReleasedEvent{})
		if _, err := result.Unpack(); err != nil {
			m.logger(ctx).WarnS(ctx, "Spend release failed",
				err,
				slog.String("outpoint", op.String()),
			)
			errs = append(
				errs, fmt.Errorf("release %s: %w", op, err),
			)

			continue
		}

		// The SpendReleasedEvent above moved the VTXO out of
		// SpendingState, which deletes its durable reservation row in
		// the same transaction as the status change (see the VTXO
		// actor's processStatusUpdate), so no separate delete is needed
		// here.
		m.dropReserved(op)
		released++
	}

	if len(errs) > 0 {
		return fn.Err[ManagerResp](
			fmt.Errorf(
				"release spend: %d/%d failed: %w", len(errs),
				len(outpoints), errors.Join(errs...),
			),
		)
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
				m.dropReserved(op)
				completed++

				continue
			}

			return fn.Err[ManagerResp](
				fmt.Errorf("no actor for outpoint %s", op),
			)
		}

		result := m.askVTXOActor(ctx, ref, &SpendCompletedEvent{})
		if _, err := result.Unpack(); err != nil {
			return fn.Err[ManagerResp](
				fmt.Errorf("complete %s: %w", op, err),
			)
		}

		// The SpendCompletedEvent above moved the VTXO out of
		// SpendingState, which deletes its durable reservation row in
		// the same transaction as the status change (see the VTXO
		// actor's processStatusUpdate), so no separate delete is needed
		// here.
		m.dropReserved(op)
		completed++
	}

	m.logger(ctx).InfoS(ctx, "Completed OOR spend",
		slog.Int("count", completed),
	)

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
func (m *Manager) isPersistedSpent(ctx context.Context, op wire.OutPoint) (bool,
	error) {

	if m.cfg.Store == nil {
		return false, nil
	}

	desc, err := m.cfg.Store.GetVTXO(ctx, op)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}

		return false, fmt.Errorf("load vtxo for spent check %s: %w", op,
			err)
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

	// Validate all outpoints are known before attempting reservation,
	// and refuse outpoints holding an in-memory spend reservation whose
	// durable Spending write may still be in flight: the child's FSM
	// would otherwise still read LiveState and accept a conflicting
	// forfeit reservation.
	for _, op := range outpoints {
		if _, ok := m.actors[op]; !ok {
			return fn.Err[ManagerResp](
				fmt.Errorf("no actor for outpoint %s", op),
			)
		}

		if m.isReserved(op) {
			return fn.Err[ManagerResp](
				fmt.Errorf("%w: outpoint %s is spend-reserved",
					ErrVTXOLiquidityLocked, op),
			)
		}

		// Refuse to forfeit a VTXO whose batch lineage is in limbo
		// (reorged out) or invalidated: forfeiting commits the VTXO
		// into a round, and a VTXO that is not on the canonical chain
		// must not be spent. The coin-selection gate
		// (gateUnavailableLineage) already excludes such VTXOs, but the
		// wallet's explicit-outpoint paths (refresh / leave / sweep-all
		// / replay) reserve by name and bypass selection, so the same
		// gate is enforced here (darepo#454).
		blocked, avail, err := m.forfeitLineageBlocked(ctx, op)
		if err != nil {
			return fn.Err[ManagerResp](err)
		}
		if blocked {
			return fn.Err[ManagerResp](
				fmt.Errorf("%w: outpoint %s batch lineage "+
					"unavailable (%s)",
					ErrVTXOLiquidityLocked, op, avail),
			)
		}
	}

	// Reserve each VTXO. Track successes for rollback on failure.
	var reserved []wire.OutPoint
	for _, op := range outpoints {
		ref := m.actors[op]
		result := m.askForfeitVTXOActor(
			ctx, ref, &PendingForfeitEvent{},
		)
		if _, err := result.Unpack(); err != nil {
			m.logger(ctx).WarnS(ctx, "Forfeit reserve failed",
				err,
				slog.String("outpoint", op.String()),
			)
			m.rollbackForfeit(ctx, reserved)

			return fn.Err[ManagerResp](
				fmt.Errorf("reserve forfeit %s: %w", op, err),
			)
		}

		reserved = append(reserved, op)
	}

	m.logger(ctx).InfoS(ctx, "Reserved VTXOs for forfeit",
		slog.Int("count", len(reserved)),
	)

	return fn.Ok[ManagerResp](&ReserveForfeitResponse{})
}

// forfeitLineageBlocked reports whether the named VTXO's batch lineage is in a
// limbo (reorged-out) or invalidated (conflict-finalized) canonicality state,
// so an explicit forfeit reservation must be refused. It mirrors the
// coin-selection gate (gateUnavailableLineage) for the explicit-outpoint
// reserve path, gating on the full lineage (direct + ancestor commitment
// txids): a no-op when no canonicality store is configured, and permissive for
// unseen / unregistered lineage.
func (m *Manager) forfeitLineageBlocked(ctx context.Context, op wire.OutPoint) (
	bool, batchcanon.Availability, error) {

	if m.cfg.BatchCanonicality == nil {
		return false, batchcanon.AvailabilityUnknown, nil
	}

	desc, err := m.cfg.Store.GetVTXO(ctx, op)
	if err != nil {
		return false, batchcanon.AvailabilityUnknown,
			fmt.Errorf("load vtxo for forfeit lineage gate %s: %w",
				op, err)
	}

	return batchcanon.LineageBlocked(
		ctx, m.cfg.BatchCanonicality, lineageCommitmentTxids(desc)...,
	)
}

// rollbackForfeit sends ForfeitReleasedEvent to previously reserved VTXOs.
// Best-effort: errors are logged but do not propagate.
func (m *Manager) rollbackForfeit(ctx context.Context,
	outpoints []wire.OutPoint) {

	rollbackCtx, cancel := m.rollbackContext(ctx)
	defer cancel()

	for _, op := range outpoints {
		ref, ok := m.actors[op]
		if !ok {
			continue
		}

		result := m.askForfeitVTXOActor(
			rollbackCtx, ref, &ForfeitReleasedEvent{},
		)
		if _, err := result.Unpack(); err != nil {
			m.logger(ctx).WarnS(ctx, "Forfeit rollback failed",
				err,
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
			errs = append(
				errs, fmt.Errorf("no actor for outpoint %s",
					op),
			)

			continue
		}

		result := m.askForfeitVTXOActor(
			ctx, ref, &ForfeitReleasedEvent{},
		)
		if _, err := result.Unpack(); err != nil {
			m.logger(ctx).WarnS(ctx, "Forfeit release failed",
				err,
				slog.String("outpoint", op.String()),
			)
			errs = append(
				errs, fmt.Errorf("release forfeit %s: %w", op,
					err),
			)

			continue
		}

		released++
	}

	if len(errs) > 0 {
		return fn.Err[ManagerResp](
			fmt.Errorf(
				"release forfeit: %d/%d failed: %w", len(errs),
				len(outpoints), errors.Join(errs...),
			),
		)
	}

	return fn.Ok[ManagerResp](&ReleaseForfeitResponse{
		ReleasedCount: released,
	})
}

// handleActivateCustomForfeitInputs starts temporary PendingForfeit actors for
// custom-policy VTXOs. If the input is not already in the store, activation
// persists a synthetic descriptor for the temporary signer. If the durable row
// already exists, activation overlays the normal actor without changing the
// row, so rollback can restore the ordinary VTXO actor from storage.
func (m *Manager) handleActivateCustomForfeitInputs(ctx context.Context,
	req *ActivateCustomForfeitInputsRequest) fn.Result[ManagerResp] {

	if len(req.Inputs) == 0 {
		return fn.Err[ManagerResp](
			fmt.Errorf("custom forfeit inputs are empty"),
		)
	}

	var (
		activated          int
		activatedOutpoints []wire.OutPoint
	)
	fail := func(err error) fn.Result[ManagerResp] {
		rollbackErr := m.rollbackActivatedCustomForfeitInputs(
			ctx, activatedOutpoints,
		)
		if rollbackErr != nil {
			err = errors.Join(
				err, fmt.Errorf("rollback activated custom "+
					"forfeit inputs: %w", rollbackErr),
			)
		}

		return fn.Err[ManagerResp](err)
	}

	storedMatch := m.customForfeitInputStoredMatch
	for _, input := range req.Inputs {
		if input.OperatorKey == nil {
			return fail(
				fmt.Errorf("custom forfeit input %s missing "+
					"operator key", input.Outpoint),
			)
		}
		if input.ClientKey.PubKey == nil {
			return fail(
				fmt.Errorf("custom forfeit input %s missing "+
					"client key", input.Outpoint),
			)
		}

		var (
			synthetic      bool
			syntheticKnown bool
		)

		if _, exists := m.actors[input.Outpoint]; exists {
			if m.customForfeitActorActive(input.Outpoint) {
				ok, err := m.customForfeitInputAlreadyActive(
					ctx, input,
				)
				if err != nil {
					return fail(err)
				}
				if !ok {
					err := fmt.Errorf("custom forfeit "+
						"input %s conflicts with "+
						"active custom signer",
						input.Outpoint)

					return fail(err)
				}

				activated++

				continue
			}

			ok, storedSynthetic, err := storedMatch(ctx, input)
			if err != nil {
				return fail(err)
			}
			if !ok {
				err := fmt.Errorf("custom forfeit input %s "+
					"conflicts with existing VTXO actor",
					input.Outpoint)

				return fail(err)
			}

			actorID := fmt.Sprintf("vtxo.%s",
				input.Outpoint.String())
			m.cfg.ActorSystem.StopAndRemoveActor(actorID)
			delete(m.actors, input.Outpoint)
			synthetic = storedSynthetic
			syntheticKnown = true
		}

		if !syntheticKnown {
			var err error
			synthetic, err = m.customForfeitInputIsSynthetic(
				ctx, input.Outpoint,
			)
			if err != nil {
				return fail(err)
			}
		}
		if !synthetic {
			ok, storedSynthetic, err := storedMatch(ctx, input)
			if err != nil {
				return fail(err)
			}
			if !ok {
				err := fmt.Errorf("custom forfeit input %s "+
					"does not match existing VTXO row",
					input.Outpoint)

				return fail(err)
			}
			synthetic = storedSynthetic
		}

		descriptor := customForfeitInputDescriptor(input)

		if synthetic {
			if err := m.cfg.Store.SaveVTXO(
				ctx, descriptor,
			); err != nil {
				return fail(
					fmt.Errorf("save custom forfeit "+
						"input %s: %w", input.Outpoint,
						err),
				)
			}
			if err := m.cfg.Store.UpdateVTXOStatus(
				ctx, input.Outpoint, VTXOStatusPendingForfeit,
			); err != nil {

				_ = m.cfg.Store.DeleteVTXO(ctx, input.Outpoint)

				return fail(
					fmt.Errorf("mark custom forfeit "+
						"input %s pending: %w",
						input.Outpoint, err),
				)
			}
		}

		ref, err := m.spawnVTXOActor(ctx, descriptor)
		if err != nil {
			if synthetic {
				_ = m.cfg.Store.DeleteVTXO(ctx, input.Outpoint)
			} else {
				_ = m.respawnCustomForfeitBaseActor(
					ctx, input.Outpoint,
				)
			}

			return fail(
				fmt.Errorf("spawn custom forfeit input %s: %w",
					input.Outpoint, err),
			)
		}

		m.actors[input.Outpoint] = ref
		m.markCustomForfeitSynthetic(input.Outpoint, synthetic)
		activatedOutpoints = append(activatedOutpoints, input.Outpoint)
		activated++
	}

	m.logger(ctx).InfoS(ctx, "Activated custom forfeit inputs",
		slog.Int("count", activated),
	)

	return fn.Ok[ManagerResp](&ActivateCustomForfeitInputsResponse{
		ActivatedCount: activated,
	})
}

// customForfeitInputDescriptor converts one caller-supplied custom refresh
// input into the temporary PendingForfeit descriptor used by the signer actor.
func customForfeitInputDescriptor(input CustomForfeitInput) *Descriptor {
	return &Descriptor{
		Outpoint:       input.Outpoint,
		Amount:         input.Amount,
		PkScript:       append([]byte(nil), input.PkScript...),
		PolicyTemplate: bytes.Clone(input.PolicyTemplate),
		ClientKey:      input.ClientKey,
		OperatorKey:    input.OperatorKey,
		RelativeExpiry: input.RelativeExpiry,
		RoundID:        input.RoundID,
		CommitmentTxID: input.CommitmentTxID,
		BatchExpiry:    input.BatchExpiry,
		ChainDepth:     input.ChainDepth,
		CreatedHeight:  input.CreatedHeight,
		Ancestry:       input.Ancestry,
		Status:         VTXOStatusPendingForfeit,
	}
}

// customForfeitInputIsSynthetic reports whether activating a custom forfeit
// input must create a throw-away descriptor. Already-known VTXOs can still use
// temporary custom signer actors, but rollback must leave their durable row
// intact because other tables, such as OOR package bindings, may reference it.
func (m *Manager) customForfeitInputIsSynthetic(ctx context.Context,
	outpoint wire.OutPoint) (bool, error) {

	desc, err := m.cfg.Store.GetVTXO(ctx, outpoint)
	switch {
	case err == nil:
		if desc == nil {
			return false, fmt.Errorf("check custom forfeit input "+
				"%s: nil descriptor", outpoint)
		}

		return false, nil

	case errors.Is(err, sql.ErrNoRows):
		return true, nil

	default:
		return false, fmt.Errorf("check custom forfeit input %s: %w",
			outpoint, err)
	}
}

// customForfeitActorActive reports whether the current actor for outpoint is a
// temporary custom signer managed by the custom refresh overlay map.
func (m *Manager) customForfeitActorActive(outpoint wire.OutPoint) bool {
	if m.customForfeitSynthetic == nil {
		return false
	}

	_, ok := m.customForfeitSynthetic[outpoint]

	return ok
}

// markCustomForfeitSynthetic records whether a custom signer owns a synthetic
// descriptor. Nil-safe so tests that construct Manager literals without
// NewManager preserve their existing setup style.
func (m *Manager) markCustomForfeitSynthetic(op wire.OutPoint, synthetic bool) {
	if m.customForfeitSynthetic == nil {
		m.customForfeitSynthetic = make(map[wire.OutPoint]bool)
	}

	m.customForfeitSynthetic[op] = synthetic
}

func (m *Manager) customForfeitInputAlreadyActive(ctx context.Context,
	input CustomForfeitInput) (bool, error) {

	return m.customForfeitInputMatchesStored(ctx, input)
}

// customForfeitInputMatchesStored reports whether the durable row for input's
// outpoint has the same immutable vHTLC identity as the custom signer request.
// The status is intentionally ignored: existing swap/recovery rows can be Live
// or Spending, while synthetic signer rows are persisted as PendingForfeit.
func (m *Manager) customForfeitInputMatchesStored(ctx context.Context,
	input CustomForfeitInput) (bool, error) {

	matches, _, err := m.customForfeitInputStoredMatch(ctx, input)

	return matches, err
}

// customForfeitInputStoredMatch reports whether the durable row for input's
// outpoint matches the custom signer request, and whether that row looks like a
// synthetic custom signer row recovered after a restart.
func (m *Manager) customForfeitInputStoredMatch(ctx context.Context,
	input CustomForfeitInput) (bool, bool, error) {

	desc, err := m.cfg.Store.GetVTXO(ctx, input.Outpoint)
	if err != nil {
		return false, false, fmt.Errorf("load existing custom forfeit "+
			"input %s: %w", input.Outpoint, err)
	}
	if desc == nil {
		return false, false, fmt.Errorf("load existing custom forfeit "+
			"input %s: nil descriptor", input.Outpoint)
	}
	synthetic := desc.Status == VTXOStatusPendingForfeit
	if desc.Amount != input.Amount {
		return false, synthetic, nil
	}
	if !bytes.Equal(desc.PkScript, input.PkScript) {
		return false, synthetic, nil
	}
	if !bytes.Equal(desc.PolicyTemplate, input.PolicyTemplate) {
		return false, synthetic, nil
	}
	if desc.ClientKey.PubKey == nil || !desc.ClientKey.PubKey.IsEqual(
		input.ClientKey.PubKey,
	) {
		return false, synthetic, nil
	}
	if !sameTaprootKey(desc.OperatorKey, input.OperatorKey) {
		return false, synthetic, nil
	}
	if desc.RoundID != input.RoundID {
		return false, synthetic, nil
	}
	if desc.CommitmentTxID != input.CommitmentTxID {
		return false, synthetic, nil
	}
	if desc.BatchExpiry != input.BatchExpiry {
		return false, synthetic, nil
	}
	if desc.ChainDepth != input.ChainDepth {
		return false, synthetic, nil
	}
	if desc.CreatedHeight != input.CreatedHeight {
		return false, synthetic, nil
	}

	return true, synthetic, nil
}

func sameTaprootKey(a, b *btcec.PublicKey) bool {
	if a == nil || b == nil {
		return false
	}

	return bytes.Equal(
		schnorr.SerializePubKey(a), schnorr.SerializePubKey(b),
	)
}

// respawnCustomForfeitBaseActor restores the normal actor for a pre-existing
// VTXO after a temporary custom signer has been stopped or failed to start.
func (m *Manager) respawnCustomForfeitBaseActor(ctx context.Context,
	outpoint wire.OutPoint) error {

	desc, err := m.cfg.Store.GetVTXO(ctx, outpoint)
	if err != nil {
		return fmt.Errorf("load base custom forfeit input %s: %w",
			outpoint, err)
	}
	if desc == nil {
		return fmt.Errorf("load base custom forfeit input %s: nil "+
			"descriptor", outpoint)
	}

	if statusToState(ctx, desc, m.cfg.Store, m.logger(ctx)).IsTerminal() {
		return nil
	}

	ref, err := m.spawnVTXOActor(ctx, desc)
	if err != nil {
		return fmt.Errorf("respawn base custom forfeit actor %s: %w",
			outpoint, err)
	}

	m.actors[outpoint] = ref

	return nil
}

// dropCustomForfeitSynthetic forgets and returns the synthetic-descriptor bit
// for a custom signer actor. Missing entries default to true for compatibility
// with actors created before this field was introduced; those actors were
// historically treated as rollback-owned descriptors.
func (m *Manager) dropCustomForfeitSynthetic(op wire.OutPoint) bool {
	if m.customForfeitSynthetic == nil {
		return true
	}

	synthetic, ok := m.customForfeitSynthetic[op]
	delete(m.customForfeitSynthetic, op)
	if !ok {
		return true
	}

	return synthetic
}

// rollbackActivatedCustomForfeitInputs drops custom signer overlays created by
// a failed activation request.
func (m *Manager) rollbackActivatedCustomForfeitInputs(ctx context.Context,
	outpoints []wire.OutPoint) error {

	if len(outpoints) == 0 {
		return nil
	}

	result := m.handleDropCustomForfeitInputs(
		ctx, &DropCustomForfeitInputsRequest{
			Outpoints: outpoints,
		},
	)
	_, err := result.Unpack()
	if err != nil {
		return err
	}

	return nil
}

// handleDropCustomForfeitInputs removes custom PendingForfeit signer overlays
// created for a round intent that did not register. Unlike
// ReleaseForfeitRequest, this must not return synthetic descriptors to
// LiveState because custom swap vHTLCs are not normal wallet coins.
// Pre-existing VTXO rows are retained and their ordinary actors are restored
// from storage.
func (m *Manager) handleDropCustomForfeitInputs(ctx context.Context,
	req *DropCustomForfeitInputsRequest) fn.Result[ManagerResp] {

	outpoints := dedupOutpoints(req.Outpoints)

	var (
		dropped int
		errs    []error
	)
	for _, op := range outpoints {
		if _, ok := m.actors[op]; ok {
			actorID := fmt.Sprintf("vtxo.%s", op.String())
			m.cfg.ActorSystem.StopAndRemoveActor(actorID)
			delete(m.actors, op)
		}

		synthetic := m.dropCustomForfeitSynthetic(op)
		if !synthetic {
			if err := m.respawnCustomForfeitBaseActor(
				ctx, op,
			); err != nil {

				errs = append(
					errs, fmt.Errorf("restore custom "+
						"forfeit base actor %s: %w",
						op, err),
				)

				continue
			}

			dropped++

			continue
		}

		if err := m.cfg.Store.DeleteVTXO(ctx, op); err != nil {
			errs = append(
				errs, fmt.Errorf("delete custom forfeit "+
					"input %s: %w", op, err),
			)

			continue
		}

		dropped++
	}

	if len(errs) > 0 {
		return fn.Err[ManagerResp](
			fmt.Errorf(
				"drop custom forfeit inputs: %w",
				errors.Join(errs...),
			),
		)
	}

	m.logger(ctx).InfoS(ctx, "Dropped custom forfeit inputs",
		slog.Int("count", dropped),
	)

	return fn.Ok[ManagerResp](&DropCustomForfeitInputsResponse{
		DroppedCount: dropped,
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
		ask:          m.askForfeitVTXOActor,
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
// metadata from the VTXOCreatedNotification. The single round-direct
// ancestry path is materialized into Descriptor.Ancestry as a length-1
// slice; cross-round multi-input cases are constructed elsewhere from
// indexer responses, not from a single client VTXO.
func clientVTXOToDescriptor(cv *round.ClientVTXO,
	msg *round.VTXOCreatedNotification) fn.Result[*Descriptor] {

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

	// The round path stamps a length-1 Ancestry on the ClientVTXO at
	// build time (TreePath/TreeDepth) and fills in the per-fragment
	// CommitmentTxID once the round confirms, so we just clone the
	// slice through here. We do NOT overwrite per-fragment CommitmentTxIDs
	// from msg.CommitmentTxID because that conflates the parent-descriptor
	// commitment with each fragment's anchoring commitment, which only
	// agree for round-direct VTXOs.
	ancestry := make([]Ancestry, len(cv.Ancestry))
	copy(ancestry, cv.Ancestry)

	return fn.Ok(&Descriptor{
		Outpoint:       cv.Outpoint,
		Amount:         cv.Amount,
		PolicyTemplate: cv.PolicyTemplate,
		PkScript:       cv.PkScript,
		ClientKey:      cv.OwnerKey,
		OperatorKey:    cv.OperatorKey,
		TapScript:      tapscript,
		Ancestry:       ancestry,
		RoundID:        msg.RoundID,
		CommitmentTxID: msg.CommitmentTxID,
		BatchExpiry:    msg.BatchExpiry,
		RelativeExpiry: cv.Expiry,
		ChainDepth:     0,
		CreatedHeight:  msg.CreatedHeight,
		Status:         VTXOStatusLive,
	})
}

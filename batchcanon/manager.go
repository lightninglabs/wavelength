package batchcanon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// ManagerServiceKey is the receptionist key the BatchCanonicalityManager
// registers under.
var ManagerServiceKey = actor.NewServiceKey[ManagerMsg, ManagerResp](
	"batch-canonicality",
)

// usabilityConfs is the confirmation count at which the manager wants the
// first positive confirmation notification. Ark's usability depth is one
// confirmation: a batch is provisionally usable as soon as it confirms, and
// the reorg-aware lifecycle keeps it correct from there. Policy finality is
// signalled separately by chainsource's Done event at its FinalityDepth.
const usabilityConfs uint32 = 1

// confState is the manager's in-memory view of a batch tx's confirmation
// observation, distinct from any input-conflict view.
type confState int

const (
	confUnseen confState = iota
	confConfirmed
	confFinalized
	confReorgedOut
)

// inputWatch tracks the conflict view of one consumed batch input.
type inputWatch struct {
	// spenderIsConflict records whether the last observed spend of this
	// input was by a transaction other than the batch itself. The batch
	// consuming its own input is the expected, non-conflicting case.
	spenderIsConflict bool

	// conflicting is true while a conflicting spend is observed and has not
	// been reorged out.
	conflicting bool

	// conflictFinal is true once a conflicting spend matured past the
	// reorg-safety depth.
	conflictFinal bool
}

// batchWatch is the manager's in-memory state for one watched batch.
type batchWatch struct {
	txid     chainhash.Hash
	pkScript []byte

	conf   confState
	inputs map[wire.OutPoint]*inputWatch

	// persisted is the State last written to the store, so the manager only
	// issues an UpdateBatchState when the derived state actually changes.
	persisted State
}

// ManagerConfig configures the BatchCanonicalityManager.
type ManagerConfig struct {
	// Store is the durable canonicality store.
	Store Store

	// ChainSource is the chain-observation actor the manager registers
	// reorg-aware conf/spend watches with.
	ChainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]

	// RestoreConsumedVTXO, when set, is invoked for each VTXO outpoint that
	// was provisionally forfeited into a batch that has now been
	// invalidated (a finalized conflict reversed its forfeit). The daemon
	// wires this to the VTXO manager to roll the VTXO back to a spendable
	// state. Nil leaves the reverse-dependency edges in place without
	// acting -- the default until the FSM restore path is wired -- so the
	// data model stays consistent and a later run can still act on the
	// persisted edges.
	RestoreConsumedVTXO func(ctx context.Context, vtxo wire.OutPoint) error

	// Log is an optional logger.
	Log fn.Option[btclog.Logger]
}

// Manager is the sole client-side interpreter of batch canonicality. It
// observes (via chainsource) each batch tx confirmation and each consumed
// input, interprets the reorg-aware lifecycle into batchcanon.State, and
// persists the result. It is an actor behavior: chainsource events arrive as
// internal messages re-wrapped onto the manager's own mailbox.
//
// Spend watches on consumed inputs are registered with the prevout pkScript
// carried on each RegisterBatchRequest.ConsumedInput, which lnd's spend
// notifier requires (it filters by output script). An input with no pkScript
// is skipped rather than failing the whole batch registration, so the
// confirmation watch and the remaining inputs still arm.
type Manager struct {
	cfg     ManagerConfig
	log     btclog.Logger
	selfRef actor.TellOnlyRef[ManagerMsg]

	watches map[chainhash.Hash]*batchWatch
}

// NewManager builds a BatchCanonicalityManager behavior. SetSelfRef must be
// called (with the registered actor's TellRef) before any batch is registered
// so the manager can route chainsource events back to itself.
func NewManager(cfg ManagerConfig) *Manager {
	return &Manager{
		cfg:     cfg,
		log:     cfg.Log.UnwrapOr(btclog.Disabled),
		watches: make(map[chainhash.Hash]*batchWatch),
	}
}

// SetSelfRef wires the manager's own mailbox ref, used to build the mapped
// chainsource notification refs.
func (m *Manager) SetSelfRef(ref actor.TellOnlyRef[ManagerMsg]) {
	m.selfRef = ref
}

// Receive implements actor.ActorBehavior. It serializes all canonicality
// mutations through the single actor mailbox.
func (m *Manager) Receive(ctx context.Context,
	msg ManagerMsg) fn.Result[ManagerResp] {

	switch v := msg.(type) {
	case *RegisterBatchRequest:
		return m.handleRegisterBatch(ctx, v)

	case *GetBatchStateRequest:
		return m.handleGetBatchState(ctx, v)

	case *batchConfirmedMsg:
		m.handleBatchConfirmed(ctx, v)

	case *batchReorgedMsg:
		m.handleBatchReorged(ctx, v)

	case *batchDoneMsg:
		m.handleBatchDone(ctx, v)

	case *inputSpentMsg:
		m.handleInputSpent(ctx, v)

	case *inputSpendReorgedMsg:
		m.handleInputSpendReorged(ctx, v)

	case *inputSpendDoneMsg:
		m.handleInputSpendDone(ctx, v)

	default:
		return fn.Err[ManagerResp](
			fmt.Errorf("unknown batchcanon message: %T", msg),
		)
	}

	return fn.Ok[ManagerResp](&ackResponse{})
}

// logger returns the configured logger, falling back to the context logger.
func (m *Manager) logger(ctx context.Context) btclog.Logger {
	return m.cfg.Log.UnwrapOr(build.LoggerFromContext(ctx))
}

// handleRegisterBatch persists the batch record and arms its watches. It is
// idempotent: a repeat for the same batch merges the dependent VTXOs into the
// record without re-arming watches.
func (m *Manager) handleRegisterBatch(ctx context.Context,
	req *RegisterBatchRequest) fn.Result[ManagerResp] {

	existing, ok := m.watches[req.BatchTxID]
	if ok {
		// Already watching: merge dependent VTXOs into the record and
		// return without duplicating watches.
		if err := m.mergeDependents(ctx, existing, req); err != nil {
			return fn.Err[ManagerResp](err)
		}

		return fn.Ok[ManagerResp](&RegisterBatchResponse{})
	}

	// Persist the initial record (unseen until the first observation).
	record := &Record{
		BatchTxID:            req.BatchTxID,
		State:                StateUnseen,
		ConfirmationHeight:   fn.None[int32](),
		ConfirmationBlock:    fn.None[chainhash.Hash](),
		CSVExpiryDelta:       req.CSVExpiryDelta,
		PolicyState:          PolicyStateDefault,
		ConfirmationPkScript: req.ConfirmationPkScript,
		ConsumedInputs:       req.ConsumedInputs,
		DependentVTXOs:       req.DependentVTXOs,
	}
	if err := m.cfg.Store.UpsertBatch(ctx, record); err != nil {
		return fn.Err[ManagerResp](
			fmt.Errorf("persist batch record: %w", err),
		)
	}

	// Record a reverse-dependency edge for every VTXO this batch forfeits,
	// so the VTXO can be restored if this batch is later invalidated. The
	// edges are persisted and idempotent on (consumedVTXO, consumerBatch).
	for _, forfeited := range req.ForfeitedVTXOs {
		err := m.cfg.Store.AddProvisionalConsumer(
			ctx, forfeited, req.BatchTxID,
		)
		if err != nil {
			return fn.Err[ManagerResp](
				fmt.Errorf("record provisional consumer %s "+
					"-> %s: %w", forfeited, req.BatchTxID,
					err),
			)
		}
	}

	w := &batchWatch{
		txid:     req.BatchTxID,
		pkScript: req.ConfirmationPkScript,
		conf:     confUnseen,
		inputs:   make(map[wire.OutPoint]*inputWatch),
	}
	for _, in := range req.ConsumedInputs {
		w.inputs[in.Outpoint] = &inputWatch{}
	}

	// Record the watch only AFTER arming succeeds. If we recorded it first
	// and arming failed, a retry would take the idempotent "already
	// watching" merge path at the top of handleRegisterBatch and never
	// re-arm the missing/partial chain watches until a restart. Leaving
	// m.watches untouched on failure means a retry re-arms from scratch;
	// re-registering the same conf/spend caller IDs is idempotent (the same
	// property Reconcile relies on after restart).
	if err := m.armWatches(ctx, w, req.ConsumedInputs); err != nil {
		return fn.Err[ManagerResp](err)
	}
	m.watches[req.BatchTxID] = w

	return fn.Ok[ManagerResp](&RegisterBatchResponse{})
}

// mergeDependents adds any new dependent VTXOs from a repeat registration to
// the persisted record, keeping the batch's existing watches and state.
func (m *Manager) mergeDependents(ctx context.Context, w *batchWatch,
	req *RegisterBatchRequest) error {

	record, err := m.cfg.Store.GetBatch(ctx, w.txid)
	if err != nil {
		return fmt.Errorf("load batch for merge: %w", err)
	}

	seen := make(map[wire.OutPoint]struct{}, len(record.DependentVTXOs))
	for _, dep := range record.DependentVTXOs {
		seen[dep] = struct{}{}
	}
	changed := false
	for _, dep := range req.DependentVTXOs {
		if _, ok := seen[dep]; ok {
			continue
		}
		record.DependentVTXOs = append(record.DependentVTXOs, dep)
		seen[dep] = struct{}{}
		changed = true
	}
	if !changed {
		return nil
	}

	return m.cfg.Store.UpsertBatch(ctx, record)
}

// armWatches registers the reorg-aware confirmation watch on the batch tx and
// a reorg-aware spend watch on each consumed input.
func (m *Manager) armWatches(ctx context.Context, w *batchWatch,
	inputs []ConsumedInput) error {

	heightHint := m.bestHeightHint(ctx)

	confReq := &chainsource.RegisterConfRequest{
		CallerID:    confCallerID(w.txid),
		Txid:        &w.txid,
		PkScript:    w.pkScript,
		TargetConfs: usabilityConfs,
		HeightHint:  heightHint,
		NotifyActor: fn.Some(
			chainsource.MapConfirmationEvent(
				m.selfRef,
				func(
					ce chainsource.ConfirmationEvent,
				) ManagerMsg {

					return &batchConfirmedMsg{
						txid:        ce.Txid,
						blockHeight: ce.BlockHeight,
						blockHash:   ce.BlockHash,
					}
				},
			),
		),
		NotifyReorged: fn.Some(
			chainsource.MapConfReorgedEvent(
				m.selfRef,
				func(
					ev chainsource.ConfReorgedEvent,
				) ManagerMsg {

					return &batchReorgedMsg{
						txid: ev.Txid,
					}
				},
			),
		),
		NotifyDone: fn.Some(
			chainsource.MapConfDoneEvent(
				m.selfRef,
				func(ev chainsource.ConfDoneEvent) ManagerMsg {
					return &batchDoneMsg{
						txid: ev.Txid,
					}
				},
			),
		),
	}
	if err := m.cfg.ChainSource.Tell(ctx, confReq); err != nil {
		return fmt.Errorf("register batch conf watch: %w", err)
	}

	for i := range inputs {
		// A consumed input with no pkScript cannot be watched: lnd's
		// spend notifier filters by output script. Rather than fail the
		// whole batch registration (which would also drop the working
		// confirmation watch), skip just this input's spend watch.
		// Conf- based reorg tracking still works; only conflict
		// detection on this one input is degraded. Empty scripts should
		// only occur on legacy/backfilled inputs that predate script
		// tracking.
		if len(inputs[i].PkScript) == 0 {
			m.logger(ctx).InfoS(ctx, "Skipping spend watch for "+
				"consumed input with no pkScript",
				slog.String("batch", w.txid.String()),
				slog.String(
					"outpoint", inputs[i].Outpoint.String(),
				))

			continue
		}

		if err := m.armSpendWatch(
			ctx, w.txid, inputs[i], heightHint,
		); err != nil {
			return err
		}
	}

	return nil
}

// armSpendWatch registers one reorg-aware spend watch on a consumed input. The
// input's pkScript is forwarded to the spend notifier, which filters by output
// script; without it lnd rejects the registration ("an output script must be
// provided") and the conflict-detection watch never arms.
func (m *Manager) armSpendWatch(ctx context.Context, txid chainhash.Hash,
	in ConsumedInput, heightHint uint32) error {

	op := in.Outpoint
	spendReq := &chainsource.RegisterSpendRequest{
		CallerID:   spendCallerID(txid, op),
		Outpoint:   &op,
		PkScript:   in.PkScript,
		HeightHint: heightHint,
		NotifyActor: fn.Some(
			chainsource.MapSpendEvent(
				m.selfRef,
				func(ev chainsource.SpendEvent) ManagerMsg {
					return &inputSpentMsg{
						outpoint:     ev.Outpoint,
						spendingTxid: ev.SpendingTxid,
						spendHeight:  ev.SpendingHeight,
					}
				},
			),
		),
		NotifyReorged: fn.Some(
			chainsource.MapSpendReorgedEvent(
				m.selfRef,
				func(
					ev chainsource.SpendReorgedEvent,
				) ManagerMsg {

					return &inputSpendReorgedMsg{
						outpoint: ev.Outpoint,
					}
				},
			),
		),
		NotifyDone: fn.Some(
			chainsource.MapSpendDoneEvent(
				m.selfRef,
				func(ev chainsource.SpendDoneEvent) ManagerMsg {
					return &inputSpendDoneMsg{
						outpoint: ev.Outpoint,
					}
				},
			),
		),
	}
	if err := m.cfg.ChainSource.Tell(ctx, spendReq); err != nil {
		return fmt.Errorf("register input spend watch %s: %w", op, err)
	}

	return nil
}

// bestHeightHint asks chainsource for the current best height to use as a
// watch height hint. On error it returns 0 (scan from the backend's default),
// logging the failure rather than aborting registration.
func (m *Manager) bestHeightHint(ctx context.Context) uint32 {
	resp, err := m.cfg.ChainSource.Ask(
		ctx, &chainsource.BestHeightRequest{},
	).Await(ctx).Unpack()
	if err != nil {
		m.logger(ctx).WarnS(ctx, "Batch canonicality best-height "+
			"query failed; using zero height hint", err)

		return 0
	}

	height, ok := resp.(*chainsource.BestHeightResponse)
	if !ok {
		return 0
	}
	if height.Height < 0 {
		return 0
	}

	return uint32(height.Height)
}

// handleGetBatchState serves a read of the persisted canonicality record.
func (m *Manager) handleGetBatchState(ctx context.Context,
	req *GetBatchStateRequest) fn.Result[ManagerResp] {

	record, err := m.cfg.Store.GetBatch(ctx, req.BatchTxID)
	switch {
	case errors.Is(err, ErrBatchNotFound):
		return fn.Ok[ManagerResp](&GetBatchStateResponse{Found: false})

	case err != nil:
		return fn.Err[ManagerResp](err)

	default:
		return fn.Ok[ManagerResp](&GetBatchStateResponse{
			Record: record,
			Found:  true,
		})
	}
}

// handleBatchConfirmed records the batch tx confirmation observation and
// re-derives the canonicality state.
func (m *Manager) handleBatchConfirmed(ctx context.Context,
	msg *batchConfirmedMsg) {

	w, ok := m.watches[msg.txid]
	if !ok {
		return
	}

	w.conf = confConfirmed
	err := m.cfg.Store.RecordConfirmation(
		ctx, msg.txid, msg.blockHeight, msg.blockHash,
	)
	if err != nil {
		m.logger(ctx).WarnS(ctx, "Failed to record batch confirmation",
			err, "batch", msg.txid)
	}

	m.deriveAndPersist(ctx, w)
}

// handleBatchReorged clears the confirmation observation (the confirming block
// left the best chain) and re-derives state.
func (m *Manager) handleBatchReorged(ctx context.Context,
	msg *batchReorgedMsg) {

	w, ok := m.watches[msg.txid]
	if !ok {
		return
	}

	w.conf = confReorgedOut
	if err := m.cfg.Store.ClearConfirmation(ctx, msg.txid); err != nil {
		m.logger(ctx).WarnS(ctx, "Failed to clear batch confirmation",
			err, "batch", msg.txid)
	}

	m.deriveAndPersist(ctx, w)
}

// handleBatchDone marks the batch confirmation as matured past the reorg-
// safety depth (policy finality) and re-derives state. The chainsource conf
// sub-actor releases its own registration on Done; the manager additionally
// releases the per-input spend watches, since a finalized batch's inputs are
// safely consumed and can no longer be double-spent.
func (m *Manager) handleBatchDone(ctx context.Context, msg *batchDoneMsg) {
	w, ok := m.watches[msg.txid]
	if !ok {
		return
	}

	w.conf = confFinalized
	m.deriveAndPersist(ctx, w)
	m.releaseSpendWatches(ctx, w)
}

// handleInputSpent interprets a spend of a consumed batch input. The SAME
// outpoint can be consumed by more than one registered batch — that is exactly
// the double-spend the manager exists to classify — so every batch watching
// the outpoint is updated, not just one. For each such batch, a spend by that
// batch's own tx is the expected consumption (not a conflict), while a spend by
// any other transaction is a conflicting double-spend of that batch's input.
func (m *Manager) handleInputSpent(ctx context.Context, msg *inputSpentMsg) {
	m.forEachInputWatch(msg.outpoint, func(w *batchWatch, iw *inputWatch) {
		conflict := msg.spendingTxid != w.txid
		iw.spenderIsConflict = conflict
		iw.conflicting = conflict
		iw.conflictFinal = false

		m.deriveAndPersist(ctx, w)
	})
}

// handleInputSpendReorged clears a previously observed spend that left the
// best chain, for every batch watching the outpoint.
func (m *Manager) handleInputSpendReorged(ctx context.Context,
	msg *inputSpendReorgedMsg) {

	m.forEachInputWatch(msg.outpoint, func(w *batchWatch, iw *inputWatch) {
		iw.conflicting = false
		iw.conflictFinal = false

		m.deriveAndPersist(ctx, w)
	})
}

// handleInputSpendDone promotes a conflicting spend to finalized once it has
// matured past the reorg-safety depth, for every batch watching the outpoint.
// A matured spend by a batch's own tx is the normal consumption, so only the
// batches for which the spend was a conflict are promoted.
func (m *Manager) handleInputSpendDone(ctx context.Context,
	msg *inputSpendDoneMsg) {

	m.forEachInputWatch(msg.outpoint, func(w *batchWatch, iw *inputWatch) {
		if iw.spenderIsConflict {
			iw.conflictFinal = true
		}

		m.deriveAndPersist(ctx, w)
	})
}

// forEachInputWatch invokes fn for every batch whose consumed-input set
// contains op. The same outpoint can appear under multiple batches (the
// conflict case: two batches spending the same input), so all matching watches
// must be visited — keying input watches by outpoint alone does NOT uniquely
// identify a batch.
func (m *Manager) forEachInputWatch(op wire.OutPoint,
	fn func(w *batchWatch, iw *inputWatch)) {

	for _, w := range m.watches {
		if iw, ok := w.inputs[op]; ok {
			fn(w, iw)
		}
	}
}

// deriveState computes the dominant canonicality state from the batch's
// in-memory confirmation and input-conflict views, applying the priority
// conflict_finalized > conflict_provisional > reorged_out >
// finalized/provisional > unseen.
func deriveState(w *batchWatch) State {
	anyConflictFinal := false
	anyConflict := false
	for _, iw := range w.inputs {
		if iw.conflictFinal {
			anyConflictFinal = true
		}
		if iw.conflicting {
			anyConflict = true
		}
	}

	switch {
	case anyConflictFinal:
		return StateConflictFinalized

	case anyConflict:
		return StateConflictProvisional

	case w.conf == confReorgedOut:
		return StateReorgedOut

	case w.conf == confFinalized:
		return StateFinalized

	case w.conf == confConfirmed:
		return StateProvisional

	default:
		return StateUnseen
	}
}

// deriveAndPersist recomputes the batch state and writes it only when it
// changed since the last persisted value.
func (m *Manager) deriveAndPersist(ctx context.Context, w *batchWatch) {
	next := deriveState(w)
	if next == w.persisted {
		return
	}

	if err := m.cfg.Store.UpdateBatchState(ctx, w.txid, next); err != nil {
		m.logger(ctx).WarnS(ctx, "Failed to persist batch state", err,
			"batch", w.txid, "state", next.String())

		return
	}
	w.persisted = next

	m.handleConsumerLifecycle(ctx, w.txid, next)
}

// handleConsumerLifecycle reacts to a batch's canonicality transition for the
// VTXOs it provisionally forfeits (its reverse-dependency edges):
//
//   - StateConflictFinalized: the batch is permanently off the canonical chain
//     (a conflicting spend matured past the reorg-safety depth), so its forfeit
//     is reversed -- restore every consumed VTXO, then drop the edges.
//   - StateFinalized: the batch is itself canonical and final, so the forfeit
//     is now safe and the restore window closes -- drop the edges without
//     restoring.
//
// Transient states (provisional / reorged-out / conflict-provisional) are left
// untouched: a reorged-out batch may still reconfirm, so its forfeit must not
// be reversed until the invalidation is final.
func (m *Manager) handleConsumerLifecycle(ctx context.Context,
	txid chainhash.Hash, next State) {

	switch next {
	case StateConflictFinalized:
		m.restoreProvisionalConsumers(ctx, txid)

	case StateFinalized:
		err := m.cfg.Store.DeleteProvisionalConsumersForBatch(ctx, txid)
		if err != nil {
			m.logger(ctx).WarnS(ctx, "Failed to clear provisional "+
				"consumer edges for finalized batch", err,
				slog.String("batch", txid.String()))
		}

	case StateUnseen, StateProvisional, StateReorgedOut,
		StateConflictProvisional:

		// Transient/non-final states: the forfeit's fate is not yet
		// decided, so the reverse-dependency edges are left untouched.
		// A reorged-out batch may still reconfirm, so its forfeit must
		// not be reversed until the invalidation (or finalization) is
		// final.
	}
}

// restoreProvisionalConsumers restores every VTXO the given (now invalidated)
// batch provisionally forfeited, then drops the edges so the restore fires at
// most once. A nil RestoreConsumedVTXO callback leaves the edges in place (the
// data model stays consistent for a later wired run).
func (m *Manager) restoreProvisionalConsumers(ctx context.Context,
	txid chainhash.Hash) {

	consumed, err := m.cfg.Store.ListProvisionalConsumersForBatch(ctx, txid)
	if err != nil {
		m.logger(ctx).WarnS(ctx, "Failed to list provisional "+
			"consumers for invalidated batch", err,
			slog.String("batch", txid.String()))

		return
	}
	if len(consumed) == 0 {
		return
	}

	if m.cfg.RestoreConsumedVTXO == nil {

		// No restore path wired: leave the edges so a future run with a
		// callback can still act on them.
		return
	}

	for _, op := range consumed {
		if err := m.cfg.RestoreConsumedVTXO(ctx, op); err != nil {
			m.logger(ctx).WarnS(ctx, "Failed to restore forfeited "+
				"VTXO after batch invalidation", err,
				slog.String("batch", txid.String()),
				slog.String("vtxo", op.String()))

			// Keep the edges so the restore can be retried; do not
			// drop them on partial failure.
			return
		}
	}

	err = m.cfg.Store.DeleteProvisionalConsumersForBatch(ctx, txid)
	if err != nil {
		m.logger(ctx).WarnS(ctx, "Failed to clear provisional "+
			"consumer edges after restore", err,
			slog.String("batch", txid.String()))
	}
}

// releaseSpendWatches unregisters the per-input spend watches for a batch,
// called once the batch finalizes.
func (m *Manager) releaseSpendWatches(ctx context.Context, w *batchWatch) {
	for op := range w.inputs {
		err := m.cfg.ChainSource.Tell(
			ctx, &chainsource.UnregisterSpendRequest{
				CallerID: spendCallerID(w.txid, op),
				Outpoint: &op,
			},
		)
		if err != nil {
			m.
				logger(ctx).
				WarnS(
					ctx,
					"Failed to release input spend "+
						"watch",
					err,
					"batch",
					w.txid,
				)
		}
	}
}

// Reconcile re-establishes watches for every non-final persisted batch after a
// restart. It seeds each batch's in-memory state from the persisted record so
// live re-observation does not transiently downgrade a persisted conflict or
// finalized state. It must run after SetSelfRef.
func (m *Manager) Reconcile(ctx context.Context) error {
	// Non-final states whose watches must be re-armed. Finalized and
	// conflict_finalized batches need no further watching.
	live := []State{
		StateUnseen, StateProvisional, StateReorgedOut,
		StateConflictProvisional,
	}

	for _, state := range live {
		records, err := m.cfg.Store.ListBatchesByState(ctx, state)
		if err != nil {
			return fmt.Errorf("list %s batches: %w", state, err)
		}

		for _, record := range records {
			m.reconcileOne(ctx, record)
		}
	}

	return nil
}

// reconcileOne rebuilds the in-memory watch for one persisted batch and
// re-arms its chain watches.
func (m *Manager) reconcileOne(ctx context.Context, record *Record) {
	if _, ok := m.watches[record.BatchTxID]; ok {
		return
	}

	w := &batchWatch{
		txid:      record.BatchTxID,
		pkScript:  record.ConfirmationPkScript,
		inputs:    make(map[wire.OutPoint]*inputWatch),
		persisted: record.State,
	}

	// Seed the confirmation view from the persisted record so a derive
	// before re-observation does not regress the stored state.
	switch record.State {
	case StateProvisional, StateConflictProvisional:
		w.conf = confConfirmed

	case StateReorgedOut:
		w.conf = confReorgedOut

	default:
		w.conf = confUnseen
	}

	for _, in := range record.ConsumedInputs {
		w.inputs[in.Outpoint] = &inputWatch{}
	}
	m.watches[record.BatchTxID] = w

	if err := m.armWatches(ctx, w, record.ConsumedInputs); err != nil {
		m.logger(ctx).WarnS(ctx, "Failed to re-arm batch watches on "+
			"reconcile", err, "batch", record.BatchTxID)
	}
}

// confCallerID is the stable chainsource caller id for a batch's confirmation
// watch.
func confCallerID(txid chainhash.Hash) string {
	return fmt.Sprintf("batchcanon-conf-%s", txid)
}

// spendCallerID is the stable chainsource caller id for a batch input's spend
// watch.
func spendCallerID(txid chainhash.Hash, op wire.OutPoint) string {
	return fmt.Sprintf("batchcanon-spend-%s-%s", txid, op)
}

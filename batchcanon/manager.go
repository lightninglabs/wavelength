package batchcanon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/build"
	"github.com/lightninglabs/wavelength/chainsource"
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

// minWatchHeightHint is the lowest block-height hint a chain notifier will
// accept: a registration with a hint of 0 is rejected outright ("a height hint
// greater than 0 must be provided"), which would leave the batch permanently
// unwatched and, because admission is fail-closed, never usable. A batch's
// persisted WatchHeightHint can legitimately resolve to 0 (for example a round
// FSM created at genesis height on a fresh regtest chain), so every watch
// registration clamps its hint to this floor. A lower hint only widens the
// notifier's scan window; it never skips the confirmation the watch exists to
// observe, so clamping up is always safe.
const minWatchHeightHint uint32 = 1

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
	// pkScript is the previous output script used to arm and later cancel
	// this input's spend watch.
	pkScript []byte

	// observed reports that this subject supplied a current observation for
	// the watch's reconciliation generation.
	observed bool

	// doneObserved records that this generation's spend watch reported
	// policy finality. SpendDoneEvent intentionally carries no spender
	// identity, so an early Done cannot classify the input by itself.
	// Keeping the evidence lets a subsequently queued SpendEvent perform
	// that classification without losing the terminal signal.
	doneObserved bool

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
	txid       chainhash.Hash
	pkScript   []byte
	heightHint uint32

	conf   confState
	inputs map[wire.OutPoint]*inputWatch

	confHeight fn.Option[int32]
	confBlock  fn.Option[chainhash.Hash]

	// generation is the durable observation generation this watch set
	// serves. confObserved plus every input's observed bit forms Ready(g).
	generation   uint64
	confObserved bool
	ready        bool

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

	// ActivateRestoredVTXO asks the VTXO manager to materialize an actor
	// after the store atomically restored the exact business marker and
	// completed its edge. A callback failure cannot undo safety: the Live
	// row is recovered at startup or lazily by selection.
	ActivateRestoredVTXO func(ctx context.Context, vtxo wire.OutPoint) error

	// Log is an optional logger.
	Log fn.Option[btclog.Logger]

	// allowIncompleteTestEvidence preserves old unit fixtures while the
	// production registration paths are migrated to serialized evidence. It
	// is intentionally unexported and therefore cannot be enabled by daemon
	// wiring outside this package.
	allowIncompleteTestEvidence bool
}

// Manager is the sole client-side interpreter of batch canonicality. It
// observes (via chainsource) each batch tx confirmation and each consumed
// input, interprets the reorg-aware lifecycle into batchcanon.State, and
// persists the result. It is an actor behavior: chainsource events arrive as
// internal messages re-wrapped onto the manager's own mailbox.
//
// Registration checks the claimed input set against the serialized batch
// transaction before persisting anything. Spend watches use each
// authenticated prevout pkScript, which lnd's notifier requires. Incomplete
// evidence fails the whole registration closed.
type Manager struct {
	// mu serializes direct startup reconciliation with actor mailbox work.
	// Reconcile is intentionally called by the daemon after actor
	// registration so watches are live before startup completes, while
	// Receive may already be processing queued chain observations.
	mu sync.Mutex

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

// Receive implements actor.ActorBehavior. It serializes canonicality
// mutations with both the single actor mailbox and direct startup
// reconciliation.
func (m *Manager) Receive(ctx context.Context,
	msg ManagerMsg) fn.Result[ManagerResp] {

	m.mu.Lock()
	defer m.mu.Unlock()

	switch v := msg.(type) {
	case *RegisterBatchRequest:
		return m.handleRegisterBatch(ctx, v)

	case *GetBatchStateRequest:
		return m.handleGetBatchState(ctx, v)

	case *QueryLineageRequest:
		return m.handleQueryLineage(ctx, v)

	case *ValidateAdmissionRequest:
		return m.handleValidateAdmission(ctx, v)

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

	if err := validateRegistration(
		req, m.cfg.allowIncompleteTestEvidence,
	); err != nil {
		return fn.Err[ManagerResp](err)
	}

	// RegisterBatch commits the batch, every actual input, every dependent,
	// and every reverse consumer edge in one transaction. A repeated call
	// validates immutable evidence and only merges monotonic edges; it
	// never replaces conflict observations with an unseen record.
	batchTx := req.BatchTx
	if len(batchTx) == 0 && m.cfg.allowIncompleteTestEvidence {
		// Keep Record.Ready's production invariant honest while
		// preserving focused manager fixtures that do not construct
		// wire transactions. The bypass is unexported and cannot be
		// enabled by daemon wiring.
		batchTx = []byte{0x00}
	}
	record := &Record{
		BatchTxID:             req.BatchTxID,
		BatchTx:               batchTx,
		BatchOutputIndex:      req.BatchOutputIndex,
		RegistrationStage:     RegistrationRegistering,
		ObservationGeneration: 1,
		ReadyGeneration:       fn.None[uint64](),
		State:                 StateUnseen,
		ConfirmationHeight:    fn.None[int32](),
		ConfirmationBlock:     fn.None[chainhash.Hash](),
		CSVExpiryDelta:        req.CSVExpiryDelta,
		PolicyState:           PolicyStateDefault,
		ConfirmationPkScript:  req.ConfirmationPkScript,
		WatchHeightHint:       req.WatchHeightHint,
		ConsumedInputs:        req.ConsumedInputs,
		DependentVTXOs:        req.DependentVTXOs,
	}
	if err := m.cfg.Store.RegisterBatch(
		ctx, record, req.ConsumedVTXOs,
	); err != nil {
		return fn.Err[ManagerResp](
			fmt.Errorf("register complete batch evidence: %w", err),
		)
	}

	// Load the authoritative record after registration. This matters when a
	// caller retries after restart before Reconcile: RegisterBatch
	// preserves the prior state and input conflict flags, so the in-memory
	// watch must be seeded from those durable observations rather than from
	// req.
	record, err := m.cfg.Store.GetBatch(ctx, req.BatchTxID)
	if err != nil {
		return fn.Err[ManagerResp](
			fmt.Errorf("load registered batch evidence: %w", err),
		)
	}

	// A retry may add an edge after this batch already reached a terminal
	// state (for example, replay after the consumed VTXO's forfeiture was
	// persisted). No chain callback will arrive to drive that new edge, so
	// resolve it from the already-ready objective terminal evidence now.
	if record.Ready() && terminalState(record.State) {
		m.handleConsumerLifecycle(ctx, record.BatchTxID, record.State)
	}

	if _, ok := m.watches[req.BatchTxID]; ok {
		return fn.Ok[ManagerResp](&RegisterBatchResponse{})
	}

	if record.State == StateFinalized ||
		record.State == StateConflictFinalized {
		return fn.Ok[ManagerResp](&RegisterBatchResponse{})
	}

	w := watchFromRecord(record)
	if err := m.armWatches(ctx, w, record.ConsumedInputs); err != nil {
		return fn.Err[ManagerResp](err)
	}
	m.watches[req.BatchTxID] = w

	return fn.Ok[ManagerResp](&RegisterBatchResponse{})
}

// validateRegistration rejects incomplete evidence before any durable write or
// chain side effect. Every actual input needs a script because a missing spend
// watch creates an undetectable conflict surface.
func validateRegistration(req *RegisterBatchRequest,
	allowIncompleteTestEvidence bool) error {

	if req.BatchTxID == (chainhash.Hash{}) {
		return fmt.Errorf("batch txid is required")
	}
	if len(req.ConfirmationPkScript) == 0 {
		return fmt.Errorf("batch confirmation pkScript is required")
	}
	if req.CSVExpiryDelta <= 0 {
		return fmt.Errorf("batch CSV expiry delta must be positive")
	}
	if len(req.ConsumedInputs) == 0 {
		return fmt.Errorf("batch must register every consumed input")
	}
	if len(req.ForfeitedVTXOs) != 0 {
		return fmt.Errorf("legacy forfeited VTXO evidence lacks " +
			"creator lineage and business revision")
	}

	seen := make(map[wire.OutPoint]struct{}, len(req.ConsumedInputs))
	for i, in := range req.ConsumedInputs {
		if in.Value < 0 {
			return fmt.Errorf("consumed input %d (%s) has "+
				"negative value", i, in.Outpoint)
		}
		if len(in.PkScript) == 0 {
			return fmt.Errorf("consumed input %d (%s) has no "+
				"pkScript", i, in.Outpoint)
		}
		if _, ok := seen[in.Outpoint]; ok {
			return fmt.Errorf("consumed input %s is duplicated",
				in.Outpoint)
		}
		seen[in.Outpoint] = struct{}{}
	}

	if len(req.BatchTx) == 0 && allowIncompleteTestEvidence {
		return nil
	}
	if len(req.BatchTx) == 0 {
		return fmt.Errorf("serialized batch transaction is required")
	}

	reader := bytes.NewReader(req.BatchTx)
	tx := wire.NewMsgTx(2)
	if err := tx.Deserialize(reader); err != nil {
		return fmt.Errorf("decode serialized batch transaction: %w",
			err)
	}
	if reader.Len() != 0 {
		return fmt.Errorf("serialized batch transaction has %d "+
			"trailing bytes", reader.Len())
	}
	if tx.TxHash() != req.BatchTxID {
		return fmt.Errorf("serialized batch transaction hash does not "+
			"match %s", req.BatchTxID)
	}
	if uint64(req.BatchOutputIndex) >= uint64(len(tx.TxOut)) {
		return fmt.Errorf("batch output index %d is out of range",
			req.BatchOutputIndex)
	}
	if !bytes.Equal(
		tx.TxOut[req.BatchOutputIndex].PkScript,
		req.ConfirmationPkScript,
	) {
		return fmt.Errorf("batch output %d does not match "+
			"confirmation pkScript", req.BatchOutputIndex)
	}
	if len(tx.TxIn) != len(req.ConsumedInputs) {
		return fmt.Errorf("serialized batch transaction has %d "+
			"inputs, registration has %d", len(tx.TxIn),
			len(req.ConsumedInputs))
	}
	for _, txIn := range tx.TxIn {
		if _, ok := seen[txIn.PreviousOutPoint]; !ok {
			return fmt.Errorf("serialized batch transaction input "+
				"%s is not registered", txIn.PreviousOutPoint)
		}
	}

	consumedVTXOs := make(
		map[wire.OutPoint]struct{}, len(req.ConsumedVTXOs),
	)
	for i, edge := range req.ConsumedVTXOs {
		if _, duplicate := consumedVTXOs[edge.ConsumedVTXO]; duplicate {
			return fmt.Errorf("consumed VTXO %s is duplicated",
				edge.ConsumedVTXO)
		}
		consumedVTXOs[edge.ConsumedVTXO] = struct{}{}
		if edge.ConsumerBatch != (chainhash.Hash{}) &&
			edge.ConsumerBatch != req.BatchTxID {
			return fmt.Errorf("consumed VTXO %d names a different "+
				"consumer batch", i)
		}
		if edge.ExpectedRevision == 0 {
			return fmt.Errorf("consumed VTXO %d has no expected "+
				"business revision", i)
		}
		if len(edge.CreatorLineage) == 0 {
			return fmt.Errorf("consumed VTXO %d has no "+
				"creator lineage", i)
		}
		lineage := make(
			map[chainhash.Hash]struct{}, len(edge.CreatorLineage),
		)
		for _, ancestor := range edge.CreatorLineage {
			if ancestor == (chainhash.Hash{}) {
				return fmt.Errorf("consumed VTXO %d has a "+
					"zero creator lineage txid", i)
			}
			if _, duplicate := lineage[ancestor]; duplicate {
				return fmt.Errorf("consumed VTXO %d "+
					"duplicates creator lineage txid %s", i,
					ancestor)
			}
			lineage[ancestor] = struct{}{}
		}
	}

	return nil
}

// armWatches registers the reorg-aware confirmation watch on the batch tx and
// a reorg-aware spend watch on each consumed input.
func (m *Manager) armWatches(ctx context.Context, w *batchWatch,
	inputs []ConsumedInput) error {

	if len(w.pkScript) == 0 {
		return fmt.Errorf("batch %s has no confirmation pkScript",
			w.txid)
	}
	if len(inputs) == 0 {
		return fmt.Errorf("batch %s has no complete consumed-input set",
			w.txid)
	}
	for _, in := range inputs {
		if len(in.PkScript) == 0 {
			return fmt.Errorf("batch %s input %s has no pkScript",
				w.txid, in.Outpoint)
		}
	}

	// Clamp the persisted hint to the notifier's accepted floor so a 0 hint
	// can never abort watch arming (see minWatchHeightHint).
	heightHint := w.heightHint
	if heightHint < minWatchHeightHint {
		heightHint = minWatchHeightHint
	}

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
						generation:  w.generation,
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
						txid:       ev.Txid,
						generation: w.generation,
					}
				},
			),
		),
		NotifyDone: fn.Some(
			chainsource.MapConfDoneEvent(
				m.selfRef,
				func(ev chainsource.ConfDoneEvent) ManagerMsg {
					return &batchDoneMsg{
						txid:       ev.Txid,
						generation: w.generation,
					}
				},
			),
		),
	}
	result := m.cfg.ChainSource.Ask(ctx, confReq).Await(ctx)
	if _, err := result.Unpack(); err != nil {
		return fmt.Errorf("register batch conf watch: %w", err)
	}

	armedInputs := make([]ConsumedInput, 0, len(inputs))
	for i := range inputs {
		if err := m.armSpendWatch(
			ctx, w.txid, w.generation, inputs[i], heightHint,
		); err != nil {

			m.releaseWatchSet(ctx, w, armedInputs, true)

			return err
		}
		armedInputs = append(armedInputs, inputs[i])
	}

	return nil
}

// armSpendWatch registers one reorg-aware spend watch on a consumed input. The
// input's pkScript is forwarded to the spend notifier, which filters by output
// script; without it lnd rejects the registration ("an output script must be
// provided") and the conflict-detection watch never arms.
func (m *Manager) armSpendWatch(ctx context.Context, txid chainhash.Hash,
	generation uint64, in ConsumedInput, heightHint uint32) error {

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
						batchTxid:    txid,
						generation:   generation,
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
						batchTxid:  txid,
						generation: generation,
						outpoint:   ev.Outpoint,
					}
				},
			),
		),
		NotifyDone: fn.Some(
			chainsource.MapSpendDoneEvent(
				m.selfRef,
				func(ev chainsource.SpendDoneEvent) ManagerMsg {
					return &inputSpendDoneMsg{
						batchTxid:  txid,
						generation: generation,
						outpoint:   ev.Outpoint,
					}
				},
			),
		),
	}
	result := m.cfg.ChainSource.Ask(ctx, spendReq).Await(ctx)
	if _, err := result.Unpack(); err != nil {
		return fmt.Errorf("register input spend watch %s: %w", op, err)
	}

	return nil
}

// releaseWatchSet synchronously cancels a partially or fully armed watch set.
// The registration fields exactly match the original service keys; omitting a
// pkScript would address a different chainsource child and leak the watch.
func (m *Manager) releaseWatchSet(ctx context.Context, w *batchWatch,
	inputs []ConsumedInput, releaseConf bool) {

	cleanupCtx := context.WithoutCancel(ctx)
	for _, in := range inputs {
		op := in.Outpoint
		result := m.cfg.ChainSource.Ask(
			cleanupCtx, &chainsource.UnregisterSpendRequest{
				CallerID: spendCallerID(w.txid, op),
				Outpoint: &op,
				PkScript: in.PkScript,
			},
		).Await(cleanupCtx)
		if _, err := result.Unpack(); err != nil {
			m.
				logger(ctx).
				WarnS(
					ctx,
					"Failed to release batch input "+
						"watch",
					err,
					slog.String("batch", w.txid.String()),
					slog.String("outpoint", op.String()),
				)
		}
	}

	if !releaseConf {
		return
	}

	result := m.cfg.ChainSource.Ask(
		cleanupCtx, &chainsource.UnregisterConfRequest{
			CallerID:    confCallerID(w.txid),
			Txid:        &w.txid,
			PkScript:    w.pkScript,
			TargetConfs: usabilityConfs,
		},
	).Await(cleanupCtx)
	if _, err := result.Unpack(); err != nil {
		m.
			logger(ctx).
			WarnS(
				ctx,
				"Failed to release batch confirmation "+
					"watch",
				err,
				slog.String("batch", w.txid.String()),
			)
	}
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

// handleQueryLineage returns a token only when every distinct ancestor is
// complete, ready for its current generation, and semantically usable.
func (m *Manager) handleQueryLineage(ctx context.Context,
	req *QueryLineageRequest) fn.Result[ManagerResp] {

	availability, lineage, err := m.loadLineage(ctx, req.BatchTxIDs)
	if err != nil {
		return fn.Err[ManagerResp](err)
	}

	resp := &QueryLineageResponse{Availability: availability}
	if availability.Usable() {
		resp.Token = &AdmissionToken{Lineage: lineage}
	}

	return fn.Ok[ManagerResp](resp)
}

// handleValidateAdmission checks that every generation and revision in a
// token still exactly matches the manager's durable view.
func (m *Manager) handleValidateAdmission(ctx context.Context,
	req *ValidateAdmissionRequest) fn.Result[ManagerResp] {

	if len(req.Token.Lineage) == 0 {
		return fn.Ok[ManagerResp](&ValidateAdmissionResponse{
			Availability: LineageReconciling,
		})
	}

	txids := make([]chainhash.Hash, 0, len(req.Token.Lineage))
	expected := make(
		map[chainhash.Hash]LineageRevision, len(req.Token.Lineage),
	)
	for _, entry := range req.Token.Lineage {
		if _, duplicate := expected[entry.BatchTxID]; duplicate {
			return fn.Ok[ManagerResp](&ValidateAdmissionResponse{
				Availability: LineageReconciling,
			})
		}
		expected[entry.BatchTxID] = entry
		txids = append(txids, entry.BatchTxID)
	}

	availability, current, err := m.loadLineage(ctx, txids)
	if err != nil {
		return fn.Err[ManagerResp](err)
	}

	valid := availability.Usable() && len(current) == len(expected)
	for _, entry := range current {
		want, ok := expected[entry.BatchTxID]
		if !ok || want.Generation != entry.Generation ||
			want.Revision != entry.Revision {

			valid = false
			break
		}
	}

	return fn.Ok[ManagerResp](&ValidateAdmissionResponse{
		Valid:        valid,
		Availability: availability,
	})
}

// loadLineage loads each distinct ancestor once and applies the readiness
// barrier before semantic priority. Missing or incomplete records return the
// retryable reconciling result even if another record has a terminal state.
func (m *Manager) loadLineage(ctx context.Context, txids []chainhash.Hash) (
	Availability, []LineageRevision, error) {

	if len(txids) == 0 {
		return LineageReconciling, nil, nil
	}

	seen := make(map[chainhash.Hash]struct{}, len(txids))
	availability := make([]Availability, 0, len(txids))
	lineage := make([]LineageRevision, 0, len(txids))
	for _, txid := range txids {
		if _, duplicate := seen[txid]; duplicate {
			continue
		}
		seen[txid] = struct{}{}
		if watch, ok := m.watches[txid]; ok && !watch.ready {
			return LineageReconciling, nil, nil
		}

		record, err := m.cfg.Store.GetBatch(ctx, txid)
		switch {
		case errors.Is(err, ErrBatchNotFound):
			return LineageReconciling, nil, nil

		case err != nil:
			return LineageReconciling, nil,
				fmt.Errorf("load lineage batch %s: %w", txid,
					err)

		case !record.Ready():
			return LineageReconciling, nil, nil
		}

		availability = append(
			availability, AvailabilityForState(record.State),
		)
		lineage = append(lineage, LineageRevision{
			BatchTxID:  txid,
			Generation: record.ObservationGeneration,
			Revision:   record.Revision,
		})
	}

	return CombineAvailability(availability...), lineage, nil
}

// handleBatchConfirmed records the batch tx confirmation observation and
// re-derives the canonicality state.
func (m *Manager) handleBatchConfirmed(ctx context.Context,
	msg *batchConfirmedMsg) {

	w, ok := m.currentWatch(msg.txid, msg.generation)
	if !ok {
		return
	}
	state := deriveState(w)
	if terminalState(state) {
		switch {
		case state == StateFinalized && w.confHeight.IsNone():
			// Chainsource orders positive observations before Done,
			// but keep the durable reducer safe under replay from
			// an older transport or mailbox. Finality remains
			// authoritative; the late block identity only repairs
			// expiry and diagnostics.
			w.confHeight = fn.Some(msg.blockHeight)
			w.confBlock = fn.Some(msg.blockHash)
			w.confObserved = true
			m.persistObservation(ctx, w)

		case !w.ready:
			w.confObserved = true
			m.persistObservation(ctx, w)
		}

		return
	}

	w.conf = confConfirmed
	w.confHeight = fn.Some(msg.blockHeight)
	w.confBlock = fn.Some(msg.blockHash)
	w.confObserved = true
	m.persistObservation(ctx, w)
}

// handleBatchReorged clears the confirmation observation (the confirming block
// left the best chain) and re-derives state.
func (m *Manager) handleBatchReorged(ctx context.Context,
	msg *batchReorgedMsg) {

	w, ok := m.currentWatch(msg.txid, msg.generation)
	if !ok {
		return
	}
	if terminalState(deriveState(w)) {
		if !w.ready {
			w.confObserved = true
			m.persistObservation(ctx, w)
		}

		return
	}

	w.conf = confReorgedOut
	w.confHeight = fn.None[int32]()
	w.confBlock = fn.None[chainhash.Hash]()
	w.confObserved = true
	m.persistObservation(ctx, w)
}

// handleBatchDone marks the batch confirmation as matured past the reorg-
// safety depth (policy finality) and re-derives state. The chainsource conf
// sub-actor releases its own registration on Done; the manager additionally
// releases the per-input spend watches, since a finalized batch's inputs are
// safely consumed and can no longer be double-spent.
func (m *Manager) handleBatchDone(ctx context.Context, msg *batchDoneMsg) {
	w, ok := m.currentWatch(msg.txid, msg.generation)
	if !ok {
		return
	}
	if terminalState(deriveState(w)) {
		if !w.ready {
			w.confObserved = true
			m.persistObservation(ctx, w)
		}

		return
	}

	w.conf = confFinalized
	w.confObserved = true
	m.persistObservation(ctx, w)
}

// handleInputSpent interprets a spend of a consumed batch input. The SAME
// outpoint can be consumed by more than one registered batch — that is exactly
// the double-spend the manager exists to classify — so every batch watching
// the outpoint is updated, not just one. For each such batch, a spend by that
// batch's own tx is the expected consumption (not a conflict), while a spend by
// any other transaction is a conflicting double-spend of that batch's input.
func (m *Manager) handleInputSpent(ctx context.Context, msg *inputSpentMsg) {
	w, iw, ok := m.currentInputWatch(
		msg.batchTxid, msg.generation, msg.outpoint,
	)
	if !ok {
		return
	}
	if terminalState(deriveState(w)) {
		if !w.ready {
			iw.observed = true
			m.persistObservation(ctx, w)
		}

		return
	}

	conflict := msg.spendingTxid != w.txid
	iw.spenderIsConflict = conflict
	iw.conflicting = conflict
	iw.conflictFinal = conflict && iw.doneObserved
	iw.observed = true
	m.persistObservation(ctx, w)
}

// handleInputSpendReorged clears a previously observed spend that left the
// best chain, for every batch watching the outpoint.
func (m *Manager) handleInputSpendReorged(ctx context.Context,
	msg *inputSpendReorgedMsg) {

	w, iw, ok := m.currentInputWatch(
		msg.batchTxid, msg.generation, msg.outpoint,
	)
	if !ok {
		return
	}
	if terminalState(deriveState(w)) {
		if !w.ready {
			iw.observed = true
			m.persistObservation(ctx, w)
		}

		return
	}

	iw.spenderIsConflict = false
	iw.conflicting = false
	iw.conflictFinal = false
	iw.doneObserved = false
	iw.observed = true
	m.persistObservation(ctx, w)
}

// handleInputSpendDone promotes a conflicting spend to finalized once it has
// matured past the reorg-safety depth, for every batch watching the outpoint.
// A matured spend by a batch's own tx is the normal consumption, so only the
// batches for which the spend was a conflict are promoted.
func (m *Manager) handleInputSpendDone(ctx context.Context,
	msg *inputSpendDoneMsg) {

	w, iw, ok := m.currentInputWatch(
		msg.batchTxid, msg.generation, msg.outpoint,
	)
	if !ok {
		return
	}
	if terminalState(deriveState(w)) {
		if !w.ready {
			iw.observed = true
			m.persistObservation(ctx, w)
		}

		return
	}

	// Done carries no spender identity. Remember it even when the matching
	// SpendEvent is still queued, but do not mark this subject observed
	// until that event identifies whether the spender is the batch or a
	// conflict.
	iw.doneObserved = true
	if !iw.observed {
		return
	}
	if iw.spenderIsConflict {
		iw.conflictFinal = true
	}
	m.persistObservation(ctx, w)
}

// currentWatch rejects messages from a released observation generation.
// Generation tagging prevents queued callbacks from an old watch set from
// mutating the fresh restart snapshot.
func (m *Manager) currentWatch(txid chainhash.Hash, generation uint64) (
	*batchWatch, bool) {

	w, ok := m.watches[txid]
	if !ok || w.generation != generation {
		return nil, false
	}

	return w, true
}

// currentInputWatch additionally binds an input callback to the batch whose
// dedicated chainsource registration emitted it.
func (m *Manager) currentInputWatch(txid chainhash.Hash, generation uint64,
	op wire.OutPoint) (*batchWatch, *inputWatch, bool) {

	w, ok := m.currentWatch(txid, generation)
	if !ok {
		return nil, nil, false
	}
	iw, ok := w.inputs[op]
	if !ok {
		return nil, nil, false
	}

	return w, iw, true
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

// observationComplete reports whether every registered chain subject supplied
// a current observation for this generation.
func observationComplete(w *batchWatch) bool {
	if !w.confObserved {
		return false
	}
	for _, iw := range w.inputs {
		if !iw.observed {
			return false
		}
	}

	return true
}

// persistObservation atomically writes the complete in-memory view. On any
// error the manager's overlay closes admission immediately, even if the old
// durable row was usable; restart reconciliation closes it durably before
// re-arming watches.
func (m *Manager) persistObservation(ctx context.Context, w *batchWatch) {
	inputs := make([]InputObservation, 0, len(w.inputs))
	for outpoint, input := range w.inputs {
		inputs = append(inputs, InputObservation{
			Outpoint:      outpoint,
			Conflicting:   input.conflicting,
			ConflictFinal: input.conflictFinal,
		})
	}

	next := deriveState(w)
	ready := observationComplete(w)
	err := m.cfg.Store.ApplyObservation(
		ctx, &ObservationSnapshot{
			BatchTxID:          w.txid,
			Generation:         w.generation,
			State:              next,
			ConfirmationHeight: w.confHeight,
			ConfirmationBlock:  w.confBlock,
			Inputs:             inputs,
			Ready:              ready,
		},
	)
	if err != nil {
		w.ready = false
		m.logger(ctx).WarnS(ctx, "Failed to persist atomic batch "+
			"observation", err,
			slog.String("batch", w.txid.String()),
			slog.Uint64("generation", w.generation),
			slog.String("state", next.String()))

		return
	}

	w.persisted = next
	w.ready = ready
	if !ready {
		return
	}

	// A creator batch becoming ready/usable may unblock a restore edge
	// owned by some already-terminal consumer. These edges are safety
	// recovery checkpoints, not ordinary operation waiters, so they must
	// progress without requiring a daemon restart or replaying a user
	// request.
	if err := m.redriveConsumersForCreator(ctx, w.txid); err != nil {
		m.logger(ctx).WarnS(ctx, "Failed to redrive consumer recovery "+
			"for changed creator lineage", err,
			slog.String("creator_batch", w.txid.String()))
	}

	if !terminalState(next) {
		return
	}

	// Terminal actions wait for a complete durable snapshot. This prevents
	// asynchronous Done delivery from releasing another subject's watch
	// before that subject has contributed to Ready(g).
	m.handleConsumerLifecycle(ctx, w.txid, next)
	if next == StateFinalized {
		m.releaseSpendWatches(ctx, w)

		return
	}
	m.releaseAllWatches(ctx, w)
}

// terminalState reports whether policy finality makes a state sticky within
// the configured basic-v1 safety claim.
func terminalState(state State) bool {
	return state == StateFinalized || state == StateConflictFinalized
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

// restoreProvisionalConsumers resolves every durable edge owned by the given
// terminally invalidated batch. Usable creator lineage may enter the atomic
// restore CAS; invalid creator lineage completes without restoration; all
// retryable/uncertain states leave the edge pending for restart redrive.
func (m *Manager) restoreProvisionalConsumers(ctx context.Context,
	txid chainhash.Hash) {

	edges, err := m.cfg.Store.ListPendingConsumerEdges(ctx, txid)
	if err != nil {
		m.
			logger(ctx).
			WarnS(
				ctx,
				"Failed to list pending consumer edges "+
					"for invalidated batch",
				err,
				slog.String("batch", txid.String()),
			)

		return
	}

	for _, edge := range edges {
		availability, _, err := m.loadLineage(ctx, edge.CreatorLineage)
		if err != nil {
			m.
				logger(ctx).
				WarnS(
					ctx,
					"Failed to load consumed VTXO "+
						"creator lineage",
					err,
					slog.String("batch", txid.String()),
					slog.String(
						"vtxo",
						edge.ConsumedVTXO.String(),
					),
				)

			continue
		}

		var restore bool
		switch {
		case availability.Usable():
			restore = true

		case availability == Invalidated:
			restore = false

		default:
			continue
		}

		resolution, err := m.cfg.Store.ResolveConsumerEdge(
			ctx, edge, restore,
		)
		if err != nil {
			m.logger(ctx).WarnS(ctx, "Failed to resolve terminal "+
				"consumer edge", err,
				slog.String("batch", txid.String()),
				slog.String("vtxo", edge.ConsumedVTXO.String()))

			continue
		}
		if resolution != ConsumerEdgeRestored ||
			m.cfg.ActivateRestoredVTXO == nil {

			continue
		}
		if err := m.cfg.ActivateRestoredVTXO(
			ctx, edge.ConsumedVTXO,
		); err != nil {

			m.
				logger(ctx).
				WarnS(
					ctx,
					"Failed to activate atomically "+
						"restored VTXO",
					err,
					slog.String("batch", txid.String()),
					slog.String(
						"vtxo",
						edge.ConsumedVTXO.String(),
					),
				)
		}
	}
}

// redriveTerminalConsumerLifecycles retries durable consumer-edge recovery
// for every ready terminal batch. A terminal consumer can be recorded before
// one of the consumed VTXO's creator batches finishes reconciliation; when
// that creator later becomes objectively usable or invalidated, no further
// event is guaranteed on the terminal consumer itself. Rechecking here makes
// restoration progress on the evidence change that can unblock it.
func (m *Manager) redriveTerminalConsumerLifecycles(ctx context.Context) error {
	terminal := []State{StateFinalized, StateConflictFinalized}
	for _, state := range terminal {
		records, err := m.cfg.Store.ListBatchesByState(ctx, state)
		if err != nil {
			return fmt.Errorf("list %s terminal batches: %w", state,
				err)
		}

		for _, record := range records {
			if !record.Ready() {
				continue
			}
			m.handleConsumerLifecycle(
				ctx, record.BatchTxID, record.State,
			)
		}
	}

	return nil
}

// redriveConsumersForCreator retries only the terminal consumer checkpoints
// whose immutable creator lineage names creatorBatch. The normalized reverse
// lookup avoids scanning every historical terminal batch on each observation.
func (m *Manager) redriveConsumersForCreator(ctx context.Context,
	creatorBatch chainhash.Hash) error {

	consumers, err := m.cfg.Store.ListPendingConsumerBatchesByCreator(
		ctx, creatorBatch,
	)
	if err != nil {
		return err
	}

	for _, consumer := range consumers {
		record, err := m.cfg.Store.GetBatch(ctx, consumer)
		if err != nil {
			return fmt.Errorf("load consumer batch %s: %w",
				consumer, err)
		}
		if !record.Ready() || !terminalState(record.State) {
			continue
		}

		m.handleConsumerLifecycle(ctx, consumer, record.State)
	}

	return nil
}

// releaseSpendWatches unregisters the per-input spend watches for a batch,
// called once the batch finalizes.
func (m *Manager) releaseSpendWatches(ctx context.Context, w *batchWatch) {
	inputs := watchInputs(w)
	m.releaseWatchSet(ctx, w, inputs, false)
}

// releaseAllWatches unregisters every subject after terminal invalidation is
// durably Ready. The input whose Done event triggered invalidation may already
// have released itself; unregister remains idempotent.
func (m *Manager) releaseAllWatches(ctx context.Context, w *batchWatch) {
	inputs := watchInputs(w)
	m.releaseWatchSet(ctx, w, inputs, true)
}

// watchInputs reconstructs the registration keys needed for synchronous
// cleanup.
func watchInputs(w *batchWatch) []ConsumedInput {
	inputs := make([]ConsumedInput, 0, len(w.inputs))
	for op, iw := range w.inputs {
		inputs = append(inputs, ConsumedInput{
			Outpoint: op,
			PkScript: iw.pkScript,
		})
	}

	return inputs
}

// Reconcile re-establishes watches for every non-final persisted batch after a
// restart. It seeds each batch's in-memory state from the persisted record so
// live re-observation does not transiently downgrade a persisted conflict or
// finalized state. It must run after SetSelfRef.
func (m *Manager) Reconcile(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

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
			if _, armed := m.watches[record.BatchTxID]; armed {
				continue
			}
			if !record.EvidenceComplete() {
				m.
					logger(ctx).
					WarnS(
						ctx,
						"Incomplete batch; fail-closed",
						nil,
						slog.String(
							"batch",
							record.BatchTxID.
								String(),
						),
					)

				continue
			}
			reconciling, err := m.cfg.Store.BeginReconcile(
				ctx, record.BatchTxID,
			)
			if err != nil {
				return fmt.Errorf("begin %s reconciliation: %w",
					record.BatchTxID, err)
			}

			if err := m.reconcileOne(ctx, reconciling); err != nil {
				return err
			}
		}
	}

	// Terminal batches have no live watches. Re-drive both interrupted
	// restores and finalized-edge cleanup from their durable checkpoints.
	if err := m.redriveTerminalConsumerLifecycles(ctx); err != nil {
		return err
	}

	return nil
}

// reconcileOne rebuilds the in-memory watch for one persisted batch and
// re-arms its chain watches.
func (m *Manager) reconcileOne(ctx context.Context, record *Record) error {
	if _, ok := m.watches[record.BatchTxID]; ok {
		return nil
	}

	w := watchFromRecord(record)

	// Arm the chain watches BEFORE recording the watch, mirroring the
	// initial registration path. If arming fails, leaving m.watches
	// untouched lets a later Reconcile retry the full arm from scratch
	// rather than treating this batch as permanently armed; re-registering
	// the same conf/spend caller IDs is idempotent.
	if err := m.armWatches(ctx, w, record.ConsumedInputs); err != nil {
		return fmt.Errorf("re-arm batch %s watches: %w",
			record.BatchTxID, err)
	}
	m.watches[record.BatchTxID] = w

	return nil
}

// watchFromRecord reconstructs conservative in-memory reducer state from
// durable observations. Persisted conflict flags take priority over a later
// confirmation replay, so restart ordering cannot briefly admit a conflict.
func watchFromRecord(record *Record) *batchWatch {
	w := &batchWatch{
		txid:       record.BatchTxID,
		pkScript:   record.ConfirmationPkScript,
		heightHint: record.WatchHeightHint,
		inputs:     make(map[wire.OutPoint]*inputWatch),
		confHeight: record.ConfirmationHeight,
		confBlock:  record.ConfirmationBlock,
		generation: record.ObservationGeneration,
		ready:      record.Ready(),
		persisted:  record.State,
	}

	switch record.State {
	case StateProvisional, StateConflictProvisional:
		w.conf = confConfirmed

	case StateFinalized:
		w.conf = confFinalized

	case StateReorgedOut:
		w.conf = confReorgedOut

	default:
		w.conf = confUnseen
	}

	for _, in := range record.ConsumedInputs {
		w.inputs[in.Outpoint] = &inputWatch{
			pkScript:          in.PkScript,
			spenderIsConflict: in.Conflicting || in.ConflictFinal,
			conflicting:       in.Conflicting,
			conflictFinal:     in.ConflictFinal,
		}
	}

	return w
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

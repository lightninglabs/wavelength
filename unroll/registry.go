package unroll

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/txconfirm"
	"github.com/lightninglabs/darepo-client/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// initialPersistRetryDelay is the first delay used when retrying
	// control-plane persistence for a live unroll child.
	initialPersistRetryDelay = 250 * time.Millisecond

	// maxPersistRetryDelay caps the exponential backoff for control-plane
	// persistence retries.
	maxPersistRetryDelay = 5 * time.Second
)

// RegistryRecord stores the coarse control-plane view of one unroll target.
type RegistryRecord struct {
	// TargetOutpoint identifies the target VTXO.
	TargetOutpoint wire.OutPoint

	// ActorID identifies the durable per-target actor.
	ActorID string

	// Trigger records why the target was started.
	Trigger StartTrigger

	// Phase is the last known coarse lifecycle phase.
	Phase Phase

	// FailReason stores the terminal failure when present.
	FailReason string

	// SweepTxid stores the terminal sweep txid when known.
	SweepTxid *chainhash.Hash
}

// IsTerminal reports whether the record reached a terminal phase.
func (r RegistryRecord) IsTerminal() bool {
	return r.Phase == PhaseCompleted || r.Phase == PhaseFailed
}

// RegistryStore is the control-plane persistence surface used by the unroll
// registry.
type RegistryStore interface {
	// UpsertRecord stores the latest control-plane view for one target.
	UpsertRecord(ctx context.Context, record RegistryRecord) error

	// GetRecord returns one target record when present.
	GetRecord(ctx context.Context, target wire.OutPoint) (
		*RegistryRecord, error,
	)

	// ListNonTerminalRecords returns all targets that still need restore.
	ListNonTerminalRecords(ctx context.Context) ([]RegistryRecord, error)

	// MarkTerminal persists one terminal target state.
	MarkTerminal(ctx context.Context, target wire.OutPoint, phase Phase,
		failReason string, sweepTxid *chainhash.Hash) error
}

// RegistryConfig configures the thin unroll registry actor.
type RegistryConfig struct {
	// Store persists coarse registry records for restore.
	Store RegistryStore

	// DeliveryStore provides durable mailbox and checkpoint persistence for
	// child actors.
	DeliveryStore actor.DeliveryStore

	// ProofAssembler resolves immutable proofs for child actors.
	ProofAssembler ProofAssembler

	// VTXOStore loads target descriptors for child actors.
	VTXOStore vtxo.VTXOStore

	// TxConfirmRef is the shared tx-confirmation actor.
	TxConfirmRef actor.ActorRef[txconfirm.Msg, txconfirm.Resp]

	// ChainSource provides best-height and fee-estimate queries.
	ChainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]

	// Wallet provides sweep destination derivation and signing.
	Wallet SweepWallet

	// Log is an optional logger.
	Log fn.Option[btclog.Logger]

	// MaxSweepFeeRateSatPerVByte clamps pathological fee estimates.
	MaxSweepFeeRateSatPerVByte int64
}

// UnrollRegistryActor wraps the thin unroll registry actor.
type UnrollRegistryActor struct {
	ref      actor.ActorRef[RegistryMsg, RegistryResp]
	registry *actor.Actor[RegistryMsg, RegistryResp]
	behavior *registryBehavior
}

// Ref returns the public registry actor reference.
func (a *UnrollRegistryActor) Ref() actor.ActorRef[RegistryMsg, RegistryResp] {
	return a.ref
}

// RestoreNonTerminal resumes all non-terminal records from the control store.
func (a *UnrollRegistryActor) RestoreNonTerminal(ctx context.Context) error {
	if a == nil || a.behavior == nil {
		return fmt.Errorf("registry actor not initialized")
	}

	return a.behavior.restoreNonTerminal(ctx)
}

// Stop stops the underlying registry actor.
func (a *UnrollRegistryActor) Stop() {
	if a == nil || a.registry == nil {
		return
	}

	a.registry.Stop()
}

// NewUnrollRegistryActor creates and starts the thin unroll registry actor.
func NewUnrollRegistryActor(cfg RegistryConfig) *UnrollRegistryActor {
	behavior := &registryBehavior{
		cfg:        cfg,
		log:        cfg.Log.UnwrapOr(btclog.Disabled),
		active:     make(map[wire.OutPoint]*VTXOUnrollActor),
		pending:    make(map[wire.OutPoint]RegistryRecord),
		persisting: make(map[wire.OutPoint]RegistryRecord),
	}

	registry := actor.NewActor(actor.ActorConfig[RegistryMsg, RegistryResp]{
		ID:          "unroll-registry",
		Behavior:    behavior,
		MailboxSize: 64,
	})
	behavior.selfRef = registry.TellRef()
	registry.Start()

	return &UnrollRegistryActor{
		ref:      registry.Ref(),
		registry: registry,
		behavior: behavior,
	}
}

// registryBehavior is the thin control-plane actor around per-target unroll
// actors.
type registryBehavior struct {
	cfg     RegistryConfig
	log     btclog.Logger
	selfRef actor.TellOnlyRef[RegistryMsg]

	active     map[wire.OutPoint]*VTXOUnrollActor
	pending    map[wire.OutPoint]RegistryRecord
	persisting map[wire.OutPoint]RegistryRecord

	spawnFunc func(context.Context, wire.OutPoint) (*VTXOUnrollActor, error)
}

// persistActiveRecordMsg asks the registry to retry persisting one active
// child's control-plane view.
type persistActiveRecordMsg struct {
	actor.BaseMessage

	// Outpoint identifies the active child to persist.
	Outpoint wire.OutPoint

	// Attempt is the zero-based retry attempt number.
	Attempt int
}

// MessageType returns the stable message type identifier.
func (m *persistActiveRecordMsg) MessageType() string {
	return "persistActiveRecordMsg"
}

// registryMsgSealed seals persistActiveRecordMsg into the registry surface.
func (m *persistActiveRecordMsg) registryMsgSealed() {}

// persistRecordResultMsg reports the outcome of one asynchronous control-plane
// persistence attempt.
type persistRecordResultMsg struct {
	actor.BaseMessage

	// Outpoint identifies the target whose record was persisted.
	Outpoint wire.OutPoint

	// Attempt is the zero-based retry attempt number.
	Attempt int

	// Record is the exact snapshot that was written.
	Record RegistryRecord

	// Err is populated when the write failed.
	Err string
}

// MessageType returns the stable message type identifier.
func (m *persistRecordResultMsg) MessageType() string {
	return "persistRecordResultMsg"
}

// registryMsgSealed seals persistRecordResultMsg into the registry surface.
func (m *persistRecordResultMsg) registryMsgSealed() {}

// Receive processes one registry message.
func (r *registryBehavior) Receive(ctx context.Context,
	msg RegistryMsg) fn.Result[RegistryResp] {

	switch req := msg.(type) {
	case *EnsureUnrollRequest:
		return r.handleEnsure(ctx, req)

	case *GetStatusRequest:
		return r.handleGetStatus(ctx, req)

	case *UnrollTerminatedMsg:
		return r.handleTerminated(ctx, req)

	case *persistActiveRecordMsg:
		return r.handlePersistActiveRecord(ctx, req)

	case *persistRecordResultMsg:
		return r.handlePersistRecordResult(ctx, req)

	default:
		return fn.Err[RegistryResp](
			fmt.Errorf("unknown registry message: %T", msg),
		)
	}
}

// OnStop stops all active child actors when the registry shuts down.
func (r *registryBehavior) OnStop(context.Context) error {
	for _, child := range r.active {
		child.Stop()
	}

	return nil
}

// handleEnsure deduplicates, spawns, starts, and persists one target.
func (r *registryBehavior) handleEnsure(ctx context.Context,
	req *EnsureUnrollRequest) fn.Result[RegistryResp] {

	if child, ok := r.active[req.Outpoint]; ok {
		return fn.Ok[RegistryResp](&EnsureUnrollResp{
			ActorID: child.Ref().ID(),
			Created: false,
		})
	}

	height, err := r.queryBestHeight(ctx)
	if err != nil {
		return fn.Err[RegistryResp](fmt.Errorf("best height: %w", err))
	}

	child, err := r.spawn(ctx, req.Outpoint)
	if err != nil {
		return fn.Err[RegistryResp](fmt.Errorf("spawn child: %w", err))
	}

	_, err = child.Ref().Ask(ctx, &StartUnrollRequest{
		Height:  height,
		Trigger: req.Trigger,
	}).Await(ctx).Unpack()
	if err != nil {
		child.Stop()
		return fn.Err[RegistryResp](fmt.Errorf("start child: %w", err))
	}

	r.active[req.Outpoint] = child

	state, err := r.childState(ctx, child)
	if err != nil {
		child.Stop()
		delete(r.active, req.Outpoint)
		return fn.Err[RegistryResp](
			fmt.Errorf("read child state: %w", err),
		)
	}

	record := recordFromChildState(
		req.Outpoint, child.Ref().ID(), state,
	)
	r.pending[req.Outpoint] = cloneRegistryRecord(record)
	r.requestPersist(req.Outpoint, 0)

	return fn.Ok[RegistryResp](&EnsureUnrollResp{
		ActorID: child.Ref().ID(),
		Created: true,
	})
}

// handleGetStatus returns active child state when available, otherwise the
// last stored control-plane view.
func (r *registryBehavior) handleGetStatus(ctx context.Context,
	req *GetStatusRequest) fn.Result[RegistryResp] {

	if child, ok := r.active[req.Outpoint]; ok {
		state, err := r.childState(ctx, child)
		if err == nil {
			return fn.Ok[RegistryResp](&GetStatusResp{
				Found:   true,
				Active:  true,
				ActorID: child.Ref().ID(),
				State:   state,
				Phase:   state.Phase,
				Trigger: state.Trigger,
			})
		}

		r.log.WarnS(ctx, "Failed to read active unroll state; "+
			"falling back to cached status", err,
			slog.String("outpoint", req.Outpoint.String()),
			slog.String("actor_id", child.Ref().ID()),
		)
	}

	if record, ok := r.pending[req.Outpoint]; ok {
		cached := cloneRegistryRecord(record)
		return fn.Ok[RegistryResp](&GetStatusResp{
			Found:      true,
			Active:     false,
			ActorID:    cached.ActorID,
			Phase:      cached.Phase,
			Trigger:    cached.Trigger,
			FailReason: cached.FailReason,
			SweepTxid:  copyHash(cached.SweepTxid),
		})
	}

	record, err := r.cfg.Store.GetRecord(ctx, req.Outpoint)
	if err != nil {
		return fn.Err[RegistryResp](fmt.Errorf("get record: %w", err))
	}

	if record == nil {
		return fn.Ok[RegistryResp](&GetStatusResp{})
	}

	return fn.Ok[RegistryResp](&GetStatusResp{
		Found:      true,
		Active:     false,
		ActorID:    record.ActorID,
		Phase:      record.Phase,
		Trigger:    record.Trigger,
		FailReason: record.FailReason,
		SweepTxid:  copyHash(record.SweepTxid),
	})
}

// handleTerminated removes the child from the active map and queues the latest
// terminal snapshot for asynchronous persistence.
func (r *registryBehavior) handleTerminated(ctx context.Context,
	req *UnrollTerminatedMsg) fn.Result[RegistryResp] {

	record := RegistryRecord{
		TargetOutpoint: req.Outpoint,
		ActorID:        req.ActorID,
		Phase:          req.Phase,
		FailReason:     req.FailReason,
		SweepTxid:      copyHash(req.SweepTxid),
	}

	if cached, ok := r.pending[req.Outpoint]; ok {
		record = cloneRegistryRecord(cached)
		record.Phase = req.Phase
		record.FailReason = req.FailReason
		record.SweepTxid = copyHash(req.SweepTxid)
		if record.ActorID == "" {
			record.ActorID = req.ActorID
		}
	}

	if child, ok := r.active[req.Outpoint]; ok {
		state, err := r.childState(ctx, child)
		if err == nil {
			record = recordFromChildState(
				req.Outpoint, child.Ref().ID(), state,
			)
		} else {
			r.log.WarnS(ctx, "Failed to read terminal unroll state; "+
				"using cached notification data", err,
				slog.String("outpoint", req.Outpoint.String()),
				slog.String("actor_id", child.Ref().ID()),
			)
		}

		child.Stop()
		delete(r.active, req.Outpoint)
	}

	r.pending[req.Outpoint] = cloneRegistryRecord(record)
	r.requestPersist(req.Outpoint, 0)

	return fn.Ok[RegistryResp](&RegistryAckResp{})
}

// restoreNonTerminal respawns and resumes every non-terminal record.
func (r *registryBehavior) restoreNonTerminal(ctx context.Context) error {
	records, err := r.cfg.Store.ListNonTerminalRecords(ctx)
	if err != nil {
		return fmt.Errorf("list non-terminal records: %w", err)
	}

	if len(records) == 0 {
		return nil
	}

	height, err := r.queryBestHeight(ctx)
	if err != nil {
		return fmt.Errorf("best height for restore: %w", err)
	}

	for i := range records {
		record := records[i]
		if _, ok := r.active[record.TargetOutpoint]; ok {
			continue
		}

		child, err := r.spawn(ctx, record.TargetOutpoint)
		if err != nil {
			_ = r.cfg.Store.MarkTerminal(
				ctx, record.TargetOutpoint, PhaseFailed,
				"spawn failed on restore: "+err.Error(), nil,
			)

			continue
		}

		_, err = child.Ref().Ask(ctx, &ResumeUnrollRequest{
			Height: height,
		}).Await(ctx).Unpack()
		if err != nil {
			child.Stop()
			_ = r.cfg.Store.MarkTerminal(
				ctx, record.TargetOutpoint, PhaseFailed,
				"resume failed on restore: "+err.Error(), nil,
			)

			continue
		}

		r.active[record.TargetOutpoint] = child
	}

	return nil
}

// handlePersistActiveRecord snapshots the latest target record and starts one
// asynchronous persistence attempt when no matching write is already in flight.
func (r *registryBehavior) handlePersistActiveRecord(ctx context.Context,
	req *persistActiveRecordMsg) fn.Result[RegistryResp] {

	record, ok, err := r.recordForPersistence(ctx, req.Outpoint)
	if err != nil {
		r.log.WarnS(ctx, "Failed to snapshot unroll record for "+
			"persistence", err,
			slog.String("outpoint", req.Outpoint.String()),
			slog.Int("attempt", req.Attempt+1),
		)
		r.schedulePersistRetry(req.Outpoint, req.Attempt+1)

		return fn.Ok[RegistryResp](&RegistryAckResp{})
	}

	if !ok {
		delete(r.persisting, req.Outpoint)
		return fn.Ok[RegistryResp](&RegistryAckResp{})
	}

	inFlight, ok := r.persisting[req.Outpoint]
	if ok && sameRegistryRecord(inFlight, record) {
		return fn.Ok[RegistryResp](&RegistryAckResp{})
	}
	if ok {
		return fn.Ok[RegistryResp](&RegistryAckResp{})
	}

	r.persisting[req.Outpoint] = cloneRegistryRecord(record)
	r.persistRecordAsync(req.Outpoint, req.Attempt, record)

	return fn.Ok[RegistryResp](&RegistryAckResp{})
}

// handlePersistRecordResult processes one asynchronous record-persistence
// completion and decides whether another attempt is needed.
func (r *registryBehavior) handlePersistRecordResult(ctx context.Context,
	req *persistRecordResultMsg) fn.Result[RegistryResp] {

	delete(r.persisting, req.Outpoint)

	if req.Err == "" {
		record, ok := r.pending[req.Outpoint]
		if ok && sameRegistryRecord(record, req.Record) {
			delete(r.pending, req.Outpoint)
		} else if ok {
			r.requestPersist(req.Outpoint, 0)
		}

		return fn.Ok[RegistryResp](&RegistryAckResp{})
	}

	err := fmt.Errorf("%s", req.Err)
	r.log.WarnS(ctx, "Failed to persist unroll record", err,
		slog.String("outpoint", req.Outpoint.String()),
		slog.Int("attempt", req.Attempt+1),
		slog.String("phase", string(req.Record.Phase)),
	)

	record, ok := r.pending[req.Outpoint]
	if ok && sameRegistryRecord(record, req.Record) {
		r.schedulePersistRetry(req.Outpoint, req.Attempt+1)
	} else if ok {
		r.requestPersist(req.Outpoint, 0)
	}

	return fn.Ok[RegistryResp](&RegistryAckResp{})
}

// spawn creates one per-target unroll actor.
func (r *registryBehavior) spawn(ctx context.Context,
	target wire.OutPoint) (*VTXOUnrollActor, error) {

	if r.spawnFunc != nil {
		return r.spawnFunc(ctx, target)
	}

	return NewVTXOUnrollActor(Config{
		TargetOutpoint:             target,
		DeliveryStore:              r.cfg.DeliveryStore,
		ProofAssembler:             r.cfg.ProofAssembler,
		VTXOStore:                  r.cfg.VTXOStore,
		TxConfirmRef:               r.cfg.TxConfirmRef,
		ChainSource:                r.cfg.ChainSource,
		Wallet:                     r.cfg.Wallet,
		Log:                        r.cfg.Log,
		MaxSweepFeeRateSatPerVByte: r.cfg.MaxSweepFeeRateSatPerVByte,
		RegistryRef:                r.selfRef,
	})
}

// queryBestHeight queries the current best height from chainsource.
func (r *registryBehavior) queryBestHeight(ctx context.Context) (int32, error) {
	resp, err := r.cfg.ChainSource.Ask(
		ctx, &chainsource.BestHeightRequest{},
	).Await(ctx).Unpack()
	if err != nil {
		return 0, err
	}

	bestHeight, ok := resp.(*chainsource.BestHeightResponse)
	if !ok {
		return 0, fmt.Errorf("unexpected best height response %T", resp)
	}

	return bestHeight.Height, nil
}

// childState reads the detailed state from one active child actor.
func (r *registryBehavior) childState(ctx context.Context,
	child *VTXOUnrollActor) (*GetStateResp, error) {

	resp, err := child.Ref().Ask(
		ctx, &GetStateRequest{},
	).Await(ctx).Unpack()
	if err != nil {
		return nil, err
	}

	state, ok := resp.(*GetStateResp)
	if !ok {
		return nil, fmt.Errorf(
			"unexpected child state response %T", resp,
		)
	}

	return state, nil
}

// recordForPersistence snapshots the latest control-plane view for one target.
func (r *registryBehavior) recordForPersistence(ctx context.Context,
	target wire.OutPoint) (RegistryRecord, bool, error) {

	if record, ok := r.pending[target]; ok {
		return cloneRegistryRecord(record), true, nil
	}

	child, ok := r.active[target]
	if !ok {
		return RegistryRecord{}, false, nil
	}

	state, err := r.childState(ctx, child)
	if err != nil {
		return RegistryRecord{}, false, fmt.Errorf(
			"read child state: %w", err,
		)
	}

	record := recordFromChildState(target, child.Ref().ID(), state)

	return record, true, nil
}

// persistRecordAsync writes one record on a background goroutine and reports
// the result back to the registry actor.
func (r *registryBehavior) persistRecordAsync(target wire.OutPoint,
	attempt int, record RegistryRecord) {

	go func() {
		cloned := cloneRegistryRecord(record)
		err := r.cfg.Store.UpsertRecord(
			context.Background(), cloned,
		)

		result := &persistRecordResultMsg{
			Outpoint: target,
			Attempt:  attempt,
			Record:   cloned,
		}
		if err != nil {
			result.Err = err.Error()
		}

		_ = r.selfRef.Tell(context.Background(), result)
	}()
}

// requestPersist enqueues one immediate persistence attempt for the target.
func (r *registryBehavior) requestPersist(target wire.OutPoint,
	attempt int) {

	_ = r.selfRef.Tell(context.Background(), &persistActiveRecordMsg{
		Outpoint: target,
		Attempt:  attempt,
	})
}

// schedulePersistRetry enqueues one delayed retry for record persistence.
func (r *registryBehavior) schedulePersistRetry(target wire.OutPoint,
	attempt int) {

	delay := persistRetryDelay(attempt)

	time.AfterFunc(delay, func() {
		msg := &persistActiveRecordMsg{
			Outpoint: target,
			Attempt:  attempt,
		}
		_ = r.selfRef.Tell(context.Background(), msg)
	})
}

// persistRetryDelay returns the backoff delay for one persistence retry.
func persistRetryDelay(attempt int) time.Duration {
	delay := initialPersistRetryDelay
	for i := 0; i < attempt; i++ {
		if delay >= maxPersistRetryDelay/2 {
			return maxPersistRetryDelay
		}

		delay *= 2
	}

	return delay
}

// recordFromChildState converts one live child snapshot into a registry
// record.
func recordFromChildState(target wire.OutPoint, actorID string,
	state *GetStateResp) RegistryRecord {

	return RegistryRecord{
		TargetOutpoint: target,
		ActorID:        actorID,
		Trigger:        state.Trigger,
		Phase:          state.Phase,
		FailReason:     state.FailReason,
		SweepTxid:      copyHash(state.SweepTxid),
	}
}

// cloneRegistryRecord deep-copies one registry record.
func cloneRegistryRecord(record RegistryRecord) RegistryRecord {
	record.SweepTxid = copyHash(record.SweepTxid)
	return record
}

// sameRegistryRecord reports whether two registry records carry the same
// control-plane snapshot.
func sameRegistryRecord(a, b RegistryRecord) bool {
	if a.TargetOutpoint != b.TargetOutpoint ||
		a.ActorID != b.ActorID ||
		a.Trigger != b.Trigger ||
		a.Phase != b.Phase ||
		a.FailReason != b.FailReason {

		return false
	}

	switch {
	case a.SweepTxid == nil && b.SweepTxid == nil:
		return true

	case a.SweepTxid == nil || b.SweepTxid == nil:
		return false

	default:
		return *a.SweepTxid == *b.SweepTxid
	}
}

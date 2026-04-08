package unroll

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/txconfirm"
	"github.com/lightninglabs/darepo-client/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
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
		cfg:    cfg,
		log:    cfg.Log.UnwrapOr(btclog.Disabled),
		active: make(map[wire.OutPoint]*VTXOUnrollActor),
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

	active map[wire.OutPoint]*VTXOUnrollActor

	spawnFunc func(context.Context, wire.OutPoint) (*VTXOUnrollActor, error)
}

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
		return fn.Err[RegistryResp](fmt.Errorf("read child state: %w", err))
	}

	err = r.cfg.Store.UpsertRecord(ctx, RegistryRecord{
		TargetOutpoint: req.Outpoint,
		ActorID:        child.Ref().ID(),
		Trigger:        req.Trigger,
		Phase:          state.Phase,
		FailReason:     state.FailReason,
		SweepTxid:      copyHash(state.SweepTxid),
	})
	if err != nil {
		child.Stop()
		delete(r.active, req.Outpoint)
		return fn.Err[RegistryResp](fmt.Errorf("upsert record: %w", err))
	}

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
		if err != nil {
			return fn.Err[RegistryResp](err)
		}

		return fn.Ok[RegistryResp](&GetStatusResp{
			Found:   true,
			Active:  true,
			ActorID: child.Ref().ID(),
			State:   state,
			Phase:   state.Phase,
			Trigger: state.Trigger,
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

// handleTerminated removes the child from the active map and marks it
// terminal in the control store.
func (r *registryBehavior) handleTerminated(ctx context.Context,
	req *UnrollTerminatedMsg) fn.Result[RegistryResp] {

	if child, ok := r.active[req.Outpoint]; ok {
		child.Stop()
		delete(r.active, req.Outpoint)
	}

	err := r.cfg.Store.MarkTerminal(
		ctx, req.Outpoint, req.Phase, req.FailReason,
		copyHash(req.SweepTxid),
	)
	if err != nil {
		return fn.Err[RegistryResp](fmt.Errorf("mark terminal: %w", err))
	}

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
		return nil, fmt.Errorf("unexpected child state response %T", resp)
	}

	return state, nil
}

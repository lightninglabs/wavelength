package unroll

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/lightninglabs/wavelength/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// childAdmissionTimeout bounds the registry's synchronous attempt to
	// start a newly-admitted child before falling back to durable retry.
	childAdmissionTimeout = 30 * time.Second

	// detailedStatusAskTimeout bounds the registry's synchronous Ask to a
	// live child for a detailed status probe. The registry is single-
	// goroutine, so an unbounded Ask against a wedged child would stall
	// every other outpoint; on timeout the probe degrades to the coarse
	// cached record.
	detailedStatusAskTimeout = 3 * time.Second

	// initialPersistRetryDelay is the first delay used when retrying
	// control-plane persistence for a live unroll child.
	initialPersistRetryDelay = 250 * time.Millisecond

	// maxPersistRetryDelay caps the exponential backoff for control-plane
	// persistence retries.
	maxPersistRetryDelay = 5 * time.Second

	// terminalChildDrainTimeout bounds the cleanup probe used before
	// stopping a terminal child actor.
	terminalChildDrainTimeout = 30 * time.Second
)

// RegistryRecord stores the coarse control-plane view of one unroll target.
type RegistryRecord struct {
	// TargetOutpoint identifies the target VTXO.
	TargetOutpoint wire.OutPoint

	// ActorID identifies the durable per-target actor.
	ActorID string

	// Trigger records why the target was started.
	Trigger StartTrigger

	// ExitPolicyKind identifies the final spend policy for this target.
	ExitPolicyKind ExitPolicyKind

	// ExitPolicyRef is the policy-specific durable reference.
	ExitPolicyRef string

	// Phase is the last known coarse lifecycle phase.
	Phase Phase

	// FailReason stores the terminal failure when present.
	FailReason string

	// SweepTxid stores the terminal sweep txid when known.
	SweepTxid *chainhash.Hash

	// RecoverableFailure is set on a terminal failure that left no
	// on-chain footprint, so the target VTXO is safe to roll back to
	// live. It is persisted (as a distinct DB status) so boot-time
	// reconciliation can recover a VTXO whose recovery notification was
	// lost before the manager applied it (wavelength#602).
	RecoverableFailure bool
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
	GetRecord(ctx context.Context,
		target wire.OutPoint) (*RegistryRecord, error)

	// ListNonTerminalRecords returns all targets that still need restore.
	ListNonTerminalRecords(ctx context.Context) ([]RegistryRecord, error)

	// MarkTerminal persists one terminal target state. recoverable marks a
	// no-footprint failure that boot-time reconciliation may roll back to
	// live.
	MarkTerminal(ctx context.Context, target wire.OutPoint, phase Phase,
		recoverable bool, failReason string,
		sweepTxid *chainhash.Hash) error
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

	// LedgerSink receives confirmed exit-cost events from child unroll
	// actors.
	LedgerSink fn.Option[ledger.Sink]

	// Log is an optional logger.
	Log fn.Option[btclog.Logger]

	// MaxSweepFeeRateSatPerVByte clamps pathological fee estimates.
	MaxSweepFeeRateSatPerVByte int64

	// ExitSpendPolicyResolver reconstructs the exit policy for each
	// spawned child from the durable (ExitPolicyKind, ExitPolicyRef)
	// identity persisted on the unroll job. If nil, every child uses
	// the built-in standard VTXO timeout resolver and any non-standard
	// kind will fail closed at sweep time. Forwarded verbatim into each
	// spawned VTXOUnrollActor's Config so policy-specific unroll jobs
	// (e.g. vHTLC recovery) reach their custom resolver after restart.
	ExitSpendPolicyResolver ExitSpendPolicyResolver

	// FraudCheckpointSafetyMargin overrides the default backstop
	// margin (in blocks) the recipient subtracts from the relative
	// expiry when deciding to self-broadcast a fraud-triggered
	// checkpoint. Zero applies defaultFraudCheckpointSafetyMargin;
	// the effective margin is always clamped to csvDelay/2 when
	// csvDelay is too small to absorb the configured value. Plumbed
	// into every spawned VTXOUnrollActor and from there into the
	// FSM Environment.
	FraudCheckpointSafetyMargin int32

	// VTXOExitObserver, when set, receives an ExitOutcomeNotification each
	// time a child unroll job reaches a terminal phase: a clean failure
	// (no on-chain footprint) asks the VTXO manager to roll the VTXO back
	// to live, and a completed exit asks it to retire the VTXO to spent.
	// This is the feedback edge that keeps VTXO lifecycle gated on the
	// unroll job's terminal on-chain outcome rather than the user's intent
	// to exit (wavelength#602). When None, terminal outcomes are not
	// forwarded (used by tests that don't exercise the manager).
	VTXOExitObserver fn.Option[actor.TellOnlyRef[vtxo.ManagerMsg]]
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
//
// The actual restore runs inside the registry actor's goroutine via a
// restoreNonTerminalMsg, so all mutations of r.active and r.pending stay
// serialized with concurrent Receive turns (handleEnsure / handleGetStatus
// can already be running by the time the daemon boot path reaches this
// call, because NewUnrollRegistryActor has already Start()ed the actor).
func (a *UnrollRegistryActor) RestoreNonTerminal(ctx context.Context) error {
	if a == nil || a.ref == nil {
		return fmt.Errorf("registry actor not initialized")
	}

	resp, err := a.ref.Ask(
		ctx, &restoreNonTerminalMsg{},
	).Await(ctx).Unpack()
	if err != nil {
		return err
	}

	result, ok := resp.(*restoreNonTerminalResp)
	if !ok {
		return fmt.Errorf("unexpected restore response %T", resp)
	}

	if result.Err != "" {
		return fmt.Errorf("%s", result.Err)
	}

	return nil
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

// childAdmissionResultMsg reports the eventual result of a child start
// request after the registry's synchronous admission wait timed out.
type childAdmissionResultMsg struct {
	actor.BaseMessage

	// Outpoint identifies the target whose start completed late.
	Outpoint wire.OutPoint

	// ActorID identifies the child that produced the late result.
	ActorID string

	// Err is populated when the child start failed.
	Err string
}

// MessageType returns the stable message type identifier.
func (m *childAdmissionResultMsg) MessageType() string {
	return "childAdmissionResultMsg"
}

// registryMsgSealed seals childAdmissionResultMsg into the registry surface.
func (m *childAdmissionResultMsg) registryMsgSealed() {}

// restoreNonTerminalMsg drives the boot-time restore of every non-terminal
// record through the registry actor's goroutine. Sending it via Ask keeps
// all mutations of r.active and r.pending serialized with concurrent
// Receive turns (handleEnsure / handleGetStatus can already be running by
// the time the daemon boot path issues this call).
type restoreNonTerminalMsg struct {
	actor.BaseMessage
}

// MessageType returns the stable message type identifier.
func (m *restoreNonTerminalMsg) MessageType() string {
	return "restoreNonTerminalMsg"
}

// registryMsgSealed seals restoreNonTerminalMsg into the registry surface.
func (m *restoreNonTerminalMsg) registryMsgSealed() {}

// restoreNonTerminalResp carries the outcome of a boot-time restore back
// to the caller of UnrollRegistryActor.RestoreNonTerminal.
type restoreNonTerminalResp struct {
	actor.BaseMessage

	// Err is populated when the restore returned an error.
	Err string
}

// MessageType returns the stable message type identifier.
func (m *restoreNonTerminalResp) MessageType() string {
	return "restoreNonTerminalResp"
}

// registryRespSealed seals restoreNonTerminalResp into the registry surface.
func (m *restoreNonTerminalResp) registryRespSealed() {}

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

	case *childAdmissionResultMsg:
		return r.handleChildAdmissionResult(ctx, req)

	case *restoreNonTerminalMsg:
		return r.handleRestoreNonTerminal(ctx)

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

// handleEnsure is the admission gate for new unroll jobs. It runs a
// four-stage check to decide whether the caller is re-asking for an
// already-tracked target or requesting a brand-new unroll, spawns and
// starts the child when needed, and makes the control-plane record durable
// before returning success.
//
// Deduplication trail, in order of cost:
//
//  1. r.active (in-memory): a live child is running right now. Return
//     its ActorID with Created=false. No store hit.
//
//  2. r.pending (in-memory): a child has terminated but its latest
//     snapshot has not yet flushed to the durable store. Returning the
//     pending ActorID here avoids clobbering a terminal sweep txid or
//     failure reason with a fresh restart record.
//
//  3. Store.GetRecord: the record exists on disk but the child was
//     never restored (e.g. the registry was just started and the caller
//     asked before RestoreNonTerminal completed). Same dedup semantics.
//
// Only if all three miss do we spawn a fresh child, fetch best height,
// persist a pending record, send StartUnrollRequest, and read back the
// resulting state.
//
// The pending store write is synchronous on purpose: returning Created=true
// is a promise that RestoreNonTerminal will see this target on the next
// boot. Persisting before the first child message also closes the window
// where a caller-context cancellation could stop the child before any
// durable control-plane row exists. If the sync write fails, we roll back
// the child and surface the error. Subsequent state refinement is best
// effort because the pending record is already enough to restore the job.
func (r *registryBehavior) handleEnsure(ctx context.Context,
	req *EnsureUnrollRequest) fn.Result[RegistryResp] {

	if err := validateExitPolicyIdentity(
		req.ExitPolicyKind, req.ExitPolicyRef,
	); err != nil {
		return fn.Err[RegistryResp](err)
	}

	if child, ok := r.active[req.Outpoint]; ok {
		return fn.Ok[RegistryResp](&EnsureUnrollResp{
			ActorID: child.Ref().ID(),
			Created: false,
		})
	}

	// Terminal records leave the active map before their persistence
	// write completes, so fall back to the in-memory pending cache and
	// the durable store. Re-spawning a fresh actor on top of an existing
	// record would clobber the recorded sweep txid or fail reason.
	//
	// A recoverable failure is the exception: the prior exit failed
	// cleanly with no on-chain footprint and the VTXO was rolled back to
	// live (wavelength#602), so a fresh exit must be admittable rather
	// than deduped against the dead attempt. Fall through to the spawn
	// path, whose UpsertRecord overwrites the stale recoverable row.
	if record, ok := r.pending[req.Outpoint]; ok &&
		!record.RecoverableFailure {
		return fn.Ok[RegistryResp](&EnsureUnrollResp{
			ActorID: record.ActorID,
			Created: false,
		})
	}

	existing, err := r.cfg.Store.GetRecord(ctx, req.Outpoint)
	if err != nil {
		return fn.Err[RegistryResp](
			fmt.Errorf("lookup existing record: %w", err),
		)
	}
	if existing != nil {
		// A durable record exists but no child is live for it. Two
		// sub-cases:
		//
		//   1. Terminal record (Completed / non-recoverable Failed)
		//      — return the historical ActorID so callers see a
		//      stable identity and do not clobber the recorded sweep
		//      txid or failure reason. A *recoverable* terminal
		//      failure is the exception: the VTXO was rolled back to
		//      live (wavelength#602), so it falls through to a
		//      fresh spawn (handled below).
		//
		//   2. Non-terminal record — the actor was admitted in a
		//      previous boot but never resumed (e.g. RestoreNonTerminal
		//      hit a transient ChainSource error). Attempt an inline
		//      restore so a fresh Ensure from the chain resolver or
		//      RPC layer can recover from a transient failure on the
		//      previous boot. If restore fails again, surface the
		//      error so the caller can retry; the durable record
		//      stays non-terminal and will be retried on the next
		//      Ensure / next daemon restart.
		if !existing.IsTerminal() {
			if err := r.validateRestorableRecords(
				[]RegistryRecord{*existing},
			); err != nil {
				return fn.Err[RegistryResp](
					fmt.Errorf("validate existing record "+
						"for restore: %w", err),
				)
			}

			height, err := r.queryBestHeight(ctx)
			if err != nil {
				return fn.Err[RegistryResp](
					fmt.Errorf("best height for "+
						"restore: %w", err),
				)
			}

			child, err := r.tryRestoreOne(ctx, *existing, height)
			if err != nil {
				return fn.Err[RegistryResp](
					fmt.Errorf("restore existing "+
						"record: %w", err),
				)
			}

			r.active[req.Outpoint] = child

			// Mirror the historical record into r.pending so
			// handleTerminated can carry over Trigger / ActorID
			// without an extra store lookup, and so handleGetStatus
			// answers from cache while the child runs.
			r.pending[req.Outpoint] = cloneRegistryRecord(*existing)

			return fn.Ok[RegistryResp](&EnsureUnrollResp{
				ActorID: child.Ref().ID(),
				Created: false,
			})
		}

		// Terminal record. A recoverable failure (clean, no on-chain
		// footprint) means the VTXO was rolled back to live, so a new
		// exit is allowed: fall through to the spawn path below, whose
		// UpsertRecord overwrites the stale record. Any other terminal
		// record (Completed, or a footprint-bearing Failed) is a real
		// end state — dedup against it and return the historical
		// ActorID so the recorded sweep txid / failure reason are
		// never clobbered.
		if !existing.RecoverableFailure {
			return fn.Ok[RegistryResp](&EnsureUnrollResp{
				ActorID: existing.ActorID,
				Created: false,
			})
		}
	}

	height, err := r.queryBestHeight(ctx)
	if err != nil {
		return fn.Err[RegistryResp](fmt.Errorf("best height: %w", err))
	}

	child, err := r.spawn(ctx, req.Outpoint)
	if err != nil {
		return fn.Err[RegistryResp](fmt.Errorf("spawn child: %w", err))
	}

	record := RegistryRecord{
		TargetOutpoint: req.Outpoint,
		ActorID:        child.Ref().ID(),
		Trigger:        req.Trigger,
		ExitPolicyKind: exitPolicyKind(req.ExitPolicyKind),
		ExitPolicyRef:  req.ExitPolicyRef,
		Phase:          PhasePending,
	}

	err = r.cfg.Store.UpsertRecord(ctx, cloneRegistryRecord(record))
	if err != nil {
		child.Stop()

		return fn.Err[RegistryResp](
			fmt.Errorf("persist unroll record: %w", err),
		)
	}

	r.active[req.Outpoint] = child
	r.pending[req.Outpoint] = cloneRegistryRecord(record)

	startReq := &StartUnrollRequest{
		Height:         height,
		Trigger:        req.Trigger,
		ExitPolicyKind: req.ExitPolicyKind,
		ExitPolicyRef:  req.ExitPolicyRef,
	}
	startCtx, cancelStart := context.WithTimeout(
		context.WithoutCancel(ctx), childAdmissionTimeout,
	)
	defer cancelStart()

	startFuture := child.Ref().Ask(context.WithoutCancel(ctx), startReq)
	_, err = startFuture.Await(startCtx).Unpack()
	if err != nil {
		// Two failure classes here, treated very differently:
		//
		//   1. Cancellation race — the admission context ended
		//      before the child responded. baselib/actor's durable
		//      Ask persists the message inside mailbox.Send BEFORE
		//      returning the future, so a context error from Await
		//      proves only that we stopped waiting; the message is
		//      already in the child's durable mailbox and the FSM
		//      will pick it up via the normal claim loop. We do
		//      NOT re-issue the request via Tell here: that would
		//      enqueue a second copy of an identical
		//      StartUnrollRequest, and the original Ask future may
		//      still complete (success or failure) inside the
		//      actor afterwards with no one listening. Returning
		//      Created=true is honest — the job IS durably
		//      admitted. The child's eventual terminal transition
		//      (success or failure) flows back through
		//      notifyRegistryIfTerminal so the registry record
		//      cannot stay PhasePending forever.
		//
		//   2. Real start error — proof assembly, store, planner.
		//      Hiding it under a Created=true would silently strand
		//      the user's funds in unilateral_exit with no
		//      progress. Mark the durable row PhaseFailed so
		//      GetUnrollStatus surfaces a terminal status.
		if isCancellationRace(err) {
			r.watchChildAdmissionResult(
				context.WithoutCancel(ctx), req.Outpoint, child,
				startFuture,
			)
			r.log.WarnS(ctx, "Unroll child start did not "+
				"respond before admission timeout; trusting "+
				"durable enqueue", err,
				slog.String("outpoint", req.Outpoint.String()),
				slog.String("actor_id", child.Ref().ID()),
			)

			return fn.Ok[RegistryResp](&EnsureUnrollResp{
				ActorID: child.Ref().ID(),
				Created: true,
			})
		}

		r.failAdmittedChild(
			ctx, req.Outpoint, child, fmt.Errorf("start child: %w",
				err),
		)

		return fn.Err[RegistryResp](fmt.Errorf("start child: %w", err))
	}

	state, err := r.childState(startCtx, child)
	if err != nil {
		if !isCancellationRace(err) {
			r.failAdmittedChild(
				ctx, req.Outpoint, child,
				fmt.Errorf("read child state: %w", err),
			)

			return fn.Err[RegistryResp](
				fmt.Errorf("read child state: %w", err),
			)
		}

		r.log.WarnS(ctx, "Failed to read started unroll state "+
			"after durable admission", err,
			slog.String("outpoint", req.Outpoint.String()),
			slog.String("actor_id", child.Ref().ID()),
		)

		return fn.Ok[RegistryResp](&EnsureUnrollResp{
			ActorID: child.Ref().ID(),
			Created: true,
		})
	}

	record = recordFromChildState(
		req.Outpoint, child.Ref().ID(), state,
	)
	r.pending[req.Outpoint] = cloneRegistryRecord(record)

	err = r.cfg.Store.UpsertRecord(startCtx, cloneRegistryRecord(record))
	if err != nil {
		r.log.WarnS(ctx, "Failed to refine unroll admission "+
			"record", err,
			slog.String("outpoint", req.Outpoint.String()),
			slog.String("actor_id", child.Ref().ID()),
		)

		// Registry persistence retries are actor-owned follow-up work.
		//nolint:contextcheck
		r.requestPersist(req.Outpoint, 0)
	}

	return fn.Ok[RegistryResp](&EnsureUnrollResp{
		ActorID: child.Ref().ID(),
		Created: true,
	})
}

// watchChildAdmissionResult waits for a previously enqueued child start to
// finish after the registry has already returned Created=true to the caller.
// The wait runs outside the registry actor goroutine, then reports back via a
// normal registry message so r.active / r.pending mutations remain serialized.
func (r *registryBehavior) watchChildAdmissionResult(ctx context.Context,
	target wire.OutPoint, child *VTXOUnrollActor,
	future actor.Future[Resp]) {

	actorID := child.Ref().ID()
	go func() {
		_, err := future.Await(ctx).Unpack()
		if err == nil || isCancellationRace(err) ||
			errors.Is(err, actor.ErrActorTerminated) {
			return
		}

		tellErr := r.selfRef.Tell(ctx,
			&childAdmissionResultMsg{
				Outpoint: target,
				ActorID:  actorID,
				Err:      err.Error(),
			},
		)
		if tellErr != nil {
			r.log.WarnS(
				ctx,
				"Failed to report late unroll child start "+
					"result",
				tellErr,
				slog.String("outpoint", target.String()),
				slog.String("actor_id", actorID),
			)
		}
	}()
}

// handleChildAdmissionResult converts a late child start failure into the
// same terminal registry state that a synchronous start failure would have
// produced.
func (r *registryBehavior) handleChildAdmissionResult(ctx context.Context,
	req *childAdmissionResultMsg) fn.Result[RegistryResp] {

	child, ok := r.active[req.Outpoint]
	if !ok || child.Ref().ID() != req.ActorID {
		return fn.Ok[RegistryResp](&RegistryAckResp{})
	}

	r.failAdmittedChild(
		ctx, req.Outpoint, child, fmt.Errorf("start child: %s",
			req.Err),
	)

	return fn.Ok[RegistryResp](&RegistryAckResp{})
}

// failAdmittedChild records a terminal failure for a child that already has
// a durable pending row. This keeps GetUnrollStatus from falling back to
// "not found" after VTXO ownership has moved to unilateral exit.
//
// These failures (start / proof-assembly / restore errors) all occur before
// the child broadcasts anything, so the VTXO has no on-chain footprint and is
// recoverable to live. The record is persisted as a recoverable failure and
// the VTXO manager is notified so the VTXO is rolled back rather than
// stranded in unilateral exit (wavelength#602).
func (r *registryBehavior) failAdmittedChild(ctx context.Context,
	target wire.OutPoint, child *VTXOUnrollActor, err error) {

	child.Stop()
	delete(r.active, target)

	record := RegistryRecord{
		TargetOutpoint:     target,
		ActorID:            child.Ref().ID(),
		Phase:              PhaseFailed,
		FailReason:         err.Error(),
		RecoverableFailure: true,
	}
	if pending, ok := r.pending[target]; ok {
		record = cloneRegistryRecord(pending)
		record.Phase = PhaseFailed
		record.FailReason = err.Error()
		record.RecoverableFailure = true
	}

	r.pending[target] = cloneRegistryRecord(record)

	// Roll the VTXO back to live. The terminal record below is the durable
	// backstop if this best-effort notification is lost. A recovery-only
	// target is held in exit instead (see notifyVTXOExit / the manager),
	// so its policy rides along.
	r.notifyVTXOExit(context.WithoutCancel(ctx), &UnrollTerminatedMsg{
		Outpoint:            target,
		ActorID:             record.ActorID,
		Phase:               PhaseFailed,
		FailReason:          err.Error(),
		HadOnChainFootprint: false,
	}, record.ExitPolicyKind)

	markErr := r.cfg.Store.MarkTerminal(
		context.WithoutCancel(ctx), target, PhaseFailed, true,
		err.Error(), nil,
	)
	if markErr != nil {
		r.log.WarnS(ctx, "Failed to mark admitted unroll child "+
			"terminal", markErr,
			slog.String("outpoint", target.String()),
			slog.String("actor_id", child.Ref().ID()),
		)
		// Registry persistence retries are actor-owned follow-up work.
		//nolint:contextcheck
		r.requestPersist(target, 0)
	}
}

// isCancellationRace reports whether admission should preserve the pending
// row and trust the durable mailbox instead of converting the job into a
// deterministic failure. It covers only the cases where mailbox.Send is
// known to have completed (the durable enqueue happened) and the future's
// Await stopped waiting:
//
//   - context.Canceled: the admission ctx (RPC ctx via WithoutCancel +
//     WithTimeout) reached its bound while the child was still
//     processing; the pending row already survives so retry is safe.
//
//   - context.DeadlineExceeded: same shape as Canceled but driven by the
//     WithTimeout cap rather than an explicit cancel. Same handoff.
//
// actor.ErrActorTerminated is intentionally NOT classified as a race:
// baselib/actor's durable Ask short-circuits and returns that error
// BEFORE calling mailbox.Send when the actor's own ctx is already done,
// so there is no persisted message to trust. Letting it fall through to
// failAdmittedChild surfaces a deterministic terminal record instead of
// pretending the job is in flight.
func isCancellationRace(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// handleGetStatus answers a status probe from the registry's cached
// control-plane view instead of asking the child actor.
//
// The child state request is read-only, but on a durable child actor it still
// becomes a durable mailbox message. Polling clients can therefore leave stale
// GetStateRequest rows behind after their RPC context expires, and those rows
// can starve progress notifications during block-mining-heavy tests. The
// registry's pending/store record is intentionally coarse, but it is enough for
// external status: admission records Materializing, terminal notifications
// update Completed/Failed, and active children are identified by ActorID.
//
// Read order:
//
//  1. r.active + r.pending/store: report the cached phase with Active=true.
//
//  2. r.pending: child has terminated but async persist has not flushed.
//     Report the cached terminal phase.
//
//  3. Store.GetRecord: neither of the above applies, so report whatever the
//     store says.
//
// Found=false is returned only when all three layers say nothing is
// known about the outpoint, letting callers distinguish "never
// requested" from "requested and in progress/terminal".
func (r *registryBehavior) handleGetStatus(ctx context.Context,
	req *GetStatusRequest) fn.Result[RegistryResp] {

	if child, ok := r.active[req.Outpoint]; ok {
		// A detailed probe enriches the coarse record with the live
		// child's planner-derived progress. The child Ask is
		// best-effort: on failure we fall through to the coarse record
		// so status never hard-fails on the detail path.
		detail := r.detailedChildState(ctx, req, child)

		if record, ok := r.pending[req.Outpoint]; ok {
			cached := cloneRegistryRecord(record)
			if cached.ActorID == "" {
				cached.ActorID = child.Ref().ID()
			}

			resp := statusFromRegistryRecord(cached, true)
			resp.State = detail

			return fn.Ok[RegistryResp](resp)
		}

		record, err := r.cfg.Store.GetRecord(ctx, req.Outpoint)
		if err != nil {
			return fn.Err[RegistryResp](
				fmt.Errorf("get record: %w", err),
			)
		}

		if record != nil {
			cached := cloneRegistryRecord(*record)
			if cached.ActorID == "" {
				cached.ActorID = child.Ref().ID()
			}

			resp := statusFromRegistryRecord(cached, true)
			resp.State = detail

			return fn.Ok[RegistryResp](resp)
		}

		return fn.Ok[RegistryResp](&GetStatusResp{
			Found:   true,
			Active:  true,
			ActorID: child.Ref().ID(),
			Phase:   PhasePending,
			State:   detail,
		})
	}

	if record, ok := r.pending[req.Outpoint]; ok {
		cached := cloneRegistryRecord(record)

		return fn.Ok[RegistryResp](
			statusFromRegistryRecord(cached, false),
		)
	}

	record, err := r.cfg.Store.GetRecord(ctx, req.Outpoint)
	if err != nil {
		return fn.Err[RegistryResp](fmt.Errorf("get record: %w", err))
	}

	if record == nil {
		return fn.Ok[RegistryResp](&GetStatusResp{})
	}

	return fn.Ok[RegistryResp](
		statusFromRegistryRecord(*record, false),
	)
}

// handleTerminated moves the child out of the active map and schedules
// the terminal snapshot for persistence.
//
// The terminal snapshot is assembled from two sources:
//
//  1. The inbound notification itself (Phase, FailReason, SweepTxid).
//
//  2. The cached r.pending record (if any) — same fields overwritten
//     from the notification, but Trigger and ActorID survive so we do
//     not drop the known history.
//
// After the snapshot is built, the child is removed from active and
// stopped only after a queued state probe has drained through it. That
// keeps the registry from synchronously cancelling the child while the
// child is still acking the terminal durable message that notified us.
// The snapshot is then enqueued for async persistence via requestPersist.
// Terminal writes intentionally stay on the async retry path — unlike
// admission, a failed terminal write does not orphan the job (the
// in-memory pending record keeps answering GetStatus), so there is no
// reason to block the registry goroutine on a flaky store.
func (r *registryBehavior) handleTerminated(ctx context.Context,
	req *UnrollTerminatedMsg) fn.Result[RegistryResp] {

	// A terminal failure with no on-chain footprint is recoverable: the
	// VTXO never left off-chain custody, so it can be rolled back to live.
	recoverable := req.Phase == PhaseFailed && !req.HadOnChainFootprint

	record := RegistryRecord{
		TargetOutpoint:     req.Outpoint,
		ActorID:            req.ActorID,
		Phase:              req.Phase,
		FailReason:         req.FailReason,
		SweepTxid:          copyHash(req.SweepTxid),
		RecoverableFailure: recoverable,
	}

	if cached, ok := r.pending[req.Outpoint]; ok {
		record = cloneRegistryRecord(cached)
		record.Phase = req.Phase
		record.FailReason = req.FailReason
		record.SweepTxid = copyHash(req.SweepTxid)
		record.RecoverableFailure = recoverable
		if record.ActorID == "" {
			record.ActorID = req.ActorID
		}
	}

	if child, ok := r.active[req.Outpoint]; ok {
		if record.ActorID == "" {
			record.ActorID = child.Ref().ID()
		}

		// Terminal child drain uses its own bounded cleanup context.
		//nolint:contextcheck
		stopChildAfterDrain(child)
		delete(r.active, req.Outpoint)
	}

	r.pending[req.Outpoint] = cloneRegistryRecord(record)
	// Registry persistence retries are actor-owned follow-up work.
	//nolint:contextcheck
	r.requestPersist(req.Outpoint, 0)

	// The child's terminal message carries its durable exit policy, which
	// outlives r.pending: a completed async persist can evict the cached
	// record before this terminal handoff. Prefer the message's kind so a
	// recovery-only target is still held in exit rather than relived as a
	// live coin (wavelength#602). We only feed it to the manager, not
	// the persisted record: the message has no policy ref, so stamping its
	// kind onto the record would drop the store's (kind, ref) identity.
	policyKind := req.ExitPolicyKind
	if policyKind == "" {
		policyKind = record.ExitPolicyKind
	}

	// Forward the terminal outcome to the VTXO manager so the VTXO's
	// lifecycle tracks the unroll job's terminal on-chain result rather
	// than the user's intent to exit (wavelength#602). The handoff must
	// survive caller-context cancellation, so detach the context. The exit
	// policy rides along so the manager can hold a recovery-only target in
	// exit rather than relive it as a live coin.
	r.notifyVTXOExit(context.WithoutCancel(ctx), req, policyKind)

	return fn.Ok[RegistryResp](&RegistryAckResp{})
}

// notifyVTXOExit forwards a child's terminal outcome to the VTXO manager.
//
//   - PhaseFailed with no on-chain footprint: the unroll never broadcast,
//     so the VTXO is still live from the operator's perspective. Ask the
//     manager to roll it back to live (ExitOutcomeRecoverable).
//   - PhaseCompleted: the exit was swept and confirmed on-chain, so ask the
//     manager to retire the VTXO to spent (ExitOutcomeConfirmed).
//   - PhaseFailed with an on-chain footprint: the exit has begun on-chain;
//     leave the VTXO in unilateral-exit (no notification).
//
// Delivery is best-effort: a failed Tell is logged, not retried. This is the
// fast runtime path; the durable backstop is the VTXO manager's startup
// reconciliation, which re-derives the same outcome from the persisted unroll
// record. The VTXO manager is in-process and the durable unroll record remains
// terminal, so a dropped notification only delays re-convergence until the next
// restart's reconciliation rather than losing funds.
func (r *registryBehavior) notifyVTXOExit(ctx context.Context,
	req *UnrollTerminatedMsg, policyKind ExitPolicyKind) {

	if r.cfg.VTXOExitObserver.IsNone() {
		return
	}
	observer := r.cfg.VTXOExitObserver.UnsafeFromSome()

	var outcome vtxo.ExitOutcome
	switch {
	case req.Phase == PhaseCompleted:
		outcome = vtxo.ExitOutcomeConfirmed

	case req.Phase == PhaseFailed && !req.HadOnChainFootprint:
		outcome = vtxo.ExitOutcomeRecoverable

	default:
		return
	}

	// Carry the exit policy so the manager can tell a recovery-only target
	// (a non-standard policy such as a vHTLC refund) apart from a normal
	// coin and refuse to relive the former on a recoverable failure.
	err := observer.Tell(ctx, &vtxo.ExitOutcomeNotification{
		Outpoint:       req.Outpoint,
		Outcome:        outcome,
		Reason:         req.FailReason,
		ExitPolicyKind: actormsg.ExitPolicyKind(policyKind),
	})
	if err != nil {
		r.log.WarnS(ctx, "Failed to notify VTXO manager of exit "+
			"outcome", err,
			slog.String("outpoint", req.Outpoint.String()),
			slog.String("outcome", outcome.String()),
		)
	}
}

// stopChildAfterDrain stops a terminal child only after a queued status probe
// has had a chance to run behind the currently-processing terminal message.
func stopChildAfterDrain(child *VTXOUnrollActor) {
	if child == nil || child.Ref() == nil {
		return
	}

	go func() {
		defer child.Stop()

		ctx, cancel := context.WithTimeout(
			context.Background(), terminalChildDrainTimeout,
		)
		defer cancel()

		// The probe is intentionally best-effort. If the child is
		// already stopped or stuck, the timeout still guarantees
		// cleanup; when it succeeds, Stop runs after the terminal
		// durable message has committed and the probe itself has
		// been acked.
		_ = child.Ref().Ask(ctx, &GetStateRequest{}).Await(ctx)
	}()
}

// handleRestoreNonTerminal is the daemon's boot entry point for the
// unroll subsystem, dispatched through the registry actor's Receive loop
// so it shares the same goroutine as handleEnsure / handleGetStatus.
//
// Running inside the actor turn is what makes the r.active / r.pending
// mutations below race-free: NewUnrollRegistryActor calls Start() before
// the boot path issues the first restore, so by the time we get here the
// actor may already have processed concurrent Ensure / GetStatus
// messages from the chain resolver or RPC layer.
//
// It reads every record from the durable store that is not already
// Completed or Failed, spawns a fresh VTXOUnrollActor per target, and
// sends ResumeUnrollRequest to each.
//
// The per-target behavior then loads its checkpoint (proof, planner
// state, sweep tx, last height), reconstructs the FSM in the same state
// it left off, and re-arms txconfirm subscriptions for every in-flight
// node and for the sweep (see routeOutbox's Reissue* branches). Thanks
// to txconfirm's txid-keyed dedup, none of this produces on-chain
// duplicates — already-broadcast transactions are absorbed and
// already-confirmed ones return immediately with their status.
//
// When restore fails for an individual target (spawn fails, or the
// resume Ask fails), we leave the durable record non-terminal so that:
//
//   - the next daemon restart will retry the restore from a clean
//     slate when the transient cause is gone (e.g. a chain backend
//     outage that prevented SubscribeBlocks / RegisterSpend on the
//     previous boot), and
//
//   - a fresh EnsureUnrollRequest for the same outpoint within the
//     current boot will attempt an inline restore via handleEnsure
//     (which detects "non-terminal record, no active child" and calls
//     tryRestoreOne).
//
// Marking the record terminal on a transient restore failure would
// strand a recovery-critical job: ListNonTerminalRecords would skip it
// on every subsequent boot and handleEnsure would short-circuit on the
// terminal record. For a VTXO that is in unilateral_exit and near
// expiry, that translates into locked or lost funds — see issue #381.
func (r *registryBehavior) handleRestoreNonTerminal(
	ctx context.Context) fn.Result[RegistryResp] {

	records, err := r.cfg.Store.ListNonTerminalRecords(ctx)
	if err != nil {
		return fn.Ok[RegistryResp](&restoreNonTerminalResp{
			Err: fmt.
				Errorf("list non-terminal records: %w", err).
				Error(),
		})
	}

	if len(records) == 0 {
		return fn.Ok[RegistryResp](&restoreNonTerminalResp{})
	}

	// Fail-secure admission gate: refuse to boot when a persisted
	// non-terminal job names a policy kind no resolver covers. Without
	// this check the registry would silently spawn each affected child,
	// and the first sweep attempt would fail closed with `unknown exit
	// policy kind` — looping every block until an operator notices. The
	// supports check applies only when the configured resolver opts into
	// ResolverKindSupport; resolvers that do not implement the optional
	// interface keep the legacy behaviour for backwards compatibility.
	if err := r.validateRestorableRecords(records); err != nil {
		return fn.Ok[RegistryResp](&restoreNonTerminalResp{
			Err: err.Error(),
		})
	}

	height, err := r.queryBestHeight(ctx)
	if err != nil {
		return fn.Ok[RegistryResp](&restoreNonTerminalResp{
			Err: fmt.
				Errorf("best height for restore: %w", err).
				Error(),
		})
	}

	var restoreErrs []error
	for i := range records {
		record := records[i]
		if _, ok := r.active[record.TargetOutpoint]; ok {
			continue
		}

		child, err := r.tryRestoreOne(ctx, record, height)
		if err != nil {
			// Leave the record non-terminal so the next boot
			// or the next EnsureUnrollRequest can retry. Log
			// loudly: a persistent restore failure is a real
			// problem even though it is recoverable.
			r.log.WarnS(ctx, "Failed to restore unroll job; "+
				"record left non-terminal for retry", err,
				slog.String(
					"outpoint",
					record.TargetOutpoint.String(),
				),
				slog.String("actor_id", record.ActorID),
			)

			restoreErrs = append(
				restoreErrs,
				fmt.Errorf(
					"%s: %w",
					record.TargetOutpoint.String(), err,
				),
			)

			continue
		}

		r.active[record.TargetOutpoint] = child

		// Mirror the historical record into r.pending so
		// handleTerminated can carry over Trigger / ActorID
		// without an extra store lookup, and so handleGetStatus
		// answers from cache while the restored child runs.
		r.pending[record.TargetOutpoint] = cloneRegistryRecord(record)
	}

	if len(restoreErrs) > 0 {
		return fn.Ok[RegistryResp](&restoreNonTerminalResp{
			Err: fmt.Errorf("restore %d unroll job(s): %w",
				len(restoreErrs), errors.Join(restoreErrs...)).
				Error(),
		})
	}

	return fn.Ok[RegistryResp](&restoreNonTerminalResp{})
}

// tryRestoreOne spawns a fresh per-target actor for one non-terminal
// record and sends it a ResumeUnrollRequest. On any error the spawned
// child is stopped and the durable record is left untouched so the
// caller can retry (either via a future EnsureUnrollRequest or on the
// next daemon restart).
func (r *registryBehavior) tryRestoreOne(ctx context.Context,
	record RegistryRecord, height int32) (*VTXOUnrollActor, error) {

	child, err := r.spawn(ctx, record.TargetOutpoint)
	if err != nil {
		return nil, fmt.Errorf("spawn failed on restore: %w", err)
	}

	_, err = child.Ref().Ask(ctx, &ResumeUnrollRequest{
		Height: height,
	}).Await(ctx).Unpack()
	if err != nil {
		child.Stop()

		return nil, fmt.Errorf("resume failed on restore: %w", err)
	}

	return child, nil
}

// handlePersistActiveRecord is half of the two-message pair that drives
// async writes to the control-plane store (the other half is
// handlePersistRecordResult).
//
// The flow exists so the registry goroutine never blocks on a slow
// store: requestPersist Tells the registry a
// persistActiveRecordMsg, which snapshots the latest r.pending view
// synchronously, hands the write off to a goroutine, and returns
// immediately. The goroutine Tells back a persistRecordResultMsg when
// it's done.
//
// Concurrency is controlled by r.persisting, which tracks the exact
// record currently being written for each outpoint. If a new snapshot
// arrives while a write is already in flight, we drop this message: the
// completion handler will see that r.pending has diverged from
// r.persisting and automatically re-enqueue a fresh attempt. This keeps
// the write path strictly serial per outpoint (never overlapping writes
// for the same target) without requiring a lock on the store.
func (r *registryBehavior) handlePersistActiveRecord(ctx context.Context,
	req *persistActiveRecordMsg) fn.Result[RegistryResp] {

	record, ok, err := r.recordForPersistence(ctx, req.Outpoint)
	if err != nil {
		r.log.WarnS(ctx, "Failed to snapshot unroll record for "+
			"persistence", err,
			slog.String("outpoint", req.Outpoint.String()),
			slog.Int("attempt", req.Attempt+1),
		)
		// Store retry timers are owned by the registry after this turn.
		//nolint:contextcheck
		r.schedulePersistRetry(req.Outpoint, req.Attempt+1)

		return fn.Ok[RegistryResp](&RegistryAckResp{})
	}

	if !ok {
		delete(r.persisting, req.Outpoint)

		return fn.Ok[RegistryResp](&RegistryAckResp{})
	}

	// If a persist is already in flight for this outpoint, drop this
	// snapshot; handlePersistRecordResult will re-enqueue from r.pending
	// on completion if the record has diverged in the meantime.
	if _, ok := r.persisting[req.Outpoint]; ok {
		return fn.Ok[RegistryResp](&RegistryAckResp{})
	}

	r.persisting[req.Outpoint] = cloneRegistryRecord(record)
	// Async store writes outlive the registry receive turn by design.
	//nolint:contextcheck
	r.persistRecordAsync(req.Outpoint, req.Attempt, record)

	return fn.Ok[RegistryResp](&RegistryAckResp{})
}

// handlePersistRecordResult reconciles an async store write with the
// current r.pending view. Four cases play out:
//
//   - Write succeeded AND r.pending matches what we wrote: the in-memory
//     cache is now redundant with the store, so drop it. Subsequent
//     GetStatus calls read from the store or from an active child.
//
//   - Write succeeded but r.pending has diverged (a new update came in
//     while the write was in flight): re-enqueue an immediate retry so
//     the newer view catches up.
//
//   - Write failed AND r.pending matches what we tried: schedule a
//     backoff retry (see persistRetryDelay) rather than hot-looping.
//
//   - Write failed but r.pending has already diverged: the newer view
//     is about to try anyway, so just re-enqueue immediately with
//     attempt=0 to reset the backoff.
func (r *registryBehavior) handlePersistRecordResult(ctx context.Context,
	req *persistRecordResultMsg) fn.Result[RegistryResp] {

	delete(r.persisting, req.Outpoint)

	if req.Err == "" {
		record, ok := r.pending[req.Outpoint]
		if ok && sameRegistryRecord(record, req.Record) {
			delete(r.pending, req.Outpoint)
		} else if ok {
			// Registry persistence retries are actor-owned work.
			//nolint:contextcheck
			r.requestPersist(req.Outpoint, 0)
		}

		return fn.Ok[RegistryResp](&RegistryAckResp{})
	}

	err := fmt.Errorf("%s", req.Err)
	r.log.WarnS(ctx, "Failed to persist unroll record",
		err,
		slog.String("outpoint", req.Outpoint.String()),
		slog.Int("attempt", req.Attempt+1),
		slog.String("phase", string(req.Record.Phase)),
	)

	record, ok := r.pending[req.Outpoint]
	if ok && sameRegistryRecord(record, req.Record) {
		// Store retry timers are owned by the registry after this turn.
		//nolint:contextcheck
		r.schedulePersistRetry(req.Outpoint, req.Attempt+1)
	} else if ok {
		// Registry persistence retries are actor-owned follow-up work.
		//nolint:contextcheck
		r.requestPersist(req.Outpoint, 0)
	}

	return fn.Ok[RegistryResp](&RegistryAckResp{})
}

// spawn creates one per-target unroll actor.
func (r *registryBehavior) spawn(ctx context.Context, target wire.OutPoint) (
	*VTXOUnrollActor, error) {

	if r.spawnFunc != nil {
		return r.spawnFunc(ctx, target)
	}

	//nolint:contextcheck // child actor owns its own durable lifecycle
	return NewVTXOUnrollActor(r.childConfig(target))
}

// validateExitPolicyIdentity enforces the kind/ref pair invariant at the
// registry admission boundary so a confused caller (a recovery row pointing
// at a stale ref, or an in-tree caller forgetting to pass either half of the
// pair) is rejected before any durable record is written. Standard timeout
// jobs must have an empty ref; non-standard kinds must have a non-empty ref
// so the resolver has something to load.
func validateExitPolicyIdentity(kind ExitPolicyKind, ref string) error {
	normalised := exitPolicyKind(kind)
	if normalised == StandardVTXOTimeoutExitPolicyKind {
		if ref != "" {
			return fmt.Errorf("standard exit policy kind %q must "+
				"have an empty ref, got %q", normalised, ref)
		}

		return nil
	}

	if ref == "" {
		return fmt.Errorf("non-standard exit policy kind %q requires "+
			"a non-empty ref", normalised)
	}

	return nil
}

// validateRestorableRecords enforces all durable policy identity checks before
// respawning previously admitted records. Admission already validates new
// EnsureUnrollRequest messages, but records loaded from storage may be legacy,
// externally edited, or from a branch that persisted an unsupported custom
// kind.
func (r *registryBehavior) validateRestorableRecords(
	records []RegistryRecord) error {

	for i := range records {
		if err := validateExitPolicyIdentity(
			records[i].ExitPolicyKind, records[i].ExitPolicyRef,
		); err != nil {
			return fmt.Errorf("invalid persisted exit policy "+
				"identity for record %s: %w",
				records[i].TargetOutpoint, err)
		}
	}

	return r.validateResolverCoversRecords(records)
}

// validateResolverCoversRecords refuses to restore non-terminal records when
// the configured resolver cannot reconstruct one of the persisted policy
// kinds. Standard timeout jobs are always handled by the built-in fallback so
// they pass through this check regardless of the configured resolver.
func (r *registryBehavior) validateResolverCoversRecords(
	records []RegistryRecord) error {

	resolver := r.cfg.ExitSpendPolicyResolver
	if resolver == nil {
		resolver = standardExitSpendPolicyResolver{}
	}
	support, ok := resolver.(ResolverKindSupport)
	if !ok {
		return nil
	}

	seen := make(map[ExitPolicyKind]struct{}, len(records))
	for i := range records {
		kind := exitPolicyKind(records[i].ExitPolicyKind)
		if kind == StandardVTXOTimeoutExitPolicyKind {
			continue
		}
		if _, ok := seen[kind]; ok {
			continue
		}
		seen[kind] = struct{}{}

		if support.SupportsKind(kind) {
			continue
		}

		return fmt.Errorf("no resolver registered for persisted exit "+
			"policy kind %q (record %s)", kind,
			records[i].TargetOutpoint)
	}

	return nil
}

// childConfig builds the Config a freshly spawned VTXOUnrollActor will run
// with. Factored out of spawn() so callers can verify the resolver and other
// per-target wiring are forwarded from RegistryConfig without having to stand
// up a durable mailbox.
func (r *registryBehavior) childConfig(target wire.OutPoint) Config {
	return Config{
		TargetOutpoint:              target,
		DeliveryStore:               r.cfg.DeliveryStore,
		ProofAssembler:              r.cfg.ProofAssembler,
		VTXOStore:                   r.cfg.VTXOStore,
		TxConfirmRef:                r.cfg.TxConfirmRef,
		ChainSource:                 r.cfg.ChainSource,
		Wallet:                      r.cfg.Wallet,
		LedgerSink:                  r.cfg.LedgerSink,
		Log:                         r.cfg.Log,
		MaxSweepFeeRateSatPerVByte:  r.cfg.MaxSweepFeeRateSatPerVByte,
		ExitSpendPolicyResolver:     r.cfg.ExitSpendPolicyResolver,
		FraudCheckpointSafetyMargin: r.cfg.FraudCheckpointSafetyMargin,
		RegistryRef:                 r.selfRef,
	}
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

// detailedChildState returns the live child's planner-derived state for a
// detailed status probe, or nil for a coarse probe or on any child-Ask
// failure. It is deliberately best-effort: the caller always has the coarse
// registry record to fall back on, and a status probe must never fail just
// because the child was momentarily busy. Only a detailed probe Asks the
// child, so the common polling path never writes a read-only mailbox row.
func (r *registryBehavior) detailedChildState(ctx context.Context,
	req *GetStatusRequest, child *VTXOUnrollActor) *GetStateResp {

	if !req.Detailed {
		return nil
	}

	// Bound the child Ask with a local timeout. The registry runs on a
	// single goroutine, and the caller's context is the (possibly
	// deadline-free) RPC context, so a child wedged in a redelivery-retry
	// loop would otherwise stall the whole registry -- admission,
	// persistence, and every other outpoint's status -- for as long as the
	// caller waits. On timeout we fall back to the coarse record.
	askCtx, cancel := context.WithTimeout(ctx, detailedStatusAskTimeout)
	defer cancel()

	state, err := r.childState(askCtx, child)
	if err != nil {
		r.log.DebugS(ctx, "detailed child state unavailable",
			slog.String("outpoint", req.Outpoint.String()),
			slog.String("err", err.Error()),
		)

		return nil
	}

	return state
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
		return nil, fmt.Errorf("unexpected child state response %T",
			resp)
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
		return RegistryRecord{}, false, fmt.Errorf("read child "+
			"state: %w", err)
	}

	record := recordFromChildState(target, child.Ref().ID(), state)

	return record, true, nil
}

// persistRecordAsync writes one record on a background goroutine and reports
// the result back to the registry actor.
func (r *registryBehavior) persistRecordAsync(target wire.OutPoint, attempt int,
	record RegistryRecord) {

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
func (r *registryBehavior) requestPersist(target wire.OutPoint, attempt int) {
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

// persistRetryDelay computes exponential backoff for persistence
// retries, clamped at maxPersistRetryDelay. The first retry waits
// initialPersistRetryDelay (250ms) and each subsequent attempt doubles
// up to the cap (5s). This is deliberately conservative: terminal
// records are idempotent and not time-sensitive, so we favor low store
// pressure over fast convergence.
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
		ExitPolicyKind: exitPolicyKind(state.ExitPolicyKind),
		ExitPolicyRef:  state.ExitPolicyRef,
		Phase:          state.Phase,
		FailReason:     state.FailReason,
		SweepTxid:      copyHash(state.SweepTxid),
	}
}

// statusFromRegistryRecord converts one cached registry record into a status
// response.
func statusFromRegistryRecord(record RegistryRecord,
	active bool) *GetStatusResp {

	return &GetStatusResp{
		Found:          true,
		Active:         active,
		ActorID:        record.ActorID,
		Phase:          record.Phase,
		Trigger:        record.Trigger,
		ExitPolicyKind: record.ExitPolicyKind,
		ExitPolicyRef:  record.ExitPolicyRef,
		FailReason:     record.FailReason,
		SweepTxid:      copyHash(record.SweepTxid),
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
		a.ExitPolicyKind != b.ExitPolicyKind ||
		a.ExitPolicyRef != b.ExitPolicyRef ||
		a.Phase != b.Phase ||
		a.FailReason != b.FailReason ||
		a.RecoverableFailure != b.RecoverableFailure {
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

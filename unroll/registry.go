//nolint:ll
package unroll

import (
	"context"
	"errors"
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
	// childAdmissionTimeout bounds the registry's synchronous attempt to
	// start a newly-admitted child before falling back to durable retry.
	childAdmissionTimeout = 30 * time.Second

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
	GetRecord(ctx context.Context,
		target wire.OutPoint) (*RegistryRecord, error)

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

	// JobStore provides SQL job persistence for child actors.
	JobStore JobStore

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

	// FraudCheckpointSafetyMargin overrides the default backstop
	// margin (in blocks) the recipient subtracts from the relative
	// expiry when deciding to self-broadcast a fraud-triggered
	// job. Zero applies defaultFraudCheckpointSafetyMargin;
	// the effective margin is always clamped to csvDelay/2 when
	// csvDelay is too small to absorb the configured value. Plumbed
	// into every spawned VTXOUnrollActor and from there into the
	// FSM Environment.
	FraudCheckpointSafetyMargin int32
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

	case *replayUnrollEffectMsg:
		return r.handleReplayEffect(ctx, req)

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
	if record, ok := r.pending[req.Outpoint]; ok {
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
		return fn.Ok[RegistryResp](&EnsureUnrollResp{
			ActorID: existing.ActorID,
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

	record := RegistryRecord{
		TargetOutpoint: req.Outpoint,
		ActorID:        child.Ref().ID(),
		Trigger:        req.Trigger,
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
		Height:  height,
		Trigger: req.Trigger,
	}
	startCtx, cancelStart := context.WithTimeout(
		context.WithoutCancel(ctx), childAdmissionTimeout,
	)
	defer cancelStart()

	_, err = child.Ref().Ask(startCtx, startReq).Await(startCtx).Unpack()
	if err != nil {
		// Two failure classes here, treated very differently:
		//
		//   1. Cancellation race — the admission context (or the
		//      child's own actor context) ended before the child
		//      committed its first message. The pending row is already
		//      durable, so re-issuing the StartUnrollRequest via a
		//      fire-and-forget Tell hands the work to the child actor.
		//      Caller still sees Created=true because the job IS
		//      admitted; the FSM will catch up from the registry row and
		//      job store.
		//
		//   2. Real start error — proof assembly, store, planner.
		//      Hide it under a Created=true would silently strand
		//      the user's funds in unroll with no progress.
		//      We mark the durable row PhaseFailed so GetUnrollStatus
		//      surfaces a terminal status instead of "not found".
		if isCancellationRace(err) {
			tellErr := child.Ref().Tell(
				context.WithoutCancel(ctx), startReq,
			)
			if tellErr != nil {
				r.failAdmittedChild(
					ctx, req.Outpoint, child,
					fmt.Errorf("requeue start child: %w",
						tellErr),
				)

				return fn.Err[RegistryResp](
					fmt.Errorf("start child: %w", err),
				)
			}

			r.log.WarnS(ctx, "Requeued unroll child start "+
				"after admission context ended", err,
				slog.String("outpoint", req.Outpoint.String()),
				slog.String("child_id", child.Ref().ID()),
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
			slog.String("child_id", child.Ref().ID()),
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
			slog.String("child_id", child.Ref().ID()),
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

// failAdmittedChild records a terminal failure for a child that already has
// a durable pending row. This keeps GetUnrollStatus from falling back to
// "not found" after VTXO ownership has moved to unilateral exit.
func (r *registryBehavior) failAdmittedChild(ctx context.Context,
	target wire.OutPoint, child *VTXOUnrollActor, err error) {

	child.Stop()
	delete(r.active, target)

	record := RegistryRecord{
		TargetOutpoint: target,
		ActorID:        child.Ref().ID(),
		Phase:          PhaseFailed,
		FailReason:     err.Error(),
	}
	if pending, ok := r.pending[target]; ok {
		record = cloneRegistryRecord(pending)
		record.Phase = PhaseFailed
		record.FailReason = err.Error()
	}

	r.pending[target] = cloneRegistryRecord(record)

	markErr := r.cfg.Store.MarkTerminal(
		context.WithoutCancel(ctx), target, PhaseFailed, err.Error(),
		nil,
	)
	if markErr != nil {
		r.log.WarnS(ctx, "Failed to mark admitted unroll child "+
			"terminal", markErr,
			slog.String("outpoint", target.String()),
			slog.String("child_id", child.Ref().ID()),
		)
		// Registry persistence retries are actor-owned follow-up work.
		//nolint:contextcheck
		r.requestPersist(target, 0)
	}
}

// isCancellationRace reports whether admission should preserve the pending
// row and retry instead of converting the job into a deterministic failure.
//
// Three error classes count as the same "lifecycle ended too early"
// signal:
//
//   - context.Canceled: the admission ctx (RPC ctx via WithoutCancel +
//     WithTimeout) reached its bound while the child was still
//     processing; the pending row already survives so retry is safe.
//
//   - context.DeadlineExceeded: same shape as Canceled but driven by the
//     WithTimeout cap rather than an explicit cancel. Same handoff.
//
//   - actor.ErrActorTerminated: the child actor's own ctx ended (e.g.
//     during a fast-shutdown race or a follow-on Stop). The durable
//     mailbox still holds the message, so RestoreNonTerminal on the
//     next boot will respawn the actor and re-process the persisted
//     StartUnrollRequest.
//
// All three are recoverable via the durable retry path; treating them
// as terminal failure would strand the VTXO in unroll with no
// surviving registry record after eviction.
func isCancellationRace(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, actor.ErrActorTerminated)
}

// handleGetStatus answers a status probe from the registry's cached
// control-plane view instead of asking the child actor.
//
// The child state request is read-only, but polling clients can still leave
// stale GetStateRequest work behind after their RPC context expires, and those
// reads can starve progress notifications during block-mining-heavy tests. The
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
		if record, ok := r.pending[req.Outpoint]; ok {
			cached := cloneRegistryRecord(record)
			if cached.ActorID == "" {
				cached.ActorID = child.Ref().ID()
			}

			return fn.Ok[RegistryResp](
				statusFromRegistryRecord(cached, true),
			)
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

			return fn.Ok[RegistryResp](
				statusFromRegistryRecord(cached, true),
			)
		}

		return fn.Ok[RegistryResp](&GetStatusResp{
			Found:   true,
			Active:  true,
			ActorID: child.Ref().ID(),
			Phase:   PhasePending,
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

	return fn.Ok[RegistryResp](&RegistryAckResp{})
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

// restoreNonTerminal is the daemon's boot entry point for the unroll
// subsystem. It reads every record from the durable store that is not
// already Completed or Failed, spawns a fresh VTXOUnrollActor per
// target, and sends ResumeUnrollRequest to each.
//
// The per-target behavior then loads its job (proof, planner
// state, sweep tx, last height), reconstructs the FSM in the same state
// it left off, and re-arms txconfirm subscriptions for every in-flight
// node and for the sweep (see routeOutbox's Reissue* branches). Thanks
// to txconfirm's txid-keyed dedup, none of this produces on-chain
// duplicates — already-broadcast transactions are absorbed and
// already-confirmed ones return immediately with their status.
//
// When restore fails for an individual target (spawn fails, or the
// resume Ask fails), we mark that target terminal with PhaseFailed and
// a descriptive reason rather than leaving the store entry non-terminal
// forever. A fresh Ensure from the chain resolver can then try again
// with a clean slate if the cause is transient.
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

func (r *registryBehavior) handleReplayEffect(ctx context.Context,
	req *replayUnrollEffectMsg) fn.Result[RegistryResp] {

	record, err := r.cfg.Store.GetRecord(ctx, req.Outpoint)
	if err != nil {
		return fn.Err[RegistryResp](
			fmt.Errorf("lookup unroll effect target: %w", err),
		)
	}
	if record == nil {
		return fn.Err[RegistryResp](
			fmt.Errorf("unroll effect target %s not found",
				req.Outpoint),
		)
	}
	if record.IsTerminal() {
		return fn.Ok[RegistryResp](&RegistryAckResp{})
	}

	height, err := r.queryBestHeight(ctx)
	if err != nil {
		return fn.Err[RegistryResp](
			fmt.Errorf("best height for effect replay: %w", err),
		)
	}

	child, ok := r.active[req.Outpoint]
	if !ok {
		child, err = r.spawn(ctx, req.Outpoint)
		if err != nil {
			return fn.Err[RegistryResp](
				fmt.Errorf("spawn effect target: %w", err),
			)
		}
		r.active[req.Outpoint] = child
	}

	_, err = child.Ref().Ask(ctx, &ResumeUnrollRequest{
		Height: height,
	}).Await(ctx).Unpack()
	if err != nil {
		return fn.Err[RegistryResp](
			fmt.Errorf("resume effect target: %w", err),
		)
	}

	return fn.Ok[RegistryResp](&RegistryAckResp{})
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

	//nolint:contextcheck // child actor owns its own lifecycle
	return NewVTXOUnrollActor(Config{
		TargetOutpoint:              target,
		JobStore:                    r.cfg.JobStore,
		ProofAssembler:              r.cfg.ProofAssembler,
		VTXOStore:                   r.cfg.VTXOStore,
		TxConfirmRef:                r.cfg.TxConfirmRef,
		ChainSource:                 r.cfg.ChainSource,
		Wallet:                      r.cfg.Wallet,
		Log:                         r.cfg.Log,
		MaxSweepFeeRateSatPerVByte:  r.cfg.MaxSweepFeeRateSatPerVByte,
		FraudCheckpointSafetyMargin: r.cfg.FraudCheckpointSafetyMargin,
		RegistryRef:                 r.selfRef,
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
		Found:      true,
		Active:     active,
		ActorID:    record.ActorID,
		Phase:      record.Phase,
		Trigger:    record.Trigger,
		FailReason: record.FailReason,
		SweepTxid:  copyHash(record.SweepTxid),
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

package unroll

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/lightninglabs/wavelength/unrollplan"
	"github.com/lightninglabs/wavelength/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// Config configures one durable per-target VTXO unroll actor.
type Config struct {
	// TargetOutpoint is the VTXO being unrolled.
	TargetOutpoint wire.OutPoint

	// ActorID is the durable actor mailbox ID. When empty it
	// falls back to a deterministic ID derived from the target.
	ActorID string

	// DeliveryStore provides durable mailbox and checkpoint persistence.
	DeliveryStore actor.DeliveryStore

	// ProofAssembler resolves the immutable local proof for the target.
	ProofAssembler ProofAssembler

	// VTXOStore loads the descriptor used for final sweep signing.
	VTXOStore vtxo.VTXOStore

	// TxConfirmRef is the shared tx-confirmation actor.
	TxConfirmRef actor.ActorRef[txconfirm.Msg, txconfirm.Resp]

	// ChainSource provides fee estimation for sweep construction.
	ChainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]

	// ExitSpendPolicyResolver reconstructs the exit policy for the durable
	// kind/ref stored on the unroll job. If nil, the actor uses the
	// built-in standard VTXO timeout resolver.
	ExitSpendPolicyResolver ExitSpendPolicyResolver

	// Wallet provides sweep destination derivation and
	// timeout-path signing.
	Wallet SweepWallet

	// Log is an optional logger.
	Log fn.Option[btclog.Logger]

	// MaxSweepFeeRateSatPerVByte clamps pathological fee estimates.
	MaxSweepFeeRateSatPerVByte int64

	// FraudCheckpointSafetyMargin overrides the recipient backstop
	// margin (in blocks) for fraud-triggered unroll jobs. Zero falls
	// back to defaultFraudCheckpointSafetyMargin. Plumbed onto the
	// FSM Environment.
	FraudCheckpointSafetyMargin int32

	// RegistryRef receives terminal notifications from this actor when set.
	RegistryRef actor.TellOnlyRef[RegistryMsg]

	// LedgerSink receives the confirmed on-chain exit fee once the
	// final sweep has confirmed.
	LedgerSink fn.Option[ledger.Sink]

	// ChainReconcilerFactory, when set, is invoked after the actor
	// loads its proof to construct a ChainReconciler that reconciles
	// the persisted checkpoint against the canonical chain at
	// startup. Anchors that are no longer live on chain (typically
	// reorged out while the daemon was offline) are pruned before
	// the FSM session is built so the actor does not broadcast a
	// sweep on top of stale planner state.
	//
	// The factory shape lets the production implementation
	// (NewChainSourceReconciler) bind to the actor's proof for
	// pkScript lookups while still letting unit tests inject a stub
	// that ignores the proof. When None the actor skips
	// reconciliation entirely.
	ChainReconcilerFactory fn.Option[ChainReconcilerFactory]
}

// VTXOUnrollActor wraps one durable per-target unroll actor.
type VTXOUnrollActor struct {
	ref     actor.ActorRef[Msg, Resp]
	durable *actor.DurableActor[Msg, Resp]
	stop    func()
}

// Ref returns the public actor reference.
func (a *VTXOUnrollActor) Ref() actor.ActorRef[Msg, Resp] {
	return a.ref
}

// Stop stops the underlying durable actor.
func (a *VTXOUnrollActor) Stop() {
	if a == nil {
		return
	}

	if a.stop != nil {
		a.stop()

		return
	}

	if a.durable != nil {
		a.durable.Stop()
	}
}

// NewVTXOUnrollActor creates and starts one durable VTXO unroll actor.
func NewVTXOUnrollActor(cfg Config) (*VTXOUnrollActor, error) {
	if cfg.ActorID == "" {
		cfg.ActorID = actorIDForTarget(cfg.TargetOutpoint)
	}

	behavior := &behavior{
		cfg: cfg,
		log: cfg.Log.UnwrapOr(btclog.Disabled),
	}
	if err := behavior.restoreCheckpoint(context.Background()); err != nil {
		return nil, err
	}

	durableCfg := actor.DefaultDurableTxActorConfig[Msg, Resp, unrollTx](
		cfg.ActorID, behavior, behavior.bindStores, cfg.DeliveryStore,
		newCodec(),
	)
	durableCfg.Log = cfg.Log

	durable, err := actor.NewDurableActor(durableCfg).Unpack()
	if err != nil {
		return nil, err
	}
	behavior.selfRef = durable.TellRef()
	durable.Start()

	return &VTXOUnrollActor{
		ref:     durable.Ref(),
		durable: durable,
		stop:    durable.Stop,
	}, nil
}

// behavior is the durable actor behavior for one target outpoint.
type behavior struct {
	cfg     Config
	log     btclog.Logger
	selfRef actor.TellOnlyRef[Msg]

	proof   *recovery.Proof
	planner *unrollplan.Planner
	desc    *vtxo.Descriptor
	session *Session
	pending *actorCheckpoint

	sweepTx           *wire.MsgTx
	blockSubActive    bool
	spendWatchActive  bool
	proofSpendWatches map[wire.OutPoint]struct{}
	terminalNotified  bool
	exitCostNotified  bool

	// requiredLockTime caches the exit policy's absolute nLockTime once the
	// policy has been resolved (in startSweep). The standard timeout policy
	// reports zero; a vHTLC refund-without-receiver policy reports the
	// height the sweep must wait for. It feeds the best-case block estimate
	// so a policy-gated sweep is not reported as imminent. None until the
	// sweep phase resolves the policy.
	requiredLockTime fn.Option[uint32]

	// reconciled records that the restored checkpoint has been
	// reconciled against the canonical chain via cfg.ChainReconciler.
	// Reconciliation runs exactly once per actor lifetime, on the
	// first ensureLoaded call, before the FSM session is bound.
	reconciled bool

	// sweepFinalized latches true after a TxFinalizedMsg arrives for
	// the txid currently recorded as the sweep in PlannerState. While
	// false, PhaseCompleted is treated as PROVISIONAL: the registry is
	// NOT told the actor has terminated, so a reorg of the sweep
	// confirmation still has a live actor to deliver the rollback to.
	// Once sweepFinalized is true the next notifyRegistryOfTerminal
	// call fires UnrollTerminatedMsg and the registry evicts the
	// child.
	sweepFinalized bool
}

// unrollTx is the transaction-scoped store handed to the unroll behavior inside
// each Read/Stage/Commit phase. The unroll actor's only durable write is its
// checkpoint, so the store is thin: it carries the actor ID plus the per-call
// DeliveryStore whose SaveCheckpoint/LoadCheckpoint join the framework
// transaction via the closure context.
type unrollTx struct {
	store   actor.DeliveryStore
	actorID string
}

// bindStores is the StoreFactory for the unroll Read/Stage/Commit path. It must
// thread the per-call DeliveryStore -- the handle SaveCheckpoint reads the
// active *sql.Tx off of -- rather than a captured one, so every checkpoint
// write lands in the transaction the framework opened for that phase.
func (b *behavior) bindStores(_ context.Context,
	ds actor.DeliveryStore) unrollTx {

	return unrollTx{
		store:   ds,
		actorID: b.cfg.ActorID,
	}
}

// behavior runs on the durable actor Read/Commit execution path: it persists
// each checkpoint with a short, lock-releasing Stage ahead of the txconfirm IO
// and consumes the message with one lease-fenced Commit, so the SQLite writer
// is never held across a cross-actor Ask.
var _ actor.TxBehavior[Msg, Resp, unrollTx] = (*behavior)(nil)

// Receive processes one durable actor message. It is the single entry
// point for every input that drives the unroll FSM: admission, restart,
// chain events, txconfirm notifications, external spends, and status
// probes. The job of Receive is purely translation — it maps the durable
// message surface onto the internal FSM [Event] surface and delegates to
// handleEvent, which runs the apply-persist-route-notify pipeline.
//
// Inputs fall into four groups:
//
//   - Admission (StartUnrollRequest, ResumeUnrollRequest): open a new
//     session or continue a restored one. Trigger is propagated so the
//     control-plane knows whether we started manually, near expiry, or
//     on restart.
//
//   - Chain observations (HeightObservedMsg, TxConfirmedMsg, TxFailedMsg,
//     SpendObservedMsg): progress the planner. TxFailedMsg reasons are
//     annotated with proof-vs-sweep context here so FSM terminal reasons
//     read usefully later. SpendObservedMsg has its own handler because
//     it needs to classify the spender before deciding whether to fail.
//
//   - Status probes (GetStateRequest): read-only, bypass the FSM apply
//     path entirely, just snapshot the current state.
//
// Unknown messages are rejected with a typed error rather than silently
// dropped so codec/dispatch mismatches are loud.
func (b *behavior) Receive(ctx context.Context, msg Msg,
	ax actor.Exec[unrollTx]) fn.Result[Resp] {

	// GetStateRequest is a read-only status probe: it drives no FSM
	// transition and writes nothing, so it returns directly without a Stage
	// or a Commit. The framework's non-transactional tail acks the Ask once
	// the behavior returns.
	if _, ok := msg.(*GetStateRequest); ok {
		return fn.Ok[Resp](b.stateResponse(ctx))
	}

	// Run the FSM pipeline. Every checkpoint write inside is a short,
	// lock-releasing Stage and the slow txconfirm IO runs with no writer
	// transaction held; dispatch never commits.
	res := b.dispatch(ctx, ax, msg)
	if res.IsErr() {

		// The behavior did not commit, so the framework nacks the
		// message; it is redelivered and replayed against the durably
		// Staged state.
		return res
	}

	// Single consume point: fold the lease-fenced ack, the dedup mark, and
	// a final checkpoint re-persist into one short writer transaction. A
	// lost lease surfaces here as actor.ErrLeaseLost and the Staged
	// checkpoints survive for an idempotent replay.
	if err := b.commitAck(ctx, ax); err != nil {
		return fn.Err[Resp](err)
	}

	return res
}

// dispatch maps one durable message onto the FSM event surface and runs the
// apply-stage-route pipeline (possibly recursively). It performs every Stage
// write and all IO but never commits: Receive owns the single lease-fenced
// Commit so the message is consumed exactly once after the whole pipeline
// settles. Unknown messages are rejected with a typed error rather than
// silently dropped so codec/dispatch mismatches are loud.
func (b *behavior) dispatch(ctx context.Context, ax actor.Exec[unrollTx],
	msg Msg) fn.Result[Resp] {

	switch m := msg.(type) {
	case *StartUnrollRequest:
		return b.handleEvent(ctx, ax, &StartEvent{
			Height:         m.Height,
			Trigger:        m.Trigger,
			ExitPolicyKind: m.ExitPolicyKind,
			ExitPolicyRef:  m.ExitPolicyRef,
		})

	case *ResumeUnrollRequest:
		return b.handleEvent(ctx, ax, &ResumeEvent{
			Height: m.Height,
		})

	case *HeightObservedMsg:
		return b.handleEvent(ctx, ax, &HeightUpdatedEvent{
			Height: m.Height,
		})

	case *TxConfirmedMsg:
		return b.handleEvent(ctx, ax, &TxConfirmedEvent{
			Txid:   m.Txid,
			Height: m.Height,
		})

	case *TxFailedMsg:
		return b.handleEvent(ctx, ax, &TxFailedEvent{
			Txid:   m.Txid,
			Reason: b.failureReasonForTx(m.Txid, m.Reason),
		})

	case *TxReorgedMsg:
		return b.handleEvent(ctx, ax, &TxReorgedEvent{
			Txid: m.Txid,
		})

	case *TxFinalizedMsg:
		// Latch sweepFinalized BEFORE handleEvent so the
		// notifyRegistry call inside driveEvent sees the
		// post-finalization view and fires UnrollTerminatedMsg
		// instead of holding the actor in provisional Completed.
		if err := b.ensureLoaded(ctx); err != nil {
			return fn.Err[Resp](err)
		}
		b.maybeLatchSweepFinalized(m.Txid)

		return b.handleEvent(ctx, ax, &TxFinalizedEvent{
			Txid: m.Txid,
		})

	case *SpendObservedMsg:
		return b.handleSpendObserved(ctx, ax, m)

	case *SpendReorgedMsg:
		return b.handleEvent(ctx, ax, &SpendReorgedEvent{})

	case *SpendFinalizedMsg:
		return b.handleEvent(ctx, ax, &SpendFinalizedEvent{})

	// GetStateRequest is intentionally NOT handled here: Receive
	// short-circuits it before it ever reaches dispatch (it is a read-only
	// probe that drives no FSM transition and writes nothing). A
	// GetStateRequest can only arrive at this switch via a future refactor
	// that breaks that invariant, in which case the default arm's loud
	// error is the signal we want -- not a silently duplicated read path.
	default:
		return fn.Err[Resp](
			fmt.Errorf("unknown unroll message: %T", msg),
		)
	}
}

// OnStop stops any loaded protofsm session.
func (b *behavior) OnStop(ctx context.Context) error {
	b.unsubscribeBlocks(ctx)
	b.unregisterSpendWatch(ctx)
	b.unregisterProofSpendWatches(ctx)

	if b.session != nil && b.session.FSM != nil {
		b.session.FSM.Stop()
	}

	return nil
}

// handleEvent is the admission wrapper around driveEvent that every
// FSM-driving branch of Receive funnels through. It exists so the lazy
// load of proof / descriptor / planner / session / subscriptions happens
// exactly once per actor lifetime regardless of which event kind arrives
// first, and so the caller sees a uniform AckResp on success.
func (b *behavior) handleEvent(ctx context.Context, ax actor.Exec[unrollTx],
	event Event) fn.Result[Resp] {

	if err := b.ensureLoaded(ctx); err != nil {
		return fn.Err[Resp](err)
	}

	if b.inTerminalState() {
		// The FSM is already terminal — either we reached this
		// state inside the current actor lifetime and have nothing
		// new to do, or we just restored from a terminal checkpoint
		// on boot and the registry's view is still the older
		// non-terminal record. notifyRegistryIfTerminal is a no-op
		// in the first case (terminalNotified short-circuits it)
		// and the load-bearing call in the second: without it, the
		// restored child sits in r.active forever while
		// handleGetStatus serves the stale non-terminal record from
		// r.pending. terminalNotified is in-memory and resets on
		// every restart, so a restored terminal checkpoint always
		// drives one notification through.
		b.notifyRegistryIfTerminal(ctx)

		return fn.Ok[Resp](&AckResp{})
	}

	if err := b.driveEvent(ctx, ax, event); err != nil {
		return fn.Err[Resp](err)
	}

	return fn.Ok[Resp](&AckResp{})
}

// driveEvent runs the core four-step pipeline that every FSM transition
// must go through:
//
//  1. Apply: hand the event to the protofsm, which computes the next
//     state and any actor-boundary OutboxEvents. The FSM itself does no
//     IO; this call returns synchronously with the new state persisted
//     in the FSM session.
//
//  2. Stage: write the resulting checkpoint to the delivery store in a
//     short, non-fenced writer transaction (ax.Stage) BEFORE doing any IO
//     on the outbox. The writer lock is released the moment Stage returns,
//     so the txconfirm IO that follows never holds it. If the process
//     crashes between the Stage and the outbox routing, restart restores
//     the exact same state that was in memory and re-emits the outbox via
//     the reissue path, so no work is lost and no work is duplicated
//     beyond what txconfirm's txid-keyed dedup already collapses. The
//     message itself is acked only once, by the single lease-fenced Commit
//     in Receive after the whole (possibly recursive) pipeline settles.
//
//  3. Route: interpret each OutboxEvent as a real IO effect — submit
//     ready proof nodes to txconfirm, re-arm in-flight subscriptions,
//     build and broadcast the sweep, or reattach a sweep-confirmation
//     watcher. Handler-level errors (and any events driven inside
//     routeOutbox, e.g. TxFailedEvent from an immediate rejection) can
//     recursively re-enter driveEvent.
//
//  4. Notify: if the transition reached Completed or Failed, tell the
//     registry exactly once so it can move the child out of the active
//     map and mark the DB row terminal. Subsequent transitions (e.g. a
//     late TxConfirmed after we already failed) are suppressed by
//     terminalNotified.
//
// Stage-before-route is the invariant that lets the actor lose its
// process mid-operation without corrupting on-chain state — every side
// effect is driven by a checkpoint that is already on disk, durably Staged
// in its own short transaction ahead of the IO.
func (b *behavior) driveEvent(ctx context.Context, ax actor.Exec[unrollTx],
	event Event) error {

	if b.session == nil || b.session.FSM == nil {
		return fmt.Errorf("session not initialized")
	}

	outbox, err := b.session.FSM.AskEvent(ctx, event).Await(ctx).Unpack()
	if err != nil {
		return err
	}

	if err := b.persistCheckpoint(ctx, ax); err != nil {
		return err
	}

	if err := b.routeOutbox(ctx, ax, outbox); err != nil {
		return err
	}

	b.notifyRegistryIfTerminal(ctx)

	return nil
}

// startSweep constructs the final timeout-path sweep, persists it, and
// hands it to txconfirm for broadcast-and-wait-for-confirmation.
//
// Ordering here is load-bearing. The guiding rule is: never cross the
// actor-boundary with a fresh sweep that the checkpoint has not yet
// seen. Three consequences of breaking that rule make the order
// non-obvious:
//
//  1. The sweep destination comes from a new BIP32 wallet address. A
//     second, freshly-derived sweep after a retry burns a new address
//     and races the first on chain. If both land we leak data about the
//     wallet's key derivation.
//
//  2. txconfirm dedups by txid. If the re-submitted sweep has a
//     different txid (even one output byte differs: new pkScript, new
//     fee) the dedup misses and we double-broadcast.
//
//  3. A crash between "sign new sweep" and "broadcast new sweep" that
//     happens AFTER the on-chain broadcast of an earlier attempt means
//     restart has no trail of the broadcast sweep at all — it would
//     build a third sweep.
//
// The fix is to reuse b.sweepTx (possibly restored from the checkpoint
// via restoreCheckpoint) when it is already set, and to persist the
// checkpoint BEFORE asking txconfirm to broadcast. On restart the same
// transaction materializes from the checkpoint, txconfirm sees the same
// txid it has been tracking, and the Ask resolves as a benign no-op.
//
// If buildSweepTx itself fails (fee estimation, signing, malformed
// descriptor), we drive a SweepBuildFailedEvent through the FSM so the
// retry budget is accounted for and we reach terminal Failed after
// maxSweepAttempts.
func (b *behavior) startSweep(ctx context.Context,
	ax actor.Exec[unrollTx]) error {

	// Idempotency guard against a lost-in-memory sweep. Before deriving a
	// fresh sweep -- which burns a new BIP32 wallet address and yields a
	// new txid -- reconcile against the durably Staged checkpoint via a
	// short read-only snapshot. If a previous processing attempt already
	// Staged a sweepTx but crashed before its Commit acked, the message is
	// being replayed; adopting the Staged sweepTx keeps us on a single
	// sweep txid / pkScript instead of racing a freshly-derived sweep on
	// chain. restoreCheckpoint repopulates b.sweepTx at boot, so this only
	// fires on an in-memory/durable desync, but it makes the
	// persist-before-broadcast guarantee robust to in-memory state loss
	// rather than dependent on the boot path alone.
	if b.sweepTx == nil {
		if err := b.adoptStagedSweep(ctx, ax); err != nil {
			return err
		}
	}

	// Reuse the sweep tx restored from the checkpoint (or built on a
	// prior attempt inside this actor lifetime) so we converge on a
	// single sweep txid / wallet pkScript across retries.
	if b.sweepTx == nil {
		policy, err := b.resolveExitSpendPolicy(ctx)
		if err != nil {
			return b.driveEvent(ctx, ax, &SweepBuildFailedEvent{
				Reason: err.Error(),
			})
		}

		// Cache the policy's absolute locktime so the read-only status
		// probe can fold it into the best-case block estimate without
		// re-resolving the policy. The value is immutable per job.
		b.requiredLockTime = fn.Some(policy.RequiredLockTime())

		b.log.InfoS(ctx, "Building unroll exit spend",
			slog.String(
				"target_outpoint",
				b.cfg.TargetOutpoint.String(),
			),
			slog.String(
				"exit_policy_kind", policy.Kind().String(),
			),
			slog.Uint64("csv_delay", uint64(policy.CSVDelay())),
		)

		// Defense in depth around the planner: when the policy
		// declares a CSV delay larger than the wrapping proof's
		// descriptor delay, the planner would have signalled
		// NeedSweep too early and the resulting BIP-68 sequence
		// would be rejected as non-final. Surface this as a
		// permanent misconfiguration so the retry budget marks the
		// job Failed instead of looping forever broadcasting an
		// invalid tx.
		if b.proof != nil &&
			policy.CSVDelay() > b.proof.CSVDelay() {
			return b.driveEvent(ctx, ax, &SweepBuildFailedEvent{
				Reason: fmt.Sprintf("policy csv delay %d "+
					"exceeds proof csv delay %d",
					policy.CSVDelay(),
					b.proof.CSVDelay()),
			})
		}

		// Defer the build entirely when the policy requires an
		// absolute locktime the chain has not yet reached. We do
		// NOT drive SweepBuildFailedEvent here: the next
		// HeightObservedMsg re-evaluates the FSM, re-emits
		// RequestSweepBuild, and this branch will pass once
		// height catches up. Returning nil keeps the FSM in
		// AwaitingSweepBroadcast without burning a retry attempt
		// or a wallet pkScript that the not-yet-final tx would
		// otherwise consume.
		if locktime := policy.RequiredLockTime(); locktime > 0 &&
			uint32(b.currentHeight()) < locktime {

			b.log.DebugS(ctx,
				"Deferring exit spend build: locktime "+
					"not matured",
				slog.String(
					"target_outpoint",
					b.cfg.TargetOutpoint.String(),
				),
				slog.Uint64(
					"required_locktime", uint64(locktime),
				),
				slog.Int64(
					"current_height",
					int64(
						b.currentHeight(),
					),
				),
			)

			return nil
		}

		sweepTx, err := buildSweepTx(
			ctx, b.cfg.Wallet, b.cfg.ChainSource, b.proof, b.desc,
			b.cfg.MaxSweepFeeRateSatPerVByte, b.currentHeight(),
			policy,
		)
		if err != nil {
			// ErrExitSpendNotMatured can still surface here as
			// defense in depth if a future policy path bypasses
			// the early gate above. Treat it the same way: stall
			// without burning the retry budget so the next height
			// observation triggers a clean retry.
			if errors.Is(err, ErrExitSpendNotMatured) {
				b.log.DebugS(ctx,
					"Deferring exit spend build: "+
						"policy reports not matured",
					slog.String(
						"target_outpoint",
						b.cfg.TargetOutpoint.
							String(),
					),
					slog.String("err", err.Error()),
				)

				return nil
			}

			return b.driveEvent(ctx, ax, &SweepBuildFailedEvent{
				Reason: err.Error(),
			})
		}

		b.sweepTx = sweepTx
	} else {
		sweepTxid := b.sweepTx.TxHash()
		b.log.DebugS(ctx, "Reusing persisted unroll exit spend",
			slog.String(
				"target_outpoint",
				b.cfg.TargetOutpoint.String(),
			),
			slog.String(
				"exit_policy_kind", b.exitPolicyKind().String(),
			),
			slog.String("txid", sweepTxid.String()),
		)
	}

	// Stage the built sweep before asking txconfirm to broadcast, so on any
	// retry the same sweepTx is restored and re-submitted under txconfirm's
	// dedup rather than a freshly-derived sweep racing it. Stage commits
	// this in its own short writer transaction and releases the lock, so
	// the EnsureConfirmedReq Ask below runs with no writer held.
	if err := b.persistCheckpoint(ctx, ax); err != nil {
		return err
	}

	sweepPkScript, err := safeTxOutPkScript(b.sweepTx, 0)
	if err != nil {
		return fmt.Errorf("sweep tx malformed: %w", err)
	}

	sweepLabel := "unroll-sweep-" + b.cfg.TargetOutpoint.String()
	sweepTxid := b.sweepTx.TxHash()

	b.log.InfoS(ctx, "Submitting unroll exit spend",
		slog.String("target_outpoint", b.cfg.TargetOutpoint.String()),
		slog.String(
			"exit_policy_kind", b.exitPolicyKind().String(),
		),
		slog.String("txid", sweepTxid.String()),
	)

	_, err = b.cfg.TxConfirmRef.Ask(ctx, &txconfirm.EnsureConfirmedReq{
		Tx:                   b.sweepTx,
		ConfirmationPkScript: sweepPkScript,
		Label:                sweepLabel,
		Subscriber:           b.notificationRef(),
	}).Await(ctx).Unpack()
	if err != nil {
		return err
	}

	return b.driveEvent(ctx, ax, &SweepBroadcastedEvent{Txid: sweepTxid})
}

// exitPolicyKind returns the durable policy kind for the current job.
func (b *behavior) exitPolicyKind() ExitPolicyKind {
	if b.pending == nil {
		return StandardVTXOTimeoutExitPolicyKind
	}

	return exitPolicyKind(b.pending.ExitPolicyKind)
}

// exitPolicyRef returns the durable policy ref for the current job.
func (b *behavior) exitPolicyRef() string {
	if b.pending == nil {
		return ""
	}

	return b.pending.ExitPolicyRef
}

// currentHeight returns the last persisted height for policy construction.
func (b *behavior) currentHeight() int32 {
	if b.pending == nil {
		return 0
	}

	return b.pending.Height
}

// currentHeightHint returns the actor's last observed best height as an
// unsigned value suitable for a confirmation-watch height hint, clamping any
// non-positive height (e.g. an uninitialized job) to zero.
func (b *behavior) currentHeightHint() uint32 {
	h := b.currentHeight()
	if h <= 0 {
		return 0
	}

	return uint32(h)
}

// resolveExitSpendPolicy reconstructs the exit policy for this job.
func (b *behavior) resolveExitSpendPolicy(ctx context.Context) (ExitSpendPolicy,
	error) {

	req := ExitSpendPolicyRequest{
		Kind:               b.exitPolicyKind(),
		Ref:                b.exitPolicyRef(),
		StandardDescriptor: b.desc,
	}
	if req.Kind == StandardVTXOTimeoutExitPolicyKind {
		resolver := standardExitSpendPolicyResolver{}

		return resolver.ResolveExitSpendPolicy(
			ctx,
			req,
		)
	}

	if b.cfg.ExitSpendPolicyResolver == nil {
		return nil, fmt.Errorf("exit spend policy resolver is " +
			"required for non-standard unroll policy")
	}

	return b.cfg.ExitSpendPolicyResolver.ResolveExitSpendPolicy(ctx, req)
}

// safeTxOutPkScript returns a defensive copy of tx.TxOut[index].PkScript.
// It reports an error instead of panicking when tx is nil, has no outputs,
// or index is out of range, so malformed proof artifacts surface as a
// retryable error rather than a goroutine panic that terminates the
// actor.
func safeTxOutPkScript(tx *wire.MsgTx, index uint32) ([]byte, error) {
	if tx == nil {
		return nil, fmt.Errorf("tx is nil")
	}

	if index >= uint32(len(tx.TxOut)) {
		return nil, fmt.Errorf("output index %d out of range (tx has "+
			"%d outputs)", index, len(tx.TxOut))
	}

	return append([]byte(nil), tx.TxOut[index].PkScript...), nil
}

// proofNodeHeightHintLookback is the number of blocks below the actor's
// current best height used as the confirmation-watch FALLBACK floor for
// proof-graph transactions when no commitment height is known. A genesis
// floor (height 1) forces neutrino's notifier to rescan every block from tip
// to block 1 (one GetCFilter per block) when the watched tx never confirms,
// which floods logs and never terminates (wavelength#884). We therefore
// bound the rescan window to a fixed lookback below the current height.
//
// The lookback MUST comfortably exceed the maximum batch lifetime plus the
// worst-case tree-depth + CSV exit window, because proof roots and
// intermediate OOR checkpoint ancestors can confirm well before the target
// descriptor's CreatedHeight — using the creation height as the floor would
// miss an ancestor that already confirmed. A generous operator-configured max
// batch lifetime is on the order of one month (~4320 blocks at 144
// blocks/day); 10000 blocks (~10 weeks) is a safe multiple that keeps the
// floor below any ancestor's real confirmation height while still capping the
// neutrino rescan at a bounded window instead of scanning to genesis.
//
// The primary, tight floor is the commitment-tx confirmation height carried
// per fragment on Descriptor.Ancestry[i].CommitmentHeight: nothing in a
// VTXO's proof graph can confirm before its commitment tx, so min() across
// fragments is a provable lower bound for every proof ancestor. When that
// height is unknown (zero) — legacy persisted VTXOs, empty-ancestry incoming
// round VTXOs, or a server that does not yet populate the field — we fall
// back to this bounded lookback, preserving the pre-commitment-height
// behaviour exactly.
const proofNodeHeightHintLookback uint32 = 10000

// proofNodeHeightHint returns the earliest safe confirmation height hint for
// proof-graph transactions given the actor's current best height. Roots and
// intermediate OOR checkpoint ancestors can confirm before the target
// descriptor's CreatedHeight, so proof watches must not use the target
// creation height as a lower bound; instead we floor at a bounded lookback
// below the current height (never below block 1). See
// proofNodeHeightHintLookback for the sizing rationale. Callers that hold a
// Descriptor should route through behavior.proofNodeConfHeightHint instead, so
// the age-exceeds-lookback breadcrumb fires.
func proofNodeHeightHint(currentHeight uint32) uint32 {
	if currentHeight <= proofNodeHeightHintLookback {
		return 1
	}

	return currentHeight - proofNodeHeightHintLookback
}

// commitmentHeightFloor returns the tight, provable confirmation-watch floor
// derived from the target descriptor's ancestry: the minimum commitment
// confirmation height across ALL fragments. It returns 0 (meaning "unknown,
// use the fallback floor") unless every fragment carries a known positive
// height.
//
// The all-or-nothing requirement is a soundness one. Nothing in a VTXO's proof
// graph confirms before its commitment tx, so once every fragment's commitment
// height is known their minimum is a provable lower bound for every proof
// ancestor. But a min taken over only the known fragments would not bound an
// unknown fragment, whose ancestor could have confirmed below that min and be
// missed by the rescan. So a single unknown fragment defers the whole target
// to the fallback. We take the min, never the max: a lower floor only widens
// the (safe) rescan window; too high a floor can miss an ancestor.
func (b *behavior) commitmentHeightFloor() int32 {
	if b.desc == nil || len(b.desc.Ancestry) == 0 {
		return 0
	}

	var lowest int32
	for i := range b.desc.Ancestry {
		h := b.desc.Ancestry[i].CommitmentHeight
		if h <= 0 {
			return 0
		}

		if lowest == 0 || h < lowest {
			lowest = h
		}
	}

	return lowest
}

// proofNodeConfHeightHint returns the confirmation-watch height floor for one
// proof-graph node. When the target descriptor carries a known commitment
// height it uses min(commitment height) across fragments as a tight, provable
// floor (clamped to at least block 1). Otherwise it falls back to the bounded
// lookback below the current height and leaves a breadcrumb when the target
// VTXO is old enough that the fallback floor may have risen above a proof
// ancestor's real confirmation height. On the fallback path the lookback keeps
// the floor below every ancestor only while the VTXO's age (in blocks) stays
// within proofNodeHeightHintLookback; past that the neutrino historical rescan
// can start too late, miss the ancestor's confirmation, and silently stall the
// exit — so we warn rather than fail.
func (b *behavior) proofNodeConfHeightHint(ctx context.Context,
	txid chainhash.Hash) uint32 {

	// The floor is a per-target property (min over the whole ancestry), not
	// per-node, so it is the same for every proof node. txid is used only
	// for the fallback-path breadcrumb below, not to select a floor.

	// Primary path: a known commitment height is the tightest sound floor.
	// commitmentHeightFloor only returns positive values, so it is already
	// clamped to at least block 1 (we never hand the notifier a zero
	// height).
	if floor := b.commitmentHeightFloor(); floor > 0 {
		return uint32(floor)
	}

	// Fallback path: no commitment height known, so bound the rescan with
	// the fixed lookback and warn if the VTXO is old enough that the floor
	// may have risen above an ancestor's real confirmation height.
	//
	// currentHeightHint returns 0 only for an uninitialized job (no height
	// observed yet), and proofNodeHeightHint(0) is the genesis floor (1) —
	// the very rescan-to-genesis behaviour #884 fixes. That is not
	// reachable on the real path: a proof watch is registered only while
	// handling a Start/Resume/HeightUpdated event, all of which set
	// b.pending.Height first, so currentHeightHint is non-zero here in
	// practice.
	currentHeight := b.currentHeightHint()
	hint := proofNodeHeightHint(currentHeight)

	// CreatedHeight is our proxy for the earliest proof-ancestor
	// confirmation height. Once the VTXO's age reaches the lookback the
	// floor sits at or above it, so an ancestor that confirmed earlier can
	// escape the rescan window.
	if b.desc != nil && b.desc.CreatedHeight > 0 {
		age := int64(currentHeight) - int64(b.desc.CreatedHeight)
		if age >= int64(proofNodeHeightHintLookback) {
			b.log.WarnS(ctx, "Proof-node confirmation floor may "+
				"exceed ancestor height; exit could stall",
				nil,
				slog.String(
					"target_outpoint",
					b.cfg.TargetOutpoint.String(),
				),
				slog.String("proof_txid", txid.String()),
				slog.Int64("vtxo_age_blocks", age),
				slog.Uint64(
					"lookback",
					uint64(proofNodeHeightHintLookback),
				),
				slog.Uint64("height_hint", uint64(hint)),
				slog.Int64(
					"created_height",
					int64(b.desc.CreatedHeight),
				),
			)
		}
	}

	return hint
}

// ensureNodeConfirmed hands one ready proof-graph node to txconfirm and
// threads any immediate rejection back through the FSM.
//
// txconfirm is idempotent on txid, so calling this for a node that is
// already broadcast or confirmed is a cheap no-op. That is why both the
// "first time ready" path and the "reissue after restart" path can funnel
// through here without coordination: the shared actor absorbs the
// duplicate.
//
// One subtlety: EnsureConfirmedReq can return an EnsureConfirmedResp with
// State=TxStateFailed synchronously (for example, mempool rejected the
// tx). We translate that into a TxFailedEvent driven right back into the
// FSM so the usual terminal path runs, rather than propagating the error
// up and leaving the actor waiting on a subscription that will never
// fire.
func (b *behavior) ensureNodeConfirmed(ctx context.Context,
	ax actor.Exec[unrollTx], txid chainhash.Hash,
	node *recovery.Node) error {

	if node == nil {
		return fmt.Errorf("proof node %s missing", txid)
	}

	pkScript, err := safeTxOutPkScript(node.Tx, 0)
	if err != nil {
		return fmt.Errorf("proof node %s: %w", txid, err)
	}

	if err := b.ensureProofSpendWatches(ctx, txid, node); err != nil {
		return err
	}

	heightHint := b.proofNodeConfHeightHint(ctx, txid)
	resp, err := b.cfg.TxConfirmRef.Ask(ctx, &txconfirm.EnsureConfirmedReq{
		Tx:                   node.Tx,
		ConfirmationPkScript: pkScript,
		Label:                "unroll-node-" + txid.String(),
		HeightHint:           heightHint,
		Subscriber:           b.notificationRef(),
	}).Await(ctx).Unpack()
	if err != nil {
		return err
	}

	ensureResp, ok := resp.(*txconfirm.EnsureConfirmedResp)
	if !ok {
		return fmt.Errorf("unexpected txconfirm response %T", resp)
	}

	if ensureResp.State == txconfirm.TxStateFailed {
		return b.driveEvent(ctx, ax, &TxFailedEvent{
			Txid: txid,
			Reason: b.failureReasonForTx(
				txid, "txconfirm returned failed state",
			),
		})
	}

	return nil
}

// watchDeferredCheckpoint registers a confirmation watch for a ready
// fraud-triggered checkpoint while the actor waits for the operator to confirm
// it first.
func (b *behavior) watchDeferredCheckpoint(ctx context.Context,
	txid chainhash.Hash, node *recovery.Node) error {

	if node == nil {
		return fmt.Errorf("proof node %s missing", txid)
	}

	pkScript, err := safeTxOutPkScript(node.Tx, 0)
	if err != nil {
		return fmt.Errorf("proof node %s: %w", txid, err)
	}

	notifyRef := chainsource.MapConfirmationEvent(
		b.selfRef, func(event chainsource.ConfirmationEvent) Msg {
			return &TxConfirmedMsg{
				Txid:     event.Txid,
				Height:   event.BlockHeight,
				NumConfs: event.NumConfs,
			}
		},
	)

	// Deferred-checkpoint watches register directly against
	// chainsource rather than txconfirm, but the rollback contract
	// is identical: an operator-confirmed checkpoint that reorgs out
	// while the actor is live must drop from ConfirmedTxids so the
	// planner does not advance off a stale anchor, and a finality
	// signal lets the chainsource sub-actor release its registration
	// at the reorg-safety horizon. Wire both lifecycle refs through
	// the same selfRef the positive event uses.
	reorgRef := chainsource.MapConfReorgedEvent(
		b.selfRef, func(event chainsource.ConfReorgedEvent) Msg {
			return &TxReorgedMsg{Txid: event.Txid}
		},
	)
	doneRef := chainsource.MapConfDoneEvent(
		b.selfRef, func(event chainsource.ConfDoneEvent) Msg {
			return &TxFinalizedMsg{Txid: event.Txid}
		},
	)

	txidCopy := txid
	_, err = b.cfg.ChainSource.Ask(ctx, &chainsource.RegisterConfRequest{
		CallerID:      b.deferredCheckpointCallerID(),
		Txid:          &txidCopy,
		PkScript:      append([]byte(nil), pkScript...),
		TargetConfs:   1,
		HeightHint:    b.proofNodeConfHeightHint(ctx, txid),
		NotifyActor:   fn.Some(notifyRef),
		NotifyReorged: fn.Some(reorgRef),
		NotifyDone:    fn.Some(doneRef),
	}).Await(ctx).Unpack()
	if err != nil {
		return err
	}

	return nil
}

// deferredCheckpointCallerID returns the stable confirmation-watch caller ID
// used for all deferred checkpoints in this actor.
func (b *behavior) deferredCheckpointCallerID() string {
	return b.cfg.ActorID + "-deferred-checkpoint"
}

// stateResponse builds the current state response for callers and tests. The
// context is threaded through to the progress derivation so any diagnostic
// logging on that read-only path stays tied to the caller's request scope.
func (b *behavior) stateResponse(ctx context.Context) *GetStateResp {
	state, err := b.currentState()
	if err != nil {
		return &GetStateResp{
			Phase:      PhaseFailed,
			FailReason: err.Error(),
		}
	}

	job := stateJob(state)
	sweepTxid := effectiveSweepTxid(job.PlannerState, b.sweepTx)
	resp := &GetStateResp{
		Started:        !isIdleState(state),
		Trigger:        stateTrigger(state),
		ExitPolicyKind: exitPolicyKind(job.ExitPolicyKind),
		ExitPolicyRef:  job.ExitPolicyRef,
		Height:         stateHeight(state),
		Phase:          phaseFromState(state),
		PlannerState:   copyPlannerState(job.PlannerState),
		FailReason:     job.FailReason,
	}

	if sweepTxid != nil {
		txid := *sweepTxid
		resp.SweepTxid = &txid
	}

	resp.Progress = b.exitProgress(ctx, state, job)

	return resp
}

// exitProgress derives the human-facing progress summary from the planner
// snapshot at the actor's current best height. It returns nil when the planner
// or proof is not yet loaded (e.g. a status probe that races actor startup) so
// callers fall back to the coarse phase view rather than a misleading zero
// snapshot.
func (b *behavior) exitProgress(ctx context.Context, state State,
	job *JobState) *ExitProgress {

	if b.planner == nil || b.proof == nil || isIdleState(state) {
		return nil
	}

	plannerState := copyPlannerState(job.PlannerState)
	snapshot, err := b.planner.Plan(job.Height, &plannerState)
	if err != nil {
		// A snapshot failure is not fatal to a read-only status probe;
		// the coarse phase still describes the job. Trace it and
		// degrade to nil.
		b.log.DebugS(ctx, "exit progress plan failed",
			slog.String("err", err.Error()),
		)

		return nil
	}

	layers := b.proof.Layers()
	totalTxs := 0
	for _, layer := range layers {
		totalTxs += len(layer)
	}

	readyTxs := len(snapshot.Ready)
	inFlightTxs := len(snapshot.InFlight)
	blockedTxs := len(snapshot.Blocked)
	unconfirmed := readyTxs + inFlightTxs + blockedTxs

	totalLayers := len(layers)
	currentLayer := frontierLayer(snapshot, totalLayers)

	// The sweep also cannot broadcast before the exit policy's absolute
	// locktime (a vHTLC refund-without-receiver gates on one; the standard
	// timeout policy reports zero). Fold the remaining locktime window into
	// the best-case estimate so a policy-gated sweep is not reported as
	// imminent. It stays zero until the sweep phase resolves the policy.
	var lockTimeBlocks int32
	b.requiredLockTime.WhenSome(func(lockTime uint32) {
		if remaining := int32(lockTime) - job.Height; remaining > 0 {
			lockTimeBlocks = remaining
		}
	})

	progress := &ExitProgress{
		ConfirmedTxs:      totalTxs - unconfirmed,
		InFlightTxs:       inFlightTxs,
		ReadyTxs:          readyTxs,
		BlockedTxs:        blockedTxs,
		TotalTxs:          totalTxs,
		CurrentLayer:      currentLayer,
		TotalLayers:       totalLayers,
		TargetConfirmed:   snapshot.TargetConfirmed,
		AllProofConfirmed: snapshot.AllProofConfirmed,
		CSV:               snapshot.CSV,
		BestCaseBlocksRemaining: bestCaseBlocksRemaining(
			snapshot, b.proof.CSVDelay(), currentLayer, totalLayers,
			lockTimeBlocks,
		),
	}

	// The exact sweep fee is the value of the target output the sweep
	// actually spends, minus the swept output value. Read the input value
	// from the proof (what the exit spend policy spends) rather than the
	// descriptor amount, which may differ from the materialized target
	// output.
	if b.sweepTx != nil {
		if targetOut, err := b.proof.TargetOutput(); err == nil {
			fee, ok := actualSweepFeeSat(b.sweepTx, targetOut.Value)
			if ok {
				progress.ActualSweepFeeSat = fn.Some(fee)
			}
		}
	}

	return progress
}

// frontierLayer returns the shallowest topological layer that still holds an
// unconfirmed transaction (ready, in-flight, or blocked). When every proof node
// has confirmed there is no frontier, so it returns totalLayers to signal that
// materialization is complete.
func frontierLayer(snapshot *unrollplan.Snapshot, totalLayers int) int {
	frontier := totalLayers
	consider := func(layer int) {
		if layer < frontier {
			frontier = layer
		}
	}

	for _, tx := range snapshot.Ready {
		consider(tx.Layer)
	}
	for _, tx := range snapshot.InFlight {
		consider(tx.Layer)
	}
	for _, tx := range snapshot.Blocked {
		consider(tx.Layer)
	}

	return frontier
}

// bestCaseBlocksRemaining is the optimistic block count until a confirmed
// sweep. It assumes one confirmation per remaining proof layer, then the wait
// for the sweep to become broadcastable, then one sweep confirmation. Before
// the target confirms the CSV wait is the full descriptor delay; afterwards the
// planner's live BlocksRemaining is used, which shrinks each block. The sweep
// can only broadcast once BOTH the CSV timeout and the exit policy's absolute
// locktime have passed, so the post-target wait is the longer of the two.
func bestCaseBlocksRemaining(snapshot *unrollplan.Snapshot, csvDelay uint32,
	currentLayer, totalLayers int, lockTimeBlocks int32) int32 {

	// A confirmed sweep is done; a broadcast sweep is one confirmation
	// away.
	if snapshot.Done {
		return 0
	}
	if snapshot.Sweep.Status == unrollplan.SweepStatusBroadcasted {
		return 1
	}

	// Each proof layer from the frontier through the target layer needs at
	// least one confirmation. Once the target confirms this term is zero.
	matBlocks := int32(totalLayers - currentLayer)
	if matBlocks < 0 {
		matBlocks = 0
	}

	// The sweep is gated only on the target's CSV maturity, not on any
	// straggler proof transaction, so once the target has confirmed the
	// materialization term must not be added on top of the live CSV
	// countdown. A multi-transaction layer can still hold an unconfirmed
	// sibling after the target confirms, which would otherwise inflate the
	// estimate.
	if snapshot.TargetConfirmed {
		matBlocks = 0
	}

	// The CSV wait is the full descriptor delay until the target confirms,
	// after which the planner reports the exact remaining window.
	csvBlocks := int32(csvDelay)
	snapshot.CSV.WhenSome(func(csv unrollplan.CSVInfo) {
		csvBlocks = csv.BlocksRemaining
	})

	// The policy's absolute locktime is an independent gate on the sweep,
	// so the effective wait is the longer of the CSV and locktime windows.
	sweepGate := csvBlocks
	if lockTimeBlocks > sweepGate {
		sweepGate = lockTimeBlocks
	}

	// Add one confirmation for the final sweep itself.
	return matBlocks + sweepGate + 1
}

// actualSweepFeeSat derives the real fee the built sweep pays: the value of the
// single target input the sweep spends (inputValueSat) minus the swept output
// value. The input value must be the proof's target output value, which is what
// the exit spend policy actually spends. It returns false until a sweep
// transaction has been built, or if the numbers do not yield a sane positive
// fee (a defensive guard against a malformed tx).
func actualSweepFeeSat(sweepTx *wire.MsgTx, inputValueSat int64) (int64, bool) {
	if sweepTx == nil {
		return 0, false
	}

	var outputSat int64
	for _, out := range sweepTx.TxOut {
		outputSat += out.Value
	}

	fee := inputValueSat - outputSat
	if fee <= 0 {
		return 0, false
	}

	return fee, true
}

// ensureLoaded lazily constructs every piece of actor-lifetime state the
// FSM needs to make progress: the immutable recovery proof, the VTXO
// descriptor, the pure planner, the protofsm session, the block epoch
// subscription, and the target-outpoint spend watch.
//
// The load is lazy (not done in NewVTXOUnrollActor) for two reasons:
//
//   - On restore, the checkpoint has already been pulled from the
//     delivery store but the chain subscription and FSM session should
//     only spin up once the first real event arrives. Booting the actor
//     must not fail if chainsource is momentarily unresponsive.
//
//   - Subsequent Receive invocations reuse the cached fields so every
//     step (proof assembly, planner construction, subscription) happens
//     exactly once per actor lifetime.
//
// Order matters: the planner validates against the proof, the FSM
// session validates against the planner, and the spend watch needs the
// proof + descriptor to derive the target pkScript. The final
// PlannerState.Validate call catches checkpoint/proof drift (e.g. an
// InFlightTxids entry that no longer resolves in the proof) loudly
// instead of letting the FSM silently desync.
func (b *behavior) ensureLoaded(ctx context.Context) error {
	if b.proof == nil {
		proof, err := b.cfg.ProofAssembler.EnsureProof(
			ctx, b.cfg.TargetOutpoint,
		)
		if err != nil {
			return err
		}

		b.proof = proof
	}

	if b.desc == nil {
		desc, err := b.cfg.VTXOStore.GetVTXO(ctx, b.cfg.TargetOutpoint)
		if err != nil {
			return err
		}

		b.desc = desc
	}

	if b.planner == nil {
		planner, err := unrollplan.NewPlanner(b.proof)
		if err != nil {
			return err
		}

		b.planner = planner
	}

	// Reconcile the restored checkpoint against the canonical chain
	// before binding the FSM session. Any confirmed-tx anchor the
	// reconciler reports as absent gets dropped (with its
	// descendants), and a target / sweep confirmation that vanished
	// while the daemon was offline downgrades the sweep state. This
	// must run before stateFromCheckpoint so the FSM is built from
	// the post-reconciliation snapshot, and before any side effects
	// (block subscription, spend watch, ResumeEvent reissue) so a
	// stale sweep is never re-broadcast on a now-unconfirmed target.
	if !b.reconciled {
		if err := b.reconcileOnRestart(ctx); err != nil {
			return err
		}

		b.reconciled = true
	}

	if b.session == nil {
		initialState := State(&Idle{})
		if b.pending != nil && b.pending.Started {
			initialState = stateFromCheckpoint(b.pending)
		}

		session, err := NewSession(
			ctx, b.proof, b.planner, initialState, b.log,
			b.cfg.FraudCheckpointSafetyMargin,
		)
		if err != nil {
			return err
		}

		b.session = session
	}

	if err := b.ensureBlockSubscription(ctx); err != nil {
		return err
	}

	if err := b.ensureSpendWatch(ctx); err != nil {
		return err
	}

	state, err := b.currentState()
	if err != nil {
		return err
	}

	return stateJob(state).PlannerState.Validate(b.proof)
}

// notificationRef builds the subscriber ref the actor hands to txconfirm.
//
// txconfirm delivers notifications in its own type space
// ([txconfirm.Notification]) but our durable mailbox only accepts [Msg]
// variants so the delivery store can codec them. This helper threads a
// [chainsource.MapNotification]-style adapter: every txconfirm
// notification is synchronously re-wrapped into the matching mailbox
// message (TxConfirmedMsg / TxFailedMsg) and forwarded to our self-ref
// for durable enqueue. An unknown notification type is mapped to a
// generic TxFailedMsg so the actor still terminates loudly instead of
// silently dropping the callback.
func (b *behavior) notificationRef() actor.TellOnlyRef[txconfirm.Notification] {
	return txconfirm.MapNotification(
		b.selfRef,
		func(msg txconfirm.Notification) Msg {
			switch m := msg.(type) {
			case *txconfirm.TxConfirmed:
				return &TxConfirmedMsg{
					Txid:     m.Txid,
					Height:   m.BlockHeight,
					NumConfs: m.NumConfs,
				}

			case *txconfirm.TxFailed:
				return &TxFailedMsg{
					Txid:   m.Txid,
					Reason: m.Reason,
				}

			case *txconfirm.TxReorged:
				return &TxReorgedMsg{
					Txid: m.Txid,
				}

			case *txconfirm.TxFinalized:
				return &TxFinalizedMsg{
					Txid: m.Txid,
				}

			default:
				return &TxFailedMsg{
					Reason: fmt.Sprintf(
						"unknown txconfirm "+
							"notification %T",
						msg,
					),
				}
			}
		},
	)
}

// restoreCheckpoint restores durable state from the delivery store.
func (b *behavior) restoreCheckpoint(ctx context.Context) error {
	if b.cfg.DeliveryStore == nil {
		return fmt.Errorf("delivery store must be provided")
	}

	checkpoint, err := b.cfg.DeliveryStore.LoadCheckpoint(
		ctx, b.cfg.ActorID,
	)
	if err != nil {
		return err
	}

	if checkpoint == nil {
		return nil
	}

	decoded, err := decodeCheckpoint(checkpoint.StateData)
	if err != nil {
		return err
	}

	if decoded.Version != checkpointVersion {
		return fmt.Errorf("unknown checkpoint version %d",
			decoded.Version)
	}

	b.pending = decoded
	b.sweepTx = copyTx(decoded.SweepTx)
	b.sweepFinalized = decoded.SweepFinalized

	return nil
}

// reconcileOnRestart asks the configured ChainReconciler whether each
// anchor recorded in the restored checkpoint is still live on the
// canonical chain, and prunes the in-memory checkpoint accordingly.
// Called from ensureLoaded exactly once per actor lifetime, BEFORE the
// FSM session is bound.
//
// A missing reconciler is a no-op: backends that cannot answer
// historical confirmation queries fall back to the conservative
// behavior of treating the restored checkpoint as authoritative.
// Transport errors from the reconciler are surfaced upward; the actor
// must not proceed to broadcast off a checkpoint we could not verify.
func (b *behavior) reconcileOnRestart(ctx context.Context) error {
	if b.cfg.ChainReconcilerFactory.IsNone() {
		return nil
	}

	if b.pending == nil || !b.pending.Started {
		return nil
	}

	factory := b.cfg.ChainReconcilerFactory.UnsafeFromSome()
	reconciler := factory(b.cfg.TargetOutpoint, b.proof)
	if reconciler == nil {
		return nil
	}

	return reconcileCheckpoint(ctx, reconciler, b.proof, b.pending)
}

// ensureBlockSubscription starts the actor's shared block epoch subscription on
// first use so CSV waits advance in the live daemon.
func (b *behavior) ensureBlockSubscription(ctx context.Context) error {
	if b.blockSubActive {
		return nil
	}

	notifyRef := chainsource.MapBlockEpoch(
		b.selfRef,
		func(epoch chainsource.BlockEpoch) Msg {
			return &HeightObservedMsg{
				Height: epoch.Height,
			}
		},
	)

	_, err := b.cfg.ChainSource.Ask(
		ctx, &chainsource.SubscribeBlocksRequest{
			CallerID:    b.blockCallerID(),
			NotifyActor: fn.Some(notifyRef),
		},
	).Await(ctx).Unpack()
	if err != nil {
		return fmt.Errorf("subscribe blocks: %w", err)
	}

	b.blockSubActive = true

	return nil
}

// unsubscribeBlocks cancels the actor's block subscription when it stops.
func (b *behavior) unsubscribeBlocks(ctx context.Context) {
	if !b.blockSubActive {
		return
	}

	err := b.cfg.ChainSource.Tell(
		ctx, &chainsource.UnsubscribeBlocksRequest{
			CallerID: b.blockCallerID(),
		},
	)
	if err == nil {
		b.blockSubActive = false
	}
}

// blockCallerID returns the stable chain subscription ID for this actor.
func (b *behavior) blockCallerID() string {
	return fmt.Sprintf("unroll.%s", b.cfg.TargetOutpoint.String())
}

// ensureSpendWatch registers a one-shot spend watch on the target outpoint so
// the actor detects external spends early.
func (b *behavior) ensureSpendWatch(ctx context.Context) error {
	if b.spendWatchActive {
		return nil
	}

	if b.proof == nil || b.desc == nil {
		return nil
	}

	targetOutpoint := b.proof.TargetOutpoint()
	targetNode, ok := b.proof.Node(targetOutpoint.Hash)
	if !ok {
		return fmt.Errorf("target tx %s not in proof",
			targetOutpoint.Hash)
	}

	pkScript, err := safeTxOutPkScript(targetNode.Tx, targetOutpoint.Index)
	if err != nil {
		return fmt.Errorf("target tx %s: %w", targetOutpoint.Hash, err)
	}

	notifyRef := chainsource.MapSpendEvent(
		b.selfRef,
		func(event chainsource.SpendEvent) Msg {
			return &SpendObservedMsg{
				Outpoint:       event.Outpoint,
				SpendingTxid:   event.SpendingTxid,
				SpendingHeight: event.SpendingHeight,
			}
		},
	)
	reorgRef := chainsource.MapSpendReorgedEvent(
		b.selfRef,
		func(_ chainsource.SpendReorgedEvent) Msg {
			return &SpendReorgedMsg{}
		},
	)
	doneRef := chainsource.MapSpendDoneEvent(
		b.selfRef,
		func(_ chainsource.SpendDoneEvent) Msg {
			return &SpendFinalizedMsg{}
		},
	)

	_, err = b.cfg.ChainSource.Ask(
		ctx, &chainsource.RegisterSpendRequest{
			CallerID:      b.spendCallerID(),
			Outpoint:      &targetOutpoint,
			PkScript:      pkScript,
			HeightHint:    uint32(b.desc.CreatedHeight),
			NotifyActor:   fn.Some(notifyRef),
			NotifyReorged: fn.Some(reorgRef),
			NotifyDone:    fn.Some(doneRef),
		},
	).Await(ctx).Unpack()
	if err != nil {
		return fmt.Errorf("register spend watch: %w", err)
	}

	b.spendWatchActive = true

	return nil
}

// unregisterSpendWatch cancels the actor's target spend watch on stop.
func (b *behavior) unregisterSpendWatch(ctx context.Context) {
	if !b.spendWatchActive {
		return
	}

	targetOutpoint := b.cfg.TargetOutpoint
	err := b.cfg.ChainSource.Tell(
		ctx, &chainsource.UnregisterSpendRequest{
			CallerID: b.spendCallerID(),
			Outpoint: &targetOutpoint,
		},
	)
	if err == nil {
		b.spendWatchActive = false
	}
}

// spendCallerID returns the stable spend-watch registration ID.
func (b *behavior) spendCallerID() string {
	return fmt.Sprintf("unroll-spend.%s", b.cfg.TargetOutpoint.String())
}

// ensureProofSpendWatches registers spend watches on proof node outputs that
// later in-proof child nodes consume. Neutrino can miss the direct
// confirmation notification under load, but a spend of one of these outputs
// still proves the parent proof transaction confirmed.
func (b *behavior) ensureProofSpendWatches(ctx context.Context,
	txid chainhash.Hash, node *recovery.Node) error {

	if node == nil {
		return fmt.Errorf("proof node %s missing", txid)
	}
	if b.proof == nil {
		return fmt.Errorf("proof required for spend watches")
	}

	outpoints, err := b.proofChildInputOutpoints(txid)
	if err != nil {
		return err
	}
	if len(outpoints) > 0 && b.proofSpendWatches == nil {
		b.proofSpendWatches = make(map[wire.OutPoint]struct{})
	}

	heightHint := uint32(0)
	if b.desc != nil && b.desc.CreatedHeight > 0 {
		heightHint = uint32(b.desc.CreatedHeight)
	}

	for _, outpoint := range outpoints {
		if outpoint == b.cfg.TargetOutpoint {
			continue
		}
		if _, ok := b.proofSpendWatches[outpoint]; ok {
			continue
		}

		pkScript, err := safeTxOutPkScript(node.Tx, outpoint.Index)
		if err != nil {
			return fmt.Errorf("proof node %s: %w", txid, err)
		}

		notifyRef := chainsource.MapSpendEvent(
			b.selfRef,
			func(event chainsource.SpendEvent) Msg {
				return &SpendObservedMsg{
					Outpoint:       event.Outpoint,
					SpendingTxid:   event.SpendingTxid,
					SpendingHeight: event.SpendingHeight,
				}
			},
		)

		_, err = b.cfg.ChainSource.Ask(
			ctx, &chainsource.RegisterSpendRequest{
				CallerID:    b.proofSpendCallerID(outpoint),
				Outpoint:    &outpoint,
				PkScript:    pkScript,
				HeightHint:  heightHint,
				NotifyActor: fn.Some(notifyRef),
			},
		).Await(ctx).Unpack()
		if err != nil {
			return fmt.Errorf("register proof spend watch: %w", err)
		}

		b.proofSpendWatches[outpoint] = struct{}{}
	}

	return nil
}

// proofChildInputOutpoints returns the proof-node outputs that are consumed by
// in-proof children of txid. Recovery proofs can include Ark transactions that
// create several sibling outputs; only the output actually referenced by a
// child belongs to this target's materialization path.
func (b *behavior) proofChildInputOutpoints(txid chainhash.Hash) (
	[]wire.OutPoint, error) {

	children, err := b.proof.ChildTxids(txid)
	if err != nil {
		return nil, err
	}

	seen := make(map[wire.OutPoint]struct{})
	outpoints := make([]wire.OutPoint, 0, len(children))

	for _, childTxid := range children {
		child, ok := b.proof.Node(childTxid)
		if !ok {
			return nil, fmt.Errorf("child proof node %s missing",
				childTxid)
		}
		if child.Tx == nil {
			return nil, fmt.Errorf("child proof node %s has nil tx",
				childTxid)
		}

		for _, txIn := range child.Tx.TxIn {
			if txIn == nil || txIn.PreviousOutPoint.Hash != txid {
				continue
			}

			outpoint := txIn.PreviousOutPoint
			if _, ok := seen[outpoint]; ok {
				continue
			}

			seen[outpoint] = struct{}{}
			outpoints = append(outpoints, outpoint)
		}
	}

	sort.Slice(outpoints, func(i, j int) bool {
		return outpoints[i].Index < outpoints[j].Index
	})

	return outpoints, nil
}

// unregisterProofSpendWatches cancels all proof-node spend watches on stop.
func (b *behavior) unregisterProofSpendWatches(ctx context.Context) {
	for outpoint := range b.proofSpendWatches {
		err := b.cfg.ChainSource.Tell(
			ctx, &chainsource.UnregisterSpendRequest{
				CallerID: b.proofSpendCallerID(outpoint),
				Outpoint: &outpoint,
			},
		)
		if err != nil {
			b.log.WarnS(
				ctx,
				"Failed to unregister proof spend watch",
				err,
				slog.String("outpoint", outpoint.String()),
			)

			continue
		}

		delete(b.proofSpendWatches, outpoint)
	}
}

// proofSpendCallerID returns the stable proof-node spend-watch registration
// ID.
func (b *behavior) proofSpendCallerID(outpoint wire.OutPoint) string {
	return fmt.Sprintf("unroll-proof-spend.%s.%s",
		b.cfg.TargetOutpoint.String(), outpoint.String())
}

// handleSpendObserved processes a chainsource spend notification on the
// target or proof-node outpoint. Spend watches are a safety net — they fire
// when a proof output is consumed on chain, and we have to classify what we
// are looking at before deciding whether the unroll job is dead.
//
// Four cases:
//
//  1. The spent output belongs to a known node in our recovery proof.
//     That proves the parent transaction confirmed, even if txconfirm's
//     direct confirmation notification was missed or delayed.
//
//  2. The spender is a known node in our recovery proof. That means an
//     ancestor just confirmed; this is normal materialization traffic.
//
//  3. The spender is our own final sweep (matched by the sweep txid
//     recorded in planner state). Again benign — our sweep confirming is
//     the goal — so just propagate the height.
//
//  4. Anything else: the watched output was spent by someone else. This can
//     happen if the operator cooperatively claims it, if a fraud party
//     beats us to a signed spend, or if a reorg replays a different
//     history. In all cases the unroll job cannot finish; drive FailEvent
//     with a reason that identifies the spending txid and height so
//     operators can triage the cause off-line.
func (b *behavior) handleSpendObserved(ctx context.Context,
	ax actor.Exec[unrollTx], msg *SpendObservedMsg) fn.Result[Resp] {

	if err := b.ensureLoaded(ctx); err != nil {
		return fn.Err[Resp](err)
	}

	if b.inTerminalState() {
		return fn.Ok[Resp](&AckResp{})
	}

	if b.proof != nil {
		acked, err := b.ackProofOutputSpend(ctx, ax, msg)
		if err != nil {
			return fn.Err[Resp](err)
		}
		if acked {
			return fn.Ok[Resp](&AckResp{})
		}

		// Case 2: the spender is a proof-graph node. That means an
		// ancestor of our target just confirmed on chain.
		if _, ok := b.proof.Node(msg.SpendingTxid); ok {
			return b.handleEvent(ctx, ax, &TxConfirmedEvent{
				Txid:   msg.SpendingTxid,
				Height: msg.SpendingHeight,
			})
		}
	}

	// Case 3: the spender is our own sweep. Same benign outcome — we
	// are watching our own success from a different vantage point.
	// Compare against the sweep txid recorded in planner state
	// (SweepBroadcastedEvent populates that) rather than b.sweepTx,
	// so we catch the late-arriving spend notification even if the
	// behavior has already cleared its in-memory cache.
	state, err := b.currentState()
	if err == nil {
		job := stateJob(state)
		if job.PlannerState.Sweep.Txid.IsSome() &&
			job.PlannerState.Sweep.Txid.UnsafeFromSome() ==
				msg.SpendingTxid {
			return b.handleEvent(ctx, ax, &HeightUpdatedEvent{
				Height: msg.SpendingHeight,
			})
		}
	}

	// Case 4: neither of the above. Someone else spent the watched
	// output. This can happen for two structurally different
	// outputs, with different reversibility properties:
	//
	//   a. The target outpoint was spent by an unknown party. This
	//      is the cooperative-claim / fraud scenario the external-
	//      spend reorg-safety work is built for: a reorg of the
	//      spending block can resurrect the recovery job, so the
	//      actor parks in AwaitingExternalSpendFinality. A
	//      subsequent SpendFinalizedMsg promotes the spend to a
	//      permanent FailReason; a SpendReorgedMsg clears the
	//      observation and resumes planning.
	//
	//   b. A proof-node output was spent by a transaction that is
	//      not itself part of the proof graph (ackProofOutputSpend
	//      acked the parent confirmation already, but returned
	//      false for the spender lookup). The proof graph cannot
	//      complete through this fork, so the unroll job is
	//      terminally dead. Fail with a reason that identifies the
	//      spender for operator triage.
	if msg.Outpoint == (wire.OutPoint{}) ||
		msg.Outpoint == b.cfg.TargetOutpoint {
		return b.handleEvent(ctx, ax, &ExternalSpendObservedEvent{
			SpendingTxid:   msg.SpendingTxid,
			SpendingHeight: msg.SpendingHeight,
		})
	}

	reason := fmt.Sprintf("watched outpoint %s spent externally by tx %s "+
		"at height %d", msg.Outpoint, msg.SpendingTxid,
		msg.SpendingHeight)

	return b.handleEvent(ctx, ax, &FailEvent{Reason: reason})
}

// ackProofOutputSpend records proof-output spend evidence and reports whether
// the observation is fully handled. If a proof output is spent by an unknown
// transaction, the parent proof tx is still confirmed, but the caller must
// continue into the external-spend failure path.
func (b *behavior) ackProofOutputSpend(ctx context.Context,
	ax actor.Exec[unrollTx], msg *SpendObservedMsg) (bool, error) {

	if msg.Outpoint == (wire.OutPoint{}) ||
		msg.Outpoint == b.cfg.TargetOutpoint {
		return false, nil
	}

	if _, ok := b.proof.Node(msg.Outpoint.Hash); !ok {
		return false, nil
	}

	err := b.driveEvent(ctx, ax, &TxConfirmedEvent{
		Txid:   msg.Outpoint.Hash,
		Height: msg.SpendingHeight,
	})
	if err != nil {
		return false, err
	}

	if msg.SpendingTxid == msg.Outpoint.Hash {
		return true, nil
	}

	if _, ok := b.proof.Node(msg.SpendingTxid); !ok {
		return false, nil
	}

	event := &TxConfirmedEvent{
		Txid:   msg.SpendingTxid,
		Height: msg.SpendingHeight,
	}

	return true, b.driveEvent(ctx, ax, event)
}

// inTerminalState reports whether the FSM has already reached a terminal
// phase. Late chain notifications can arrive after completion while the
// registry is draining the child for cleanup; those observations are already
// reflected in the terminal checkpoint and should ack as idempotent no-ops.
func (b *behavior) inTerminalState() bool {
	state, err := b.currentState()
	if err != nil {
		return false
	}

	return state.IsTerminal()
}

// checkpointWrite snapshots the current FSM state into a durable checkpoint and
// returns it alongside a closure that persists it through a transaction-scoped
// store. The closure is shared verbatim by the Stage path (persistCheckpoint)
// and the lease-fenced Commit path (commitAck) so both write identical bytes;
// only the transaction they run in differs.
func (b *behavior) checkpointWrite() (*actorCheckpoint,
	func(context.Context, unrollTx) error, error) {

	state, err := b.currentState()
	if err != nil {
		return nil, nil, err
	}

	checkpoint := checkpointFromState(state, b.sweepTx)

	// Persist the sweep-finalized latch so it survives restart: commitAck
	// re-persists this checkpoint atomically with the message ack, so a
	// finalized sweep whose terminal handoff was deferred/failed is not
	// lost — the actor rehydrates as terminal-eligible rather than stuck
	// "provisional completed".
	checkpoint.SweepFinalized = b.sweepFinalized

	raw, err := encodeCheckpoint(checkpoint)
	if err != nil {
		return nil, nil, err
	}

	write := func(txCtx context.Context, tx unrollTx) error {
		return tx.store.SaveCheckpoint(txCtx, actor.CheckpointParams{
			ActorID:   tx.actorID,
			StateType: checkpointStateType,
			StateData: raw,
			Version:   checkpointVersion,
		})
	}

	return checkpoint, write, nil
}

// persistCheckpoint durably advances the actor checkpoint with a short,
// non-fenced Stage write ahead of the outbox IO. Stage runs the write in its
// own writer transaction and releases the SQLite writer the moment it returns,
// so the txconfirm round-trips that follow never hold the lock. The message is
// not consumed here -- it is acked once, later, by commitAck.
func (b *behavior) persistCheckpoint(ctx context.Context,
	ax actor.Exec[unrollTx]) error {

	checkpoint, write, err := b.checkpointWrite()
	if err != nil {
		return err
	}

	if err := ax.Stage(ctx, write); err != nil {
		return err
	}

	b.pending = checkpoint

	return nil
}

// commitAck is the single consume point for a message. It folds the
// lease-fenced ack and the dedup mark into one short writer transaction and
// re-persists the final checkpoint so the last state advance is atomic with
// consumption. Every intermediate checkpoint was already durably Staged ahead
// of the IO, so this Commit is short and never wraps a txconfirm round-trip. A
// lost lease surfaces here as actor.ErrLeaseLost: the ack rolls back, the
// Staged checkpoints survive, and the framework redelivers the message for an
// idempotent replay against the advanced state.
func (b *behavior) commitAck(ctx context.Context,
	ax actor.Exec[unrollTx]) error {

	checkpoint, write, err := b.checkpointWrite()
	if err != nil {
		return err
	}

	if err := ax.Commit(ctx, write); err != nil {
		return err
	}

	b.pending = checkpoint

	return nil
}

// adoptStagedSweep reconciles the in-memory sweep cache with the durable
// checkpoint through a short read-only snapshot (ax.Read). It exists so a
// replay after a Staged-but-uncommitted sweep build reuses the persisted sweep
// transaction instead of building -- and broadcasting -- a second one with a
// fresh wallet address and a new txid. It only adopts the sweep transaction,
// never b.pending, so it cannot regress the FSM-derived state; it is a no-op
// when no sweep has been staged yet.
func (b *behavior) adoptStagedSweep(ctx context.Context,
	ax actor.Exec[unrollTx]) error {

	return ax.Read(ctx, func(rCtx context.Context, tx unrollTx) error {
		checkpoint, err := tx.store.LoadCheckpoint(rCtx, tx.actorID)
		if err != nil {
			return err
		}

		if checkpoint == nil {
			return nil
		}

		decoded, err := decodeCheckpoint(checkpoint.StateData)
		if err != nil {
			return err
		}

		if decoded.Version != checkpointVersion {
			return fmt.Errorf("unknown checkpoint version %d",
				decoded.Version)
		}

		if decoded.SweepTx != nil {
			b.sweepTx = copyTx(decoded.SweepTx)
		}

		return nil
	})
}

// routeOutbox interprets each [OutboxEvent] emitted by the FSM as a real
// actor-boundary IO effect. The FSM itself is a pure state function — it
// never talks to txconfirm, never persists anything, never broadcasts —
// so this router is where "what to do next" turns into "do it."
//
// Outbox event semantics:
//
//   - EnsureReadyTransactions: newly-unblocked proof nodes. Look each up
//     in the immutable proof graph, then submit to txconfirm. A missing
//     node is a bug in the proof-vs-checkpoint alignment (not a runtime
//     condition) so it surfaces as a hard error instead of being
//     silently skipped.
//
//   - ReissueInFlightTransactions: restart path. The FSM restored
//     InFlightTxids from the checkpoint and needs each one re-submitted
//     to txconfirm so the shared actor re-attaches its subscription.
//     Same node-missing rule applies — a silent skip would leave the FSM
//     permanently waiting on a confirmation that was never re-armed.
//
//   - RequestSweepBuild: the planner says the target has matured and the
//     final sweep can be constructed. Delegates to startSweep for the
//     persist-then-broadcast dance.
//
//   - ReissueSweepConfirmation: restart path for a sweep that was
//     already broadcast before the crash. The checkpoint carried the
//     sweep tx, so we re-submit it to txconfirm (idempotent via dedup)
//     to re-attach the confirmation subscription. A nil sweepTx here
//     means the checkpoint is corrupt and we fail loudly rather than
//     silently losing the job.
func (b *behavior) routeOutbox(ctx context.Context, ax actor.Exec[unrollTx],
	outbox []OutboxEvent) error {

	for i := range outbox {
		switch evt := outbox[i].(type) {
		case *EnsureReadyTransactions:
			// Newly-unblocked proof ancestors that the planner
			// has determined are ready to broadcast. Look each
			// up in the immutable proof graph so we can submit
			// the actual wire.MsgTx to txconfirm.
			for _, txid := range evt.Txids {
				node, ok := b.proof.Node(txid)
				if !ok {

					// This is a bug, not a runtime
					// condition: the FSM told us a txid
					// is ready, but our proof graph no
					// longer carries it. Silent skip
					// would strand the FSM waiting on a
					// subscription that never gets armed.
					return fmt.Errorf("proof node "+
						"%s missing", txid)
				}

				err := b.ensureNodeConfirmed(
					ctx, ax, txid, node,
				)
				if err != nil {
					return err
				}
			}

		case *ReissueInFlightTransactions:
			for _, txid := range evt.Txids {
				node, ok := b.proof.Node(txid)
				if !ok {

					// A missing node on reissue means
					// the checkpoint referenced a
					// transaction our current proof no
					// longer knows about; silently
					// skipping would leave the FSM
					// waiting on a txconfirm
					// subscription that was never
					// re-registered.
					return fmt.Errorf("proof node %s "+
						"missing on reissue", txid)
				}

				err := b.ensureNodeConfirmed(
					ctx, ax, txid, node,
				)
				if err != nil {
					return err
				}
			}

		case *WatchDeferredCheckpoints:
			for _, txid := range evt.Txids {
				node, ok := b.proof.Node(txid)
				if !ok {
					return fmt.Errorf("proof node %s "+
						"missing for deferred "+
						"checkpoint watch", txid)
				}

				err := b.watchDeferredCheckpoint(
					ctx, txid, node,
				)
				if err != nil {
					return err
				}
			}

		case *RequestSweepBuild:
			if err := b.startSweep(ctx, ax); err != nil {
				return err
			}

		case *ReissueSweepConfirmation:
			if b.sweepTx == nil {

				// The FSM asked us to re-arm the sweep
				// confirmation watcher, so the checkpoint
				// must have carried a sweep transaction;
				// a nil sweepTx here signals corrupted
				// state rather than a recoverable race.
				return fmt.Errorf("sweep tx missing on reissue")
			}

			sweepPkScript, err := safeTxOutPkScript(b.sweepTx, 0)
			if err != nil {
				return fmt.Errorf("sweep tx malformed: %w", err)
			}

			_, err = b.cfg.TxConfirmRef.Ask(
				ctx, &txconfirm.EnsureConfirmedReq{
					Tx:                   b.sweepTx,
					ConfirmationPkScript: sweepPkScript,
					Label: "unroll-sweep-" +
						b.cfg.TargetOutpoint.String(),
					Subscriber: b.notificationRef(),
				},
			).Await(ctx).Unpack()
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// currentState returns the current concrete protofsm state.
func (b *behavior) currentState() (State, error) {
	if b.session != nil && b.session.FSM != nil {
		rawState, err := b.session.FSM.CurrentState()
		if err != nil {
			if errors.Is(err, protofsm.ErrStateMachineShutdown) &&
				b.pending != nil {
				return stateFromCheckpoint(b.pending), nil
			}

			return nil, err
		}

		state, ok := rawState.(State)
		if !ok {
			return nil, fmt.Errorf("unexpected unroll state %T",
				rawState)
		}

		return state, nil
	}

	if b.pending != nil && b.pending.Started {
		return stateFromCheckpoint(b.pending), nil
	}

	return &Idle{}, nil
}

// failureReasonForTx turns a raw txconfirm failure reason into a string
// that identifies whether the failed transaction was a proof-graph node
// or our sweep.
//
// When a failure crosses the boundary into the FSM it becomes the
// terminal FailReason on the control-plane record, and operators reading
// that reason need to know which transaction the mempool or node
// rejected. Rather than exposing planner internals at every call site,
// this helper checks the recorded sweep txid (in either the pending
// checkpoint or the current FSM state) and annotates accordingly.
func (b *behavior) failureReasonForTx(txid chainhash.Hash,
	reason string) string {

	if b.pending != nil && b.pending.State.Sweep.Txid.IsSome() &&
		b.pending.State.Sweep.Txid.UnsafeFromSome() == txid {
		return fmt.Sprintf("sweep tx %s failed: %s", txid, reason)
	}

	state, err := b.currentState()
	if err == nil {
		job := stateJob(state)
		if job.PlannerState.Sweep.Txid.IsSome() &&
			job.PlannerState.Sweep.Txid.UnsafeFromSome() == txid {
			return fmt.Sprintf("sweep tx %s failed: %s", txid,
				reason)
		}
	}

	return fmt.Sprintf("proof tx %s failed: %s", txid, reason)
}

// maybeLatchSweepFinalized sets b.sweepFinalized when an incoming
// TxFinalizedMsg refers to the txid currently recorded as the sweep in
// PlannerState. The flag gates notifyRegistryIfTerminal so that a
// PhaseCompleted entry without a matching finalization stays
// provisional — the actor is kept alive by the registry until the
// sweep is past the backend's reorg-safety depth, so a reorg of the
// sweep confirmation has a live actor to receive the rollback.
//
// Late finalizations (e.g. for a proof tx, or for a sweep txid that has
// since been replaced by a re-broadcast) are ignored.
func (b *behavior) maybeLatchSweepFinalized(txid chainhash.Hash) {
	state, err := b.currentState()
	if err != nil {
		return
	}

	job := stateJob(state)
	if job.PlannerState.Sweep.Txid.IsNone() {
		return
	}
	if job.PlannerState.Sweep.Txid.UnsafeFromSome() != txid {
		return
	}

	b.sweepFinalized = true
}

// notifyRegistryIfTerminal forwards one UnrollTerminatedMsg to the
// registry when the FSM reaches a TRULY terminal state, at most once
// per actor lifetime.
//
// PhaseFailed always means FailReason has been set (proof-tx terminal
// failure, sweep retry budget exhausted, or a finalized external
// spend); none of those are reorg-recoverable so the actor is terminal
// the moment we get there.
//
// PhaseCompleted is treated as PROVISIONAL until b.sweepFinalized is
// latched by a matching TxFinalizedMsg for the recorded sweep txid.
// While provisional, the registry keeps the child in its active map so
// a reorg of the sweep confirmation has a live actor to deliver the
// rollback to. The sweep's finality signal arrives via txconfirm's
// TxFinalized, which txconfirm emits off chainsource's height-based Done
// synthesis -- enabled for every backend (including lndclient over gRPC,
// whose native Done channel is allocated-but-never-written) by wiring
// FinalityDepth = ResolveReorgSafetyDepth() at waved/server.go. So the
// child IS evicted once the sweep confirmation buries past the
// reorg-safety depth, not retained indefinitely.
//
// PhaseExternalSpendObserved is reversible by design and never reaches
// this function as terminal; SpendFinalizedMsg promotes the
// provisional anchor to FailReason and the actor moves through
// PhaseFailed instead.
//
// Failure to Tell is warned but not fatal — the registry will
// rediscover the terminal phase the next time it queries child state,
// and it holds its own persistence retry loop for the control-plane
// record.
func (b *behavior) notifyRegistryIfTerminal(ctx context.Context) {
	state, err := b.currentState()
	if err != nil {
		b.log.WarnS(ctx, "Failed to inspect unroll terminal state", err)

		return
	}

	phase := phaseFromState(state)
	trulyTerminal := phase == PhaseFailed ||
		(phase == PhaseCompleted && b.sweepFinalized)
	if !trulyTerminal {
		return
	}

	job := stateJob(state)
	if !b.emitExitCostIfCompleted(ctx, phase, job) {

		// The exit-cost leg has not yet been durably handed to the
		// ledger. Defer the terminal handoff — which would stop this
		// child and retire the VTXO — so a later height tick retries
		// the emission. VTXO retirement has its own
		// startup-reconciliation backstop, so deferring it is safe.
		return
	}

	if b.cfg.RegistryRef == nil || b.terminalNotified {
		return
	}

	msg := &UnrollTerminatedMsg{
		Outpoint:            b.cfg.TargetOutpoint,
		ActorID:             b.cfg.ActorID,
		Phase:               phase,
		FailReason:          job.FailReason,
		HadOnChainFootprint: jobHadOnChainFootprint(job),
		ExitPolicyKind:      b.exitPolicyKind(),
	}

	if sweepTxid := effectiveSweepTxid(
		job.PlannerState, b.sweepTx,
	); sweepTxid != nil {

		msg.SweepTxid = sweepTxid
	}

	// Registry persistence is independent of the child transaction.
	// The durable child has already committed its local state before
	// this handoff, while the registry is a separate in-memory actor
	// that records terminal state from its own handler. Propagating
	// this tx cannot make the two actor updates atomic; it only hands
	// the registry a transaction handle that may be used after the
	// child's ExecTx closure commits or rolls back. Strip cancellation
	// too, so a caller disconnect cannot suppress the terminal handoff.
	notifyCtx := actor.WithoutTx(context.WithoutCancel(ctx))
	if err := b.cfg.RegistryRef.Tell(notifyCtx, msg); err != nil {
		b.log.WarnS(ctx, "Failed to notify unroll registry", err)

		return
	}

	b.terminalNotified = true
}

// emitExitCostIfCompleted sends the final unilateral exit miner fee to the
// ledger once the unroll sweep confirms. The ledger sink is a durable
// mailbox: a Tell that returns nil is durably accepted and processed
// at-least-once, and the ledger handler dedups by target outpoint so a
// post-crash or post-restart replay is a safe no-op.
//
// It returns false only when a transient delivery failure means the caller
// must defer the terminal registry handoff. Without that deferral a Tell
// that errors (e.g. mailbox backpressure) would be lost: the registry stops
// this child right after the handoff and terminal records are not restored
// on boot, so there would be no later opportunity to retry. Returning false
// keeps the child alive and subscribed, so the next height tick re-enters
// notifyRegistryIfTerminal and retries the emission.
//
// It returns true when there is nothing to defer for: the exit cost is not
// applicable (non-completed phase, already emitted, or no sink), it was
// delivered, or it is deterministically un-buildable. A completed actor
// restores its proof and sweep tx from the checkpoint, so a build failure is
// an internal inconsistency that retrying cannot fix — it is surfaced at
// error level and then allowed through so the terminal handoff is not wedged
// on every future block.
func (b *behavior) emitExitCostIfCompleted(ctx context.Context, phase Phase,
	job *JobState) bool {

	if phase != PhaseCompleted || b.exitCostNotified ||
		b.cfg.LedgerSink.IsNone() {
		return true
	}

	msg, err := b.exitCostMsg(job)
	if err != nil {
		b.log.ErrorS(ctx, "Unbuildable unroll exit cost on completed "+
			"actor", err)
		b.exitCostNotified = true

		return true
	}

	notifyCtx := actor.WithoutTx(context.WithoutCancel(ctx))
	if err := b.cfg.LedgerSink.UnsafeFromSome().Tell(
		notifyCtx, msg,
	); err != nil {

		b.log.WarnS(ctx, "Deferring unroll terminal handoff; ledger "+
			"exit-cost tell failed", err)

		return false
	}

	b.exitCostNotified = true

	return true
}

// exitCostMsg derives the ledger event from the proof target output and the
// persisted final sweep transaction.
func (b *behavior) exitCostMsg(job *JobState) (*ledger.ExitCostMsg, error) {
	if b.proof == nil {
		return nil, fmt.Errorf("missing proof")
	}

	if b.sweepTx == nil {
		return nil, fmt.Errorf("missing sweep transaction")
	}

	targetOutput, err := b.proof.TargetOutput()
	if err != nil {
		return nil, err
	}
	if targetOutput == nil || targetOutput.Value <= 0 {
		return nil, fmt.Errorf("invalid target output value %d",
			targetOutputValue(targetOutput))
	}

	outputValue := int64(0)
	for _, txOut := range b.sweepTx.TxOut {
		if txOut == nil || txOut.Value <= 0 {
			continue
		}

		outputValue += txOut.Value
	}

	exitCost := targetOutput.Value - outputValue
	if exitCost <= 0 {
		return nil, fmt.Errorf("non-positive exit cost %d", exitCost)
	}

	height := uint32(0)
	sweepState := job.PlannerState.Sweep
	if sweepState.ConfirmHeight.IsSome() {
		confirmHeight := sweepState.ConfirmHeight.UnsafeFromSome()
		if confirmHeight > 0 {
			height = uint32(confirmHeight)
		}
	}

	target := b.proof.TargetOutpoint()

	return &ledger.ExitCostMsg{
		OutpointHash:  target.Hash,
		OutpointIndex: target.Index,
		AmountSat:     targetOutput.Value,
		ExitCostSat:   exitCost,
		BlockHeight:   height,
	}, nil
}

// targetOutputValue returns the output value while tolerating nil pointers for
// error reporting.
func targetOutputValue(targetOutput *wire.TxOut) int64 {
	if targetOutput == nil {
		return 0
	}

	return targetOutput.Value
}

// actorIDForTarget derives a deterministic actor ID for one target outpoint.
func actorIDForTarget(target wire.OutPoint) string {
	return "unroll-" + target.String()
}

// ActorIDForTarget derives the durable actor ID for one target outpoint.
func ActorIDForTarget(target wire.OutPoint) string {
	return actorIDForTarget(target)
}

// copyPlannerState deep-copies one planner state for durable use. Option
// fields and SweepState are value types, so a struct assignment already
// produces an independent copy; only the slices need explicit copies.
func copyPlannerState(state unrollplan.State) unrollplan.State {
	copyState := unrollplan.State{
		ConfirmedTxids: append(
			[]chainhash.Hash(nil), state.ConfirmedTxids...,
		),
		InFlightTxids: append(
			[]chainhash.Hash(nil), state.InFlightTxids...,
		),
		TargetConfirmHeight: state.TargetConfirmHeight,
		Sweep:               state.Sweep,
	}

	sortHashes(copyState.ConfirmedTxids)
	sortHashes(copyState.InFlightTxids)

	return copyState
}

// copyDeferredCheckpoints deep-copies and sorts deferred checkpoint state.
func copyDeferredCheckpoints(
	checkpoints []DeferredCheckpoint) []DeferredCheckpoint {

	copyCheckpoints := append([]DeferredCheckpoint(nil), checkpoints...)
	sortDeferredCheckpoints(copyCheckpoints)

	return copyCheckpoints
}

// copyTx deep-copies one transaction when present.
func copyTx(tx *wire.MsgTx) *wire.MsgTx {
	if tx == nil {
		return nil
	}

	return tx.Copy()
}

// removeDeferredCheckpoint removes one deferred checkpoint when present.
func removeDeferredCheckpoint(checkpoints []DeferredCheckpoint,
	txid chainhash.Hash) []DeferredCheckpoint {

	result := make([]DeferredCheckpoint, 0, len(checkpoints))
	for _, checkpoint := range checkpoints {
		if checkpoint.Txid == txid {
			continue
		}

		result = append(result, checkpoint)
	}

	return result
}

// findDeferredCheckpoint returns the deferred checkpoint for txid if present.
func findDeferredCheckpoint(checkpoints []DeferredCheckpoint,
	txid chainhash.Hash) (DeferredCheckpoint, bool) {

	for _, checkpoint := range checkpoints {
		if checkpoint.Txid == txid {
			return checkpoint, true
		}
	}

	return DeferredCheckpoint{}, false
}

// appendDeferredCheckpoint appends a deferred checkpoint when absent.
func appendDeferredCheckpoint(checkpoints []DeferredCheckpoint,
	checkpoint DeferredCheckpoint) []DeferredCheckpoint {

	if _, ok := findDeferredCheckpoint(checkpoints, checkpoint.Txid); ok {
		return copyDeferredCheckpoints(checkpoints)
	}

	checkpoints = append(
		append(
			[]DeferredCheckpoint(nil), checkpoints...,
		),
		checkpoint,
	)
	sortDeferredCheckpoints(checkpoints)

	return checkpoints
}

// removeHash removes one hash when present.
func removeHash(hashes []chainhash.Hash, hash chainhash.Hash) []chainhash.Hash {
	result := make([]chainhash.Hash, 0, len(hashes))
	for _, current := range hashes {
		if current == hash {
			continue
		}

		result = append(result, current)
	}

	return result
}

// containsHash reports whether one hash is present in the slice.
func containsHash(hashes []chainhash.Hash, hash chainhash.Hash) bool {
	for _, current := range hashes {
		if current == hash {
			return true
		}
	}

	return false
}

// sortHashes sorts hashes deterministically by string form.
func sortHashes(hashes []chainhash.Hash) {
	sort.Slice(hashes, func(i, j int) bool {
		return hashes[i].String() < hashes[j].String()
	})
}

// sortDeferredCheckpoints sorts deferred checkpoints deterministically.
func sortDeferredCheckpoints(checkpoints []DeferredCheckpoint) {
	sort.Slice(checkpoints, func(i, j int) bool {
		iDeadline := checkpoints[i].DeadlineHeight
		jDeadline := checkpoints[j].DeadlineHeight
		if iDeadline != jDeadline {
			return iDeadline < jDeadline
		}

		iTxid := checkpoints[i].Txid.String()
		jTxid := checkpoints[j].Txid.String()

		return iTxid < jTxid
	})
}

// copyHash returns a heap-independent pointer copy of one hash.
func copyHash(hash *chainhash.Hash) *chainhash.Hash {
	if hash == nil {
		return nil
	}

	hashCopy := *hash

	return &hashCopy
}

// appendUniqueSorted appends missing hashes and returns deterministic order.
func appendUniqueSorted(hashes []chainhash.Hash,
	newHashes ...chainhash.Hash) []chainhash.Hash {

	result := append([]chainhash.Hash(nil), hashes...)
	for _, hash := range newHashes {
		if containsHash(result, hash) {
			continue
		}

		result = append(result, hash)
	}

	sortHashes(result)

	return result
}

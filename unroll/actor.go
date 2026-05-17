package unroll

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/lib/recovery"
	"github.com/lightninglabs/darepo-client/txconfirm"
	"github.com/lightninglabs/darepo-client/unrollplan"
	"github.com/lightninglabs/darepo-client/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// JobStore is the SQL persistence surface used by one per-target unroll FSM.
// The actor itself is in-memory; restart safety comes from loading and saving
// the target-keyed unroll_jobs row around every FSM transition.
type JobStore interface {
	UpsertJob(context.Context, db.UnrollJobRecord) error

	GetJob(context.Context, wire.OutPoint) (*db.UnrollJobRecord, error)

	MarkEffectDone(context.Context, string, string) error
}

// Config configures one per-target VTXO unroll actor.
type Config struct {
	// TargetOutpoint is the VTXO being unrolled.
	TargetOutpoint wire.OutPoint

	// JobStore provides SQL job persistence.
	JobStore JobStore

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
}

// VTXOUnrollActor wraps one in-memory per-target unroll actor.
type VTXOUnrollActor struct {
	ref     actor.ActorRef[Msg, Resp]
	runtime *actor.Actor[Msg, Resp]
	wg      *sync.WaitGroup
	stop    func()
}

// Ref returns the public actor reference.
func (a *VTXOUnrollActor) Ref() actor.ActorRef[Msg, Resp] {
	return a.ref
}

// Stop stops the underlying in-memory actor.
func (a *VTXOUnrollActor) Stop() {
	if a == nil {
		return
	}

	if a.stop != nil {
		a.stop()

		return
	}

	if a.runtime != nil {
		a.runtime.Stop()
	}
}

// StopAndWait stops the actor and waits for its processing loop to exit.
func (a *VTXOUnrollActor) StopAndWait(ctx context.Context) error {
	a.Stop()

	if a == nil || a.wg == nil {
		return nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.wg.Wait()
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()

	case <-done:
		return nil
	}
}

// NewVTXOUnrollActor creates and starts one VTXO unroll actor.
func NewVTXOUnrollActor(cfg Config) (*VTXOUnrollActor, error) {
	behavior := &behavior{
		cfg: cfg,
		log: cfg.Log.UnwrapOr(btclog.Disabled),
	}
	if err := behavior.restoreJob(context.Background()); err != nil {
		return nil, err
	}

	runtimeID := actorIDForTarget(cfg.TargetOutpoint)
	wg := &sync.WaitGroup{}
	runtime := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          runtimeID,
		Behavior:    behavior,
		MailboxSize: 128,
		Wg:          wg,
	})
	behavior.selfRef = runtime.TellRef()
	runtime.Start()

	return &VTXOUnrollActor{
		ref:     runtime.Ref(),
		runtime: runtime,
		wg:      wg,
		stop:    runtime.Stop,
	}, nil
}

// behavior is the local actor behavior for one target outpoint.
type behavior struct {
	cfg     Config
	log     btclog.Logger
	selfRef actor.TellOnlyRef[Msg]

	proof   *recovery.Proof
	planner *unrollplan.Planner
	desc    *vtxo.Descriptor
	session *Session
	pending *unrollSnapshot

	sweepTx           *wire.MsgTx
	blockSubActive    bool
	spendWatchActive  bool
	proofSpendWatches map[wire.OutPoint]struct{}
	terminalNotified  bool
}

// Receive processes one local actor message. It is the single entry
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
func (b *behavior) Receive(ctx context.Context, msg Msg) fn.Result[Resp] {
	switch m := msg.(type) {
	case *StartUnrollRequest:
		return b.handleEvent(ctx, &StartEvent{
			Height:  m.Height,
			Trigger: m.Trigger,
		})

	case *ResumeUnrollRequest:
		return b.handleEvent(ctx, &ResumeEvent{
			Height: m.Height,
		})

	case *HeightObservedMsg:
		return b.handleEvent(ctx, &HeightUpdatedEvent{
			Height: m.Height,
		})

	case *TxConfirmedMsg:
		return b.handleEvent(ctx, &TxConfirmedEvent{
			Txid:   m.Txid,
			Height: m.Height,
		})

	case *TxFailedMsg:
		return b.handleEvent(ctx, &TxFailedEvent{
			Txid:   m.Txid,
			Reason: b.failureReasonForTx(m.Txid, m.Reason),
		})

	case *SpendObservedMsg:
		return b.handleSpendObserved(ctx, m)

	case *GetStateRequest:
		return fn.Ok[Resp](b.stateResponse())

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
func (b *behavior) handleEvent(ctx context.Context,
	event Event) fn.Result[Resp] {

	if err := b.ensureLoaded(ctx); err != nil {
		return fn.Err[Resp](err)
	}

	if b.inTerminalState() {
		return fn.Ok[Resp](&AckResp{})
	}

	if err := b.driveEvent(ctx, event); err != nil {
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
//  2. Persist: write the resulting job to the delivery store
//     BEFORE doing any IO on the outbox. If the process crashes between
//     the FSM transition and the outbox routing, restart restores the
//     exact same state that was in memory and re-emits the outbox via
//     the reissue path, so no work is lost and no work is duplicated
//     beyond what txconfirm's txid-keyed dedup already collapses.
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
// Persist-before-route is the invariant that lets the actor lose its
// process mid-operation without corrupting on-chain state — every side
// effect is driven by a job that is already on disk.
func (b *behavior) driveEvent(ctx context.Context, event Event) error {
	if b.session == nil || b.session.FSM == nil {
		return fmt.Errorf("session not initialized")
	}

	outbox, err := b.session.FSM.AskEvent(ctx, event).Await(ctx).Unpack()
	if err != nil {
		return err
	}

	if err := b.persistJob(ctx); err != nil {
		return err
	}

	b.markRuntimeEffectDone(ctx, "subscribe-blocks")
	b.markRuntimeEffectDone(ctx, "watch-target-spend")

	if err := b.routeOutbox(ctx, outbox); err != nil {
		return err
	}

	b.notifyRegistryIfTerminal(ctx)

	return nil
}

// startSweep constructs the final timeout-path sweep, persists it, and
// hands it to txconfirm for broadcast-and-wait-for-confirmation.
//
// Ordering here is load-bearing. The guiding rule is: never cross the
// actor-boundary with a fresh sweep that the job has not yet
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
// The fix is to reuse b.sweepTx (possibly restored from the job
// via restoreJob) when it is already set, and to persist the
// job BEFORE asking txconfirm to broadcast. On restart the same
// transaction materializes from the job, txconfirm sees the same
// txid it has been tracking, and the Ask resolves as a benign no-op.
//
// If buildSweepTx itself fails (fee estimation, signing, malformed
// descriptor), we drive a SweepBuildFailedEvent through the FSM so the
// retry budget is accounted for and we reach terminal Failed after
// maxSweepAttempts.
func (b *behavior) startSweep(ctx context.Context) error {
	// Reuse the sweep tx restored from the job (or built on a
	// prior attempt inside this actor lifetime) so we converge on a
	// single sweep txid / wallet pkScript across retries.
	if b.sweepTx == nil {
		sweepTx, err := buildSweepTx(
			ctx, b.cfg.Wallet, b.cfg.ChainSource, b.proof, b.desc,
			b.cfg.MaxSweepFeeRateSatPerVByte,
		)
		if err != nil {
			return b.driveEvent(ctx, &SweepBuildFailedEvent{
				Reason: err.Error(),
			})
		}

		b.sweepTx = sweepTx
	}

	// Persist the built sweep before asking txconfirm to broadcast, so
	// on any retry the same sweepTx is restored and re-submitted under
	// txconfirm's dedup rather than a freshly-derived sweep racing it.
	if err := b.persistJob(ctx); err != nil {
		return err
	}

	sweepPkScript, err := safeTxOutPkScript(b.sweepTx, 0)
	if err != nil {
		return fmt.Errorf("sweep tx malformed: %w", err)
	}

	sweepLabel := "unroll-sweep-" + b.cfg.TargetOutpoint.String()

	_, err = b.cfg.TxConfirmRef.Ask(ctx, &txconfirm.EnsureConfirmedReq{
		Tx:                   b.sweepTx,
		ConfirmationPkScript: sweepPkScript,
		Label:                sweepLabel,
		Subscriber:           b.notificationRef(),
	}).Await(ctx).Unpack()
	if err != nil {
		return err
	}

	b.markRuntimeEffectDone(ctx, "build-sweep")

	sweepTxid := b.sweepTx.TxHash()

	return b.driveEvent(ctx, &SweepBroadcastedEvent{Txid: sweepTxid})
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

// proofNodeHeightHint is the earliest safe confirmation height hint for
// proof-graph transactions. Roots and intermediate OOR job ancestors
// can confirm before the target descriptor's CreatedHeight, so proof watches
// must not use the target creation height as a lower bound.
const proofNodeHeightHint uint32 = 1

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
func (b *behavior) ensureNodeConfirmed(ctx context.Context, txid chainhash.Hash,
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

	resp, err := b.cfg.TxConfirmRef.Ask(ctx, &txconfirm.EnsureConfirmedReq{
		Tx:                   node.Tx,
		ConfirmationPkScript: pkScript,
		Label:                "unroll-node-" + txid.String(),
		HeightHint:           proofNodeHeightHint,
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
		return b.driveEvent(ctx, &TxFailedEvent{
			Txid: txid,
			Reason: b.failureReasonForTx(
				txid, "txconfirm returned failed state",
			),
		})
	}

	return nil
}

// watchDeferredCheckpoint registers a confirmation watch for a ready
// fraud-triggered job while the actor waits for the operator to confirm
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

	txidCopy := txid
	_, err = b.cfg.ChainSource.Ask(ctx, &chainsource.RegisterConfRequest{
		CallerID:    b.deferredCheckpointCallerID(),
		Txid:        &txidCopy,
		PkScript:    append([]byte(nil), pkScript...),
		TargetConfs: 1,
		HeightHint:  proofNodeHeightHint,
		NotifyActor: fn.Some(notifyRef),
	}).Await(ctx).Unpack()
	if err != nil {
		return err
	}

	return nil
}

// deferredCheckpointCallerID returns the stable confirmation-watch caller ID
// used for all deferred jobs in this actor.
func (b *behavior) deferredCheckpointCallerID() string {
	return actorIDForTarget(b.cfg.TargetOutpoint) + "-deferred-job"
}

// stateResponse builds the current state response for callers and tests.
func (b *behavior) stateResponse() *GetStateResp {
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
		Started:      !isIdleState(state),
		Trigger:      stateTrigger(state),
		Height:       stateHeight(state),
		Phase:        phaseFromState(state),
		PlannerState: copyPlannerState(job.PlannerState),
		FailReason:   job.FailReason,
	}

	if sweepTxid != nil {
		txid := *sweepTxid
		resp.SweepTxid = &txid
	}

	return resp
}

// ensureLoaded lazily constructs every piece of actor-lifetime state the
// FSM needs to make progress: the immutable recovery proof, the VTXO
// descriptor, the pure planner, the protofsm session, the block epoch
// subscription, and the target-outpoint spend watch.
//
// The load is lazy (not done in NewVTXOUnrollActor) for two reasons:
//
//   - On restore, the job has already been pulled from the
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
// PlannerState.Validate call catches job/proof drift (e.g. an
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

	if b.session == nil {
		initialState := State(&Idle{})
		if b.pending != nil && b.pending.Started {
			initialState = stateFromSnapshot(b.pending)
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
// ([txconfirm.Notification]) while this actor accepts [Msg] variants. This
// helper threads a [chainsource.MapNotification]-style adapter: every
// txconfirm notification is synchronously re-wrapped into the matching actor
// message (TxConfirmedMsg / TxFailedMsg) and forwarded to our self-ref. An
// unknown notification type is mapped to a generic TxFailedMsg so the actor
// still terminates loudly instead of silently dropping the callback.
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

// restoreJob restores durable state from SQL.
func (b *behavior) restoreJob(ctx context.Context) error {
	if b.cfg.JobStore == nil {
		return fmt.Errorf("unroll job store must be provided")
	}

	record, err := b.cfg.JobStore.GetJob(ctx, b.cfg.TargetOutpoint)
	if err != nil {
		if errors.Is(err, db.ErrUnrollJobNotFound) {
			return nil
		}

		return err
	}

	if record == nil {
		return nil
	}

	snapshot, err := snapshotFromJobRecord(record)
	if err != nil {
		return err
	}

	b.pending = snapshot
	b.sweepTx = copyTx(snapshot.SweepTx)

	return nil
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

	_, err = b.cfg.ChainSource.Ask(
		ctx, &chainsource.RegisterSpendRequest{
			CallerID:    b.spendCallerID(),
			Outpoint:    &targetOutpoint,
			PkScript:    pkScript,
			HeightHint:  uint32(b.desc.CreatedHeight),
			NotifyActor: fn.Some(notifyRef),
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
	msg *SpendObservedMsg) fn.Result[Resp] {

	if err := b.ensureLoaded(ctx); err != nil {
		return fn.Err[Resp](err)
	}

	if b.inTerminalState() {
		return fn.Ok[Resp](&AckResp{})
	}

	if b.proof != nil {
		acked, err := b.ackProofOutputSpend(ctx, msg)
		if err != nil {
			return fn.Err[Resp](err)
		}
		if acked {
			return fn.Ok[Resp](&AckResp{})
		}

		// Case 2: the spender is a proof-graph node. That means an
		// ancestor of our target just confirmed on chain.
		if _, ok := b.proof.Node(msg.SpendingTxid); ok {
			return b.handleEvent(ctx, &TxConfirmedEvent{
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
			return b.handleEvent(ctx, &HeightUpdatedEvent{
				Height: msg.SpendingHeight,
			})
		}
	}

	// Case 4: neither of the above. Someone else spent the watched output.
	// This happens if the operator cooperatively claimed the VTXO,
	// if a reorg replaced history, or in fraud scenarios. There is
	// no way for this unroll to proceed, so terminate with a
	// reason string that identifies the spender for operator
	// triage.
	spentOutpoint := b.cfg.TargetOutpoint
	if msg.Outpoint != (wire.OutPoint{}) {
		spentOutpoint = msg.Outpoint
	}
	reason := fmt.Sprintf("watched outpoint %s spent externally by tx %s "+
		"at height %d", spentOutpoint, msg.SpendingTxid,
		msg.SpendingHeight)

	return b.handleEvent(ctx, &FailEvent{Reason: reason})
}

// ackProofOutputSpend records proof-output spend evidence and reports whether
// the observation is fully handled. If a proof output is spent by an unknown
// transaction, the parent proof tx is still confirmed, but the caller must
// continue into the external-spend failure path.
func (b *behavior) ackProofOutputSpend(ctx context.Context,
	msg *SpendObservedMsg) (bool, error) {

	if msg.Outpoint == (wire.OutPoint{}) ||
		msg.Outpoint == b.cfg.TargetOutpoint {
		return false, nil
	}

	if _, ok := b.proof.Node(msg.Outpoint.Hash); !ok {
		return false, nil
	}

	err := b.driveEvent(ctx, &TxConfirmedEvent{
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

	return true, b.driveEvent(ctx, event)
}

// inTerminalState reports whether the FSM has already reached a terminal
// phase. Late chain notifications can arrive after completion while the
// registry is draining the child for cleanup; those observations are already
// reflected in the terminal job and should ack as idempotent no-ops.
func (b *behavior) inTerminalState() bool {
	state, err := b.currentState()
	if err != nil {
		return false
	}

	return state.IsTerminal()
}

// persistJob writes the current SQL job.
func (b *behavior) persistJob(ctx context.Context) error {
	state, err := b.currentState()
	if err != nil {
		return err
	}

	snapshot := snapshotFromState(state, b.sweepTx)
	record, err := jobRecordFromSnapshot(b.cfg.TargetOutpoint, snapshot)
	if err != nil {
		return err
	}

	err = b.cfg.JobStore.UpsertJob(ctx, *record)
	if err != nil {
		return err
	}

	b.pending = snapshot

	return nil
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
//     node is a bug in the proof-vs-job alignment (not a runtime
//     condition) so it surfaces as a hard error instead of being
//     silently skipped.
//
//   - ReissueInFlightTransactions: restart path. The FSM restored
//     InFlightTxids from the job and needs each one re-submitted
//     to txconfirm so the shared actor re-attaches its subscription.
//     Same node-missing rule applies — a silent skip would leave the FSM
//     permanently waiting on a confirmation that was never re-armed.
//
//   - RequestSweepBuild: the planner says the target has matured and the
//     final sweep can be constructed. Delegates to startSweep for the
//     persist-then-broadcast dance.
//
//   - ReissueSweepConfirmation: restart path for a sweep that was
//     already broadcast before the crash. The job carried the
//     sweep tx, so we re-submit it to txconfirm (idempotent via dedup)
//     to re-attach the confirmation subscription. A nil sweepTx here
//     means the job is corrupt and we fail loudly rather than
//     silently losing the job.
func (b *behavior) routeOutbox(ctx context.Context,
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

				err := b.ensureNodeConfirmed(ctx, txid, node)
				if err != nil {
					return err
				}
				b.markRuntimeEffectDone(
					ctx,
					"ensure-tx-"+hashEffectSuffix(txid),
				)
			}

		case *ReissueInFlightTransactions:
			for _, txid := range evt.Txids {
				node, ok := b.proof.Node(txid)
				if !ok {

					// A missing node on reissue means
					// the job referenced a
					// transaction our current proof no
					// longer knows about; silently
					// skipping would leave the FSM
					// waiting on a txconfirm
					// subscription that was never
					// re-registered.
					return fmt.Errorf("proof node %s "+
						"missing on reissue", txid)
				}

				err := b.ensureNodeConfirmed(ctx, txid, node)
				if err != nil {
					return err
				}
				b.markRuntimeEffectDone(
					ctx,
					"ensure-tx-"+hashEffectSuffix(txid),
				)
			}

		case *WatchDeferredCheckpoints:
			for _, txid := range evt.Txids {
				node, ok := b.proof.Node(txid)
				if !ok {
					return fmt.Errorf("proof node %s "+
						"missing for deferred "+
						"job watch", txid)
				}

				err := b.watchDeferredCheckpoint(
					ctx, txid, node,
				)
				if err != nil {
					return err
				}
				b.markRuntimeEffectDone(
					ctx, "watch-deferred-"+
						hashEffectSuffix(txid),
				)
			}

		case *RequestSweepBuild:
			if err := b.startSweep(ctx); err != nil {
				return err
			}

		case *ReissueSweepConfirmation:
			if b.sweepTx == nil {

				// The FSM asked us to re-arm the sweep
				// confirmation watcher, so the job
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

			sweepTxid := b.sweepTx.TxHash()
			b.markRuntimeEffectDone(
				ctx,
				"ensure-sweep-"+hashEffectSuffix(sweepTxid),
			)
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
				return stateFromSnapshot(b.pending), nil
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
		return stateFromSnapshot(b.pending), nil
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
// job or the current FSM state) and annotates accordingly.
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

// notifyRegistryIfTerminal forwards one UnrollTerminatedMsg to the
// registry when the FSM reaches Completed or Failed, at most once per
// actor lifetime.
//
// The registry uses this to move the outpoint out of its active map and
// mark the durable store terminal. If the FSM receives additional events
// after reaching terminal (e.g. a late TxConfirmed for a proof node that
// materialized before we failed for other reasons) terminalNotified
// keeps us from spamming the registry with repeats.
//
// Failure to Tell is warned but not fatal — the registry will rediscover
// the terminal phase the next time it queries child state, and it holds
// its own persistence retry loop for the control-plane record.
func (b *behavior) notifyRegistryIfTerminal(ctx context.Context) {
	if b.cfg.RegistryRef == nil || b.terminalNotified {
		return
	}

	state, err := b.currentState()
	if err != nil {
		b.log.WarnS(ctx, "Failed to inspect unroll terminal state", err)

		return
	}

	phase := phaseFromState(state)
	if phase != PhaseCompleted && phase != PhaseFailed {
		return
	}

	job := stateJob(state)
	msg := &UnrollTerminatedMsg{
		Outpoint:   b.cfg.TargetOutpoint,
		ActorID:    actorIDForTarget(b.cfg.TargetOutpoint),
		Phase:      phase,
		FailReason: job.FailReason,
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

func (b *behavior) markRuntimeEffectDone(ctx context.Context, suffix string) {
	if b.cfg.JobStore == nil {
		return
	}

	effectID := "unroll/" + b.cfg.TargetOutpoint.String() + "/" + suffix
	if err := b.cfg.JobStore.MarkEffectDone(ctx, effectID, ""); err != nil {
		b.log.WarnS(ctx, "Failed to mark unroll effect done",
			err,
			slog.String("effect_id", effectID),
		)
	}
}

func hashEffectSuffix(hash chainhash.Hash) string {
	return fmt.Sprintf("%x", hash[:])
}

// actorIDForTarget derives a deterministic actor ID for one target outpoint.
func actorIDForTarget(target wire.OutPoint) string {
	return "unroll-" + target.String()
}

// ActorIDForTarget derives the local actor ID for one target outpoint.
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

// copyDeferredCheckpoints deep-copies and sorts deferred job state.
func copyDeferredCheckpoints(jobs []DeferredCheckpoint) []DeferredCheckpoint {
	copyJobs := append([]DeferredCheckpoint(nil), jobs...)
	sortDeferredCheckpoints(copyJobs)

	return copyJobs
}

// copyTx deep-copies one transaction when present.
func copyTx(tx *wire.MsgTx) *wire.MsgTx {
	if tx == nil {
		return nil
	}

	return tx.Copy()
}

// removeDeferredCheckpoint removes one deferred job when present.
func removeDeferredCheckpoint(jobs []DeferredCheckpoint,
	txid chainhash.Hash) []DeferredCheckpoint {

	result := make([]DeferredCheckpoint, 0, len(jobs))
	for _, job := range jobs {
		if job.Txid == txid {
			continue
		}

		result = append(result, job)
	}

	return result
}

// findDeferredCheckpoint returns the deferred job for txid if present.
func findDeferredCheckpoint(jobs []DeferredCheckpoint,
	txid chainhash.Hash) (DeferredCheckpoint, bool) {

	for _, job := range jobs {
		if job.Txid == txid {
			return job, true
		}
	}

	return DeferredCheckpoint{}, false
}

// appendDeferredCheckpoint appends a deferred job when absent.
func appendDeferredCheckpoint(jobs []DeferredCheckpoint,
	job DeferredCheckpoint) []DeferredCheckpoint {

	if _, ok := findDeferredCheckpoint(jobs, job.Txid); ok {
		return copyDeferredCheckpoints(jobs)
	}

	jobs = append(
		append(
			[]DeferredCheckpoint(nil), jobs...,
		),
		job,
	)
	sortDeferredCheckpoints(jobs)

	return jobs
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

// sortDeferredCheckpoints sorts deferred jobs deterministically.
func sortDeferredCheckpoints(jobs []DeferredCheckpoint) {
	sort.Slice(jobs, func(i, j int) bool {
		iDeadline := jobs[i].DeadlineHeight
		jDeadline := jobs[j].DeadlineHeight
		if iDeadline != jDeadline {
			return iDeadline < jDeadline
		}

		iTxid := jobs[i].Txid.String()
		jTxid := jobs[j].Txid.String()

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

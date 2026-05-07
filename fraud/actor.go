// Package fraud implements the server-side fraud response actor.
package fraud

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/txconfirm"
	"github.com/lightninglabs/darepo/batchwatcher"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	// Subsystem is the logging subsystem label for fraud response.
	Subsystem = "FRAD"

	// ServiceKeyName is the receptionist key used for the fraud responder.
	ServiceKeyName = "fraud-responder"

	// CheckpointLabel is the txconfirm label for OOR checkpoint broadcasts.
	CheckpointLabel = "fraud-oor-checkpoint"

	// CheckpointSweepLabel is the txconfirm label for checkpoint timeout
	// sweep broadcasts.
	CheckpointSweepLabel = "fraud-oor-checkpoint-sweep"
)

// ResponsePlan is the ordered set of transactions needed to complete one
// fraud response.
type ResponsePlan struct {
	// Ancestors are optional parent transactions that must confirm before
	// ResponseTx can confirm. They must be ordered from roots to leaves.
	Ancestors []*wire.MsgTx

	// ResponseTx is the transaction that races the fraudulent spend.
	ResponseTx *wire.MsgTx

	// Label is used in txconfirm logs.
	Label string
}

// Planner resolves a classified spend notification into a broadcast plan.
type Planner interface {
	// PlanResponse returns the ordered response plan for notification.
	PlanResponse(ctx context.Context,
		notif *batchwatcher.UnexpectedSpendNotification) (
		*ResponsePlan, error)
}

// SweepBuilder constructs the operator checkpoint timeout sweep.
type SweepBuilder func(context.Context,
	*CheckpointSweepRequest) (*wire.MsgTx, error)

// Config configures the fraud response actor.
type Config struct {
	// TxConfirmRef broadcasts and confirms response transactions.
	TxConfirmRef actor.ActorRef[txconfirm.Msg, txconfirm.Resp]

	// Planner resolves response transactions and optional ancestry.
	Planner Planner

	// CheckpointPlanner resolves spent VTXO on-chain notifications into
	// checkpoint response jobs.
	CheckpointPlanner *CheckpointPlanner

	// CheckpointSweepStore loads persisted checkpoint output metadata used
	// to reconstruct timeout sweeps.
	CheckpointSweepStore CheckpointSweepStore

	// CheckpointPolicy is the operator checkpoint output policy.
	CheckpointPolicy arkscript.CheckpointPolicy

	// OperatorKey signs the checkpoint timeout leaf.
	OperatorKey keychain.KeyDescriptor

	// Signer signs checkpoint timeout sweep inputs.
	Signer input.Signer

	// NewSweepPkScript returns a fresh server-controlled destination.
	NewSweepPkScript func(context.Context) ([]byte, error)

	// BuildSweep builds the checkpoint timeout sweep. Tests may override
	// this while production uses BuildCheckpointTimeoutSweep.
	BuildSweep SweepBuilder

	// Log is an optional logger.
	Log fn.Option[btclog.Logger]
}

// Actor consumes batchwatcher fraud notifications and drives txconfirm.
//
// Three maps coordinate in-flight work; their keys differ on purpose:
//
//   - pending[txTxid] -> *job: the txconfirm round-trip index. Every tx
//     handed to txconfirm has an entry here, removed on TxConfirmed /
//     TxFailed. Owns no semantics beyond "we're waiting on this txid".
//
//   - checkpointsByTxid[checkpointTxid] -> *checkpointJob: dedups
//     checkpoint submissions per checkpoint txid. The same checkpoint
//     can be requested via VTXOOnChain and (separately) classified
//     SpentLeaf — both paths converge here.
//
//   - sweepsByOutput[checkpointOutpoint] -> sweepTxid: dedups timeout
//     sweep submissions per checkpoint output. Separate from the
//     checkpoint dedup because one confirmed checkpoint can later
//     produce its own (distinct) sweep job.
//
// Invariants the handlers preserve:
//
//   - For a checkpoint job, both pending[checkpointTxid] and
//     checkpointsByTxid[checkpointTxid] are populated together and
//     cleared together.
//   - For a sweep job, both pending[sweepTxid] and
//     sweepsByOutput[outpoint] are populated together and cleared
//     together.
//   - Dedup entries (checkpointsByTxid / sweepsByOutput) are added
//     AFTER txconfirm has accepted the submission so a synchronous Ask
//     failure cannot strand a future re-notification.
type Actor struct {
	cfg Config
	log btclog.Logger

	notifyRef actor.TellOnlyRef[txconfirm.Notification]

	pending map[chainhash.Hash]*job

	checkpointsByTxid map[chainhash.Hash]*checkpointJob
	sweepsByOutput    map[wire.OutPoint]chainhash.Hash
}

// job tracks a single ordered sequence of transactions handed to
// txconfirm. Most jobs have one tx (a checkpoint or a sweep), but the
// legacy fraud-response path can ship optional ancestor txs ahead of
// the response tx; index walks the sequence as each tx confirms.
//
// stage classifies the lifecycle the job belongs to so handleTxConfirmed
// and handleTxFailed can route to the right handler. A non-nil
// checkpoint pointer carries the per-checkpoint state shared between
// the checkpoint broadcast stage and its eventual timeout-sweep stage.
type job struct {
	// id is a stable string identifier used for log correlation
	// (typically the input outpoint or checkpoint output outpoint).
	id string

	// txs is the ordered list of transactions to broadcast. Index 0
	// must confirm before index 1, etc.
	txs []*wire.MsgTx

	// index is the next tx in txs to broadcast; advanced after each
	// txconfirm.TxConfirmed.
	index int

	// label is the txconfirm label propagated to logs and metrics.
	label string

	// checkpoint is the shared per-checkpoint state when this job
	// belongs to the checkpoint or checkpoint-sweep lifecycle. nil for
	// jobStageLegacy jobs.
	checkpoint *checkpointJob

	// stage selects which TxConfirmed/TxFailed handler runs.
	stage jobStage
}

// jobStage classifies which lifecycle a job participates in. The fraud
// actor uses it to dispatch txconfirm callbacks to the right handler.
type jobStage uint8

const (
	// jobStageLegacy is the generic fraud-response path: forfeit-leaf
	// or missed-branch-tx broadcasts driven from
	// UnexpectedSpendNotification. Multi-tx ancestors land here too.
	jobStageLegacy jobStage = iota

	// jobStageCheckpoint is the OOR checkpoint broadcast stage. Its
	// confirmation arms the CSV countdown stored on checkpointJob.
	jobStageCheckpoint

	// jobStageCheckpointSweep is the operator timeout sweep stage that
	// fires once the checkpoint output has aged through CSV maturity
	// without a recipient ark tx racing to spend it.
	jobStageCheckpointSweep
)

// checkpointJob holds the per-checkpoint state shared across the
// checkpoint broadcast and its eventual timeout-sweep follow-up. One
// instance is created per spent OOR input and persists across both
// stages so the sweep can reuse the confirmation height already
// observed for the checkpoint.
type checkpointJob struct {
	// inputOutpoint is the original spent OOR VTXO input that this
	// checkpoint protects.
	inputOutpoint wire.OutPoint

	// checkpointTx is the finalized OOR checkpoint transaction.
	checkpointTx *wire.MsgTx

	// checkpointTxid caches checkpointTx.TxHash() for dedup keying.
	checkpointTxid chainhash.Hash

	// outputOutpoint is the checkpoint output (output 0) that the
	// timeout sweep eventually consumes.
	outputOutpoint wire.OutPoint

	// confirmed is set once the checkpoint tx has at least one
	// confirmation; arms the CSV maturity countdown.
	confirmed bool

	// confirmationHeight is the block height at which the checkpoint
	// tx confirmed. Zero until confirmed is true.
	confirmationHeight int32

	// maturityHeight is the first block height at which the operator
	// timeout leaf is spendable (confirmationHeight + CSVDelay).
	maturityHeight int32

	// sweeping indicates that an operator timeout sweep has been
	// submitted for outputOutpoint.
	sweeping bool

	// complete is set once the sweep has confirmed; this is the
	// terminal state for the job.
	complete bool

	// sweepTxid is the txid of the submitted timeout sweep, used as
	// the key into Actor.pending.
	sweepTxid chainhash.Hash
}

// NewActor creates a fraud response actor. Required Config fields are
// validated up front so a misconfigured operator deployment fails loudly
// at construction time rather than silently dropping notifications until
// the first runtime touch surfaces a nil dereference.
//
// Optional fields with sensible defaults: Planner (DefaultPlanner),
// BuildSweep (BuildCheckpointTimeoutSweep), Log (btclog.Disabled).
func NewActor(cfg Config) (*Actor, error) {
	if cfg.TxConfirmRef == nil {
		return nil, fmt.Errorf("fraud: TxConfirmRef is required")
	}
	if cfg.CheckpointPlanner == nil {
		return nil, fmt.Errorf("fraud: CheckpointPlanner is required")
	}
	if cfg.CheckpointSweepStore == nil {
		return nil, fmt.Errorf("fraud: CheckpointSweepStore is " +
			"required")
	}
	if cfg.NewSweepPkScript == nil {
		return nil, fmt.Errorf("fraud: NewSweepPkScript is required")
	}
	if cfg.Signer == nil {
		return nil, fmt.Errorf("fraud: Signer is required")
	}
	if cfg.OperatorKey.PubKey == nil {
		return nil, fmt.Errorf("fraud: OperatorKey.PubKey is required")
	}
	if cfg.CheckpointPolicy.OperatorKey == nil {
		return nil, fmt.Errorf(
			"fraud: CheckpointPolicy.OperatorKey is required",
		)
	}

	if cfg.Planner == nil {
		cfg.Planner = DefaultPlanner{}
	}
	if cfg.BuildSweep == nil {
		cfg.BuildSweep = BuildCheckpointTimeoutSweep
	}

	return &Actor{
		cfg:               cfg,
		log:               cfg.Log.UnwrapOr(btclog.Disabled),
		pending:           make(map[chainhash.Hash]*job),
		checkpointsByTxid: make(map[chainhash.Hash]*checkpointJob),
		sweepsByOutput:    make(map[wire.OutPoint]chainhash.Hash),
	}, nil
}

// SetNotificationRef sets the mapped txconfirm notification subscriber.
func (a *Actor) SetNotificationRef(
	ref actor.TellOnlyRef[txconfirm.Notification]) {

	a.notifyRef = ref
}

// Receive processes fraud notifications and txconfirm terminal callbacks.
func (a *Actor) Receive(ctx context.Context,
	msg actor.Message) fn.Result[actor.Message] {

	a.log.TraceS(ctx, "Fraud actor received message",
		"msg_type", fmt.Sprintf("%T", msg))

	switch m := msg.(type) {
	case *batchwatcher.VTXOOnChainNotification:
		if err := a.handleVTXOOnChain(ctx, m); err != nil {
			return fn.Err[actor.Message](err)
		}

	case *batchwatcher.UnexpectedSpendNotification:
		if err := a.handleUnexpectedSpend(ctx, m); err != nil {
			return fn.Err[actor.Message](err)
		}

	case *batchwatcher.CheckpointSweepNotification:
		if err := a.handleCheckpointSweepRequest(ctx, m); err != nil {
			return fn.Err[actor.Message](err)
		}

	case *txconfirm.TxConfirmed:
		if err := a.handleTxConfirmed(ctx, m); err != nil {
			return fn.Err[actor.Message](err)
		}

	case *txconfirm.TxFailed:
		a.handleTxFailed(ctx, m)

	default:
		return fn.Err[actor.Message](fmt.Errorf(
			"unknown fraud response message: %T", msg,
		))
	}

	return fn.Ok[actor.Message](nil)
}

// handleVTXOOnChain starts a checkpoint response for spent VTXOs.
func (a *Actor) handleVTXOOnChain(ctx context.Context,
	msg *batchwatcher.VTXOOnChainNotification) error {

	// CheckpointPlanner is guaranteed non-nil by NewActor's validation.
	plan, actionable, err := a.cfg.CheckpointPlanner.PlanCheckpoint(
		ctx, msg,
	)
	if err != nil {
		return fmt.Errorf("plan checkpoint response: %w", err)
	}
	if !actionable {
		return nil
	}

	txid := plan.CheckpointTx.TxHash()
	if existing, ok := a.checkpointsByTxid[txid]; ok {
		a.log.DebugS(ctx, "Checkpoint response already active",
			"input", msg.VTXOOutpoint,
			"checkpoint_tx", txid,
			"complete", existing.complete)

		return nil
	}

	outputOutpoint := wire.OutPoint{Hash: txid, Index: 0}
	j := &checkpointJob{
		inputOutpoint:  msg.VTXOOutpoint,
		checkpointTx:   plan.CheckpointTx,
		checkpointTxid: txid,
		outputOutpoint: outputOutpoint,
	}

	a.log.InfoS(ctx, "Starting OOR checkpoint response",
		"input", msg.VTXOOutpoint,
		"checkpoint_tx", txid)

	// Mirror the symmetric handleCheckpointSweepRequest ordering: only
	// register the dedup entry after ensureCheckpoint has accepted the
	// submission. A synchronous Ask failure (transient txconfirm error,
	// package-relay backend down at startup, logger error) must not
	// leave a dead entry in checkpointsByTxid — every subsequent
	// VTXOOnChainNotification for the same input would then be silently
	// deduped and the operator would never broadcast the checkpoint.
	if err := a.ensureCheckpoint(ctx, j); err != nil {
		return err
	}

	a.checkpointsByTxid[txid] = j

	return nil
}

// handleUnexpectedSpend starts the response for actionable classifications.
func (a *Actor) handleUnexpectedSpend(ctx context.Context,
	msg *batchwatcher.UnexpectedSpendNotification) error {

	if !actionable(msg) {
		return nil
	}

	plan, err := a.cfg.Planner.PlanResponse(ctx, msg)
	if err != nil {
		return fmt.Errorf("plan fraud response: %w", err)
	}
	if plan == nil || plan.ResponseTx == nil {
		return fmt.Errorf("missing response tx for %s",
			msg.Classification)
	}

	responseTxid := plan.ResponseTx.TxHash()
	if existing, ok := a.checkpointsByTxid[responseTxid]; ok {
		a.log.DebugS(ctx,
			"Skipping duplicate unexpected-spend submission for "+
				"tracked checkpoint",
			"classification", msg.Classification.String(),
			"outpoint", msg.TrackedOutput.Outpoint,
			"checkpoint_tx", responseTxid,
			"confirmed", existing.confirmed)

		return nil
	}

	txs := make([]*wire.MsgTx, 0, len(plan.Ancestors)+1)
	txs = append(txs, plan.Ancestors...)
	txs = append(txs, plan.ResponseTx)

	label := plan.Label
	if label == "" {
		label = fmt.Sprintf("fraud-%s", msg.Classification)
	}

	j := &job{
		id:    msg.TrackedOutput.Outpoint.String(),
		txs:   txs,
		label: label,
		stage: jobStageLegacy,
	}

	a.log.InfoS(ctx, "Starting fraud response",
		"classification", msg.Classification.String(),
		"outpoint", msg.TrackedOutput.Outpoint,
		"response_tx", plan.ResponseTx.TxHash(),
		"tx_count", len(txs))

	return a.submitNextTxn(ctx, j)
}

// handleTxConfirmed advances a response job after one tx confirms.
func (a *Actor) handleTxConfirmed(ctx context.Context,
	msg *txconfirm.TxConfirmed) error {

	j, ok := a.pending[msg.Txid]
	if !ok {
		// A stale TxConfirmed for an entry we already cleared (e.g.
		// a TxFailed raced ahead, or the actor restarted between
		// submission and confirmation). Trace-level so it shows up
		// when investigating a missing job but does not noise the
		// happy path.
		a.log.TraceS(ctx, "TxConfirmed for unknown pending tx",
			"txid", msg.Txid, "height", msg.BlockHeight)

		return nil
	}
	delete(a.pending, msg.Txid)

	switch j.stage {
	case jobStageCheckpoint:
		return a.handleCheckpointConfirmed(ctx, j.checkpoint, msg)

	case jobStageCheckpointSweep:
		a.handleCheckpointSweepConfirmed(ctx, j.checkpoint, msg)
		return nil
	}

	j.index++
	if j.index == len(j.txs) {
		a.log.InfoS(ctx, "Fraud response confirmed",
			"job", j.id,
			"txid", msg.Txid,
			"height", msg.BlockHeight)

		return nil
	}

	return a.submitNextTxn(ctx, j)
}

// handleTxFailed logs a terminal txconfirm failure.
//
// Both broadcast paths (checkpoint and checkpoint sweep) clear their
// dedup index here so that the same input/output can be retried
// within the lifetime of the daemon — otherwise a single async
// failure would silently strand the response until the next restart
// even though batchwatcher will keep re-notifying.
func (a *Actor) handleTxFailed(ctx context.Context, msg *txconfirm.TxFailed) {
	j, ok := a.pending[msg.Txid]
	if ok {
		delete(a.pending, msg.Txid)
	}
	if ok && j.checkpoint != nil {
		switch j.stage {
		case jobStageCheckpoint:
			delete(
				a.checkpointsByTxid,
				j.checkpoint.checkpointTxid,
			)

		case jobStageCheckpointSweep:
			delete(
				a.sweepsByOutput,
				j.checkpoint.outputOutpoint,
			)
		}
	}

	a.log.WarnS(ctx, "Fraud response transaction failed",
		nil, "job", jobID(j), "txid", msg.Txid, "reason", msg.Reason)
}

// handleCheckpointConfirmed records checkpoint confirmation and schedules the
// timeout sweep after CSV maturity.
func (a *Actor) handleCheckpointConfirmed(ctx context.Context,
	j *checkpointJob, msg *txconfirm.TxConfirmed) error {

	if j == nil {
		return nil
	}

	j.confirmed = true
	j.confirmationHeight = msg.BlockHeight
	j.maturityHeight = msg.BlockHeight + int32(
		a.cfg.CheckpointPolicy.CSVDelay,
	)

	a.log.InfoS(ctx, "OOR checkpoint confirmed",
		"input", j.inputOutpoint,
		"checkpoint_tx", j.checkpointTxid,
		"height", msg.BlockHeight,
		"maturity_height", j.maturityHeight)

	return nil
}

// handleCheckpointSweepConfirmed marks a checkpoint timeout job complete.
func (a *Actor) handleCheckpointSweepConfirmed(ctx context.Context,
	j *checkpointJob, msg *txconfirm.TxConfirmed) {

	if j == nil {
		return
	}

	j.complete = true
	a.log.InfoS(ctx, "OOR checkpoint sweep confirmed",
		"input", j.inputOutpoint,
		"checkpoint_output", j.outputOutpoint,
		"sweep_tx", msg.Txid,
		"height", msg.BlockHeight)
}

// handleCheckpointSweepRequest submits the operator timeout sweep for a
// checkpoint output that batchwatcher has kept in the active frontier until
// CSV maturity.
func (a *Actor) handleCheckpointSweepRequest(ctx context.Context,
	msg *batchwatcher.CheckpointSweepNotification) error {

	if msg == nil {
		return fmt.Errorf("checkpoint sweep notification is nil")
	}
	if existing, ok := a.sweepsByOutput[msg.CheckpointOutpoint]; ok {
		a.log.DebugS(ctx, "Checkpoint sweep already active",
			"input", msg.InputOutpoint,
			"checkpoint_output", msg.CheckpointOutpoint,
			"sweep_tx", existing)

		return nil
	}
	if a.cfg.CheckpointSweepStore == nil {
		return fmt.Errorf("checkpoint sweep store not configured")
	}
	if a.cfg.NewSweepPkScript == nil {
		return fmt.Errorf("checkpoint sweep destination not configured")
	}

	info, found, err := a.cfg.CheckpointSweepStore.
		LoadCheckpointSweepInfoByInput(ctx, msg.InputOutpoint)
	if err != nil {
		return fmt.Errorf("load checkpoint sweep info: %w", err)
	}
	if !found {
		return fmt.Errorf("checkpoint sweep info missing for %s",
			msg.InputOutpoint)
	}

	sweepPkScript, err := a.cfg.NewSweepPkScript(ctx)
	if err != nil {
		return fmt.Errorf("new checkpoint sweep pkScript: %w", err)
	}

	sweepTx, err := a.cfg.BuildSweep(ctx, &CheckpointSweepRequest{
		Info:          info,
		Policy:        a.cfg.CheckpointPolicy,
		OperatorKey:   a.cfg.OperatorKey,
		Signer:        a.cfg.Signer,
		SweepPkScript: sweepPkScript,
	})
	if err != nil {
		return fmt.Errorf("build checkpoint sweep: %w", err)
	}

	j := &checkpointJob{
		inputOutpoint:  msg.InputOutpoint,
		checkpointTx:   info.CheckpointTx,
		checkpointTxid: info.CheckpointTx.TxHash(),
		outputOutpoint: msg.CheckpointOutpoint,
		confirmed:      true,
		maturityHeight: int32(msg.MaturityHeight),
		sweeping:       true,
		sweepTxid:      sweepTx.TxHash(),
	}

	// Only mark the sweep as in-flight after txconfirm has accepted the
	// submission. If ensureCheckpointSweep returns synchronously with an
	// error, leaving the entry behind would suppress every future
	// notification for this checkpoint output until the daemon restarts —
	// even though no broadcast has actually happened. The async TxFailed
	// path already clears sweepsByOutput so a post-acceptance failure
	// also recovers.
	if err := a.ensureCheckpointSweep(ctx, j, sweepTx); err != nil {
		return err
	}

	a.sweepsByOutput[msg.CheckpointOutpoint] = j.sweepTxid

	return nil
}

// ensureCheckpoint submits the stored checkpoint transaction via txconfirm.
func (a *Actor) ensureCheckpoint(ctx context.Context,
	checkpoint *checkpointJob) error {

	j := &job{
		id:         checkpoint.inputOutpoint.String(),
		txs:        []*wire.MsgTx{checkpoint.checkpointTx},
		label:      CheckpointLabel,
		checkpoint: checkpoint,
		stage:      jobStageCheckpoint,
	}

	return a.submitNextTxn(ctx, j)
}

// ensureCheckpointSweep submits the built timeout sweep via txconfirm.
func (a *Actor) ensureCheckpointSweep(ctx context.Context,
	checkpoint *checkpointJob, sweepTx *wire.MsgTx) error {

	checkpoint.sweeping = true
	checkpoint.sweepTxid = sweepTx.TxHash()

	j := &job{
		id:         checkpoint.outputOutpoint.String(),
		txs:        []*wire.MsgTx{sweepTx},
		label:      CheckpointSweepLabel,
		checkpoint: checkpoint,
		stage:      jobStageCheckpointSweep,
	}

	return a.submitNextTxn(ctx, j)
}

// submitNextTxn submits the next transaction in the job to txconfirm.
func (a *Actor) submitNextTxn(ctx context.Context, j *job) error {
	if a.notifyRef == nil {
		return fmt.Errorf("txconfirm notification ref not set")
	}
	if j.index >= len(j.txs) {
		return nil
	}

	tx := j.txs[j.index]
	if tx == nil {
		return fmt.Errorf("fraud response tx %d is nil", j.index)
	}

	txid := tx.TxHash()
	a.pending[txid] = j

	resp, err := a.cfg.TxConfirmRef.Ask(ctx, &txconfirm.EnsureConfirmedReq{
		Tx:          tx,
		Label:       j.label,
		TargetConfs: 1,
		Subscriber:  a.notifyRef,
	}).Await(ctx).Unpack()
	if err != nil {
		delete(a.pending, txid)
		return fmt.Errorf("ensure fraud response tx %s: %w", txid, err)
	}

	ensureResp, ok := resp.(*txconfirm.EnsureConfirmedResp)
	if !ok {
		delete(a.pending, txid)
		return fmt.Errorf("unexpected txconfirm response %T", resp)
	}

	a.log.InfoS(ctx, "Fraud response tx submitted",
		"job", j.id,
		"txid", txid,
		"state", ensureResp.State.String(),
		"created", ensureResp.Created)

	return nil
}

// DefaultPlanner uses the response transaction already attached by
// batchwatcher.
type DefaultPlanner struct{}

// PlanResponse returns the direct response transaction from notification.
func (DefaultPlanner) PlanResponse(_ context.Context,
	msg *batchwatcher.UnexpectedSpendNotification) (*ResponsePlan, error) {

	if msg == nil {
		return nil, fmt.Errorf("notification is nil")
	}

	return &ResponsePlan{
		ResponseTx: msg.ResponseTx,
		Label:      fmt.Sprintf("fraud-%s", msg.Classification),
	}, nil
}

// actionable returns true for classifications with a direct fraud response.
//
// Every actionable classification requires a non-nil ResponseTx — without
// the broadcastable bytes the actor has nothing to submit. Surfacing this
// uniformly here means downstream code (Planner.PlanResponse, submitNextTxn)
// can treat msg.ResponseTx as guaranteed non-nil.
func actionable(msg *batchwatcher.UnexpectedSpendNotification) bool {
	if msg == nil {
		return false
	}

	switch msg.Classification {
	case batchwatcher.SpendClassificationForfeitedLeaf,
		batchwatcher.SpendClassificationOORCheckpointLeaf,
		batchwatcher.SpendClassificationSpentLeaf,
		batchwatcher.SpendClassificationInFlightLeaf:

		return msg.ResponseTx != nil

	default:
		return false
	}
}

// jobID returns a stable log value for a possibly missing job.
func jobID(j *job) string {
	if j == nil {
		return ""
	}

	return j.id
}

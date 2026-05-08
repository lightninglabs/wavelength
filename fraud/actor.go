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

	// ForfeitLabel is the txconfirm label for forfeit tx broadcasts.
	ForfeitLabel = "fraud-forfeit"

	// ForfeitSweepLabel is the txconfirm label for forfeit penalty output
	// sweep broadcasts.
	ForfeitSweepLabel = "fraud-forfeit-sweep"

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

// ForfeitSweepBuilder constructs the operator forfeit penalty output sweep.
type ForfeitSweepBuilder func(context.Context,
	*ForfeitSweepRequest) (*wire.MsgTx, error)

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

	// BuildForfeitSweep builds the forfeit penalty output sweep. Tests may
	// override this while production uses BuildForfeitOutputSweep.
	BuildForfeitSweep ForfeitSweepBuilder

	// Log is an optional logger.
	Log fn.Option[btclog.Logger]
}

// Actor consumes batchwatcher fraud notifications and drives txconfirm.
//
// Four maps coordinate in-flight work; their keys differ on purpose:
//
//   - pending[txTxid] -> set of *job: the txconfirm round-trip index.
//     Every tx handed to txconfirm has an entry here, removed on
//     TxConfirmed / TxFailed. Owns no semantics beyond "we're waiting on
//     this txid".
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
//   - forfeitsByOutpoint[vtxoOutpoint] -> active forfeit job state: dedups
//     forfeit response and penalty-sweep submissions per forfeited VTXO
//     outpoint. batchwatcher re-emits VTXOOnChainNotification while the leaf
//     remains in the active frontier, so without this map every reorg-stable
//     confirmation would re-broadcast the connector ancestors or sweep.
//
// Invariants the handlers preserve:
//
//   - For a checkpoint job, both pending[checkpointTxid] and
//     checkpointsByTxid[checkpointTxid] are populated together and
//     cleared together.
//   - For a sweep job, both pending[sweepTxid] and
//     sweepsByOutput[outpoint] are populated together and cleared
//     together.
//   - For a forfeit job, forfeitsByOutpoint[outpoint] is populated when the
//     first ancestor is accepted by txconfirm. It remains populated until
//     the follow-up penalty sweep confirms, or any response/sweep TxFailed
//     clears it for retry.
//   - Dedup entries (checkpointsByTxid / sweepsByOutput /
//     forfeitsByOutpoint) are added AFTER txconfirm has accepted the
//     submission so a synchronous Ask failure cannot strand a future
//     re-notification.
type Actor struct {
	cfg Config
	log btclog.Logger

	notifyRef actor.TellOnlyRef[txconfirm.Notification]

	// pending maps a txid to the set of jobs waiting on its confirmation.
	// The set (rather than a single pointer) is load-bearing for the
	// connector-spend-race case: multiple forfeits from the same batch
	// share connector tree ancestors, so two distinct jobs hand the same
	// txid to txconfirm. A single-valued map would let the second
	// submitNextTxn overwrite the first, stranding the first job's chain.
	pending map[chainhash.Hash]map[*job]struct{}

	checkpointsByTxid  map[chainhash.Hash]*checkpointJob
	sweepsByOutput     map[wire.OutPoint]chainhash.Hash
	forfeitsByOutpoint map[wire.OutPoint]forfeitJobState
}

// forfeitJobPhase describes which txid is currently active for a forfeited
// VTXO response.
type forfeitJobPhase uint8

const (
	forfeitPhaseResponse forfeitJobPhase = iota
	forfeitPhaseSweep
)

// String returns the log label for a forfeit job phase.
func (p forfeitJobPhase) String() string {
	switch p {
	case forfeitPhaseResponse:
		return "response"
	case forfeitPhaseSweep:
		return "sweep"
	default:
		return "unknown"
	}
}

// forfeitJobState records the active txid for one forfeited VTXO response.
type forfeitJobState struct {
	phase forfeitJobPhase
	txid  chainhash.Hash
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

	// forfeitOutpoint is the dedup key in forfeitsByOutpoint when this
	// job belongs to the forfeit response/sweep lifecycle. The zero value
	// indicates a non-forfeit legacy job.
	forfeitOutpoint wire.OutPoint

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

	// jobStageForfeitResponse broadcasts connector ancestors and the
	// stored forfeit transaction. Its terminal confirmation schedules a
	// wallet-recognized sweep of the forfeit penalty output.
	jobStageForfeitResponse

	// jobStageForfeitSweep spends the forfeit penalty output to a fresh
	// operator wallet output.
	jobStageForfeitSweep
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
	if cfg.BuildForfeitSweep == nil {
		cfg.BuildForfeitSweep = BuildForfeitOutputSweep
	}

	return &Actor{
		cfg:                cfg,
		log:                cfg.Log.UnwrapOr(btclog.Disabled),
		pending:            make(map[chainhash.Hash]map[*job]struct{}),
		checkpointsByTxid:  make(map[chainhash.Hash]*checkpointJob),
		sweepsByOutput:     make(map[wire.OutPoint]chainhash.Hash),
		forfeitsByOutpoint: make(map[wire.OutPoint]forfeitJobState),
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
			a.log.WarnS(ctx, "Failed to handle VTXO on-chain notification",
				err, "outpoint", m.VTXOOutpoint)

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

// handleVTXOOnChain starts the appropriate response when a VTXO leaf appears
// on-chain.
func (a *Actor) handleVTXOOnChain(ctx context.Context,
	msg *batchwatcher.VTXOOnChainNotification) error {

	// CheckpointPlanner is guaranteed non-nil by NewActor's validation.
	plan, actionable, err := a.cfg.CheckpointPlanner.PlanCheckpoint(
		ctx, msg,
	)
	if err != nil {
		return fmt.Errorf("plan on-chain response: %w", err)
	}
	if !actionable {
		return nil
	}
	if plan.ForfeitPlan != nil {
		return a.ensureForfeit(ctx, msg.VTXOOutpoint, plan.ForfeitPlan)
	}
	if plan.CheckpointTx == nil {
		return fmt.Errorf("missing on-chain response tx")
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

// ensureForfeit submits the stored forfeit transaction via txconfirm.
//
// Mirrors the symmetric handleVTXOOnChain ordering: only register the
// dedup entry after submitNextTxn has accepted the first submission. A
// synchronous Ask failure must not leave a dead entry in
// forfeitsByOutpoint — every subsequent VTXOOnChainNotification for the
// same input would then be silently deduped and the operator would never
// broadcast the response.
func (a *Actor) ensureForfeit(ctx context.Context,
	vtxoOutpoint wire.OutPoint, plan *ResponsePlan) error {

	if plan == nil || plan.ResponseTx == nil {
		return fmt.Errorf("missing forfeit response tx")
	}

	if existing, ok := a.forfeitsByOutpoint[vtxoOutpoint]; ok {
		a.log.DebugS(ctx, "Forfeit response already active",
			"outpoint", vtxoOutpoint,
			"phase", existing.phase.String(),
			"txid", existing.txid)

		return nil
	}

	txs := make([]*wire.MsgTx, 0, len(plan.Ancestors)+1)
	txs = append(txs, plan.Ancestors...)
	txs = append(txs, plan.ResponseTx)

	label := plan.Label
	if label == "" {
		label = ForfeitLabel
	}

	forfeitTxid := plan.ResponseTx.TxHash()
	j := &job{
		id:              vtxoOutpoint.String(),
		txs:             txs,
		label:           label,
		stage:           jobStageForfeitResponse,
		forfeitOutpoint: vtxoOutpoint,
	}

	if err := a.submitNextTxn(ctx, j); err != nil {
		return err
	}

	a.forfeitsByOutpoint[vtxoOutpoint] = forfeitJobState{
		phase: forfeitPhaseResponse,
		txid:  forfeitTxid,
	}

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

	stage := jobStageLegacy
	forfeitOutpoint := wire.OutPoint{}
	if msg.Classification == batchwatcher.SpendClassificationForfeitedLeaf {
		stage = jobStageForfeitResponse
		forfeitOutpoint = msg.TrackedOutput.Outpoint

		// Forfeit-response jobs share the forfeitsByOutpoint dedup
		// map regardless of which classification path drove the
		// actor here, so we still update it below after txconfirm
		// accepts the submission. We deliberately do NOT skip on a
		// dedup hit: the on-chain VTXOOnChainNotification path may
		// have just submitted ancestors that failed locally (e.g.
		// post-restart, when the connector chain is already mined
		// and bitcoind rejects re-broadcast with
		// bad-txns-inputs-missingorspent), and the legacy
		// UnexpectedSpend path's submission of the forfeit tx alone
		// is what kicks txconfirm into observing the already-mined
		// confirmation and scheduling the sweep.
	}

	j := &job{
		id:              msg.TrackedOutput.Outpoint.String(),
		txs:             txs,
		label:           label,
		stage:           stage,
		forfeitOutpoint: forfeitOutpoint,
	}

	a.log.InfoS(ctx, "Starting fraud response",
		"classification", msg.Classification.String(),
		"outpoint", msg.TrackedOutput.Outpoint,
		"response_tx", plan.ResponseTx.TxHash(),
		"tx_count", len(txs))

	if err := a.submitNextTxn(ctx, j); err != nil {
		return err
	}

	// Register the forfeit dedup entry only after txconfirm accepted the
	// first submission. A synchronous Ask failure must not leave a stale
	// entry that suppresses every future re-notification.
	if forfeitOutpoint != (wire.OutPoint{}) {
		a.forfeitsByOutpoint[forfeitOutpoint] = forfeitJobState{
			phase: forfeitPhaseResponse,
			txid:  responseTxid,
		}
	}

	return nil
}

// handleTxConfirmed advances a response job after one tx confirms.
func (a *Actor) handleTxConfirmed(ctx context.Context,
	msg *txconfirm.TxConfirmed) error {

	jobs, ok := a.pending[msg.Txid]
	if !ok || len(jobs) == 0 {
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

	// Fan out the confirmation to every job that was waiting on this
	// txid. Multiple forfeits from the same batch share connector tree
	// ancestors, so a single TxConfirmed for a shared ancestor must
	// advance every dependent forfeit's chain — not just the most
	// recently submitted one. The terminal forfeit tx is different:
	// once it confirms, there is only one penalty output per forfeited
	// outpoint, so duplicate jobs for the same outpoint must coalesce
	// before sweep construction.
	var firstErr error
	sweptForfeits := make(map[wire.OutPoint]struct{})
	for j := range jobs {
		forfeitOutpoint, terminalForfeit := terminalForfeitJob(
			j, msg.Txid,
		)
		if terminalForfeit {
			if _, ok := sweptForfeits[forfeitOutpoint]; ok {
				a.log.DebugS(ctx,
					"Skipping duplicate forfeit sweep",
					"outpoint", forfeitOutpoint,
					"forfeit_tx", msg.Txid)

				continue
			}

			sweptForfeits[forfeitOutpoint] = struct{}{}
		}

		if err := a.advanceJobOnConfirm(ctx, j, msg); err != nil {
			a.log.WarnS(ctx, "Advance job on confirm failed", err,
				"job", jobID(j), "txid", msg.Txid)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}

// terminalForfeitJob returns true when j is waiting on its final stored
// forfeit transaction. Shared connector ancestors should fan out to every job,
// but the terminal forfeit confirmation should schedule only one penalty sweep
// per forfeited outpoint.
func terminalForfeitJob(j *job,
	txid chainhash.Hash) (wire.OutPoint, bool) {

	if j == nil || j.stage != jobStageForfeitResponse {
		return wire.OutPoint{}, false
	}
	if j.forfeitOutpoint == (wire.OutPoint{}) {
		return wire.OutPoint{}, false
	}
	if len(j.txs) == 0 || j.index != len(j.txs)-1 {
		return wire.OutPoint{}, false
	}
	tx := j.txs[j.index]
	if tx == nil || tx.TxHash() != txid {
		return wire.OutPoint{}, false
	}

	return j.forfeitOutpoint, true
}

// advanceJobOnConfirm dispatches a single confirmed job to its stage
// handler or, for legacy multi-tx jobs, advances the index and submits
// the next tx in the chain.
func (a *Actor) advanceJobOnConfirm(ctx context.Context, j *job,
	msg *txconfirm.TxConfirmed) error {

	switch j.stage {
	case jobStageCheckpoint:
		return a.handleCheckpointConfirmed(ctx, j.checkpoint, msg)

	case jobStageCheckpointSweep:
		a.handleCheckpointSweepConfirmed(ctx, j.checkpoint, msg)
		return nil

	case jobStageForfeitResponse:
		return a.handleForfeitResponseConfirmed(ctx, j, msg)

	case jobStageForfeitSweep:
		a.handleForfeitSweepConfirmed(ctx, j, msg)
		return nil
	}

	j.index++
	if j.index == len(j.txs) {
		if j.forfeitOutpoint != (wire.OutPoint{}) {
			delete(a.forfeitsByOutpoint, j.forfeitOutpoint)
		}

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
	jobs, ok := a.pending[msg.Txid]
	if !ok || len(jobs) == 0 {
		a.log.WarnS(ctx, "Fraud response transaction failed",
			nil, "job", jobID(nil),
			"txid", msg.Txid, "reason", msg.Reason)

		return
	}
	delete(a.pending, msg.Txid)

	// Fan out the failure to every job that was waiting on this txid.
	// Distinct forfeits sharing a connector ancestor each register their
	// own per-stage dedup entry (checkpointsByTxid / sweepsByOutput /
	// forfeitsByOutpoint), so each one must be cleared independently.
	for j := range jobs {
		if j.checkpoint != nil {
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
		if j.forfeitOutpoint != (wire.OutPoint{}) {
			delete(a.forfeitsByOutpoint, j.forfeitOutpoint)
		}

		a.log.WarnS(ctx, "Fraud response transaction failed",
			nil, "job", jobID(j),
			"txid", msg.Txid, "reason", msg.Reason)
	}
}

// handleForfeitResponseConfirmed advances a forfeit response job and sweeps
// the confirmed penalty output once the stored forfeit tx confirms.
func (a *Actor) handleForfeitResponseConfirmed(ctx context.Context, j *job,
	msg *txconfirm.TxConfirmed) error {

	j.index++
	if j.index < len(j.txs) {
		if err := a.submitNextTxn(ctx, j); err != nil {
			if j.forfeitOutpoint != (wire.OutPoint{}) {
				delete(a.forfeitsByOutpoint, j.forfeitOutpoint)
			}

			return err
		}

		return nil
	}

	forfeitTx := j.txs[len(j.txs)-1]
	a.log.InfoS(ctx, "Forfeit response confirmed",
		"job", j.id,
		"txid", msg.Txid,
		"height", msg.BlockHeight)

	if err := a.ensureForfeitSweep(
		ctx, j.forfeitOutpoint, forfeitTx,
	); err != nil {
		if j.forfeitOutpoint != (wire.OutPoint{}) {
			delete(a.forfeitsByOutpoint, j.forfeitOutpoint)
		}

		return err
	}

	return nil
}

// handleForfeitSweepConfirmed marks the forfeit penalty sweep complete.
func (a *Actor) handleForfeitSweepConfirmed(ctx context.Context, j *job,
	msg *txconfirm.TxConfirmed) {

	if j.forfeitOutpoint != (wire.OutPoint{}) {
		delete(a.forfeitsByOutpoint, j.forfeitOutpoint)
	}

	a.log.InfoS(ctx, "Forfeit penalty sweep confirmed",
		"job", j.id,
		"forfeit_outpoint", j.forfeitOutpoint,
		"sweep_tx", msg.Txid,
		"height", msg.BlockHeight)
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

// ensureForfeitSweep builds and submits the wallet-recognized penalty sweep.
func (a *Actor) ensureForfeitSweep(ctx context.Context,
	forfeitOutpoint wire.OutPoint, forfeitTx *wire.MsgTx) error {

	sweepPkScript, err := a.cfg.NewSweepPkScript(ctx)
	if err != nil {
		return fmt.Errorf("new forfeit sweep pkScript: %w", err)
	}

	sweepTx, err := a.cfg.BuildForfeitSweep(ctx, &ForfeitSweepRequest{
		ForfeitTx:       forfeitTx,
		ForfeitOutpoint: forfeitOutpoint,
		OperatorKey:     a.cfg.OperatorKey,
		Signer:          a.cfg.Signer,
		SweepPkScript:   sweepPkScript,
	})
	if err != nil {
		return fmt.Errorf("build forfeit sweep: %w", err)
	}

	j := &job{
		id:              forfeitTx.TxHash().String(),
		txs:             []*wire.MsgTx{sweepTx},
		label:           ForfeitSweepLabel,
		forfeitOutpoint: forfeitOutpoint,
		stage:           jobStageForfeitSweep,
	}

	if err := a.submitNextTxn(ctx, j); err != nil {
		return err
	}

	if forfeitOutpoint != (wire.OutPoint{}) {
		a.forfeitsByOutpoint[forfeitOutpoint] = forfeitJobState{
			phase: forfeitPhaseSweep,
			txid:  sweepTx.TxHash(),
		}
	}

	return nil
}

// submitNextTxn submits the next transaction in the job to txconfirm. The job
// is added to pending[txid] so any concurrent forfeit response that happens
// to reach the same shared connector ancestor is also notified when the tx
// confirms or fails.
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
	jobs, ok := a.pending[txid]
	if !ok {
		jobs = make(map[*job]struct{})
		a.pending[txid] = jobs
	}
	jobs[j] = struct{}{}

	resp, err := a.cfg.TxConfirmRef.Ask(ctx, &txconfirm.EnsureConfirmedReq{
		Tx:          tx,
		Label:       j.label,
		TargetConfs: 1,
		Subscriber:  a.notifyRef,
	}).Await(ctx).Unpack()
	if err != nil {
		a.removePendingJob(txid, j)
		return fmt.Errorf("ensure fraud response tx %s: %w", txid, err)
	}

	ensureResp, ok := resp.(*txconfirm.EnsureConfirmedResp)
	if !ok {
		a.removePendingJob(txid, j)
		return fmt.Errorf("unexpected txconfirm response %T", resp)
	}

	a.log.InfoS(ctx, "Fraud response tx submitted",
		"job", j.id,
		"txid", txid,
		"state", ensureResp.State.String(),
		"created", ensureResp.Created)

	return nil
}

// removePendingJob removes j from pending[txid]. The slot is dropped once
// the inner map empties so the outer map stays compact.
func (a *Actor) removePendingJob(txid chainhash.Hash, j *job) {
	jobs, ok := a.pending[txid]
	if !ok {
		return
	}
	delete(jobs, j)
	if len(jobs) == 0 {
		delete(a.pending, txid)
	}
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

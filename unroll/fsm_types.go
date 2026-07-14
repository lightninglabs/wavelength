package unroll

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightninglabs/wavelength/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// StateMachine is the protofsm instance for one VTXO unroll session.
type StateMachine = protofsm.StateMachine[Event, OutboxEvent, *Environment]

// StateTransition is the unroll-specific protofsm transition type.
type StateTransition = protofsm.StateTransition[
	Event, OutboxEvent, *Environment,
]

// EmittedEvent is the unroll-specific protofsm emitted-event type.
type EmittedEvent = protofsm.EmittedEvent[Event, OutboxEvent]

// Environment carries immutable context for one unroll FSM instance.
type Environment struct {
	// Proof is the immutable local recovery proof for the target.
	Proof *recovery.Proof

	// Planner evaluates ready, blocked, CSV, and sweep progress.
	Planner *unrollplan.Planner

	// FraudCheckpointSafetyMargin is the number of blocks subtracted
	// from a checkpoint's relative-expiry window to compute the
	// recipient backstop deadline under TriggerFraudSpend. The
	// margin gives the operator (or a peer fraud-triggered unroll)
	// time to publish the checkpoint first; the recipient only steps
	// in when the deadline arrives without observed confirmation.
	//
	// Zero falls back to defaultFraudCheckpointSafetyMargin.
	// checkpointBackstopHeight clamps the effective margin to
	// csvDelay/2 for chains with a very short CSV so the deadline
	// does not fall before the current height.
	FraudCheckpointSafetyMargin int32
}

// contextErrorReporter reports protofsm execution errors through the actor
// logger.
//
//nolint:containedctx
type contextErrorReporter struct {
	ctx    context.Context
	logger btclog.Logger
	prefix string
}

// newContextErrorReporter creates an FSM error reporter for one actor session.
func newContextErrorReporter(ctx context.Context, logger btclog.Logger,
	prefix string) *contextErrorReporter {

	return &contextErrorReporter{
		ctx:    ctx,
		logger: logger,
		prefix: prefix,
	}
}

// ReportError logs one FSM execution error.
func (r *contextErrorReporter) ReportError(err error) {
	r.logger.WithPrefix(r.prefix).ErrorS(r.ctx, "FSM error", err)
}

// maxSweepAttempts is the maximum number of sweep build or broadcast failures
// tolerated before the actor transitions to terminal failure.
const maxSweepAttempts = 3

// JobState is the durable state owned by the unroll FSM.
type JobState struct {
	// Height is the current best height known to the actor.
	Height int32

	// Trigger identifies why the actor was started.
	Trigger StartTrigger

	// ExitPolicyKind identifies the final spend policy for this job.
	ExitPolicyKind ExitPolicyKind

	// ExitPolicyRef is the policy-specific durable reference.
	ExitPolicyRef string

	// PlannerState is the durable caller-owned planning progress.
	PlannerState unrollplan.State

	// DeferredCheckpoints tracks fraud-triggered checkpoint
	// transactions that are ready but intentionally not broadcast yet,
	// giving the operator time to confirm them first.
	DeferredCheckpoints []DeferredCheckpoint

	// FailReason records a terminal failure reason, if any.
	FailReason string

	// SweepAttempts counts sweep build or broadcast failures so the actor
	// can retry up to maxSweepAttempts before giving up.
	SweepAttempts int
}

// Copy returns a deep copy of the job state.
func (j *JobState) Copy() *JobState {
	if j == nil {
		return nil
	}

	deferred := copyDeferredCheckpoints(j.DeferredCheckpoints)
	copyState := &JobState{
		Height:              j.Height,
		Trigger:             j.Trigger,
		ExitPolicyKind:      exitPolicyKind(j.ExitPolicyKind),
		ExitPolicyRef:       j.ExitPolicyRef,
		PlannerState:        copyPlannerState(j.PlannerState),
		DeferredCheckpoints: deferred,
		FailReason:          j.FailReason,
		SweepAttempts:       j.SweepAttempts,
	}

	return copyState
}

// DeferredCheckpoint is a fraud-triggered checkpoint that is ready to be
// materialized, but is held until DeadlineHeight unless it confirms first.
type DeferredCheckpoint struct {
	// Txid identifies the checkpoint transaction.
	Txid chainhash.Hash

	// DeadlineHeight is the first height at which the recipient should
	// broadcast the checkpoint itself.
	DeadlineHeight int32
}

// Event is the sealed input event surface accepted by the unroll FSM.
type Event interface {
	eventSealed()
}

// StartEvent starts a new VTXO unroll session.
type StartEvent struct {
	// Height is the current best height at start time.
	Height int32

	// Trigger identifies why the actor was started.
	Trigger StartTrigger

	// ExitPolicyKind identifies the final spend policy to persist for this
	// target. Empty events use the standard VTXO timeout policy.
	ExitPolicyKind ExitPolicyKind

	// ExitPolicyRef is the policy-specific durable reference.
	ExitPolicyRef string
}

// eventSealed marks StartEvent as an FSM event.
func (e *StartEvent) eventSealed() {}

// ResumeEvent resumes a previously checkpointed VTXO unroll session.
type ResumeEvent struct {
	// Height is the current best height at resume time.
	Height int32
}

// eventSealed marks ResumeEvent as an FSM event.
func (e *ResumeEvent) eventSealed() {}

// HeightUpdatedEvent records a newly observed best height.
type HeightUpdatedEvent struct {
	// Height is the latest observed best height.
	Height int32
}

// eventSealed marks HeightUpdatedEvent as an FSM event.
func (e *HeightUpdatedEvent) eventSealed() {}

// TxConfirmedEvent records confirmation of one proof or sweep transaction.
type TxConfirmedEvent struct {
	// Txid is the confirmed transaction hash.
	Txid chainhash.Hash

	// Height is the block height where the transaction confirmed.
	Height int32
}

// eventSealed marks TxConfirmedEvent as an FSM event.
func (e *TxConfirmedEvent) eventSealed() {}

// TxFailedEvent records terminal failure of one proof or sweep transaction.
type TxFailedEvent struct {
	// Txid identifies the failed transaction when known.
	Txid chainhash.Hash

	// Reason is the stable human-readable failure reason.
	Reason string
}

// eventSealed marks TxFailedEvent as an FSM event.
func (e *TxFailedEvent) eventSealed() {}

// SweepBroadcastedEvent records that the actor built the final sweep and
// submitted it to txconfirm.
type SweepBroadcastedEvent struct {
	// Txid is the final sweep transaction hash.
	Txid chainhash.Hash
}

// eventSealed marks SweepBroadcastedEvent as an FSM event.
func (e *SweepBroadcastedEvent) eventSealed() {}

// FailEvent records a generic terminal failure.
type FailEvent struct {
	// Reason is the stable human-readable failure reason.
	Reason string
}

// eventSealed marks FailEvent as an FSM event.
func (e *FailEvent) eventSealed() {}

// SweepBuildFailedEvent records a sweep construction failure. The actor retries
// up to maxSweepAttempts before giving up.
type SweepBuildFailedEvent struct {
	// Reason is the stable human-readable failure reason.
	Reason string
}

// eventSealed marks SweepBuildFailedEvent as an FSM event.
func (e *SweepBuildFailedEvent) eventSealed() {}

// OutboxEvent is the sealed outbox side-effect surface emitted by the FSM.
type OutboxEvent interface {
	outboxEventSealed()
}

// EnsureReadyTransactions asks the actor boundary to submit newly-ready proof
// txids to txconfirm.
type EnsureReadyTransactions struct {
	// Txids are the newly-ready proof txids to submit.
	Txids []chainhash.Hash
}

// outboxEventSealed marks EnsureReadyTransactions as an outbox event.
func (o *EnsureReadyTransactions) outboxEventSealed() {}

// ReissueInFlightTransactions asks the actor boundary to reattach to already
// in-flight proof txids after a restart.
type ReissueInFlightTransactions struct {
	// Txids are the in-flight proof txids to reissue to txconfirm.
	Txids []chainhash.Hash
}

// outboxEventSealed marks ReissueInFlightTransactions as an outbox event.
func (o *ReissueInFlightTransactions) outboxEventSealed() {}

// WatchDeferredCheckpoints asks the actor boundary to watch deferred
// checkpoints for operator confirmation while waiting for the backstop height.
type WatchDeferredCheckpoints struct {
	// Txids are the deferred checkpoint txids to watch.
	Txids []chainhash.Hash
}

// outboxEventSealed marks WatchDeferredCheckpoints as an outbox event.
func (o *WatchDeferredCheckpoints) outboxEventSealed() {}

// RequestSweepBuild asks the actor boundary to build and submit the final
// timeout sweep.
type RequestSweepBuild struct{}

// outboxEventSealed marks RequestSweepBuild as an outbox event.
func (o *RequestSweepBuild) outboxEventSealed() {}

// ReissueSweepConfirmation asks the actor boundary to reattach txconfirm to an
// already-broadcast sweep after a restart.
type ReissueSweepConfirmation struct{}

// outboxEventSealed marks ReissueSweepConfirmation as an outbox event.
func (o *ReissueSweepConfirmation) outboxEventSealed() {}

// State is the sealed interface implemented by all unroll FSM states.
type State interface {
	protofsm.State[Event, OutboxEvent, *Environment]

	stateSealed()
}

// Idle is the initial FSM state before the actor has started work.
type Idle struct{}

// String returns a human-readable state label.
func (s *Idle) String() string {
	return "Idle"
}

// IsTerminal returns false because Idle is not terminal.
func (s *Idle) IsTerminal() bool {
	return false
}

// stateSealed marks Idle as implementing State.
func (s *Idle) stateSealed() {}

// ProcessEvent handles FSM events while idle.
func (s *Idle) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	switch e := event.(type) {
	case *StartEvent:
		job := &JobState{
			Height:         e.Height,
			Trigger:        e.Trigger,
			ExitPolicyKind: exitPolicyKind(e.ExitPolicyKind),
			ExitPolicyRef:  e.ExitPolicyRef,
		}

		return deriveStateTransition(
			ctx, job, env, false, fn.None[int32](),
		)

	case *ResumeEvent:
		job := &JobState{
			Height:     e.Height,
			Trigger:    TriggerRestart,
			FailReason: "",
		}

		return deriveStateTransition(
			ctx, job, env, true, fn.None[int32](),
		)

	default:
		return nil, fmt.Errorf("unexpected event %T in %s", event, s)
	}
}

// AwaitingMaterialization indicates proof transactions are still being
// broadcast or confirmed.
type AwaitingMaterialization struct {
	// Job is the durable FSM state.
	Job *JobState
}

// String returns a human-readable state label.
func (s *AwaitingMaterialization) String() string {
	return "AwaitingMaterialization"
}

// IsTerminal returns false because this state is not terminal.
func (s *AwaitingMaterialization) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingMaterialization as implementing State.
func (s *AwaitingMaterialization) stateSealed() {}

// ProcessEvent handles FSM events while proof materialization is ongoing.
func (s *AwaitingMaterialization) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	return processEventWithJob(ctx, s.Job, event, env)
}

// AwaitingCSV indicates the target confirmed but its CSV delay has not matured
// yet.
type AwaitingCSV struct {
	// Job is the durable FSM state.
	Job *JobState
}

// String returns a human-readable state label.
func (s *AwaitingCSV) String() string {
	return "AwaitingCSV"
}

// IsTerminal returns false because this state is not terminal.
func (s *AwaitingCSV) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingCSV as implementing State.
func (s *AwaitingCSV) stateSealed() {}

// ProcessEvent handles FSM events while waiting for CSV.
func (s *AwaitingCSV) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	return processEventWithJob(ctx, s.Job, event, env)
}

// AwaitingSweepBroadcast indicates the sweep is ready and the actor boundary
// needs to build and submit it.
type AwaitingSweepBroadcast struct {
	// Job is the durable FSM state.
	Job *JobState
}

// String returns a human-readable state label.
func (s *AwaitingSweepBroadcast) String() string {
	return "AwaitingSweepBroadcast"
}

// IsTerminal returns false because this state is not terminal.
func (s *AwaitingSweepBroadcast) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingSweepBroadcast as implementing State.
func (s *AwaitingSweepBroadcast) stateSealed() {}

// ProcessEvent handles FSM events while the sweep is waiting to be built.
func (s *AwaitingSweepBroadcast) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	return processEventWithJob(ctx, s.Job, event, env)
}

// AwaitingSweepConfirmation indicates the sweep has been submitted to
// txconfirm and is waiting for confirmation.
type AwaitingSweepConfirmation struct {
	// Job is the durable FSM state.
	Job *JobState
}

// String returns a human-readable state label.
func (s *AwaitingSweepConfirmation) String() string {
	return "AwaitingSweepConfirmation"
}

// IsTerminal returns false because this state is not terminal.
func (s *AwaitingSweepConfirmation) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingSweepConfirmation as implementing State.
func (s *AwaitingSweepConfirmation) stateSealed() {}

// ProcessEvent handles FSM events while waiting for sweep confirmation.
func (s *AwaitingSweepConfirmation) ProcessEvent(ctx context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	return processEventWithJob(ctx, s.Job, event, env)
}

// Completed indicates the final sweep has confirmed.
type Completed struct {
	// Job is the durable FSM state.
	Job *JobState
}

// String returns a human-readable state label.
func (s *Completed) String() string {
	return "Completed"
}

// IsTerminal returns true because this state is terminal.
func (s *Completed) IsTerminal() bool {
	return true
}

// stateSealed marks Completed as implementing State.
func (s *Completed) stateSealed() {}

// ProcessEvent rejects further events in the terminal completed state.
func (s *Completed) ProcessEvent(context.Context, Event, *Environment) (
	*StateTransition, error) {

	return nil, fmt.Errorf("completed state is terminal")
}

// Failed indicates the actor reached terminal failure.
type Failed struct {
	// Job is the durable FSM state.
	Job *JobState
}

// String returns a human-readable state label.
func (s *Failed) String() string {
	return "Failed"
}

// IsTerminal returns true because this state is terminal.
func (s *Failed) IsTerminal() bool {
	return true
}

// stateSealed marks Failed as implementing State.
func (s *Failed) stateSealed() {}

// ProcessEvent rejects further events in the terminal failed state.
func (s *Failed) ProcessEvent(context.Context, Event, *Environment) (
	*StateTransition, error) {

	return nil, fmt.Errorf("failed state is terminal")
}

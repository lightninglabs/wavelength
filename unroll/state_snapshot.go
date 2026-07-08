package unroll

import (
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// checkpointFromState exports the current protofsm state into the durable actor
// checkpoint shape.
func checkpointFromState(state State, sweepTx *wire.MsgTx) *actorCheckpoint {
	checkpoint := &actorCheckpoint{
		Version:        checkpointVersion,
		ExitPolicyKind: StandardVTXOTimeoutExitPolicyKind,
		SweepTx:        copyTx(sweepTx),
	}

	if state == nil || isIdleState(state) {
		return checkpoint
	}

	job := stateJob(state)
	checkpoint.Height = job.Height
	checkpoint.Started = true
	checkpoint.Trigger = job.Trigger
	checkpoint.ExitPolicyKind = exitPolicyKind(job.ExitPolicyKind)
	checkpoint.ExitPolicyRef = job.ExitPolicyRef
	checkpoint.State = copyPlannerState(job.PlannerState)
	checkpoint.DeferredCheckpoints = copyDeferredCheckpoints(
		job.DeferredCheckpoints,
	)
	if sweepTxid := effectiveSweepTxid(
		job.PlannerState, sweepTx,
	); sweepTxid != nil {

		checkpoint.State.Sweep.Txid = fn.Some(*sweepTxid)
	}
	checkpoint.Fail = job.FailReason
	checkpoint.SweepAttempts = job.SweepAttempts
	checkpoint.ProvisionalExternalSpend = job.ProvisionalExternalSpend

	return checkpoint
}

// jobHadOnChainFootprint reports whether the job ever published anything
// on-chain. A footprint exists if any proof node confirmed or is still
// in-flight (submitted to txconfirm, so potentially in the mempool), or if
// the sweep advanced past pending. It is false only for a clean failure
// that never broadcast, which is the sole case where the target VTXO is
// safe to roll back to live: any footprint means the unilateral exit has
// begun on-chain and the operator no longer treats the VTXO as live. See
// darepo-client#602.
//
// This reflects THIS job's own footprint. An exit driven on-chain by a
// third party (the operator, or a prior holder of a fraudulently re-spent
// VTXO) does not reach the recoverable branch: our submission of the same
// proof node is an ignorable "already known" broadcast rather than a hard
// failure, and the proof-node confirmation/spend watch fires, so that job
// terminates Completed (→ ExitOutcomeConfirmed), not as a no-footprint
// failure. Sweeping an externally exposed checkpoint is the fraud-response
// path, which runs as its own (fraud-triggered) job with a real footprint.
func jobHadOnChainFootprint(job *JobState) bool {
	if job == nil {
		return false
	}

	return len(job.PlannerState.ConfirmedTxids) > 0 ||
		len(job.PlannerState.InFlightTxids) > 0 ||
		job.PlannerState.Sweep.Status != unrollplan.SweepStatusPending
}

// effectiveSweepTxid returns the durable sweep txid from planner state when
// present, or derives it from the stored sweep transaction once the sweep has
// advanced beyond pending.
func effectiveSweepTxid(state unrollplan.State,
	sweepTx *wire.MsgTx) *chainhash.Hash {

	if state.Sweep.Txid.IsSome() {
		hash := state.Sweep.Txid.UnsafeFromSome()

		return &hash
	}

	if state.Sweep.Status == unrollplan.SweepStatusPending ||
		sweepTx == nil {
		return nil
	}

	txid := sweepTx.TxHash()

	return &txid
}

// stateFromCheckpoint restores a concrete protofsm state from the durable
// checkpoint shape.
func stateFromCheckpoint(checkpoint *actorCheckpoint) State {
	if checkpoint == nil || !checkpoint.Started {
		return &Idle{}
	}

	deferred := copyDeferredCheckpoints(checkpoint.DeferredCheckpoints)
	job := &JobState{
		Height:  checkpoint.Height,
		Trigger: checkpoint.Trigger,
		ExitPolicyKind: exitPolicyKind(
			checkpoint.ExitPolicyKind,
		),
		ExitPolicyRef:            checkpoint.ExitPolicyRef,
		PlannerState:             copyPlannerState(checkpoint.State),
		DeferredCheckpoints:      deferred,
		FailReason:               checkpoint.Fail,
		SweepAttempts:            checkpoint.SweepAttempts,
		ProvisionalExternalSpend: checkpoint.ProvisionalExternalSpend,
	}

	switch phaseFromPlannerState(job) {
	case PhaseCompleted:
		return &Completed{Job: job}

	case PhaseFailed:
		return &Failed{Job: job}

	case PhaseExternalSpendObserved:
		return &AwaitingExternalSpendFinality{Job: job}

	case PhaseSweepConfirmation:
		return &AwaitingSweepConfirmation{Job: job}

	case PhaseSweepBroadcast:
		return &AwaitingSweepBroadcast{Job: job}

	case PhaseCSVPending:
		return &AwaitingCSV{Job: job}

	default:
		return &AwaitingMaterialization{Job: job}
	}
}

// phaseFromState projects the concrete protofsm state into the public coarse
// phase enum.
func phaseFromState(state State) Phase {
	switch state.(type) {
	case *Idle:
		return PhasePending

	case *AwaitingMaterialization:
		return PhaseMaterializing

	case *AwaitingCSV:
		return PhaseCSVPending

	case *AwaitingSweepBroadcast:
		return PhaseSweepBroadcast

	case *AwaitingSweepConfirmation:
		return PhaseSweepConfirmation

	case *AwaitingExternalSpendFinality:
		return PhaseExternalSpendObserved

	case *Completed:
		return PhaseCompleted

	case *Failed:
		return PhaseFailed

	default:
		return PhaseFailed
	}
}

// phaseFromPlannerState derives a coarse phase from the durable planner state
// when restoring from checkpoint before the planner is bound.
func phaseFromPlannerState(job *JobState) Phase {
	if job == nil {
		return PhasePending
	}

	if job.FailReason != "" {
		return PhaseFailed
	}

	// A persisted provisional external spend takes precedence over the
	// sweep-based phase derivation: the actor was parked waiting for
	// either a reorg (which clears the anchor) or finality (which
	// promotes it to FailReason). Surfacing this phase keeps restart
	// reconciliation and the live reducer aligned on the same state.
	if job.ProvisionalExternalSpend.IsSome() {
		return PhaseExternalSpendObserved
	}

	switch {
	case job.PlannerState.Sweep.Status == unrollplan.SweepStatusConfirmed:
		return PhaseCompleted

	case job.PlannerState.Sweep.Status == unrollplan.SweepStatusBroadcasted:
		return PhaseSweepConfirmation

	case job.PlannerState.TargetConfirmHeight.IsSome():
		return PhaseCSVPending

	default:
		return PhaseMaterializing
	}
}

// stateJob extracts the durable job state from a concrete protofsm state.
func stateJob(state State) *JobState {
	switch s := state.(type) {
	case *Idle:
		return &JobState{}

	case *AwaitingMaterialization:
		return s.Job.Copy()

	case *AwaitingCSV:
		return s.Job.Copy()

	case *AwaitingSweepBroadcast:
		return s.Job.Copy()

	case *AwaitingSweepConfirmation:
		return s.Job.Copy()

	case *AwaitingExternalSpendFinality:
		return s.Job.Copy()

	case *Completed:
		return s.Job.Copy()

	case *Failed:
		return s.Job.Copy()

	default:
		panic(fmt.Sprintf("unexpected state type %T", state))
	}
}

// stateHeight returns the best height tracked by the current state.
func stateHeight(state State) int32 {
	return stateJob(state).Height
}

// stateTrigger returns the start trigger tracked by the current state.
func stateTrigger(state State) StartTrigger {
	return stateJob(state).Trigger
}

// isIdleState reports whether the current state is idle.
func isIdleState(state State) bool {
	_, ok := state.(*Idle)

	return ok
}

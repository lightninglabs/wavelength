package unroll

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// snapshotFromState exports the current protofsm state into the local actor
// snapshot shape.
func snapshotFromState(state State, sweepTx *wire.MsgTx) *unrollSnapshot {
	snapshot := &unrollSnapshot{
		ExitPolicyKind: StandardVTXOTimeoutExitPolicyKind,
		SweepTx:        copyTx(sweepTx),
	}

	if state == nil || isIdleState(state) {
		return snapshot
	}

	job := stateJob(state)
	snapshot.Height = job.Height
	snapshot.Started = true
	snapshot.Trigger = job.Trigger
	snapshot.State = copyPlannerState(job.PlannerState)
	snapshot.DeferredCheckpoints = copyDeferredCheckpoints(
		job.DeferredCheckpoints,
	)
	if sweepTxid := effectiveSweepTxid(
		job.PlannerState, sweepTx,
	); sweepTxid != nil {

		snapshot.State.Sweep.Txid = fn.Some(*sweepTxid)
	}
	snapshot.Fail = job.FailReason
	snapshot.SweepAttempts = job.SweepAttempts

	return snapshot
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

// stateFromSnapshot restores a concrete protofsm state from the durable
// snapshot shape.
func stateFromSnapshot(snapshot *unrollSnapshot) State {
	if snapshot == nil || !snapshot.Started {
		return &Idle{}
	}

	deferred := copyDeferredCheckpoints(snapshot.DeferredCheckpoints)
	job := &JobState{
		Height:              snapshot.Height,
		Trigger:             snapshot.Trigger,
		PlannerState:        copyPlannerState(snapshot.State),
		DeferredCheckpoints: deferred,
		FailReason:          snapshot.Fail,
		SweepAttempts:       snapshot.SweepAttempts,
	}

	switch phaseFromPlannerState(job) {
	case PhaseCompleted:
		return &Completed{Job: job}

	case PhaseFailed:
		return &Failed{Job: job}

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

	case *Completed:
		return PhaseCompleted

	case *Failed:
		return PhaseFailed

	default:
		return PhaseFailed
	}
}

// phaseFromPlannerState derives a coarse phase from the durable planner state
// when restoring from snapshot before the planner is bound.
func phaseFromPlannerState(job *JobState) Phase {
	if job == nil {
		return PhasePending
	}

	if job.FailReason != "" {
		return PhaseFailed
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

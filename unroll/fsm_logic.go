package unroll

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// processEventWithJob applies one event to the supplied job state and derives
// the next concrete FSM state plus any actor-boundary outbox.
func processEventWithJob(ctx context.Context, job *JobState,
	event Event, env *Environment) (*StateTransition, error) {

	if job == nil {
		return nil, fmt.Errorf("job state must be provided")
	}

	nextJob := job.Copy()
	reissue := false

	switch e := event.(type) {
	case *ResumeEvent:
		if e.Height > nextJob.Height {
			nextJob.Height = e.Height
		}
		reissue = true

	case *HeightUpdatedEvent:
		if e.Height > nextJob.Height {
			nextJob.Height = e.Height
		}

	case *TxConfirmedEvent:
		applyConfirmedEvent(nextJob, e, env)

	case *TxFailedEvent:
		applyFailedEvent(nextJob, e)

	case *SweepBroadcastedEvent:
		nextJob.PlannerState.Sweep.Status =
			unrollplan.SweepStatusBroadcasted
		nextJob.PlannerState.Sweep.Txid = copyHash(&e.Txid)

	case *SweepBuildFailedEvent:
		applySweepBuildFailed(nextJob, e.Reason)

	case *FailEvent:
		nextJob.FailReason = e.Reason

	case *StartEvent:
		if e.Height > nextJob.Height {
			nextJob.Height = e.Height
		}
		nextJob.Trigger = e.Trigger

	default:
		return nil, fmt.Errorf("unexpected event %T", event)
	}

	return deriveStateTransition(ctx, nextJob, env, reissue)
}

// deriveStateTransition computes the next concrete FSM state and any side
// effects required to make progress from the supplied durable job state.
func deriveStateTransition(_ context.Context, job *JobState,
	env *Environment, reissue bool) (*StateTransition, error) {

	if job == nil {
		return nil, fmt.Errorf("job state must be provided")
	}

	if env == nil || env.Proof == nil || env.Planner == nil {
		return nil, fmt.Errorf(
			"unroll environment must be fully populated",
		)
	}

	if err := job.PlannerState.Validate(env.Proof); err != nil {
		return nil, err
	}

	if job.FailReason != "" {
		return &StateTransition{
			NextState: &Failed{Job: job.Copy()},
		}, nil
	}

	snapshot, err := env.Planner.Plan(job.Height, &job.PlannerState)
	if err != nil {
		return nil, err
	}

	var outbox []OutboxEvent
	if reissue {
		if len(job.PlannerState.InFlightTxids) > 0 {
			outbox = append(outbox, &ReissueInFlightTransactions{
				Txids: append([]chainhash.Hash(nil),
					job.PlannerState.InFlightTxids...),
			})
		}

		sweepBroadcasted := job.PlannerState.Sweep.Status ==
			unrollplan.SweepStatusBroadcasted
		if sweepBroadcasted {
			outbox = append(outbox, &ReissueSweepConfirmation{})
		}
	}

	switch {
	case snapshot.Done:
		return transitionWithOutbox(
			&Completed{Job: job.Copy()}, outbox,
		), nil

	case job.PlannerState.Sweep.Status == unrollplan.SweepStatusBroadcasted:
		return transitionWithOutbox(
			&AwaitingSweepConfirmation{Job: job.Copy()}, outbox,
		), nil

	case snapshot.NeedSweep:
		outbox = append(outbox, &RequestSweepBuild{})
		return transitionWithOutbox(
			&AwaitingSweepBroadcast{Job: job.Copy()}, outbox,
		), nil

	case snapshot.CSV != nil && !snapshot.CSV.Ready:
		return transitionWithOutbox(
			&AwaitingCSV{Job: job.Copy()}, outbox,
		), nil

	default:
		readyTxids := readyTxids(snapshot.Ready)
		if len(readyTxids) > 0 {
			job.PlannerState.InFlightTxids = appendUniqueSorted(
				job.PlannerState.InFlightTxids, readyTxids...,
			)
			outbox = append(outbox, &EnsureReadyTransactions{
				Txids: readyTxids,
			})
		}

		return transitionWithOutbox(
			&AwaitingMaterialization{Job: job.Copy()}, outbox,
		), nil
	}
}

// transitionWithOutbox wraps the next state and optional outbox into one state
// transition result.
func transitionWithOutbox(nextState State,
	outbox []OutboxEvent) *StateTransition {

	transition := &StateTransition{
		NextState: nextState,
	}

	if len(outbox) == 0 {
		return transition
	}

	transition.NewEvents = fn.Some(EmittedEvent{
		Outbox: outbox,
	})

	return transition
}

// applyConfirmedEvent applies one transaction confirmation to the durable job
// state.
func applyConfirmedEvent(job *JobState, event *TxConfirmedEvent,
	env *Environment) {

	if job == nil || event == nil {
		return
	}

	if job.PlannerState.Sweep.Txid != nil &&
		*job.PlannerState.Sweep.Txid == event.Txid {

		job.PlannerState.Sweep.Status = unrollplan.SweepStatusConfirmed
		job.PlannerState.Sweep.ConfirmHeight = copyHeight(event.Height)
		if event.Height > job.Height {
			job.Height = event.Height
		}

		return
	}

	job.PlannerState.ConfirmedTxids = appendUniqueSorted(
		job.PlannerState.ConfirmedTxids, event.Txid,
	)
	job.PlannerState.InFlightTxids = removeHash(
		job.PlannerState.InFlightTxids, event.Txid,
	)

	if env != nil && env.Proof != nil &&
		event.Txid == env.Proof.TargetOutpoint().Hash &&
		job.PlannerState.TargetConfirmHeight == nil {

		job.PlannerState.TargetConfirmHeight = copyHeight(event.Height)
	}

	if event.Height > job.Height {
		job.Height = event.Height
	}
}

// applyFailedEvent applies one transaction failure to the durable job state.
// Sweep-tx failures are retried up to maxSweepAttempts; proof-tx failures are
// always terminal.
func applyFailedEvent(job *JobState, event *TxFailedEvent) {
	if job == nil || event == nil {
		return
	}

	// Detect sweep-tx failure by matching against the recorded sweep txid.
	if job.PlannerState.Sweep.Txid != nil &&
		*job.PlannerState.Sweep.Txid == event.Txid {

		applySweepBuildFailed(job, event.Reason)

		return
	}

	// Proof-tx failure is always terminal.
	job.PlannerState.InFlightTxids = removeHash(
		job.PlannerState.InFlightTxids, event.Txid,
	)

	job.FailReason = event.Reason
}

// applySweepBuildFailed records a sweep build or broadcast failure. If the
// actor has not exhausted its retry budget the sweep state resets to pending
// so the FSM re-enters AwaitingSweepBroadcast on the next evaluation.
func applySweepBuildFailed(job *JobState, reason string) {
	job.SweepAttempts++

	if job.SweepAttempts >= maxSweepAttempts {
		job.FailReason = reason

		return
	}

	// Reset sweep to pending so the planner sees NeedSweep again.
	job.PlannerState.Sweep.Status = unrollplan.SweepStatusPending
	job.PlannerState.Sweep.Txid = nil
}

// readyTxids projects a planner frontier into deterministic txid order.
func readyTxids(frontier []unrollplan.TxFrontier) []chainhash.Hash {
	txids := make([]chainhash.Hash, 0, len(frontier))
	for i := range frontier {
		txids = append(txids, frontier[i].Txid)
	}

	sortHashes(txids)

	return txids
}

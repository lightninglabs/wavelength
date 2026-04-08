package unroll

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// processEventWithJob is the single-source-of-truth update function for
// every non-Idle state. Each concrete FSM state (AwaitingMaterialization,
// AwaitingCSV, AwaitingSweepBroadcast, AwaitingSweepConfirmation)
// delegates its ProcessEvent implementation here instead of duplicating
// event handling per state.
//
// The function runs in two phases:
//
//  1. Mutate a deep copy of the inbound JobState based on the event
//     kind. This is where "what just happened" is recorded: a height
//     moved, a tx confirmed, a tx failed, the sweep broadcast, etc. The
//     apply* helpers isolate the per-event arithmetic.
//
//  2. Hand the updated JobState to deriveStateTransition, which asks
//     the pure [unrollplan.Planner] what to do next from this state and
//     picks the matching FSM state + outbox.
//
// Splitting event application from transition derivation is what lets
// ResumeEvent re-run the planner without inventing any new mutation: it
// just bumps height and flips the reissue flag so deriveStateTransition
// emits Reissue* outbox events.
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
		nextJob.PlannerState.Sweep.Txid = fn.Some(e.Txid)

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

// deriveStateTransition is the core "what FSM state should we be in
// now?" function. It is a pure reduction: given the updated JobState,
// the immutable Environment (proof graph + planner), and a reissue
// flag, it returns the next concrete State plus any OutboxEvents the
// actor boundary needs to execute.
//
// The decision order reflects lifecycle precedence:
//
//  1. Planner-state invariants (ConfirmedTxids and InFlightTxids
//     consistent with the proof graph) are validated up front so the
//     FSM fails loudly on desync rather than making progress on a
//     corrupted state.
//
//  2. FailReason != "" short-circuits to Failed. This catches both
//     proof-tx terminal failures (set in applyFailedEvent) and
//     explicit FailEvents (e.g. from external spend detection).
//
//  3. On a reissue (ResumeEvent path) we emit ReissueInFlightTransactions
//     for every currently in-flight node and ReissueSweepConfirmation
//     if the sweep was already broadcast. This re-arms every txconfirm
//     subscription that the checkpoint knows about.
//
//  4. The planner decides phase:
//     - Done (every input confirmed, sweep confirmed if needed) →
//     Completed.
//     - Sweep already broadcasted → AwaitingSweepConfirmation.
//     - NeedSweep → AwaitingSweepBroadcast + RequestSweepBuild outbox.
//     - CSV not ready yet → AwaitingCSV.
//     - Otherwise → AwaitingMaterialization with EnsureReadyTransactions
//     for any newly-unblocked ready frontier.
//
// Notice: no IO, no time, no randomness. This function can be exercised
// deterministically in unit tests, which is why the FSM intentionally
// lives separate from the behavior.
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

	// Guard against checkpoint/proof drift: every txid recorded as
	// confirmed or in-flight must resolve against the current proof
	// graph. A mismatch means the checkpoint references a transaction
	// our resolver no longer knows about — fail loudly now instead of
	// driving the FSM into an impossible state.
	if err := job.PlannerState.Validate(env.Proof); err != nil {
		return nil, err
	}

	// Terminal short-circuit. applyFailedEvent populates FailReason
	// for proof-tx terminal failures, and handleSpendObserved emits
	// explicit FailEvents for external-spend detection; both land
	// here before any planner work.
	if job.FailReason != "" {
		return &StateTransition{
			NextState: &Failed{Job: job.Copy()},
		}, nil
	}

	// Consult the pure planner. Plan() is stateless; it reads
	// PlannerState + the proof graph and returns a snapshot with Done
	// / NeedSweep / CSV / Ready fields. All phase decisions below
	// come from that snapshot.
	snapshot, err := env.Planner.Plan(job.Height, &job.PlannerState)
	if err != nil {
		return nil, err
	}

	// On a restart-triggered evaluation we re-emit the reissue
	// outbox events for every subscription the checkpoint knows
	// about. These are additive to whatever the phase decision
	// below needs — txconfirm dedup absorbs the duplicates on
	// on-chain submission.
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

	// Phase decision cascade. Order is deliberate: Done before any
	// sweep branch, sweep-broadcasted before sweep-build (so a
	// restart with a broadcast sweep does not re-enter build), build
	// before CSV (since NeedSweep implies CSV already matured), and
	// materialization as the default.
	switch {
	case snapshot.Done:
		// Every required tx (proof nodes + sweep) has confirmed.
		return transitionWithOutbox(
			&Completed{Job: job.Copy()}, outbox,
		), nil

	case job.PlannerState.Sweep.Status == unrollplan.SweepStatusBroadcasted:
		// Sweep already out on the wire, waiting for confirm.
		return transitionWithOutbox(
			&AwaitingSweepConfirmation{Job: job.Copy()}, outbox,
		), nil

	case snapshot.NeedSweep:
		// Target confirmed + CSV matured = it is time to build
		// and broadcast the sweep. The RequestSweepBuild outbox
		// event is what triggers startSweep in the actor behavior.
		outbox = append(outbox, &RequestSweepBuild{})
		return transitionWithOutbox(
			&AwaitingSweepBroadcast{Job: job.Copy()}, outbox,
		), nil

	case snapshot.CSV.IsSome() && !snapshot.CSV.UnsafeFromSome().Ready:
		// Target confirmed but the CSV delay has not matured;
		// nothing to do until HeightUpdatedEvent carries the
		// chain forward.
		return transitionWithOutbox(
			&AwaitingCSV{Job: job.Copy()}, outbox,
		), nil

	default:
		// There is still proof ancestry to confirm. Hand the
		// ready frontier to the actor so it can submit each one
		// to txconfirm; record them as in-flight so subsequent
		// runs do not try to resubmit (idempotent at the
		// txconfirm layer either way, but a clean planner state
		// is a nicer invariant to hold).
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

// applyConfirmedEvent records a single txconfirm success against the
// durable job state. It dispatches on whether the confirmed txid is:
//
//   - The final sweep (by txid match against PlannerState.Sweep.Txid):
//     flip Sweep.Status to Confirmed and remember its block height.
//     This is what graduates AwaitingSweepConfirmation → Completed.
//
//   - A proof-graph node: move the txid from InFlightTxids to
//     ConfirmedTxids. If this happens to be the target transaction
//     itself we also record TargetConfirmHeight — the planner needs
//     that to compute when the CSV delay has matured.
//
// Height is always advanced (max of current and event height) so late
// confirmations for earlier blocks do not roll the clock back.
func applyConfirmedEvent(job *JobState, event *TxConfirmedEvent,
	env *Environment) {

	if job == nil || event == nil {
		return
	}

	if job.PlannerState.Sweep.Txid.IsSome() &&
		job.PlannerState.Sweep.Txid.UnsafeFromSome() == event.Txid {

		job.PlannerState.Sweep.Status = unrollplan.SweepStatusConfirmed
		job.PlannerState.Sweep.ConfirmHeight = fn.Some(event.Height)
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
		job.PlannerState.TargetConfirmHeight.IsNone() {

		job.PlannerState.TargetConfirmHeight = fn.Some(event.Height)
	}

	if event.Height > job.Height {
		job.Height = event.Height
	}
}

// applyFailedEvent records a single txconfirm failure. Proof-node
// failures and sweep failures have different semantics:
//
//   - A proof-graph transaction failing is terminal. There is no way
//     for the client to rebuild or replace an operator-signed proof
//     node, so the only option is to surface the reason and stop.
//
//   - A sweep failing is often recoverable (fee too low, mempool
//     contention, fee-spike eviction). applySweepBuildFailed bumps a
//     retry counter and, if the budget is not exhausted, clears the
//     planner's sweep state so deriveStateTransition will emit a fresh
//     RequestSweepBuild. The actor's cached sweepTx also gets cleared
//     on the next attempt because the FSM state carries
//     SweepStatusPending again.
func applyFailedEvent(job *JobState, event *TxFailedEvent) {
	if job == nil || event == nil {
		return
	}

	// Detect sweep-tx failure by matching against the recorded sweep txid.
	if job.PlannerState.Sweep.Txid.IsSome() &&
		job.PlannerState.Sweep.Txid.UnsafeFromSome() == event.Txid {

		applySweepBuildFailed(job, event.Reason)

		return
	}

	// Proof-tx failure is always terminal.
	job.PlannerState.InFlightTxids = removeHash(
		job.PlannerState.InFlightTxids, event.Txid,
	)

	job.FailReason = event.Reason
}

// applySweepBuildFailed records a sweep build or broadcast failure and
// decides whether to terminate or retry.
//
// Budget: maxSweepAttempts tries. Past that, we stop hammering the
// mempool and transition to Failed with the most recent reason.
//
// Within the budget, we reset sweep-specific planner fields (Status and
// Txid) so the planner returns NeedSweep=true on the next evaluation
// and deriveStateTransition emits a fresh RequestSweepBuild. Note that
// the actor behavior's cached b.sweepTx is NOT cleared here — startSweep
// reuses it on retry to keep txconfirm's txid-keyed dedup working and
// to avoid burning a new BIP32 wallet address per attempt. If the
// failure cause is persistent (fee-rate too low, double-spend of the
// input) the retry will simply rediscover the same rejection, and we
// rely on maxSweepAttempts to bound the loop.
func applySweepBuildFailed(job *JobState, reason string) {
	job.SweepAttempts++

	if job.SweepAttempts >= maxSweepAttempts {
		job.FailReason = reason

		return
	}

	// Reset sweep to pending so the planner sees NeedSweep again.
	job.PlannerState.Sweep.Status = unrollplan.SweepStatusPending
	job.PlannerState.Sweep.Txid = fn.None[chainhash.Hash]()
}

// readyTxids projects one planner ready-frontier into a deterministic
// txid list.
//
// The planner returns frontier entries in whatever order its internal
// graph walk produced, which can differ across invocations even when
// the logical set is the same. Sorting here gives us stable
// checkpoint bytes (good for diffing), stable log lines, and stable
// txconfirm Ask ordering.
func readyTxids(frontier []unrollplan.TxFrontier) []chainhash.Hash {
	txids := make([]chainhash.Hash, 0, len(frontier))
	for i := range frontier {
		txids = append(txids, frontier[i].Txid)
	}

	sortHashes(txids)

	return txids
}

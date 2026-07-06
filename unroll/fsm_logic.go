package unroll

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/darepo-client/lib/recovery"
	"github.com/lightninglabs/darepo-client/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// defaultFraudCheckpointSafetyMargin is the number of blocks subtracted
// from a checkpoint's relative-expiry window to compute the recipient
// backstop deadline. The margin gives the operator (or a peer
// fraud-triggered unroll) time to publish the checkpoint first; the
// recipient only steps in when the deadline arrives without observed
// confirmation. 24 blocks (~4h on mainnet) is a conservative budget
// that survives mempool congestion, fee-rate spikes, and a slow
// operator restart while still leaving useful slack before the CSV
// matures.
//
// For chains with a very short csvDelay (e.g. itest with
// VTXOExitDelay=16) checkpointBackstopHeight clamps the margin to
// csvDelay/2 so the deadline does not fall before the current height.
// See checkpointBackstopHeight for the clamp logic.
const defaultFraudCheckpointSafetyMargin int32 = 24

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
func processEventWithJob(ctx context.Context, job *JobState, event Event,
	env *Environment) (*StateTransition, error) {

	if job == nil {
		return nil, fmt.Errorf("job state must be provided")
	}

	nextJob := job.Copy()
	reissue := false

	// deferralAnchor pins the base height used when stamping a brand-new
	// DeferredCheckpoint deadline this turn. A TxConfirmedEvent that
	// promotes a child to the planner's ready frontier carries the parent's
	// own confirmation height in e.Height; that is the height at which the
	// child became unblocked, and it is the height we want the operator's
	// deferral window measured from. Without this anchor the deadline
	// collapses onto whatever job.Height has already drifted to (e.g. a
	// bulk-flush of cached HeightUpdatedEvents that ran ahead of the
	// TxConfirmedEvent in the actor mailbox), pushing the recipient's
	// backstop arbitrarily far into the future. Non-confirmation events
	// leave the anchor unset so deriveStateTransition falls back to
	// job.Height for the StartEvent and ResumeEvent paths, preserving the
	// previous behavior there.
	deferralAnchor := fn.None[int32]()

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
		deferralAnchor = fn.Some(e.Height)

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
		if e.ExitPolicyKind != "" {
			nextJob.ExitPolicyKind = exitPolicyKind(
				e.ExitPolicyKind,
			)
			nextJob.ExitPolicyRef = e.ExitPolicyRef
		}

	default:
		return nil, fmt.Errorf("unexpected event %T", event)
	}

	return deriveStateTransition(ctx, nextJob, env, reissue, deferralAnchor)
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
func deriveStateTransition(ctx context.Context, job *JobState, env *Environment,
	reissue bool,
	deferralAnchor fn.Option[int32]) (*StateTransition, error) {

	if job == nil {
		return nil, fmt.Errorf("job state must be provided")
	}

	if env == nil || env.Proof == nil || env.Planner == nil {
		return nil, fmt.Errorf("unroll environment must be fully " +
			"populated")
	}

	// Reject a zero CSV delay under TriggerFraudSpend up front. The
	// fraud trigger relies on a deferral window between checkpoint
	// readiness and the recipient backstop deadline; with csvDelay=0
	// checkpointBackstopHeight returns the current height and the
	// recipient broadcasts the checkpoint immediately, defeating the
	// deferral. This combination is almost certainly a
	// misconfiguration; surface it loudly rather than silently
	// submitting checkpoints without any deferral window.
	if job.Trigger == TriggerFraudSpend && env.Proof.CSVDelay() == 0 {
		return nil, fmt.Errorf("fraud-trigger unroll requires a " +
			"non-zero CSV delay")
	}

	if err := validateDeferredCheckpoints(job, env); err != nil {
		return nil, err
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
			NextState: &Failed{
				Job: job.Copy(),
			},
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
				Txids: append(
					[]chainhash.Hash(nil),
					job.PlannerState.InFlightTxids...,
				),
			})
		}

		sweepBroadcasted := job.PlannerState.Sweep.Status ==
			unrollplan.SweepStatusBroadcasted
		if sweepBroadcasted {
			outbox = append(outbox, &ReissueSweepConfirmation{})
		}

		if len(job.DeferredCheckpoints) > 0 {
			outbox = append(outbox, &WatchDeferredCheckpoints{
				Txids: deferredCheckpointTxids(
					job.DeferredCheckpoints,
				),
			})
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
			&Completed{
				Job: job.Copy(),
			},
			outbox,
		), nil

	case job.PlannerState.Sweep.Status == unrollplan.SweepStatusBroadcasted:
		// Sweep already out on the wire, waiting for confirm.
		return transitionWithOutbox(
			&AwaitingSweepConfirmation{
				Job: job.Copy(),
			},
			outbox,
		), nil

	case snapshot.NeedSweep:
		// Target confirmed + CSV matured = it is time to build
		// and broadcast the sweep. The RequestSweepBuild outbox
		// event is what triggers startSweep in the actor behavior.
		outbox = append(outbox, &RequestSweepBuild{})

		return transitionWithOutbox(
			&AwaitingSweepBroadcast{
				Job: job.Copy(),
			},
			outbox,
		), nil

	case snapshot.CSV.IsSome() && !snapshot.CSV.UnsafeFromSome().Ready:
		// Target confirmed but the CSV delay has not matured;
		// nothing to do until HeightUpdatedEvent carries the
		// chain forward.
		return transitionWithOutbox(
			&AwaitingCSV{
				Job: job.Copy(),
			},
			outbox,
		), nil

	default:
		// There is still proof ancestry to confirm. Hand the
		// ready frontier to the actor so it can submit each one
		// to txconfirm; record them as in-flight so subsequent
		// runs do not try to resubmit (idempotent at the
		// txconfirm layer either way, but a clean planner state
		// is a nicer invariant to hold).
		readyTxids, watchTxids := readyMaterializationTxids(
			job, env, snapshot.Ready, deferralAnchor,
		)
		if len(watchTxids) > 0 {
			outbox = append(outbox, &WatchDeferredCheckpoints{
				Txids: watchTxids,
			})
		}
		if len(readyTxids) > 0 {
			job.PlannerState.InFlightTxids = appendUniqueSorted(
				job.PlannerState.InFlightTxids, readyTxids...,
			)
			outbox = append(outbox, &EnsureReadyTransactions{
				Txids: readyTxids,
			})
		}

		return transitionWithOutbox(
			&AwaitingMaterialization{
				Job: job.Copy(),
			},
			outbox,
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
	job.DeferredCheckpoints = removeDeferredCheckpoint(
		job.DeferredCheckpoints, event.Txid,
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
	job.DeferredCheckpoints = removeDeferredCheckpoint(
		job.DeferredCheckpoints, event.Txid,
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
//
// Replay caveat (durable actor Read/Commit path): SweepAttempts++ is the
// only non-monotone mutation in this FSM, so it is not idempotent under
// message replay. On the Read/Commit path the failure event is Staged in
// its own short transaction ahead of the lease-fenced Commit; if the
// process crashes (or the lease is lost) in the window between that Stage
// and the Commit, the un-acked failure message is redelivered and applied
// again, incrementing the counter a second time for one logical failure.
// The pre-migration whole-Receive-in-one-transaction path did not have this
// edge because the increment and the ack rolled back together on a nack.
// The impact is bounded and loses no funds: the sweep itself is never
// double-broadcast (b.sweepTx is reused under a stable txid), only the retry
// counter is inflated, so the worst case is a unilateral-exit job reaching
// terminal Failed up to maxSweepAttempts retries early, which the client can
// re-initiate. A fully idempotent retry accounting (deduping the failure per
// build attempt rather than per event arrival) is tracked as a follow-up; it
// is a standalone FSM change, not part of the Read/Commit migration that
// merely exposed this latent non-idempotency.
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

// readyMaterializationTxids splits planner-ready proof nodes into transactions
// to submit now and deferred checkpoint watches to register.
//
// deferralAnchor pins the height used as the base of any newly-stamped
// DeferredCheckpoint deadline. When set (a TxConfirmedEvent just promoted
// these frontier items), it is the parent's confirmation height, so the
// deferral window measures the operator's response time from when the
// child actually became unblocked. When unset (StartEvent / ResumeEvent),
// we fall back to job.Height — the only signal available before any
// confirmation event has arrived.
func readyMaterializationTxids(job *JobState, env *Environment,
	frontier []unrollplan.TxFrontier,
	deferralAnchor fn.Option[int32]) ([]chainhash.Hash, []chainhash.Hash) {

	if job == nil || env == nil || env.Proof == nil {
		return readyTxids(frontier), nil
	}

	deadlineBase := deferralAnchor.UnwrapOr(job.Height)

	ready := make([]chainhash.Hash, 0, len(frontier))
	watch := make([]chainhash.Hash, 0, len(frontier))
	for i := range frontier {
		item := frontier[i]
		if shouldSubmitReadyFrontier(job, item) {
			ready = append(ready, item.Txid)
			continue
		}

		if _, ok := findDeferredCheckpoint(
			job.DeferredCheckpoints, item.Txid,
		); ok {

			continue
		}

		deadline := checkpointBackstopHeight(
			deadlineBase, env.Proof.CSVDelay(),
			env.FraudCheckpointSafetyMargin,
		)
		if deadline <= job.Height {
			ready = append(ready, item.Txid)
			continue
		}

		job.DeferredCheckpoints = appendDeferredCheckpoint(
			job.DeferredCheckpoints, DeferredCheckpoint{
				Txid:           item.Txid,
				DeadlineHeight: deadline,
			},
		)
		watch = append(watch, item.Txid)
	}

	sortHashes(ready)
	sortHashes(watch)

	return ready, watch
}

// shouldSubmitReadyFrontier reports whether a planner-ready node should be
// handed to txconfirm immediately.
func shouldSubmitReadyFrontier(job *JobState, item unrollplan.TxFrontier) bool {
	if job.Trigger != TriggerFraudSpend {
		return true
	}

	if item.Node == nil || item.Node.Kind != recovery.NodeKindCheckpoint {
		return true
	}

	deferred, ok := findDeferredCheckpoint(
		job.DeferredCheckpoints, item.Txid,
	)
	if !ok {
		return false
	}

	if job.Height < deferred.DeadlineHeight {
		return false
	}

	job.DeferredCheckpoints = removeDeferredCheckpoint(
		job.DeferredCheckpoints, item.Txid,
	)

	return true
}

// checkpointBackstopHeight returns the first height at which a fraud-triggered
// unroll should broadcast a ready checkpoint itself. A non-positive
// configuredMargin falls back to defaultFraudCheckpointSafetyMargin; the
// effective margin is then clamped to csvDelay/2 if csvDelay is too small to
// absorb it, so the deadline never falls before the current height.
func checkpointBackstopHeight(height int32, csvDelay uint32,
	configuredMargin int32) int32 {

	margin := configuredMargin
	if margin <= 0 {
		margin = defaultFraudCheckpointSafetyMargin
	}

	delay := int32(csvDelay)
	if delay <= margin {
		margin = delay / 2
	}

	return height + delay - margin
}

// deferredCheckpointTxids projects deferred checkpoints into sorted txids.
func deferredCheckpointTxids(
	checkpoints []DeferredCheckpoint) []chainhash.Hash {

	txids := make([]chainhash.Hash, 0, len(checkpoints))
	for _, checkpoint := range checkpoints {
		txids = append(txids, checkpoint.Txid)
	}

	sortHashes(txids)

	return txids
}

// validateDeferredCheckpoints checks that every deferred checkpoint still
// references a checkpoint node in the immutable proof graph.
func validateDeferredCheckpoints(job *JobState, env *Environment) error {
	seen := make(map[chainhash.Hash]struct{}, len(job.DeferredCheckpoints))
	for _, checkpoint := range job.DeferredCheckpoints {
		if _, ok := seen[checkpoint.Txid]; ok {
			return fmt.Errorf("duplicate deferred checkpoint %s",
				checkpoint.Txid)
		}
		seen[checkpoint.Txid] = struct{}{}

		txid := checkpoint.Txid
		node, ok := env.Proof.Node(txid)
		if !ok {
			return fmt.Errorf("deferred checkpoint %s is not "+
				"in proof", txid)
		}
		if node.Kind != recovery.NodeKindCheckpoint {
			return fmt.Errorf("deferred checkpoint %s has kind %s",
				txid, node.Kind)
		}
		if containsHash(job.PlannerState.ConfirmedTxids, txid) {
			return fmt.Errorf("deferred checkpoint %s is confirmed",
				txid)
		}
		if containsHash(job.PlannerState.InFlightTxids, txid) {
			return fmt.Errorf("deferred checkpoint %s is in-flight",
				txid)
		}
	}

	return nil
}

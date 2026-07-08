package unroll

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/recovery"
	"github.com/lightninglabs/darepo-client/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// ConfirmedAnchor describes a tx confirmation that the backend reports
// is currently live on the canonical chain.
type ConfirmedAnchor struct {
	// Txid is the confirmed transaction hash.
	Txid chainhash.Hash

	// Height is the block height the transaction confirmed at.
	Height int32
}

// SpendAnchor describes an outpoint spend that the backend reports is
// currently live on the canonical chain.
type SpendAnchor struct {
	// Outpoint is the spent outpoint.
	Outpoint wire.OutPoint

	// SpendingTxid is the transaction that consumed the outpoint.
	SpendingTxid chainhash.Hash

	// SpendingHeight is the block height the spending tx confirmed at.
	SpendingHeight int32
}

// ChainReconciler is the narrow interface the unroll actor uses on
// restart to verify that the chain anchors recorded in its checkpoint
// are still live on the canonical chain.
//
// The interface is intentionally small: it answers two questions, both
// keyed on data the actor already has in its checkpoint. Implementations
// query the backend (lnd's chainntnfs, neutrino, or a bitcoind RPC) and
// return fn.None for "not on chain anymore" so the unroll layer can
// stay backend-agnostic.
//
// Both methods return errors for transport-level failures the caller
// should retry; an "anchor is not on chain" answer is conveyed via
// fn.None, not via error.
type ChainReconciler interface {
	// ConfirmedTx reports the current confirmation status of a tx on
	// the canonical chain. fn.None means the tx is no longer
	// confirmed (typically because the block was reorged out while
	// the daemon was offline).
	ConfirmedTx(ctx context.Context,
		txid chainhash.Hash) (fn.Option[ConfirmedAnchor], error)

	// SpentOutpoint reports the current spend status of an outpoint
	// on the canonical chain. fn.None means the outpoint is currently
	// unspent.
	SpentOutpoint(ctx context.Context,
		outpoint wire.OutPoint) (fn.Option[SpendAnchor], error)
}

// ChainReconcilerFactory builds a ChainReconciler bound to a specific
// per-target unroll actor. Per-target actors invoke the factory after
// loading their proof; the target outpoint identifies the calling
// actor and lets the implementation namespace its chainsource probe
// caller IDs so two actors that share a proof-graph ancestor (the
// common case for sibling VTXOs) cannot collide on chainsource
// service keys when they reconcile concurrently. The proof argument
// supplies output scripts because lnd's tx index is not always
// available, so the (txid, pkScript) pair is the canonical way to
// identify a tx during a historical block scan. Test stubs that do
// not need either argument can ignore them.
type ChainReconcilerFactory func(target wire.OutPoint,
	proof *recovery.Proof) ChainReconciler

// reconcileCheckpoint walks the persisted checkpoint and prunes any
// chain anchor that the reconciler reports is no longer live on the
// canonical chain. The checkpoint is mutated in place.
//
// The reconciliation order mirrors applyReorgedEvent:
//
//   - For every confirmed proof-graph txid, ask the reconciler whether
//     it is still confirmed. Anchors that are not are dropped, and so
//     are their descendants in the proof graph (State.Validate's
//     topological invariant requires every confirmed/in-flight node to
//     have confirmed parents).
//   - If the target tx was confirmed but is now absent, clear
//     TargetConfirmHeight and downgrade the sweep to Pending — same
//     gate the live reorg reducer enforces.
//   - If a Broadcasted / Confirmed sweep tx is no longer on chain,
//     reset the sweep state so the actor re-broadcasts on its next
//     planning step.
//   - Reconcile the target outpoint spend status against any persisted
//     ProvisionalExternalSpend anchor: set, refresh, or clear it so
//     the restored FSM enters the same parked state a live
//     SpendObservedEvent / SpendReorgedEvent sequence would have
//     produced.
//
// On any reconciler error we surface it untouched: the actor must not
// proceed to broadcast off a checkpoint we could not verify.
func reconcileCheckpoint(ctx context.Context, reconciler ChainReconciler,
	proof *recovery.Proof, checkpoint *actorCheckpoint) error {

	if reconciler == nil || checkpoint == nil || proof == nil {
		return nil
	}

	// Snapshot the input list so the loop iterates over a stable
	// slice while we mutate the live ConfirmedTxids field.
	confirmedIn := append(
		[]chainhash.Hash(nil), checkpoint.State.ConfirmedTxids...,
	)

	for _, txid := range confirmedIn {
		anchor, err := reconciler.ConfirmedTx(ctx, txid)
		if err != nil {
			return fmt.Errorf("reconcile confirmed tx %s: %w", txid,
				err)
		}
		if anchor.IsSome() {
			// The tx is still confirmed. Refresh the
			// target-derived height in case the tx was re-mined
			// at a different block height while the daemon was
			// offline; leaving the stale height in place would
			// make CSV maturity look closer than it actually is
			// and could drive a premature sweep on the next
			// planning step. Also raise the checkpoint's
			// best-height watermark to at least the live anchor
			// height so derived state is internally consistent.
			liveHeight := anchor.UnsafeFromSome().Height
			if txid == proof.TargetOutpoint().Hash {
				checkpoint.State.TargetConfirmHeight = fn.Some(
					liveHeight,
				)
			}
			if liveHeight > checkpoint.Height {
				checkpoint.Height = liveHeight
			}

			continue
		}

		pruneReorgedSubtree(proof, &checkpoint.State, txid)

		if txid == proof.TargetOutpoint().Hash {
			downgradeSweepOnTargetLoss(&checkpoint.State)
		}
	}

	// If a sweep was already broadcast or confirmed, verify that it
	// is still on chain. A sweep that vanished while the daemon was
	// offline must be re-broadcastable; resetting Status to Pending
	// triggers a fresh RequestSweepBuild on the next derivation, and
	// the cached b.sweepTx (restored from the checkpoint at boot) is
	// reused so the txid stays stable.
	if checkpoint.State.Sweep.Txid.IsSome() &&
		checkpoint.State.Sweep.Status !=
			unrollplan.SweepStatusPending {

		sweepTxid := checkpoint.State.Sweep.Txid.UnsafeFromSome()
		anchor, err := reconciler.ConfirmedTx(ctx, sweepTxid)
		if err != nil {
			return fmt.Errorf("reconcile sweep tx %s: %w",
				sweepTxid, err)
		}
		switch {
		case anchor.IsNone():
			// Sweep is no longer on chain. Reset so the next
			// derivation re-broadcasts using the cached bytes.
			checkpoint.State.Sweep.Status =
				unrollplan.SweepStatusPending
			checkpoint.State.Sweep.Txid =
				fn.None[chainhash.Hash]()
			checkpoint.State.Sweep.ConfirmHeight =
				fn.None[int32]()

		case checkpoint.State.Sweep.Status ==
			unrollplan.SweepStatusConfirmed:

			// Sweep is still on chain but at potentially a new
			// height. Recompute ConfirmHeight from the live
			// answer so a sweep that confirmed in a different
			// block is not stuck reporting a stale height.
			checkpoint.State.Sweep.ConfirmHeight = fn.Some(
				anchor.UnsafeFromSome().Height,
			)
		}
	}

	if err := reconcileExternalSpend(
		ctx, reconciler, proof, checkpoint,
	); err != nil {
		return err
	}

	return nil
}

// reconcileExternalSpend cross-checks the target outpoint's spend
// status on the canonical chain against any ProvisionalExternalSpend
// anchor the checkpoint carries. Four cases land here:
//
//  1. Chain says target spent + checkpoint anchor matches the same
//     spender txid: no-op (refresh the height in case it shifted to a
//     re-organized block).
//  2. Chain says target spent + checkpoint anchor for a DIFFERENT
//     spender: update the anchor to the live spender.
//  3. Chain says target spent + no checkpoint anchor: install one.
//     This is the "external spend confirmed while the daemon was
//     offline" path.
//  4. Chain says target unspent + checkpoint anchor present: clear
//     the anchor. The spending block was reorged out during downtime.
//
// In cases 2/3, "spent by a proof-graph node" or "spent by our own
// sweep" are treated as benign: those are the same classifications
// the live spend-watch handler runs in handleSpendObserved.
func reconcileExternalSpend(ctx context.Context, reconciler ChainReconciler,
	proof *recovery.Proof, checkpoint *actorCheckpoint) error {

	targetOutpoint := proof.TargetOutpoint()
	anchor, err := reconciler.SpentOutpoint(ctx, targetOutpoint)
	if err != nil {
		return fmt.Errorf("reconcile target spend %s: %w",
			targetOutpoint, err)
	}

	if anchor.IsNone() {
		// Case 4: chain reports target unspent. Any persisted
		// anchor was reorged out while we were offline; clear it
		// so the FSM does NOT park in
		// AwaitingExternalSpendFinality on restore.
		checkpoint.ProvisionalExternalSpend =
			fn.None[ExternalSpendAnchor]()

		return nil
	}

	live := anchor.UnsafeFromSome()

	// Skip benign spenders that the live spend-watch path would also
	// classify as "expected materialization traffic": a proof-graph
	// node, or our own sweep.
	if proof != nil {
		if _, ok := proof.Node(live.SpendingTxid); ok {
			checkpoint.ProvisionalExternalSpend =
				fn.None[ExternalSpendAnchor]()

			return nil
		}
	}
	if checkpoint.State.Sweep.Txid.IsSome() &&
		checkpoint.State.Sweep.Txid.UnsafeFromSome() ==
			live.SpendingTxid {

		checkpoint.ProvisionalExternalSpend =
			fn.None[ExternalSpendAnchor]()

		return nil
	}

	// Cases 1, 2, 3: install or refresh the anchor with the live
	// spender so the restored FSM enters AwaitingExternalSpendFinality
	// rather than blindly resuming planner-driven materialization.
	checkpoint.ProvisionalExternalSpend = fn.Some(ExternalSpendAnchor{
		SpendingTxid:   live.SpendingTxid,
		SpendingHeight: live.SpendingHeight,
	})

	return nil
}

// pruneReorgedSubtree drops a txid and every transitive descendant from
// both ConfirmedTxids and InFlightTxids, and clears any deferred
// checkpoints referencing the subtree.
func pruneReorgedSubtree(proof *recovery.Proof, state *unrollplan.State,
	root chainhash.Hash) {

	subtree := collectReorgedSubtree(
		&Environment{Proof: proof}, root,
	)
	state.ConfirmedTxids = removeHashes(state.ConfirmedTxids, subtree)
	state.InFlightTxids = removeHashes(state.InFlightTxids, subtree)
}

// downgradeSweepOnTargetLoss resets the target-derived planner state
// when the target tx is no longer on the canonical chain. Mirrors the
// "target reorg" branch of applyReorgedEvent so restart reconciliation
// and live rollback converge on identical post-conditions.
func downgradeSweepOnTargetLoss(state *unrollplan.State) {
	state.TargetConfirmHeight = fn.None[int32]()

	if state.Sweep.Status == unrollplan.SweepStatusPending {
		return
	}

	state.Sweep.Status = unrollplan.SweepStatusPending
	state.Sweep.Txid = fn.None[chainhash.Hash]()
	state.Sweep.ConfirmHeight = fn.None[int32]()
}

package unroll

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/lightninglabs/wavelength/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// feeRejectedSweepReason recognizes the old terminal error format only when
// it names this exact sweep. Proof failures and unrelated policy errors cannot
// authorize migration into a live sweep obligation.
func feeRejectedSweepReason(txid chainhash.Hash, reason string) bool {
	prefix := "sweep tx " + txid.String() + " failed:"

	return strings.HasPrefix(reason, prefix) &&
		txconfirm.ClassifyBroadcastFailure(errors.New(reason)) ==
			txconfirm.BroadcastFailureFee
}

// feeRejectedRecord selects terminal rows eligible for checkpoint validation.
// This predicate alone never authorizes spawning or changing a durable row.
func feeRejectedRecord(record RegistryRecord) bool {
	return record.Phase == PhaseFailed && !record.ConflictedFailure &&
		record.SweepTxid != nil &&
		feeRejectedSweepReason(*record.SweepTxid, record.FailReason)
}

// reviveRejectedSweep restores legacy fee failures as the same obligation.
// The next normal Stage persists this state before cleanup or broadcast. A
// crash before that Stage repeats the deterministic conversion from version 1.
func reviveRejectedSweep(checkpoint *actorCheckpoint) bool {
	if checkpoint == nil || checkpoint.SweepTx == nil ||
		checkpoint.Conflicted ||
		!feeRejectedSweepReason(
			checkpoint.SweepTx.TxHash(), checkpoint.Fail,
		) {
		return false
	}
	txid := checkpoint.SweepTx.TxHash()
	checkpoint.Fail = ""
	checkpoint.SweepAttempts = 0
	checkpoint.State.Sweep.Status = unrollplan.SweepStatusPending
	checkpoint.State.Sweep.Txid = fn.None[chainhash.Hash]()
	checkpoint.RejectedSweep = fn.Some(txid)
	checkpoint.RepriceAfter = checkpoint.Height
	checkpoint.RetrySame = false

	return true
}

// prepareFeeReadmission validates the durable candidate and policy before
// making a legacy terminal row discoverable by normal restart recovery. The
// row is persisted before spawning, so a crash cannot lose the obligation.
func (r *registryBehavior) prepareFeeReadmission(ctx context.Context,
	record *RegistryRecord, req *EnsureUnrollRequest) error {

	if record.ActorID != actorIDForTarget(req.Outpoint) ||
		exitPolicyKind(record.ExitPolicyKind) != exitPolicyKind(
			req.ExitPolicyKind,
		) ||
		record.ExitPolicyRef != req.ExitPolicyRef {
		return fmt.Errorf("rejected sweep obligation identity mismatch")
	}
	if err := r.validateRestorableRecords(
		[]RegistryRecord{*record},
	); err != nil {
		return err
	}
	stored, err := r.cfg.DeliveryStore.LoadCheckpoint(ctx, record.ActorID)
	if err != nil {
		return err
	}
	if stored == nil {
		return fmt.Errorf("rejected sweep checkpoint missing")
	}
	checkpoint, err := decodeCheckpoint(stored.StateData)
	if err != nil {
		return err
	}
	candidate := checkpoint.SweepTx
	if candidate == nil || len(candidate.TxIn) != 1 ||
		len(candidate.TxOut) != 1 ||
		candidate.TxIn[0].PreviousOutPoint != req.Outpoint ||
		candidate.TxHash() != *record.SweepTxid ||
		exitPolicyKind(checkpoint.ExitPolicyKind) != exitPolicyKind(
			record.ExitPolicyKind,
		) ||
		checkpoint.ExitPolicyRef != record.ExitPolicyRef ||
		!reviveRejectedSweep(checkpoint) {
		return fmt.Errorf("rejected sweep checkpoint does not match " +
			"terminal record")
	}
	next := cloneRegistryRecord(*record)
	next.Phase = PhaseSweepBroadcast
	next.FailReason = ""
	next.RecoverableFailure = false
	if err := r.cfg.Store.UpsertRecord(ctx, next); err != nil {
		return fmt.Errorf("persist rejected sweep readmission: %w", err)
	}
	*record = next

	return nil
}

// feeReadmissionReady fences readmission behind the old mailbox owner and its
// asynchronous terminal write. The caller can retry after either completes.
func (r *registryBehavior) feeReadmissionReady(target wire.OutPoint) error {
	if drained, ok := r.feeFailureDrains[target]; ok {
		select {
		case <-drained:
			delete(r.feeFailureDrains, target)

		default:
			return fmt.Errorf("prior sweep owner draining; retry " +
				"admission")
		}
	}
	if record, ok := r.pending[target]; ok && feeRejectedRecord(record) {
		return fmt.Errorf("terminal sweep persistence pending; retry " +
			"admission")
	}

	return nil
}

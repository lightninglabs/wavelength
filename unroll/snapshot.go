//nolint:ll
package unroll

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// unrollSnapshot is the in-memory projection of the visible unroll_jobs row.
type unrollSnapshot struct {
	Height              int32
	Started             bool
	Trigger             StartTrigger
	State               unrollplan.State
	SweepTx             *wire.MsgTx
	Fail                string
	SweepAttempts       int
	DeferredCheckpoints []DeferredCheckpoint
}

func jobRecordFromSnapshot(target wire.OutPoint,
	snapshot *unrollSnapshot) (*db.UnrollJobRecord, error) {

	if snapshot == nil {
		return nil, fmt.Errorf("unroll snapshot cannot be nil")
	}

	plannerState, err := unrollplan.EncodeState(&snapshot.State)
	if err != nil {
		return nil, fmt.Errorf("encode planner state: %w", err)
	}

	deferred, err := encodeDeferredCheckpoints(snapshot.DeferredCheckpoints)
	if err != nil {
		return nil, fmt.Errorf("encode deferred checkpoints: %w", err)
	}

	sweepTx, err := serializeTx(snapshot.SweepTx)
	if err != nil {
		return nil, fmt.Errorf("serialize sweep tx: %w", err)
	}

	var sweepTxid []byte
	if txid := effectiveSweepTxid(
		snapshot.State, snapshot.SweepTx,
	); txid != nil {

		sweepTxid = append([]byte(nil), txid[:]...)
	}

	record := &db.UnrollJobRecord{
		TargetOutpoint: target,
		State:          string(phaseFromSnapshot(snapshot)),
		Trigger:        triggerToString(snapshot.Trigger),
		BestHeight:     snapshot.Height,
		TargetConfirmHeight: optionInt32Ptr(
			snapshot.State.TargetConfirmHeight,
		),
		PlannerState:        plannerState,
		DeferredCheckpoints: deferred,
		SweepTx:             sweepTx,
		SweepTxid:           sweepTxid,
		SweepAttempts:       int32(snapshot.SweepAttempts),
		FailReason:          snapshot.Fail,
	}

	if snapshot.State.Sweep.Status == unrollplan.SweepStatusConfirmed {
		record.SweepConfirmHeight = optionInt32Ptr(
			snapshot.State.Sweep.ConfirmHeight,
		)
	}

	return record, nil
}

func snapshotFromJobRecord(record *db.UnrollJobRecord) (*unrollSnapshot,
	error) {

	if record == nil {
		return nil, nil
	}

	plannerState, err := unrollplan.DecodeState(record.PlannerState)
	if err != nil {
		return nil, fmt.Errorf("decode planner state: %w", err)
	}

	deferred, err := decodeDeferredCheckpoints(record.DeferredCheckpoints)
	if err != nil {
		return nil, fmt.Errorf("decode deferred checkpoints: %w", err)
	}

	sweepTx, err := deserializeTx(record.SweepTx)
	if err != nil {
		return nil, fmt.Errorf("decode sweep tx: %w", err)
	}

	trigger, err := triggerFromString(record.Trigger)
	if err != nil {
		return nil, err
	}

	return &unrollSnapshot{
		Height:              record.BestHeight,
		Started:             record.State != string(PhasePending),
		Trigger:             trigger,
		State:               *plannerState,
		SweepTx:             sweepTx,
		Fail:                record.FailReason,
		SweepAttempts:       int(record.SweepAttempts),
		DeferredCheckpoints: deferred,
	}, nil
}

func serializeTx(tx *wire.MsgTx) ([]byte, error) {
	if tx == nil {
		return nil, nil
	}

	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func deserializeTx(raw []byte) (*wire.MsgTx, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	tx := wire.NewMsgTx(0)
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, err
	}

	return tx, nil
}

func optionInt32Ptr(value fn.Option[int32]) *int32 {
	if value.IsNone() {
		return nil
	}

	plain := value.UnsafeFromSome()

	return &plain
}

func phaseFromSnapshot(snapshot *unrollSnapshot) Phase {
	if snapshot == nil || !snapshot.Started {
		return PhasePending
	}

	job := &JobState{
		Height:       snapshot.Height,
		Trigger:      snapshot.Trigger,
		PlannerState: copyPlannerState(snapshot.State),
		DeferredCheckpoints: copyDeferredCheckpoints(
			snapshot.DeferredCheckpoints,
		),
		FailReason:    snapshot.Fail,
		SweepAttempts: snapshot.SweepAttempts,
	}

	return phaseFromPlannerState(job)
}

func triggerToString(trigger StartTrigger) string {
	switch trigger {
	case TriggerCriticalExpiry:
		return "critical_expiry"

	case TriggerRestart:
		return "restart"

	case TriggerFraudSpend:
		return "fraud_spend"

	default:
		return "manual"
	}
}

func triggerFromString(trigger string) (StartTrigger, error) {
	switch trigger {
	case "manual", "":
		return TriggerManual, nil

	case "critical_expiry":
		return TriggerCriticalExpiry, nil

	case "restart":
		return TriggerRestart, nil

	case "fraud_spend":
		return TriggerFraudSpend, nil

	default:
		return TriggerManual, fmt.Errorf("unknown unroll trigger %q",
			trigger)
	}
}

func txidBytesToHash(raw []byte) (*chainhash.Hash, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	hash, err := chainhash.NewHash(raw)
	if err != nil {
		return nil, err
	}

	return hash, nil
}

package unroll

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/db"
)

// DBRegistryStore adapts the legacy unilateral-exit job store into the new
// unroll registry control-plane store.
type DBRegistryStore struct {
	// UEStore is the underlying unilateral-exit persistence store.
	UEStore *db.UnilateralExitPersistenceStore
}

// UpsertRecord stores one registry record in the unilateral-exit job table.
func (s *DBRegistryStore) UpsertRecord(ctx context.Context,
	record RegistryRecord) error {

	if s == nil || s.UEStore == nil {
		return fmt.Errorf("unilateral-exit store must be provided")
	}

	return s.UEStore.UpsertJob(ctx, db.UnilateralExitJobRecord{
		TargetOutpoint: record.TargetOutpoint,
		ActorID:        record.ActorID,
		Status:         statusForPhase(record.Phase),
		Trigger:        triggerToDB(record.Trigger),
		LastError:      record.FailReason,
		SweepTxid:      sweepTxidBytes(record.SweepTxid),
	})
}

// GetRecord returns one registry record when present.
func (s *DBRegistryStore) GetRecord(ctx context.Context,
	target wire.OutPoint) (*RegistryRecord, error) {

	if s == nil || s.UEStore == nil {
		return nil, fmt.Errorf("unilateral-exit store must be provided")
	}

	job, err := s.UEStore.GetJob(ctx, target)
	if errors.Is(err, db.ErrUnilateralExitJobNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	record := recordFromDB(*job)

	return &record, nil
}

// ListNonTerminalRecords returns all non-terminal registry records.
func (s *DBRegistryStore) ListNonTerminalRecords(
	ctx context.Context) ([]RegistryRecord, error) {

	if s == nil || s.UEStore == nil {
		return nil, fmt.Errorf("unilateral-exit store must be provided")
	}

	jobs, err := s.UEStore.ListNonTerminalJobs(ctx)
	if err != nil {
		return nil, err
	}

	records := make([]RegistryRecord, 0, len(jobs))
	for i := range jobs {
		records = append(records, recordFromDB(jobs[i]))
	}

	return records, nil
}

// MarkTerminal marks one target terminal in the unilateral-exit job table.
func (s *DBRegistryStore) MarkTerminal(ctx context.Context,
	target wire.OutPoint, phase Phase, failReason string,
	sweepTxid *chainhash.Hash) error {

	if s == nil || s.UEStore == nil {
		return fmt.Errorf("unilateral-exit store must be provided")
	}

	status := statusForPhase(phase)
	if !status.IsTerminal() {
		return fmt.Errorf("phase %s is not terminal", phase)
	}

	return s.UEStore.MarkJobTerminal(
		ctx, target, status, failReason, sweepTxidBytes(sweepTxid),
	)
}

// recordFromDB converts one unilateral-exit job row into a registry record.
func recordFromDB(job db.UnilateralExitJobRecord) RegistryRecord {
	return RegistryRecord{
		TargetOutpoint: job.TargetOutpoint,
		ActorID:        job.ActorID,
		Trigger:        triggerFromDB(job.Trigger),
		Phase:          phaseFromDB(job.Status),
		FailReason:     job.LastError,
		SweepTxid:      sweepTxidFromBytes(job.SweepTxid),
	}
}

// statusForPhase maps a registry phase into the legacy job status enum.
func statusForPhase(phase Phase) db.UnilateralExitJobStatus {
	switch phase {
	case PhasePending:
		return db.UnilateralExitJobStatusPending

	case PhaseCSVPending:
		return db.UnilateralExitJobStatusCSVPending

	case PhaseSweepBroadcast, PhaseSweepConfirmation:
		return db.UnilateralExitJobStatusSweeping

	case PhaseCompleted:
		return db.UnilateralExitJobStatusCompleted

	case PhaseFailed:
		return db.UnilateralExitJobStatusFailed

	default:
		return db.UnilateralExitJobStatusMaterializing
	}
}

// phaseFromDB maps a unilateral-exit job status into the new registry phase.
func phaseFromDB(status db.UnilateralExitJobStatus) Phase {
	switch status {
	case db.UnilateralExitJobStatusPending:
		return PhasePending

	case db.UnilateralExitJobStatusCSVPending:
		return PhaseCSVPending

	case db.UnilateralExitJobStatusSweeping:
		return PhaseSweepConfirmation

	case db.UnilateralExitJobStatusCompleted:
		return PhaseCompleted

	case db.UnilateralExitJobStatusFailed:
		return PhaseFailed

	default:
		return PhaseMaterializing
	}
}

// triggerToDB maps a new unroll trigger into the legacy db enum.
func triggerToDB(trigger StartTrigger) db.UnilateralExitJobTrigger {
	switch trigger {
	case TriggerCriticalExpiry:
		return db.UnilateralExitJobTriggerCriticalExpiry

	case TriggerRestart:
		return db.UnilateralExitJobTriggerRestart

	default:
		return db.UnilateralExitJobTriggerManual
	}
}

// triggerFromDB maps a legacy db trigger into the new unroll trigger.
func triggerFromDB(trigger db.UnilateralExitJobTrigger) StartTrigger {
	switch trigger {
	case db.UnilateralExitJobTriggerCriticalExpiry:
		return TriggerCriticalExpiry

	case db.UnilateralExitJobTriggerRestart:
		return TriggerRestart

	default:
		return TriggerManual
	}
}

// sweepTxidBytes converts an optional txid into the stored byte format.
func sweepTxidBytes(txid *chainhash.Hash) []byte {
	if txid == nil {
		return nil
	}

	return append([]byte(nil), txid[:]...)
}

// sweepTxidFromBytes converts stored bytes into an optional txid.
func sweepTxidFromBytes(raw []byte) *chainhash.Hash {
	if len(raw) != chainhash.HashSize {
		return nil
	}

	var hash chainhash.Hash
	copy(hash[:], raw)

	return &hash
}

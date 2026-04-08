package unroll

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/db"
)

// db_store.go adapts the db package's UnilateralExitJob table into the
// [RegistryStore] interface the in-memory registry consumes.
//
// Two enum translations happen here and are covered by round-trip tests:
//
//   - Phase ↔ DB status. The two sweep-related phases
//     (PhaseSweepBroadcast, PhaseSweepConfirmation) deliberately map to
//     two distinct DB statuses (SweepBroadcasting, Sweeping) so the
//     operator-visible lifecycle does not collapse "sweep built but
//     not yet submitted" into "sweep broadcast awaiting confirmation".
//     The Go enum appends SweepBroadcasting at the end of iota so
//     existing rows written against the older numeric layout keep
//     decoding to the same Phase they were originally written as.
//
//   - Trigger ↔ DB trigger. TriggerFraudSpend round-trips through
//     its own DB constant; earlier revisions silently downgraded it to
//     TriggerManual, losing the "target was externally spent" signal
//     from the control plane.
//
// Round-trip tests in db_store_test.go pin these mappings.

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
// PhaseSweepBroadcast and PhaseSweepConfirmation use distinct DB statuses
// so operators can distinguish "sweep built, not yet submitted" from
// "sweep broadcast, awaiting confirmation" on restart — collapsing the
// two would silently erase half the sweep lifecycle.
func statusForPhase(phase Phase) db.UnilateralExitJobStatus {
	switch phase {
	case PhasePending:
		return db.UnilateralExitJobStatusPending

	case PhaseCSVPending:
		return db.UnilateralExitJobStatusCSVPending

	case PhaseSweepBroadcast:
		return db.UnilateralExitJobStatusSweepBroadcasting

	case PhaseSweepConfirmation:
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

	case db.UnilateralExitJobStatusSweepBroadcasting:
		return PhaseSweepBroadcast

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

	case TriggerFraudSpend:
		return db.UnilateralExitJobTriggerFraudSpend

	default:
		return db.UnilateralExitJobTriggerManual
	}
}

// triggerFromDB maps a legacy db trigger into the new unroll trigger.
// FraudSpend rows previously round-tripped as TriggerManual, which hid
// the external-spend escalation class from the control plane entirely;
// round-trip it through a dedicated constant now that one exists.
func triggerFromDB(trigger db.UnilateralExitJobTrigger) StartTrigger {
	switch trigger {
	case db.UnilateralExitJobTriggerCriticalExpiry:
		return TriggerCriticalExpiry

	case db.UnilateralExitJobTriggerRestart:
		return TriggerRestart

	case db.UnilateralExitJobTriggerFraudSpend:
		return TriggerFraudSpend

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

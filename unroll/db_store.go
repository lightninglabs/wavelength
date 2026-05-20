package unroll

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/unrollplan"
)

// DBRegistryStore adapts the SQL unroll_jobs table into the in-memory
// registry control-plane store.
type DBRegistryStore struct {
	// JobStore is the underlying unroll job persistence store.
	JobStore *db.UnrollJobPersistenceStore
}

// UpsertRecord stores one registry record in the unroll job table.
func (s *DBRegistryStore) UpsertRecord(ctx context.Context,
	record RegistryRecord) error {

	if s == nil || s.JobStore == nil {
		return fmt.Errorf("unroll job store must be provided")
	}

	job := db.UnrollJobRecord{
		TargetOutpoint: record.TargetOutpoint,
		State:          string(record.Phase),
		Trigger:        triggerToString(record.Trigger),
		ExitPolicyKind: exitPolicyKind(record.ExitPolicyKind),
		ExitPolicyRef:  record.ExitPolicyRef,
		FailReason:     record.FailReason,
		SweepTxid:      sweepTxidBytes(record.SweepTxid),
	}

	existing, err := s.JobStore.GetJob(ctx, record.TargetOutpoint)
	switch {
	case err == nil && existing != nil:
		job.BestHeight = existing.BestHeight
		job.TargetConfirmHeight = existing.TargetConfirmHeight
		job.PlannerState = existing.PlannerState
		job.DeferredCheckpoints = existing.DeferredCheckpoints
		job.SweepTx = existing.SweepTx
		job.ExitPolicyKind, job.ExitPolicyRef = registryExitPolicy(
			record, existing,
		)
		if len(job.SweepTxid) == 0 {
			job.SweepTxid = existing.SweepTxid
		}
		job.SweepConfirmHeight = existing.SweepConfirmHeight
		job.SweepAttempts = existing.SweepAttempts
		job.CreatedAt = existing.CreatedAt
		job.TxProgress = existing.TxProgress
		job.Watches = existing.Watches

	case errors.Is(err, db.ErrUnrollJobNotFound):
		plannerState, encodeErr := unrollplan.EncodeState(
			&unrollplan.State{},
		)
		if encodeErr != nil {
			return fmt.Errorf("encode empty planner state: %w",
				encodeErr)
		}
		job.PlannerState = plannerState

	case err != nil:
		return err
	}

	return s.JobStore.UpsertJob(ctx, job)
}

// GetRecord returns one registry record when present.
func (s *DBRegistryStore) GetRecord(ctx context.Context, target wire.OutPoint) (
	*RegistryRecord, error) {

	if s == nil || s.JobStore == nil {
		return nil, fmt.Errorf("unroll job store must be provided")
	}

	job, err := s.JobStore.GetJob(ctx, target)
	if errors.Is(err, db.ErrUnrollJobNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	record, err := recordFromDB(*job)
	if err != nil {
		return nil, err
	}

	return &record, nil
}

// ListNonTerminalRecords returns all non-terminal registry records.
func (s *DBRegistryStore) ListNonTerminalRecords(ctx context.Context) (
	[]RegistryRecord, error) {

	if s == nil || s.JobStore == nil {
		return nil, fmt.Errorf("unroll job store must be provided")
	}

	jobs, err := s.JobStore.ListNonTerminalJobs(ctx)
	if err != nil {
		return nil, err
	}

	records := make([]RegistryRecord, 0, len(jobs))
	for i := range jobs {
		record, err := recordFromDB(jobs[i])
		if err != nil {
			return nil, err
		}

		records = append(records, record)
	}

	return records, nil
}

// MarkTerminal marks one target terminal in the unroll job table.
func (s *DBRegistryStore) MarkTerminal(ctx context.Context,
	target wire.OutPoint, phase Phase, failReason string,
	sweepTxid *chainhash.Hash) error {

	if s == nil || s.JobStore == nil {
		return fmt.Errorf("unroll job store must be provided")
	}

	if phase != PhaseCompleted && phase != PhaseFailed {
		return fmt.Errorf("phase %s is not terminal", phase)
	}

	return s.JobStore.MarkJobTerminal(
		ctx, target, string(phase), failReason,
		sweepTxidBytes(sweepTxid),
	)
}

// recordFromDB converts one unroll job row into a registry record.
func recordFromDB(job db.UnrollJobRecord) (RegistryRecord, error) {
	trigger, err := triggerFromString(job.Trigger)
	if err != nil {
		return RegistryRecord{}, err
	}

	return RegistryRecord{
		TargetOutpoint: job.TargetOutpoint,
		ActorID:        actorIDForTarget(job.TargetOutpoint),
		Trigger:        trigger,
		ExitPolicyKind: exitPolicyKind(job.ExitPolicyKind),
		ExitPolicyRef:  job.ExitPolicyRef,
		Phase:          Phase(job.State),
		FailReason:     job.FailReason,
		SweepTxid:      sweepTxidFromBytes(job.SweepTxid),
	}, nil
}

// registryExitPolicy chooses the policy identity to write when the registry
// refines an existing DB row. Policy kind and ref are treated as one durable
// identity pair: a record with no kind preserves both existing values, while a
// record with any kind replaces both values so stale custom refs cannot attach
// to a new standard policy.
func registryExitPolicy(record RegistryRecord,
	existing *db.UnrollJobRecord) (string, string) {

	if record.ExitPolicyKind == "" && existing != nil {
		return existing.ExitPolicyKind, existing.ExitPolicyRef
	}

	return exitPolicyKind(record.ExitPolicyKind), record.ExitPolicyRef
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

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
)

var (
	// ErrUnilateralExitJobNotFound indicates the job row does not exist.
	ErrUnilateralExitJobNotFound = errors.New(
		"unilateral exit job not found",
	)
)

// UnilateralExitJobStatus is the manager-facing status of one target job.
type UnilateralExitJobStatus int32

const (
	// UnilateralExitJobStatusPending means the job row exists but work has not
	// started materially.
	UnilateralExitJobStatusPending UnilateralExitJobStatus = iota

	// UnilateralExitJobStatusMaterializing means proof nodes are still being
	// materialized.
	UnilateralExitJobStatusMaterializing

	// UnilateralExitJobStatusCSVPending means the target is confirmed and the
	// job is waiting for CSV maturity.
	UnilateralExitJobStatusCSVPending

	// UnilateralExitJobStatusSweeping means the final sweep was broadcast.
	UnilateralExitJobStatusSweeping

	// UnilateralExitJobStatusCompleted means the job completed successfully.
	UnilateralExitJobStatusCompleted

	// UnilateralExitJobStatusFailed means the job failed terminally.
	UnilateralExitJobStatusFailed
)

// IsTerminal reports whether the control-plane job status is terminal.
func (s UnilateralExitJobStatus) IsTerminal() bool {
	return s == UnilateralExitJobStatusCompleted ||
		s == UnilateralExitJobStatusFailed
}

// UnilateralExitJobTrigger records what started an exit job.
type UnilateralExitJobTrigger int32

const (
	// UnilateralExitJobTriggerManual is an operator-triggered start.
	UnilateralExitJobTriggerManual UnilateralExitJobTrigger = iota

	// UnilateralExitJobTriggerCriticalExpiry is a VTXO expiry handoff.
	UnilateralExitJobTriggerCriticalExpiry

	// UnilateralExitJobTriggerRestart marks a restored in-flight job.
	UnilateralExitJobTriggerRestart

	// UnilateralExitJobTriggerFraudSpend is reserved for active-job spend
	// escalation.
	UnilateralExitJobTriggerFraudSpend
)

// UnilateralExitJobRecord is one manager-faced job control-plane row.
type UnilateralExitJobRecord struct {
	TargetOutpoint wire.OutPoint
	ActorID        string
	Status         UnilateralExitJobStatus
	Trigger        UnilateralExitJobTrigger
	LastError      string
	SweepTxid      []byte
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// UnilateralExitStore groups SQL methods needed by the unilateral-exit
// job store. Proof persistence has been removed — proofs are derived on
// demand from the VTXO descriptor and OOR artifact data.
type UnilateralExitStore interface {
	UpsertUnilateralExitJob(ctx context.Context,
		arg sqlc.UpsertUnilateralExitJobParams) error

	GetUnilateralExitJob(ctx context.Context,
		arg sqlc.GetUnilateralExitJobParams) (
		sqlc.UnilateralExitJob, error,
	)

	ListNonTerminalUnilateralExitJobs(ctx context.Context) (
		[]sqlc.UnilateralExitJob, error,
	)

	MarkUnilateralExitJobTerminal(ctx context.Context,
		arg sqlc.MarkUnilateralExitJobTerminalParams) error
}

// BatchedUnilateralExitStore combines the query surface with transactions.
type BatchedUnilateralExitStore interface {
	UnilateralExitStore
	BatchedTx[UnilateralExitStore]
}

// UnilateralExitPersistenceStore persists immutable proofs and manager-facing
// job rows for the unilateral-exit subsystem.
type UnilateralExitPersistenceStore struct {
	db    BatchedUnilateralExitStore
	clock clock.Clock
}

// NewUnilateralExitPersistenceStore creates a unilateral-exit store.
func NewUnilateralExitPersistenceStore(
	db BatchedUnilateralExitStore, clk clock.Clock,
) *UnilateralExitPersistenceStore {

	return &UnilateralExitPersistenceStore{
		db:    db,
		clock: clk,
	}
}

// UpsertJob persists or updates one manager-facing job record.
// NOTE: The proof-related methods (UpsertProof, GetProof, MarkProofFailed)
// have been removed. Proofs are now derived on demand from the authoritative
// VTXO descriptor and OOR artifact data via the ProofAssembler.
func (s *UnilateralExitPersistenceStore) UpsertJob(ctx context.Context,
	job UnilateralExitJobRecord) error {

	nowUnix := s.clock.Now().Unix()
	createdAt := job.CreatedAt.Unix()
	if job.CreatedAt.IsZero() {
		createdAt = nowUnix
	}

	return s.db.ExecTx(ctx, WriteTxOption(), func(q UnilateralExitStore) error {
		return q.UpsertUnilateralExitJob(ctx,
			sqlc.UpsertUnilateralExitJobParams{
				TargetOutpointHash:  job.TargetOutpoint.Hash[:],
				TargetOutpointIndex: int32(job.TargetOutpoint.Index),
				ActorID:             job.ActorID,
				Status:              int32(job.Status),
				Trigger:             int32(job.Trigger),
				LastError: sql.NullString{
					String: job.LastError,
					Valid:  job.LastError != "",
				},
				CreatedAt: createdAt,
				UpdatedAt: nowUnix,
			},
		)
	})
}

// GetJob loads one manager-facing job control-plane row.
func (s *UnilateralExitPersistenceStore) GetJob(ctx context.Context,
	target wire.OutPoint) (*UnilateralExitJobRecord, error) {

	var job *UnilateralExitJobRecord

	err := s.db.ExecTx(ctx, ReadTxOption(), func(q UnilateralExitStore) error {
		row, err := q.GetUnilateralExitJob(ctx,
			sqlc.GetUnilateralExitJobParams{
				TargetOutpointHash:  target.Hash[:],
				TargetOutpointIndex: int32(target.Index),
			},
		)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrUnilateralExitJobNotFound
			}

			return err
		}

		record, err := jobRecordFromRow(row)
		if err != nil {
			return err
		}

		job = &record

		return nil
	})
	if err != nil {
		return nil, err
	}

	return job, nil
}

// ListNonTerminalJobs loads all non-terminal manager-facing job rows.
func (s *UnilateralExitPersistenceStore) ListNonTerminalJobs(
	ctx context.Context) ([]UnilateralExitJobRecord, error) {

	result := make([]UnilateralExitJobRecord, 0)

	err := s.db.ExecTx(ctx, ReadTxOption(), func(q UnilateralExitStore) error {
		rows, err := q.ListNonTerminalUnilateralExitJobs(ctx)
		if err != nil {
			return err
		}

		result = make([]UnilateralExitJobRecord, 0, len(rows))
		for i := range rows {
			record, convErr := jobRecordFromRow(rows[i])
			if convErr != nil {
				return convErr
			}

			result = append(result, record)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// MarkJobTerminal updates one job row to a terminal status.
func (s *UnilateralExitPersistenceStore) MarkJobTerminal(ctx context.Context,
	target wire.OutPoint, status UnilateralExitJobStatus,
	reason string, sweepTxid []byte) error {

	if !status.IsTerminal() {
		return fmt.Errorf("status %d is not terminal", status)
	}

	return s.db.ExecTx(ctx, WriteTxOption(), func(q UnilateralExitStore) error {
		return q.MarkUnilateralExitJobTerminal(ctx,
			sqlc.MarkUnilateralExitJobTerminalParams{
				TargetOutpointHash:  target.Hash[:],
				TargetOutpointIndex: int32(target.Index),
				Status:              int32(status),
				LastError: sql.NullString{
					String: reason,
					Valid:  reason != "",
				},
				UpdatedAt: s.clock.Now().Unix(),
				SweepTxid: sweepTxid,
			},
		)
	})
}

func jobRecordFromRow(row sqlc.UnilateralExitJob) (
	UnilateralExitJobRecord, error) {

	if len(row.TargetOutpointHash) != 32 {
		return UnilateralExitJobRecord{}, fmt.Errorf("unexpected "+
			"target outpoint hash length %d",
			len(row.TargetOutpointHash))
	}

	var hash [32]byte
	copy(hash[:], row.TargetOutpointHash)

	record := UnilateralExitJobRecord{
		TargetOutpoint: wire.OutPoint{
			Hash:  hash,
			Index: uint32(row.TargetOutpointIndex),
		},
		ActorID:   row.ActorID,
		Status:    UnilateralExitJobStatus(row.Status),
		Trigger:   UnilateralExitJobTrigger(row.Trigger),
		SweepTxid: row.SweepTxid,
		CreatedAt: time.Unix(row.CreatedAt, 0),
		UpdatedAt: time.Unix(row.UpdatedAt, 0),
	}

	if row.LastError.Valid {
		record.LastError = row.LastError.String
	}

	return record, nil
}

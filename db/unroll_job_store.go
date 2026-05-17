//nolint:ll
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
	// ErrUnrollJobNotFound indicates the job row does not exist.
	ErrUnrollJobNotFound = errors.New("unroll job not found")
)

// UnrollJobRecord is the restart-safe SQL state for one VTXO unroll target.
type UnrollJobRecord struct {
	TargetOutpoint      wire.OutPoint
	State               string
	Trigger             string
	BestHeight          int32
	TargetConfirmHeight *int32
	PlannerState        []byte
	DeferredCheckpoints []byte
	SweepTx             []byte
	SweepTxid           []byte
	SweepConfirmHeight  *int32
	SweepAttempts       int32
	FailReason          string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// IsTerminal reports whether the job is in a terminal state.
func (r UnrollJobRecord) IsTerminal() bool {
	return r.State == "completed" || r.State == "failed"
}

// UnrollJobStore groups SQL methods needed by the VTXO unroll job store.
//
//nolint:interfacebloat
type UnrollJobStore interface {
	UpsertUnrollJob(ctx context.Context,
		arg sqlc.UpsertUnrollJobParams) error

	GetUnrollJob(ctx context.Context,
		arg sqlc.GetUnrollJobParams) (sqlc.UnrollJob, error)

	ListNonTerminalUnrollJobs(ctx context.Context) ([]sqlc.UnrollJob, error)

	MarkUnrollJobTerminal(ctx context.Context,
		arg sqlc.MarkUnrollJobTerminalParams) error
}

// BatchedUnrollJobStore combines the query surface with transactions.
type BatchedUnrollJobStore interface {
	UnrollJobStore
	BatchedTx[UnrollJobStore]
}

// UnrollJobPersistenceStore persists the visible unroll FSM row.
type UnrollJobPersistenceStore struct {
	db    BatchedUnrollJobStore
	clock clock.Clock
}

// NewUnrollJobPersistenceStore creates an unroll job store.
func NewUnrollJobPersistenceStore(db BatchedUnrollJobStore,
	clk clock.Clock) *UnrollJobPersistenceStore {

	return &UnrollJobPersistenceStore{
		db:    db,
		clock: clk,
	}
}

// UpsertJob persists or updates one unroll job row.
func (s *UnrollJobPersistenceStore) UpsertJob(ctx context.Context,
	job UnrollJobRecord) error {

	if len(job.PlannerState) == 0 {
		return fmt.Errorf("planner state is required")
	}

	nowUnix := s.clock.Now().Unix()
	createdAt := job.CreatedAt.Unix()
	if job.CreatedAt.IsZero() {
		createdAt = nowUnix
	}

	target := job.TargetOutpoint
	writeFn := func(q UnrollJobStore) error {
		return q.UpsertUnrollJob(
			ctx,
			sqlc.UpsertUnrollJobParams{
				TargetOutpointHash:  target.Hash[:],
				TargetOutpointIndex: int32(target.Index),
				State:               job.State,
				Trigger:             job.Trigger,
				BestHeight:          job.BestHeight,
				TargetConfirmHeight: nullableInt32(
					job.TargetConfirmHeight,
				),
				PlannerState: append(
					[]byte(nil), job.PlannerState...,
				),
				DeferredCheckpoints: append(
					[]byte(nil), job.DeferredCheckpoints...,
				),
				SweepTx:   append([]byte(nil), job.SweepTx...),
				SweepTxid: append([]byte(nil), job.SweepTxid...),
				SweepConfirmHeight: nullableInt32(
					job.SweepConfirmHeight,
				),
				SweepAttempts: job.SweepAttempts,
				FailReason: sql.NullString{
					String: job.FailReason,
					Valid:  job.FailReason != "",
				},
				CreatedAt: createdAt,
				UpdatedAt: nowUnix,
			},
		)
	}

	return s.db.ExecTx(ctx, WriteTxOption(), writeFn)
}

// GetJob loads one unroll job row.
func (s *UnrollJobPersistenceStore) GetJob(ctx context.Context,
	target wire.OutPoint) (*UnrollJobRecord, error) {

	var job *UnrollJobRecord
	readFn := func(q UnrollJobStore) error {
		row, err := q.GetUnrollJob(ctx, sqlc.GetUnrollJobParams{
			TargetOutpointHash:  target.Hash[:],
			TargetOutpointIndex: int32(target.Index),
		})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrUnrollJobNotFound
			}

			return err
		}

		record, err := unrollJobRecordFromRow(row)
		if err != nil {
			return err
		}

		job = &record

		return nil
	}

	err := s.db.ExecTx(ctx, ReadTxOption(), readFn)
	if err != nil {
		return nil, err
	}

	return job, nil
}

// ListNonTerminalJobs loads every non-terminal unroll job row.
func (s *UnrollJobPersistenceStore) ListNonTerminalJobs(ctx context.Context) (
	[]UnrollJobRecord, error) {

	result := make([]UnrollJobRecord, 0)
	readFn := func(q UnrollJobStore) error {
		rows, err := q.ListNonTerminalUnrollJobs(ctx)
		if err != nil {
			return err
		}

		result = make([]UnrollJobRecord, 0, len(rows))
		for i := range rows {
			record, convErr := unrollJobRecordFromRow(rows[i])
			if convErr != nil {
				return convErr
			}

			result = append(result, record)
		}

		return nil
	}

	err := s.db.ExecTx(ctx, ReadTxOption(), readFn)
	if err != nil {
		return nil, err
	}

	return result, nil
}

// MarkJobTerminal updates one job row to a terminal state.
func (s *UnrollJobPersistenceStore) MarkJobTerminal(ctx context.Context,
	target wire.OutPoint, state string, reason string,
	sweepTxid []byte) error {

	if state != "completed" && state != "failed" {
		return fmt.Errorf("state %q is not terminal", state)
	}

	writeFn := func(q UnrollJobStore) error {
		return q.MarkUnrollJobTerminal(
			ctx,
			sqlc.MarkUnrollJobTerminalParams{
				TargetOutpointHash:  target.Hash[:],
				TargetOutpointIndex: int32(target.Index),
				State:               state,
				FailReason: sql.NullString{
					String: reason,
					Valid:  reason != "",
				},
				UpdatedAt: s.clock.Now().Unix(),
				SweepTxid: append([]byte(nil), sweepTxid...),
			},
		)
	}

	return s.db.ExecTx(ctx, WriteTxOption(), writeFn)
}

func unrollJobRecordFromRow(row sqlc.UnrollJob) (UnrollJobRecord, error) {
	hash, err := hashFromBytes(row.TargetOutpointHash)
	if err != nil {
		return UnrollJobRecord{}, fmt.Errorf("unexpected target "+
			"outpoint hash: %w", err)
	}

	record := UnrollJobRecord{
		TargetOutpoint: wire.OutPoint{
			Hash:  hash,
			Index: uint32(row.TargetOutpointIndex),
		},
		State:               row.State,
		Trigger:             row.Trigger,
		BestHeight:          row.BestHeight,
		TargetConfirmHeight: int32FromNull(row.TargetConfirmHeight),
		PlannerState: append(
			[]byte(nil), row.PlannerState...,
		),
		DeferredCheckpoints: append(
			[]byte(nil), row.DeferredCheckpoints...,
		),
		SweepTx:            append([]byte(nil), row.SweepTx...),
		SweepTxid:          append([]byte(nil), row.SweepTxid...),
		SweepConfirmHeight: int32FromNull(row.SweepConfirmHeight),
		SweepAttempts:      row.SweepAttempts,
		CreatedAt:          time.Unix(row.CreatedAt, 0),
		UpdatedAt:          time.Unix(row.UpdatedAt, 0),
	}
	if row.FailReason.Valid {
		record.FailReason = row.FailReason.String
	}

	return record, nil
}

func nullableInt32(value *int32) sql.NullInt32 {
	if value == nil {
		return sql.NullInt32{}
	}

	return sql.NullInt32{
		Int32: *value,
		Valid: true,
	}
}

func int32FromNull(value sql.NullInt32) *int32 {
	if !value.Valid {
		return nil
	}

	plain := value.Int32

	return &plain
}

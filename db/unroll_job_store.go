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
	TxProgress          []UnrollTxProgressRecord
	Watches             []UnrollWatchRecord
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// UnrollTxProgressRecord is one transaction progress row for an unroll job.
type UnrollTxProgressRecord struct {
	Txid          []byte
	Role          string
	Status        string
	TxBytes       []byte
	ConfirmHeight *int32
	LastError     string
}

// UnrollWatchRecord is one durable chain watch row for an unroll job.
type UnrollWatchRecord struct {
	WatchID            string
	Role               string
	Txid               []byte
	SpendOutpointHash  []byte
	SpendOutpointIndex *int32
	Status             string
	HeightHint         *int32
	ConfirmationHeight *int32
	LastError          string
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

	DeleteUnrollTxProgressForJob(ctx context.Context,
		arg sqlc.DeleteUnrollTxProgressForJobParams) error

	ListUnrollTxProgressForJob(ctx context.Context,
		arg sqlc.ListUnrollTxProgressForJobParams) (
		[]sqlc.UnrollTxProgress, error)

	UpsertUnrollTxProgress(ctx context.Context,
		arg sqlc.UpsertUnrollTxProgressParams) error

	DeleteUnrollWatchesForJob(ctx context.Context,
		arg sqlc.DeleteUnrollWatchesForJobParams) error

	ListUnrollWatchesForJob(ctx context.Context,
		arg sqlc.ListUnrollWatchesForJobParams) (
		[]sqlc.UnrollWatch,
		error,
	)

	UpsertUnrollWatch(ctx context.Context,
		arg sqlc.UpsertUnrollWatchParams) error
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
		err := q.UpsertUnrollJob(
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
		if err != nil {
			return err
		}

		key := jobKeyParams(target)
		if err := q.DeleteUnrollTxProgressForJob(
			ctx, sqlc.DeleteUnrollTxProgressForJobParams(key),
		); err != nil {
			return err
		}

		for i := range job.TxProgress {
			params := txProgressParams(
				target, job.TxProgress[i], createdAt, nowUnix,
			)
			if err := q.UpsertUnrollTxProgress(
				ctx, params,
			); err != nil {
				return err
			}
		}

		if err := q.DeleteUnrollWatchesForJob(
			ctx, sqlc.DeleteUnrollWatchesForJobParams(key),
		); err != nil {
			return err
		}

		for i := range job.Watches {
			params := watchParams(
				target, job.Watches[i], createdAt, nowUnix,
			)
			if err := q.UpsertUnrollWatch(ctx, params); err != nil {
				return err
			}
		}

		return nil
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
		if err := loadUnrollJobChildren(ctx, q, &record); err != nil {
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
			if err := loadUnrollJobChildren(
				ctx, q, &record,
			); err != nil {
				return err
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

type jobKey struct {
	TargetOutpointHash  []byte
	TargetOutpointIndex int32
}

func jobKeyParams(target wire.OutPoint) jobKey {
	return jobKey{
		TargetOutpointHash:  target.Hash[:],
		TargetOutpointIndex: int32(target.Index),
	}
}

func txProgressParams(target wire.OutPoint, progress UnrollTxProgressRecord,
	createdAt, updatedAt int64) sqlc.UpsertUnrollTxProgressParams {

	return sqlc.UpsertUnrollTxProgressParams{
		TargetOutpointHash:  target.Hash[:],
		TargetOutpointIndex: int32(target.Index),
		Txid:                append([]byte(nil), progress.Txid...),
		Role:                progress.Role,
		Status:              progress.Status,
		TxBytes:             append([]byte(nil), progress.TxBytes...),
		ConfirmHeight:       nullableInt32(progress.ConfirmHeight),
		LastError: sql.NullString{
			String: progress.LastError,
			Valid:  progress.LastError != "",
		},
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
}

func watchParams(target wire.OutPoint, watch UnrollWatchRecord, createdAt,
	updatedAt int64) sqlc.UpsertUnrollWatchParams {

	return sqlc.UpsertUnrollWatchParams{
		TargetOutpointHash:  target.Hash[:],
		TargetOutpointIndex: int32(target.Index),
		WatchID:             watch.WatchID,
		Role:                watch.Role,
		Txid:                append([]byte(nil), watch.Txid...),
		SpendOutpointHash: append(
			[]byte(nil), watch.SpendOutpointHash...,
		),
		SpendOutpointIndex: nullableInt32(watch.SpendOutpointIndex),
		Status:             watch.Status,
		HeightHint:         nullableInt32(watch.HeightHint),
		ConfirmationHeight: nullableInt32(watch.ConfirmationHeight),
		LastError: sql.NullString{
			String: watch.LastError,
			Valid:  watch.LastError != "",
		},
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
}

func loadUnrollJobChildren(ctx context.Context, q UnrollJobStore,
	record *UnrollJobRecord) error {

	key := jobKeyParams(record.TargetOutpoint)

	progressRows, err := q.ListUnrollTxProgressForJob(
		ctx, sqlc.ListUnrollTxProgressForJobParams(key),
	)
	if err != nil {
		return err
	}
	record.TxProgress = make([]UnrollTxProgressRecord, 0, len(progressRows))
	for i := range progressRows {
		record.TxProgress = append(
			record.TxProgress,
			txProgressRecordFromRow(progressRows[i]),
		)
	}

	watchRows, err := q.ListUnrollWatchesForJob(
		ctx, sqlc.ListUnrollWatchesForJobParams(key),
	)
	if err != nil {
		return err
	}
	record.Watches = make([]UnrollWatchRecord, 0, len(watchRows))
	for i := range watchRows {
		record.Watches = append(
			record.Watches, watchRecordFromRow(watchRows[i]),
		)
	}

	return nil
}

func txProgressRecordFromRow(row sqlc.UnrollTxProgress) UnrollTxProgressRecord {
	record := UnrollTxProgressRecord{
		Txid:          append([]byte(nil), row.Txid...),
		Role:          row.Role,
		Status:        row.Status,
		TxBytes:       append([]byte(nil), row.TxBytes...),
		ConfirmHeight: int32FromNull(row.ConfirmHeight),
	}
	if row.LastError.Valid {
		record.LastError = row.LastError.String
	}

	return record
}

func watchRecordFromRow(row sqlc.UnrollWatch) UnrollWatchRecord {
	record := UnrollWatchRecord{
		WatchID: row.WatchID,
		Role:    row.Role,
		Txid:    append([]byte(nil), row.Txid...),
		SpendOutpointHash: append(
			[]byte(nil), row.SpendOutpointHash...,
		),
		SpendOutpointIndex: int32FromNull(row.SpendOutpointIndex),
		Status:             row.Status,
		HeightHint:         int32FromNull(row.HeightHint),
		ConfirmationHeight: int32FromNull(row.ConfirmationHeight),
	}
	if row.LastError.Valid {
		record.LastError = row.LastError.String
	}

	return record
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

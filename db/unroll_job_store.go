//nolint:ll
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/google/uuid"
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

// UnrollEffectRecord is one retryable side-effect row for an unroll job.
type UnrollEffectRecord struct {
	ID             string
	TargetOutpoint wire.OutPoint
	EffectType     string
	Txid           []byte
	Status         string
	IdempotencyKey string
	ClaimToken     sql.NullString
	Attempts       int32
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

	InsertUnrollEffect(ctx context.Context,
		arg sqlc.InsertUnrollEffectParams) error

	ListDueUnrollEffectIDs(ctx context.Context,
		arg sqlc.ListDueUnrollEffectIDsParams) ([]string, error)

	ClaimUnrollEffect(ctx context.Context,
		arg sqlc.ClaimUnrollEffectParams) (sqlc.UnrollEffect, error)

	MarkUnrollEffectDone(ctx context.Context,
		arg sqlc.MarkUnrollEffectDoneParams) error

	ReleaseUnrollEffectForRetry(ctx context.Context,
		arg sqlc.ReleaseUnrollEffectForRetryParams) error

	ReleaseExpiredUnrollEffectClaims(ctx context.Context,
		arg sqlc.ReleaseExpiredUnrollEffectClaimsParams) error
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

		return insertUnrollEffectsForJob(ctx, q, job, nowUnix)
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

// ClaimDueEffects claims up to limit due unroll effect rows.
func (s *UnrollJobPersistenceStore) ClaimDueEffects(ctx context.Context,
	owner string, limit int, lease time.Duration) ([]UnrollEffectRecord,
	error) {

	if limit <= 0 {
		return nil, nil
	}

	now := s.clock.Now()
	claimed := make([]UnrollEffectRecord, 0, limit)

	err := s.db.ExecTx(ctx, WriteTxOption(), func(q UnrollJobStore) error {
		ids, err := q.ListDueUnrollEffectIDs(
			ctx, sqlc.ListDueUnrollEffectIDsParams{
				NextAttemptAt: now.Unix(),
				Limit:         int32(limit),
			},
		)
		if err != nil {
			return err
		}

		for _, id := range ids {
			token := uuid.NewString()
			row, err := q.ClaimUnrollEffect(
				ctx, sqlc.ClaimUnrollEffectParams{
					ID: id,
					ClaimOwner: sql.NullString{
						String: owner,
						Valid:  true,
					},
					ClaimToken: sql.NullString{
						String: token,
						Valid:  true,
					},
					ClaimUntil: sql.NullInt64{
						Int64: now.Add(lease).Unix(),
						Valid: true,
					},
					UpdatedAt: now.Unix(),
				},
			)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}

			effect, err := unrollEffectRecordFromRow(row)
			if err != nil {
				return err
			}
			claimed = append(claimed, effect)
		}

		return nil
	})

	return claimed, err
}

// MarkEffectDone marks an unroll effect done. An empty claim token allows the
// foreground actor path to close a pending effect it just executed; workers
// pass the claim token they acquired.
func (s *UnrollJobPersistenceStore) MarkEffectDone(ctx context.Context, id,
	claimToken string) error {

	now := s.clock.Now().Unix()
	var token any
	if claimToken != "" {
		token = claimToken
	}

	return s.db.ExecTx(ctx, WriteTxOption(), func(q UnrollJobStore) error {
		return q.MarkUnrollEffectDone(
			ctx, sqlc.MarkUnrollEffectDoneParams{
				ID:      id,
				Column2: token,
				DoneAt: sql.NullInt64{
					Int64: now,
					Valid: true,
				},
			},
		)
	})
}

// ReleaseEffectForRetry releases a failed claimed effect back to pending or
// dead after max attempts.
func (s *UnrollJobPersistenceStore) ReleaseEffectForRetry(ctx context.Context,
	id, claimToken string, retryAfter time.Duration, failure error) error {

	now := s.clock.Now()
	errText := ""
	if failure != nil {
		errText = failure.Error()
	}

	return s.db.ExecTx(ctx, WriteTxOption(), func(q UnrollJobStore) error {
		return q.ReleaseUnrollEffectForRetry(
			ctx, sqlc.ReleaseUnrollEffectForRetryParams{
				ID: id,
				ClaimToken: sql.NullString{
					String: claimToken,
					Valid:  true,
				},
				NextAttemptAt: now.Add(retryAfter).Unix(),
				UpdatedAt:     now.Unix(),
				LastError: sql.NullString{
					String: errText,
					Valid:  errText != "",
				},
			},
		)
	})
}

// ReleaseExpiredEffectClaims releases timed-out effect claims.
func (s *UnrollJobPersistenceStore) ReleaseExpiredEffectClaims(
	ctx context.Context) error {

	now := s.clock.Now().Unix()

	return s.db.ExecTx(ctx, WriteTxOption(), func(q UnrollJobStore) error {
		return q.ReleaseExpiredUnrollEffectClaims(
			ctx, sqlc.ReleaseExpiredUnrollEffectClaimsParams{
				ClaimUntil: sql.NullInt64{
					Int64: now,
					Valid: true,
				},
				UpdatedAt: now,
			},
		)
	})
}

func insertUnrollEffectsForJob(ctx context.Context, q UnrollJobStore,
	job UnrollJobRecord, now int64) error {

	if job.IsTerminal() {
		return nil
	}

	target := job.TargetOutpoint
	insert := func(effectType string, txid []byte, suffix string) error {
		id := unrollEffectID(target, suffix)

		return q.InsertUnrollEffect(ctx, sqlc.InsertUnrollEffectParams{
			ID:                  id,
			TargetOutpointHash:  target.Hash[:],
			TargetOutpointIndex: int32(target.Index),
			EffectType:          effectType,
			Txid:                append([]byte(nil), txid...),
			IdempotencyKey:      id,
			MaxAttempts:         10,
			NextAttemptAt:       now,
			CreatedAt:           now,
		})
	}

	if err := insert(
		"subscribe_blocks", nil, "subscribe-blocks",
	); err != nil {
		return err
	}
	if err := insert(
		"watch_target_spend", nil, "watch-target-spend",
	); err != nil {
		return err
	}

	if job.State == "sweep_broadcast" {
		if err := insert(
			"build_sweep", nil, "build-sweep",
		); err != nil {
			return err
		}
	}

	for i := range job.TxProgress {
		progress := job.TxProgress[i]
		switch progress.Role {
		case "proof":
			if progress.Status == "in_flight" ||
				progress.Status == "ready" {

				err := insert(
					"ensure_tx_confirmed", progress.Txid,
					"ensure-tx-"+fmt.Sprintf("%x",
						progress.Txid),
				)
				if err != nil {
					return err
				}
			}

		case "deferred_checkpoint":
			if progress.Status == "ready" {
				err := insert(
					"watch_deferred_checkpoint",
					progress.Txid,
					"watch-deferred-"+fmt.Sprintf("%x",
						progress.Txid),
				)
				if err != nil {
					return err
				}
			}

		case "sweep":
			if progress.Status == "in_flight" ||
				progress.Status == "ready" {

				err := insert(
					"ensure_sweep_confirmed", progress.Txid,
					"ensure-sweep-"+fmt.Sprintf("%x",
						progress.Txid),
				)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func unrollEffectID(target wire.OutPoint, suffix string) string {
	return "unroll/" + target.String() + "/" + suffix
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

func unrollEffectRecordFromRow(row sqlc.UnrollEffect) (UnrollEffectRecord,
	error) {

	hash, err := hashFromBytes(row.TargetOutpointHash)
	if err != nil {
		return UnrollEffectRecord{}, fmt.Errorf("unexpected target "+
			"outpoint hash: %w", err)
	}

	return UnrollEffectRecord{
		ID: row.ID,
		TargetOutpoint: wire.OutPoint{
			Hash:  hash,
			Index: uint32(row.TargetOutpointIndex),
		},
		EffectType:     row.EffectType,
		Txid:           append([]byte(nil), row.Txid...),
		Status:         row.Status,
		IdempotencyKey: row.IdempotencyKey,
		ClaimToken:     row.ClaimToken,
		Attempts:       row.Attempts,
	}, nil
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

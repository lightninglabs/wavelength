package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/vhtlcrecovery"
	"github.com/lightningnetwork/lnd/clock"
)

var (
	// ErrVHTLCRecoveryJobNotFound indicates the recovery row does not
	// exist.
	ErrVHTLCRecoveryJobNotFound = errors.New("vhtlc recovery job not found")

	// ErrVHTLCRecoveryIdempotencyConflict indicates an idempotent arm
	// request reused an existing key with different recovery parameters.
	ErrVHTLCRecoveryIdempotencyConflict = errors.New("vhtlc recovery " +
		"idempotency conflict")

	// ErrVHTLCRecoveryCannotEscalate indicates the recovery row exists but
	// is no longer in an armed state that can be escalated.
	ErrVHTLCRecoveryCannotEscalate = errors.New("vhtlc recovery cannot " +
		"escalate from current state")

	// ErrVHTLCRecoveryAlreadyTerminal indicates the recovery row exists
	// but is already in a terminal state (`completed`, `cancelled`, or
	// `failed`), so the requested transition is a no-op. Callers can
	// distinguish this from the missing-row case to avoid masking lost
	// updates between two racing terminal transitions.
	ErrVHTLCRecoveryAlreadyTerminal = errors.New("vhtlc recovery job is " +
		"already terminal")
)

// VHTLCRecoveryStoreDB persists vHTLC recovery jobs.
type VHTLCRecoveryStoreDB struct {
	*TransactionExecutor[*sqlc.Queries]

	clk clock.Clock
}

// NewVHTLCRecoveryStore creates a vHTLC recovery store from the shared DB.
func NewVHTLCRecoveryStore(store *Store,
	clk clock.Clock) *VHTLCRecoveryStoreDB {

	if clk == nil {
		clk = clock.NewDefaultClock()
	}

	txExec := NewTransactionExecutor(
		store.BaseDB(),
		func(tx *sql.Tx) *sqlc.Queries {
			return store.Queries().WithTx(tx)
		},
		store.log,
	)

	return &VHTLCRecoveryStoreDB{
		TransactionExecutor: txExec,
		clk:                 clk,
	}
}

// ArmRecovery stores an armed recovery job or returns the existing idempotent
// row. A retry can return a terminal row when the original recovery already
// completed, failed, or was cancelled; callers must inspect IsTerminal before
// deciding whether to escalate or short-circuit.
func (s *VHTLCRecoveryStoreDB) ArmRecovery(ctx context.Context,
	job vhtlcrecovery.RecoveryJob) (*vhtlcrecovery.RecoveryJob, bool,
	error) {

	if job.RequestID == "" {
		return nil, false, fmt.Errorf("request id is required")
	}
	if len(job.SwapID) == 0 {
		return nil, false, fmt.Errorf("swap id is required")
	}
	if job.ID == "" {
		job.ID = uuid.NewString()
	}
	if job.State == "" {
		job.State = vhtlcrecovery.StateArmed
	}
	if job.State != vhtlcrecovery.StateArmed {
		return nil, false, fmt.Errorf("arm state must be %q, got %q",
			vhtlcrecovery.StateArmed, job.State)
	}

	policyKind, err := vhtlcrecovery.ExitPolicyKindForAction(job.Action)
	if err != nil {
		return nil, false, err
	}
	if job.ExitPolicyKind != "" && job.ExitPolicyKind != policyKind {
		return nil, false, fmt.Errorf("exit policy kind %q does not "+
			"match action %q", job.ExitPolicyKind, job.Action)
	}
	job.ExitPolicyKind = policyKind

	now := s.clk.Now().UTC()
	job.CreatedAt = now
	job.UpdatedAt = now
	job.ArmedAt = &now

	var stored *vhtlcrecovery.RecoveryJob
	created := false
	err = s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		existing, err := q.GetVHTLCRecoveryJobByRequestID(
			ctx, job.RequestID,
		)
		if err == nil {
			record, err := vhtlcRecoveryJobFromRow(existing)
			if err != nil {
				return err
			}
			if !sameArmParameters(record, job) {
				return ErrVHTLCRecoveryIdempotencyConflict
			}

			stored = &record

			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		rows, err := q.ListVHTLCRecoveryJobsBySwapAction(
			ctx, sqlc.ListVHTLCRecoveryJobsBySwapActionParams{
				SwapID: job.SwapID,
				Action: job.Action,
			},
		)
		if err != nil {
			return err
		}
		for i := range rows {
			record, err := vhtlcRecoveryJobFromRow(rows[i])
			if err != nil {
				return err
			}
			if sameArmParameters(record, job) {
				stored = &record

				return nil
			}
			if record.VTXOOutpoint == job.VTXOOutpoint {
				return ErrVHTLCRecoveryIdempotencyConflict
			}
		}

		if err := q.InsertVHTLCRecoveryJob(
			ctx, vhtlcRecoveryInsertParams(job, now),
		); err != nil {
			return err
		}

		created = true
		storedJob := job
		stored = &storedJob

		return nil
	})
	if err != nil {
		return nil, false, err
	}

	return stored, created, nil
}

// GetRecovery loads one recovery job row.
func (s *VHTLCRecoveryStoreDB) GetRecovery(ctx context.Context, id string) (
	*vhtlcrecovery.RecoveryJob, error) {

	var job *vhtlcrecovery.RecoveryJob
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		row, err := q.GetVHTLCRecoveryJob(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrVHTLCRecoveryJobNotFound
		}
		if err != nil {
			return err
		}

		record, err := vhtlcRecoveryJobFromRow(row)
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

// ListNonTerminalRecoveries loads every active recovery job.
func (s *VHTLCRecoveryStoreDB) ListNonTerminalRecoveries(ctx context.Context) (
	[]vhtlcrecovery.RecoveryJob, error) {

	var jobs []vhtlcrecovery.RecoveryJob
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.ListNonTerminalVHTLCRecoveryJobs(ctx)
		if err != nil {
			return err
		}

		jobs = make([]vhtlcrecovery.RecoveryJob, 0, len(rows))
		for i := range rows {
			job, err := vhtlcRecoveryJobFromRow(rows[i])
			if err != nil {
				return err
			}

			jobs = append(jobs, job)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return jobs, nil
}

// ListRecoveries loads every recovery job in newest-updated order.
func (s *VHTLCRecoveryStoreDB) ListRecoveries(ctx context.Context) (
	[]vhtlcrecovery.RecoveryJob, error) {

	var jobs []vhtlcrecovery.RecoveryJob
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.ListVHTLCRecoveryJobs(ctx)
		if err != nil {
			return err
		}

		jobs = make([]vhtlcrecovery.RecoveryJob, 0, len(rows))
		for i := range rows {
			job, err := vhtlcRecoveryJobFromRow(rows[i])
			if err != nil {
				return err
			}

			jobs = append(jobs, job)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return jobs, nil
}

// EscalateRecovery marks an armed recovery job as active. The optional claim
// preimage is written in the same transaction so cross-process claim recovery
// can restart after escalation without depending on the caller process.
func (s *VHTLCRecoveryStoreDB) EscalateRecovery(ctx context.Context, id string,
	claimPreimage []byte) error {

	now := s.clk.Now().Unix()

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.EscalateVHTLCRecoveryJob(
			ctx, sqlc.EscalateVHTLCRecoveryJobParams{
				ID:            id,
				UpdatedAt:     now,
				ClaimPreimage: cloneBytes(claimPreimage),
			},
		)
		if err != nil {
			return err
		}
		if rows > 0 {
			return nil
		}

		_, err = q.GetVHTLCRecoveryJob(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrVHTLCRecoveryJobNotFound
		}
		if err != nil {
			return err
		}

		return ErrVHTLCRecoveryCannotEscalate
	})
}

// CancelRecovery marks a non-terminal recovery job as cancelled.
func (s *VHTLCRecoveryStoreDB) CancelRecovery(ctx context.Context, id,
	reason string, cooperativeTxid []byte) error {

	now := s.clk.Now().Unix()

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.CancelVHTLCRecoveryJob(
			ctx, sqlc.CancelVHTLCRecoveryJobParams{
				ID: id,
				CancelReason: sql.NullString{
					String: reason,
					Valid:  reason != "",
				},
				CooperativeTxid: cloneBytes(cooperativeTxid),
				UpdatedAt:       now,
			},
		)
		if err != nil {
			return err
		}

		return classifyTerminalTransition(ctx, q, id, rows)
	})
}

// CompleteRecovery marks a non-terminal recovery job as completed.
func (s *VHTLCRecoveryStoreDB) CompleteRecovery(ctx context.Context,
	id string) error {

	now := s.clk.Now().Unix()

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.CompleteVHTLCRecoveryJob(
			ctx, sqlc.CompleteVHTLCRecoveryJobParams{
				ID:        id,
				UpdatedAt: now,
			},
		)
		if err != nil {
			return err
		}

		return classifyTerminalTransition(ctx, q, id, rows)
	})
}

// FailRecovery marks a non-terminal recovery job as failed.
func (s *VHTLCRecoveryStoreDB) FailRecovery(ctx context.Context, id string,
	failure error) error {

	now := s.clk.Now().Unix()
	errText := ""
	if failure != nil {
		errText = failure.Error()
	}

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.FailVHTLCRecoveryJob(
			ctx, sqlc.FailVHTLCRecoveryJobParams{
				ID: id,
				LastError: sql.NullString{
					String: errText,
					Valid:  errText != "",
				},
				UpdatedAt: now,
			},
		)
		if err != nil {
			return err
		}

		return classifyTerminalTransition(ctx, q, id, rows)
	})
}

// classifyTerminalTransition disambiguates a zero-row terminal-state UPDATE.
// The three terminal queries (Cancel, Complete, Fail) all guard on
// `state NOT IN ('completed', 'cancelled', 'failed')` so a zero row count
// means either the recovery row is missing or it was already terminal when
// the transition fired. Callers can tell the two apart without masking
// lost-update bugs by checking the returned sentinel with errors.Is.
func classifyTerminalTransition(ctx context.Context, q *sqlc.Queries, id string,
	rows int64) error {

	if rows > 0 {
		return nil
	}

	if _, err := q.GetVHTLCRecoveryJob(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrVHTLCRecoveryJobNotFound
		}

		return err
	}

	return ErrVHTLCRecoveryAlreadyTerminal
}

// vhtlcRecoveryInsertParams converts a recovery job into insert parameters.
func vhtlcRecoveryInsertParams(job vhtlcrecovery.RecoveryJob,
	now time.Time) sqlc.InsertVHTLCRecoveryJobParams {

	params := sqlc.InsertVHTLCRecoveryJobParams{
		ID:        job.ID,
		RequestID: job.RequestID,
		SwapID:    cloneBytes(job.SwapID),
		Direction: job.Direction,
		Action:    job.Action,
		VtxoTxid:  job.VTXOOutpoint.Hash[:],
		VtxoVout: int32(
			job.VTXOOutpoint.Index,
		),
		VtxoAmountSat: job.VTXOAmountSat,
		SenderPubkey: cloneBytes(
			job.SenderPubkey,
		),
		ReceiverPubkey: cloneBytes(
			job.ReceiverPubkey,
		),
		ServerPubkey: cloneBytes(
			job.ServerPubkey,
		),
		RefundLocktime:                       job.RefundLocktime,
		UnilateralClaimDelay:                 job.UnilateralClaimDelay,
		UnilateralRefundDelay:                job.UnilateralRefundDelay,
		UnilateralRefundWithoutReceiverDelay: job.UnilateralRefundWithoutReceiverDelay, //nolint:ll
		PreimageHash: cloneBytes(
			job.PreimageHash,
		),
		SignerKeyFamily: job.SignerKeyFamily,
		SignerKeyIndex:  job.SignerKeyIndex,
		DestinationScript: cloneBytes(
			job.DestinationScript,
		),
		MaxFeeRateSatPerKw: job.MaxFeeRateSatPerKWeight,
		ExitPolicyKind:     job.ExitPolicyKind,
		CreatedAt:          now.Unix(),
	}

	if job.UnrollTargetOutpoint != nil {
		params.UnrollTargetOutpointHash = job.UnrollTargetOutpoint.
			Hash[:]
		params.UnrollTargetOutpointIndex = sql.NullInt32{
			Int32: int32(job.UnrollTargetOutpoint.Index),
			Valid: true,
		}
	}

	return params
}

// vhtlcRecoveryJobFromRow converts a generated SQL row into a recovery job.
func vhtlcRecoveryJobFromRow(row sqlc.VhtlcRecoveryJob) (
	vhtlcrecovery.RecoveryJob, error) {

	targetHash, err := hashFromBytes(row.VtxoTxid)
	if err != nil {
		return vhtlcrecovery.RecoveryJob{}, fmt.Errorf("decode vhtlc "+
			"recovery target txid: %w", err)
	}

	unrollTarget, err := outpointFromNullable(
		row.UnrollTargetOutpointHash, row.UnrollTargetOutpointIndex,
	)
	if err != nil {
		return vhtlcrecovery.RecoveryJob{}, fmt.Errorf("decode vhtlc "+
			"recovery unroll target: %w", err)
	}

	outpoint := wire.OutPoint{
		Hash:  targetHash,
		Index: uint32(row.VtxoVout),
	}

	return vhtlcrecovery.RecoveryJob{
		ID:            row.ID,
		RequestID:     row.RequestID,
		SwapID:        cloneBytes(row.SwapID),
		Direction:     row.Direction,
		Action:        row.Action,
		State:         row.State,
		VTXOOutpoint:  outpoint,
		VTXOAmountSat: row.VtxoAmountSat,
		SenderPubkey: cloneBytes(
			row.SenderPubkey,
		),
		ReceiverPubkey: cloneBytes(
			row.ReceiverPubkey,
		),
		ServerPubkey: cloneBytes(
			row.ServerPubkey,
		),
		RefundLocktime:                       row.RefundLocktime,
		UnilateralClaimDelay:                 row.UnilateralClaimDelay,
		UnilateralRefundDelay:                row.UnilateralRefundDelay,
		UnilateralRefundWithoutReceiverDelay: row.UnilateralRefundWithoutReceiverDelay, //nolint:ll
		PreimageHash: cloneBytes(
			row.PreimageHash,
		),
		ClaimPreimage: cloneBytes(
			row.ClaimPreimage,
		),
		SignerKeyFamily: row.SignerKeyFamily,
		SignerKeyIndex:  row.SignerKeyIndex,
		DestinationScript: cloneBytes(
			row.DestinationScript,
		),
		MaxFeeRateSatPerKWeight: row.MaxFeeRateSatPerKw,
		UnrollTargetOutpoint:    unrollTarget,
		ExitPolicyKind:          row.ExitPolicyKind,
		ExitTx:                  cloneBytes(row.ExitTx),
		ExitTxid:                cloneBytes(row.ExitTxid),
		CooperativeTxid:         cloneBytes(row.CooperativeTxid),
		LastError:               stringFromNull(row.LastError),
		CancelReason:            stringFromNull(row.CancelReason),
		CreatedAt:               unixTimeUTC(row.CreatedAt),
		UpdatedAt:               unixTimeUTC(row.UpdatedAt),
		ArmedAt:                 timeFromNull(row.ArmedAt),
		EscalatedAt:             timeFromNull(row.EscalatedAt),
		TargetDetectedAt:        timeFromNull(row.TargetDetectedAt),
		ExitTxBuiltAt:           timeFromNull(row.ExitTxBuiltAt),
		ExitTxBroadcastAt:       timeFromNull(row.ExitTxBroadcastAt),
		TerminalAt:              timeFromNull(row.TerminalAt),
	}, nil
}

// sameArmParameters reports whether two jobs describe the same recovery intent.
func sameArmParameters(a, b vhtlcrecovery.RecoveryJob) bool {
	return bytes.Equal(a.SwapID, b.SwapID) &&
		a.Direction == b.Direction &&
		a.Action == b.Action &&
		a.VTXOOutpoint == b.VTXOOutpoint &&
		a.VTXOAmountSat == b.VTXOAmountSat &&
		bytes.Equal(a.SenderPubkey, b.SenderPubkey) &&
		bytes.Equal(a.ReceiverPubkey, b.ReceiverPubkey) &&
		bytes.Equal(a.ServerPubkey, b.ServerPubkey) &&
		a.RefundLocktime == b.RefundLocktime &&
		a.UnilateralClaimDelay == b.UnilateralClaimDelay &&
		a.UnilateralRefundDelay == b.UnilateralRefundDelay &&
		a.UnilateralRefundWithoutReceiverDelay ==
			b.UnilateralRefundWithoutReceiverDelay &&
		bytes.Equal(a.PreimageHash, b.PreimageHash) &&
		a.SignerKeyFamily == b.SignerKeyFamily &&
		a.SignerKeyIndex == b.SignerKeyIndex &&
		bytes.Equal(a.DestinationScript, b.DestinationScript) &&
		a.MaxFeeRateSatPerKWeight == b.MaxFeeRateSatPerKWeight &&
		sameOptionalOutpoint(
			a.UnrollTargetOutpoint, b.UnrollTargetOutpoint,
		)
}

// sameOptionalOutpoint compares nullable outpoints.
func sameOptionalOutpoint(a, b *wire.OutPoint) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}

	return *a == *b
}

// outpointFromNullable converts nullable SQL outpoint fields.
func outpointFromNullable(hash []byte,
	index sql.NullInt32) (*wire.OutPoint, error) {

	if len(hash) == 0 || !index.Valid {
		return nil, nil
	}

	outpointHash, err := hashFromBytes(hash)
	if err != nil {
		return nil, err
	}

	return &wire.OutPoint{
		Hash:  outpointHash,
		Index: uint32(index.Int32),
	}, nil
}

// cloneBytes returns an owned copy of a byte slice.
func cloneBytes(value []byte) []byte {
	return append([]byte(nil), value...)
}

// stringFromNull converts a nullable string to its zero-value form.
func stringFromNull(value sql.NullString) string {
	if !value.Valid {
		return ""
	}

	return value.String
}

// timeFromNull converts a nullable unix timestamp to UTC time.
func timeFromNull(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}

	t := unixTimeUTC(value.Int64)

	return &t
}

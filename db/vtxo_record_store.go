package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightninglabs/darepo/vtxo"
)

// VTXORecordStoreDB implements vtxo.Store using the existing vtxos table.
//
// Unlike rounds.VTXOStore (which operates on rounds.VTXO), this store exposes a
// minimal record model used by the OOR coordinator to:
//   - mark inputs in-flight/spent, and
//   - materialize recipient outputs as live VTXOs.
//
// NOTE: This store intentionally does not mutate round-specific metadata such
// as round_id or batch_output_index.
type VTXORecordStoreDB struct {
	*TransactionExecutor[*sqlc.Queries]

	q *sqlc.Queries
}

const (
	errVTXOExistsDifferentPkScript = "vtxo %v already exists with " +
		"different pk_script"
	errVTXOExistsWithStatus    = "vtxo %v already exists with status %s"
	errVTXOExistsWithLockOwner = "vtxo %v already exists with " +
		"lock_owner=%s"
	errVTXOInFlightBy     = "vtxo %v in-flight by %s"
	errLockUnknownReasons = "failed to lock vtxo %v for " +
		"unknown reasons"
	errNotSpendableWithOwner   = "vtxo %v not spendable (%s, owner=%s)"
	errClearInFlightLockFailed = "failed to clear in-flight lock for %v"
)

// NewVTXORecordStoreDB creates a new VTXORecordStoreDB from a Store.
func NewVTXORecordStoreDB(store *Store) *VTXORecordStoreDB {
	txExec := NewTransactionExecutor(
		store, func(tx *sql.Tx) *sqlc.Queries {
			return store.WithTx(tx)
		}, store.log,
	)

	return &VTXORecordStoreDB{
		TransactionExecutor: txExec,
		q:                   store.Queries,
	}
}

func outpointToGetParams(outpoint wire.OutPoint) sqlc.GetVTXOParams {
	return sqlc.GetVTXOParams{
		OutpointHash:  outpoint.Hash[:],
		OutpointIndex: int32(outpoint.Index),
	}
}

func outpointToUpdateParams(outpoint wire.OutPoint,
	status string) sqlc.UpdateVTXOStatusParams {

	return sqlc.UpdateVTXOStatusParams{
		OutpointHash:  outpoint.Hash[:],
		OutpointIndex: int32(outpoint.Index),
		Status:        status,
	}
}

func rowToRecord(row sqlc.Vtxo) (*vtxo.Record, error) {
	var outpoint wire.OutPoint
	copy(outpoint.Hash[:], row.OutpointHash)
	outpoint.Index = uint32(row.OutpointIndex)

	rec := &vtxo.Record{
		Outpoint: outpoint,
		Value:    row.Amount,
		PkScript: bytes.Clone(row.PkScript),
		Status:   vtxo.Status(row.Status),
	}

	if row.LockOwnerKind.Valid && len(row.LockOwnerID) > 0 {
		rec.InFlightOwner = lockOwnerToValue(
			row.LockOwnerKind.String, row.LockOwnerID,
		)
	}

	return rec, nil
}

// Get returns the record for outpoint, or (nil, nil) if none exists.
func (v *VTXORecordStoreDB) Get(ctx context.Context,
	outpoint wire.OutPoint) (*vtxo.Record, error) {

	var result *vtxo.Record

	err := v.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		row, err := q.GetVTXO(ctx, outpointToGetParams(outpoint))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("get vtxo %v: %w", outpoint, err)
		}

		rec, err := rowToRecord(row)
		if err != nil {
			return err
		}

		result = rec

		return nil
	})

	return result, err
}

// Create inserts a record if it does not already exist.
//
// If a record already exists, this is treated as idempotent only if all
// relevant fields match (value, pk_script, status, and in-flight owner).
func (v *VTXORecordStoreDB) Create(ctx context.Context,
	record *vtxo.Record) error {

	if record == nil {
		return fmt.Errorf("record must be provided")
	}

	if record.Value < 0 {
		return fmt.Errorf("record value must be non-negative")
	}

	if len(record.PkScript) == 0 {
		return fmt.Errorf("record pkScript must be provided")
	}

	if record.Status == "" {
		return fmt.Errorf("record status must be provided")
	}

	if record.Status == vtxo.StatusInFlight && record.InFlightOwner == "" {
		return fmt.Errorf("in-flight owner must be provided")
	}

	if record.Status != vtxo.StatusInFlight && record.InFlightOwner != "" {
		return fmt.Errorf("in-flight owner set for status %s",
			record.Status)
	}

	lockOwnerKind := sql.NullString{Valid: false}
	lockOwnerID := []byte(nil)
	if record.Status == vtxo.StatusInFlight {
		kind, ownerID, err := parseLockOwner(record.InFlightOwner)
		if err != nil {
			return err
		}

		lockOwnerKind = sql.NullString{
			String: kind,
			Valid:  true,
		}
		lockOwnerID = ownerID
	}

	operatorKey, err := cosignerFromPkScript(record.PkScript)
	if err != nil {
		return err
	}

	return v.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		// TODO(elle): The operator key, exit delay, and
		// key locator are known at construction time and
		// should be threaded through. The owner key is the
		// only genuinely unknown field here — the OOR
		// record model only has the PkScript, not the
		// decomposed owner key.
		insertParams := sqlc.InsertVTXOParams{
			OutpointHash:  record.Outpoint.Hash[:],
			OutpointIndex: int32(record.Outpoint.Index),
			RoundID:       nil,
			BatchOutputIndex: sql.NullInt32{
				Valid: false,
			},
			Amount:            record.Value,
			ExitDelay:         0,
			PkScript:          record.PkScript,
			OwnerKey:          operatorKey,
			OperatorKey:       operatorKey,
			OperatorKeyFamily: 0,
			OperatorKeyIndex:  0,
			Status:            string(record.Status),
			LockOwnerKind:     lockOwnerKind,
			LockOwnerID:       lockOwnerID,
		}
		err := qtx.InsertVTXO(ctx, insertParams)
		if err == nil {
			return nil
		}

		dbErr := MapSQLError(err)
		var uniqueErr *ErrSQLUniqueConstraintViolation
		if !errors.As(dbErr, &uniqueErr) {
			return fmt.Errorf("insert vtxo %v: %w",
				record.Outpoint, dbErr)
		}

		// Existing row.
		//
		// Only allow idempotent re-inserts.
		// This prevents clobbering round metadata for an existing
		// outpoint.
		getParams := outpointToGetParams(record.Outpoint)
		row, err := qtx.GetVTXO(ctx, getParams)
		if errors.Is(err, sql.ErrNoRows) {
			// Extremely unlikely race.
			// Surface as the original error.
			return fmt.Errorf("insert vtxo %v: %w",
				record.Outpoint, dbErr)
		}
		if err != nil {
			return fmt.Errorf(
				"get vtxo %v after insert conflict: %w",
				record.Outpoint,
				err,
			)
		}

		// Compare the relevant fields. We intentionally skip
		// owner_key, operator_key, and exit_delay because the OOR
		// path writes placeholders for those columns (see the TODO
		// above). A round-inserted row may already exist with the
		// correct keys; re-checking would always mismatch.
		if row.Amount != record.Value {
			return fmt.Errorf(
				"vtxo %v already exists with different value",
				record.Outpoint,
			)
		}
		if !bytes.Equal(row.PkScript, record.PkScript) {
			return fmt.Errorf(
				errVTXOExistsDifferentPkScript,
				record.Outpoint,
			)
		}
		if row.Status != string(record.Status) {
			return fmt.Errorf(
				errVTXOExistsWithStatus,
				record.Outpoint,
				row.Status,
			)
		}
		if row.LockOwnerKind != lockOwnerKind ||
			!bytes.Equal(row.LockOwnerID, lockOwnerID) {

			existingOwner := lockOwnerToValue(
				row.LockOwnerKind.String,
				row.LockOwnerID,
			)

			return fmt.Errorf(
				errVTXOExistsWithLockOwner,
				record.Outpoint,
				existingOwner,
			)
		}

		return nil
	})
}

// MarkInFlight marks the outpoints in-flight for owner.
func (v *VTXORecordStoreDB) MarkInFlight(ctx context.Context,
	outpoints []wire.OutPoint, owner vtxo.LockOwner) error {

	if len(outpoints) == 0 {
		return nil
	}

	if owner == "" {
		return fmt.Errorf("owner must be provided")
	}

	ownerKind, ownerID, err := parseLockOwner(owner)
	if err != nil {
		return err
	}

	err = vtxo.ValidateUniqueOutpoints(outpoints)
	if err != nil {
		return err
	}

	return v.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		for _, outpoint := range outpoints {
			lockParams := sqlc.LockVTXOParams{
				OutpointHash:  outpoint.Hash[:],
				OutpointIndex: int32(outpoint.Index),
				LockOwnerKind: sql.NullString{
					String: ownerKind,
					Valid:  true,
				},
				LockOwnerID: ownerID,
			}
			rowsAffected, err := qtx.LockVTXO(ctx, lockParams)
			if err != nil {
				return fmt.Errorf(
					"lock vtxo %v: %w",
					outpoint,
					err,
				)
			}

			if rowsAffected > 0 {
				continue
			}

			// Provide a specific reason.
			getParams := outpointToGetParams(outpoint)
			row, err := qtx.GetVTXO(ctx, getParams)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("unknown vtxo: %v", outpoint)
			}
			if err != nil {
				return fmt.Errorf(
					"get vtxo %v after lock failed: %w",
					outpoint,
					err,
				)
			}

			// Idempotent case.
			if row.Status == string(vtxo.StatusInFlight) &&
				row.LockOwnerKind.Valid &&
				row.LockOwnerKind.String == ownerKind &&
				bytes.Equal(row.LockOwnerID, ownerID) {

				continue
			}

			switch vtxo.Status(row.Status) {
			case vtxo.StatusLive:
				hasOwner := row.LockOwnerKind.Valid &&
					len(row.LockOwnerID) > 0
				existingOwner := lockOwnerToValue(
					row.LockOwnerKind.String,
					row.LockOwnerID,
				)
				if hasOwner {
					lockKindDiff :=
						row.LockOwnerKind.String !=
							ownerKind
					lockIDDiff := !bytes.Equal(
						row.LockOwnerID, ownerID,
					)
					lockedByOther :=
						lockKindDiff || lockIDDiff
					if lockedByOther {
						return fmt.Errorf(
							errVTXOInFlightBy,
							outpoint,
							existingOwner,
						)
					}
				}

				return fmt.Errorf(
					errLockUnknownReasons,
					outpoint,
				)

			default:
				if row.LockOwnerKind.Valid &&
					len(row.LockOwnerID) > 0 {

					existingOwner := lockOwnerToValue(
						row.LockOwnerKind.String,
						row.LockOwnerID,
					)

					return fmt.Errorf(
						errNotSpendableWithOwner,
						outpoint,
						row.Status,
						existingOwner,
					)
				}

				return fmt.Errorf("vtxo %v not spendable (%s)",
					outpoint, row.Status)
			}
		}

		return nil
	})
}

// MarkSpent marks the outpoints spent.
func (v *VTXORecordStoreDB) MarkSpent(ctx context.Context,
	outpoints []wire.OutPoint) error {

	if len(outpoints) == 0 {
		return nil
	}

	err := vtxo.ValidateUniqueOutpoints(outpoints)
	if err != nil {
		return err
	}

	type vtxoState struct {
		outpoint  wire.OutPoint
		status    string
		ownerKind sql.NullString
		ownerID   []byte
	}

	return v.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		states := make([]vtxoState, 0, len(outpoints))

		// Validate first so the update appears atomic to callers.
		for _, outpoint := range outpoints {
			getParams := outpointToGetParams(outpoint)
			row, err := qtx.GetVTXO(ctx, getParams)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("unknown vtxo: %v", outpoint)
			}
			if err != nil {
				return fmt.Errorf(
					"get vtxo %v: %w",
					outpoint,
					err,
				)
			}

			switch vtxo.Status(row.Status) {
			case vtxo.StatusLive:
			case vtxo.StatusInFlight:
			case vtxo.StatusSpent:
				// ok
			default:
				return fmt.Errorf("vtxo %v not spendable (%s)",
					outpoint, row.Status)
			}

			states = append(states, vtxoState{
				outpoint:  outpoint,
				status:    row.Status,
				ownerKind: row.LockOwnerKind,
				ownerID:   bytes.Clone(row.LockOwnerID),
			})
		}

		for _, state := range states {
			// Idempotent success.
			if state.status == string(vtxo.StatusSpent) {
				v.log.Warnf("mark spent called for "+
					"already-spent vtxo %v (idempotent)",
					state.outpoint)

				continue
			}

			// If the vtxo is in-flight, clear its lock first.
			// This avoids leaving stale lock owner metadata set.
			// (Spent outputs should not be locked.)
			if state.status == string(vtxo.StatusInFlight) &&
				state.ownerKind.Valid &&
				len(state.ownerID) > 0 {

				unlockParams := sqlc.UnlockVTXOParams{
					OutpointHash: state.outpoint.Hash[:],
					OutpointIndex: int32(
						state.outpoint.Index,
					),
					LockOwnerKind: sql.NullString{
						String: state.ownerKind.String,
						Valid:  true,
					},
					LockOwnerID: state.ownerID,
				}
				rowsAffected, err := qtx.UnlockVTXO(
					ctx, unlockParams,
				)
				if err != nil {
					return fmt.Errorf("unlock vtxo %v: %w",
						state.outpoint, err)
				}
				if rowsAffected == 0 {
					return fmt.Errorf(
						errClearInFlightLockFailed,
						state.outpoint,
					)
				}
			}

			updateParams := outpointToUpdateParams(
				state.outpoint, string(vtxo.StatusSpent),
			)
			affected, err := qtx.UpdateVTXOStatus(ctx,
				updateParams,
			)
			if err != nil {
				return fmt.Errorf("mark vtxo %v spent: %w",
					state.outpoint, err)
			}
			if affected == 0 {
				return fmt.Errorf(
					"vtxo %v not found",
					state.outpoint,
				)
			}
		}

		return nil
	})
}

var _ vtxo.Store = (*VTXORecordStoreDB)(nil)

package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
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

	q           *sqlc.Queries
	clock       clock.Clock
	operatorKey keychain.KeyDescriptor
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
		clock:               store.clock,
	}
}

// SetOperatorKey configures the operator key descriptor used when a
// receive-script-backed generic record is materialized into a real Ark VTXO.
func (v *VTXORecordStoreDB) SetOperatorKey(operatorKey keychain.KeyDescriptor) {
	v.operatorKey = operatorKey
}

// outpointToGetParams converts a wire outpoint into the sqlc parameter set
// used by point lookups.
func outpointToGetParams(outpoint wire.OutPoint) sqlc.GetVTXOParams {
	return sqlc.GetVTXOParams{
		OutpointHash:  outpoint.Hash[:],
		OutpointIndex: int32(outpoint.Index),
	}
}

// outpointToUpdateParams converts a wire outpoint into the sqlc parameter
// set used by status update statements.
func outpointToUpdateParams(outpoint wire.OutPoint,
	status string) sqlc.UpdateVTXOStatusParams {

	return sqlc.UpdateVTXOStatusParams{
		OutpointHash:  outpoint.Hash[:],
		OutpointIndex: int32(outpoint.Index),
		Status:        status,
	}
}

// rowToRecord reconstructs a vtxo.Record from its persisted sqlc row.
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

	// Generic/materialized recipient rows do not carry a collaborative
	// Ark descriptor. They use placeholder owner/operator columns with
	// a zero exit delay, so only round-trip descriptor metadata when the
	// row has the real collaborative fields.
	if row.ExitDelay <= 0 {
		return rec, nil
	}

	ownerKey, err := btcec.ParsePubKey(row.OwnerKey)
	if err != nil {
		return nil, fmt.Errorf("parse owner key for %v: %w",
			outpoint, err)
	}

	operatorKey, err := btcec.ParsePubKey(row.OperatorKey)
	if err != nil {
		return nil, fmt.Errorf("parse operator key for %v: %w",
			outpoint, err)
	}

	rec.OwnerKey = ownerKey
	rec.OperatorKeyDesc = &keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(row.OperatorKeyFamily),
			Index:  uint32(row.OperatorKeyIndex),
		},
		PubKey: operatorKey,
	}
	rec.ExitDelay = uint32(row.ExitDelay)

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

// CreateVTXORecordTx inserts a record if it does not already exist using the
// caller's transaction/query context.
//
// If a record already exists, this is treated as idempotent only if all
// relevant fields match (value, pk_script, status, and in-flight owner).
func CreateVTXORecordTx(ctx context.Context, qtx *sqlc.Queries,
	record *vtxo.Record, expiresAtUnixS int64,
	operatorKey keychain.KeyDescriptor) error {

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

	effectiveRecord, err := enrichRecordDescriptorMetadataTx(
		ctx, qtx, record, expiresAtUnixS, operatorKey,
	)
	if err != nil {
		return err
	}

	lockOwnerKind := sql.NullString{Valid: false}
	lockOwnerID := []byte(nil)
	if effectiveRecord.Status == vtxo.StatusInFlight {
		kind, ownerID, err := parseLockOwner(
			effectiveRecord.InFlightOwner,
		)
		if err != nil {
			return err
		}

		lockOwnerKind = sql.NullString{
			String: kind,
			Valid:  true,
		}
		lockOwnerID = ownerID
	}

	insertParams, compareDescriptor, err := insertParamsFromRecord(
		effectiveRecord, lockOwnerKind, lockOwnerID,
	)
	if err != nil {
		return err
	}

	rowsAffected, err := qtx.InsertVTXOIfAbsent(
		ctx, sqlc.InsertVTXOIfAbsentParams(insertParams),
	)
	if err == nil && rowsAffected == 1 {
		return nil
	}
	if err != nil {
		dbErr := MapSQLError(err)
		return fmt.Errorf("insert vtxo %v: %w",
			effectiveRecord.Outpoint, dbErr)
	}

	// Existing row.
	//
	// Only allow idempotent re-inserts.
	// This prevents clobbering round metadata for an existing
	// outpoint.
	getParams := outpointToGetParams(effectiveRecord.Outpoint)
	row, err := qtx.GetVTXO(ctx, getParams)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf(
			"insert vtxo %v: no row present after insert conflict",
			effectiveRecord.Outpoint,
		)
	}
	if err != nil {
		return fmt.Errorf(
			"get vtxo %v after insert conflict: %w",
			effectiveRecord.Outpoint,
			err,
		)
	}

	// Compare the relevant fields. When the caller supplies the
	// full collaborative descriptor metadata, include it in the
	// idempotency check. Generic recipient scripts intentionally
	// skip those columns because they do not commit Ark VTXO
	// descriptor metadata.
	if row.Amount != effectiveRecord.Value {
		return fmt.Errorf(
			"vtxo %v already exists with different value",
			effectiveRecord.Outpoint,
		)
	}
	if !bytes.Equal(row.PkScript, effectiveRecord.PkScript) {
		return fmt.Errorf(
			errVTXOExistsDifferentPkScript,
			effectiveRecord.Outpoint,
		)
	}
	if row.Status != string(effectiveRecord.Status) {
		return fmt.Errorf(
			errVTXOExistsWithStatus,
			effectiveRecord.Outpoint,
			row.Status,
		)
	}
	if compareDescriptor {
		if !bytes.Equal(
			row.OwnerKey,
			effectiveRecord.OwnerKey.SerializeCompressed(),
		) {

			return fmt.Errorf(
				"vtxo %v already exists with "+
					"different owner key",
				effectiveRecord.Outpoint,
			)
		}
		if !bytes.Equal(
			row.OperatorKey,
			effectiveRecord.OperatorKeyDesc.PubKey.
				SerializeCompressed(),
		) {

			return fmt.Errorf(
				"vtxo %v already exists with "+
					"different operator key",
				effectiveRecord.Outpoint,
			)
		}
		if row.ExitDelay <= 0 ||
			uint32(row.ExitDelay) != effectiveRecord.ExitDelay {

			return fmt.Errorf(
				"vtxo %v already exists with "+
					"different exit delay",
				effectiveRecord.Outpoint,
			)
		}
		if row.OperatorKeyFamily != int32(
			effectiveRecord.OperatorKeyDesc.Family,
		) || row.OperatorKeyIndex != int32(
			effectiveRecord.OperatorKeyDesc.Index,
		) {

			return fmt.Errorf(
				"vtxo %v already exists with "+
					"different operator key "+
					"locator",
				effectiveRecord.Outpoint,
			)
		}
	}
	if row.LockOwnerKind != lockOwnerKind ||
		!bytes.Equal(row.LockOwnerID, lockOwnerID) {

		existingOwner := lockOwnerToValue(
			row.LockOwnerKind.String,
			row.LockOwnerID,
		)

		return fmt.Errorf(
			errVTXOExistsWithLockOwner,
			effectiveRecord.Outpoint,
			existingOwner,
		)
	}

	return nil
}

// Create inserts a record if it does not already exist.
//
// If a record already exists, this is treated as idempotent only if all
// relevant fields match (value, pk_script, status, and in-flight owner).
func (v *VTXORecordStoreDB) Create(ctx context.Context,
	record *vtxo.Record) error {

	return v.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		return CreateVTXORecordTx(
			ctx, qtx, record, v.clock.Now().Unix(), v.operatorKey,
		)
	})
}

// insertParamsFromRecord converts a record into InsertVTXO parameters and
// reports whether descriptor columns should participate in idempotency
// comparison when a conflicting row already exists.
func insertParamsFromRecord(record *vtxo.Record,
	lockOwnerKind sql.NullString,
	lockOwnerID []byte) (sqlc.InsertVTXOParams, bool, error) {

	insertParams := sqlc.InsertVTXOParams{
		OutpointHash:  record.Outpoint.Hash[:],
		OutpointIndex: int32(record.Outpoint.Index),
		RoundID:       nil,
		BatchOutputIndex: sql.NullInt32{
			Valid: false,
		},
		Amount:        record.Value,
		PkScript:      record.PkScript,
		Status:        string(record.Status),
		LockOwnerKind: lockOwnerKind,
		LockOwnerID:   lockOwnerID,
	}

	if record.OwnerKey != nil {
		if record.OperatorKeyDesc.Family >
			keychain.KeyFamily(math.MaxInt32) {

			return sqlc.InsertVTXOParams{}, false, fmt.Errorf(
				"operator key family out of range: %d",
				record.OperatorKeyDesc.Family,
			)
		}
		if record.OperatorKeyDesc.Index > math.MaxInt32 {
			return sqlc.InsertVTXOParams{}, false, fmt.Errorf(
				"operator key index out of range: %d",
				record.OperatorKeyDesc.Index,
			)
		}
		if record.ExitDelay > math.MaxInt32 {
			return sqlc.InsertVTXOParams{}, false, fmt.Errorf(
				"exit delay out of range: %d",
				record.ExitDelay,
			)
		}

		insertParams.ExitDelay = int32(record.ExitDelay)
		insertParams.OwnerKey = record.OwnerKey.SerializeCompressed()
		insertParams.OperatorKey =
			record.OperatorKeyDesc.PubKey.SerializeCompressed()
		insertParams.OperatorKeyFamily = int32(
			record.OperatorKeyDesc.Family,
		)
		insertParams.OperatorKeyIndex = int32(
			record.OperatorKeyDesc.Index,
		)

		return insertParams, true, nil
	}

	operatorKey, err := cosignerFromPkScript(record.PkScript)
	if err != nil {
		return sqlc.InsertVTXOParams{}, false, err
	}

	insertParams.OwnerKey = operatorKey
	insertParams.OperatorKey = operatorKey

	return insertParams, false, nil
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

// MarkVTXORecordsSpentTx marks the outpoints spent using the caller's
// transaction/query context.
func MarkVTXORecordsSpentTx(ctx context.Context, qtx *sqlc.Queries,
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
}

// MarkSpent marks the outpoints spent.
func (v *VTXORecordStoreDB) MarkSpent(ctx context.Context,
	outpoints []wire.OutPoint) error {

	return v.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		return MarkVTXORecordsSpentTx(ctx, qtx, outpoints)
	})
}

var _ vtxo.Store = (*VTXORecordStoreDB)(nil)

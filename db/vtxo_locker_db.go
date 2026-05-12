package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightninglabs/darepo/vtxo"
)

// VTXOLockerDB implements vtxo.Locker on top of the existing vtxos table.
//
// It uses the vtxos.status + lock owner columns as the shared lock
// state. This matches the coordinator's lifecycle semantics:
//   - LockMany transitions live -> in_flight(owner) using the sqlc LockVTXO
//     query.
//   - UnlockMany transitions in_flight(owner) -> live using the sqlc UnlockVTXO
//     query.
//
// This does not currently implement vtxo.LeaseLocker.
type VTXOLockerDB struct {
	tx *TransactionExecutor[*sqlc.Queries]
}

// NewVTXOLockerDB creates a DB-backed locker from any BatchedQuerier.
func NewVTXOLockerDB(dbq BatchedQuerier, log btclog.Logger) *VTXOLockerDB {
	if log == nil {
		log = btclog.Disabled
	}

	txExec := NewTransactionExecutor[*sqlc.Queries](
		dbq,
		func(tx *sql.Tx) *sqlc.Queries {
			return sqlc.NewWithBackend(tx, dbq.Backend())
		},
		log,
	)

	return &VTXOLockerDB{tx: txExec}
}

// LockMany attempts to lock all outpoints for the owner atomically.
func (l *VTXOLockerDB) LockMany(ctx context.Context, outpoints []wire.OutPoint,
	owner vtxo.LockOwner) error {

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

	return l.tx.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
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
				return fmt.Errorf("lock vtxo %v: %w", outpoint,
					err)
			}

			// Success.
			// Idempotent success if already locked by owner.
			if rowsAffected > 0 {
				continue
			}

			// Provide a specific reason by reading the row.
			getParams := outpointToGetParams(outpoint)
			row, err := qtx.GetVTXO(ctx, getParams)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("unknown vtxo: %v", outpoint)
			}
			if err != nil {
				return fmt.Errorf("get vtxo %v after lock "+
					"failed: %w", outpoint, err)
			}

			// Idempotent case.
			if row.Status == string(vtxo.StatusInFlight) &&
				row.LockOwnerKind.Valid &&
				row.LockOwnerKind.String == ownerKind &&
				bytes.Equal(row.LockOwnerID, ownerID) {

				continue
			}

			// If the vtxo is already in-flight by another owner,
			// expose it as a typed ErrLocked.
			//
			// This lets callers distinguish conflicts
			// from other failures.
			lockKindDiff := row.LockOwnerKind.String != ownerKind
			lockIDDiff := !bytes.Equal(row.LockOwnerID, ownerID)
			if row.Status == string(vtxo.StatusInFlight) &&
				row.LockOwnerKind.Valid &&
				len(row.LockOwnerID) > 0 &&
				(lockKindDiff || lockIDDiff) {

				existingOwner := lockOwnerToValue(
					row.LockOwnerKind.String,
					row.LockOwnerID,
				)

				return &vtxo.ErrLocked{
					Outpoint: outpoint,
					Owner:    existingOwner,
				}
			}

			return fmt.Errorf("vtxo %v not lockable (%s)", outpoint,
				row.Status)
		}

		return nil
	})
}

// UnlockMany releases locks held by owner.
func (l *VTXOLockerDB) UnlockMany(ctx context.Context,
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

	return l.tx.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		for _, outpoint := range outpoints {
			unlockParams := sqlc.UnlockVTXOParams{
				OutpointHash:  outpoint.Hash[:],
				OutpointIndex: int32(outpoint.Index),
				LockOwnerKind: sql.NullString{
					String: ownerKind,
					Valid:  true,
				},
				LockOwnerID: ownerID,
			}

			rowsAffected, err := qtx.UnlockVTXO(ctx, unlockParams)
			if err != nil {
				return fmt.Errorf("unlock vtxo %v: %w",
					outpoint, err)
			}

			if rowsAffected > 0 {
				continue
			}

			// No-op cases require checking the current row.
			// (Or confirming the row does not exist.)
			getParams := outpointToGetParams(outpoint)
			row, err := qtx.GetVTXO(ctx, getParams)
			if errors.Is(err, sql.ErrNoRows) {
				// Unknown outpoint: treat as unlocked (no-op),
				// matching the in-memory semantics.
				continue
			}
			if err != nil {
				return fmt.Errorf("get vtxo %v after unlock "+
					"failed: %w", outpoint, err)
			}

			// Not locked: no-op.
			if row.Status != string(vtxo.StatusInFlight) ||
				!row.LockOwnerKind.Valid ||
				len(row.LockOwnerID) == 0 {

				continue
			}

			// Locked by another owner.
			// Fail without modifying anything.
			if row.LockOwnerKind.String != ownerKind ||
				!bytes.Equal(row.LockOwnerID, ownerID) {

				existingOwner := lockOwnerToValue(
					row.LockOwnerKind.String,
					row.LockOwnerID,
				)

				return &vtxo.ErrNotOwner{
					Outpoint: outpoint,
					Owner:    existingOwner,
					Attempt:  owner,
				}
			}

			// If we got here, the row looks unlockable
			// but UnlockVTXO did not match it.
			//
			// Surface a generic error.
			return fmt.Errorf("failed to unlock vtxo %v", outpoint)
		}

		return nil
	})
}

var _ vtxo.Locker = (*VTXOLockerDB)(nil)

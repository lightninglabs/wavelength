package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightninglabs/darepo/rounds"
	vtxostate "github.com/lightninglabs/darepo/vtxo"
)

// VTXOStoreDB implements rounds.VTXOStore using sqlc-generated queries.
type VTXOStoreDB struct {
	*TransactionExecutor[*sqlc.Queries]

	q *sqlc.Queries
}

// NewVTXOStoreDB creates a new VTXOStoreDB from a Store.
func NewVTXOStoreDB(store *Store) *VTXOStoreDB {
	txExec := NewTransactionExecutor(
		store, func(tx *sql.Tx) *sqlc.Queries {
			return store.WithTx(tx)
		}, store.log,
	)

	return &VTXOStoreDB{
		TransactionExecutor: txExec,
		q:                   store.Queries,
	}
}

// PersistVTXOs saves a batch of newly created VTXOs to storage. These VTXOs
// are in unconfirmed state until the commitment transaction is confirmed
// on-chain.
func (v *VTXOStoreDB) PersistVTXOs(ctx context.Context,
	vtxos []*rounds.VTXO) error {

	return v.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		for _, vtxo := range vtxos {
			// Serialize descriptor fields.
			cosignerKeyBytes := vtxo.Descriptor.CoSignerKey.
				SerializeCompressed()

			err := qtx.InsertVTXO(ctx, sqlc.InsertVTXOParams{
				OutpointHash:  vtxo.Outpoint.Hash[:],
				OutpointIndex: int32(vtxo.Outpoint.Index),
				RoundID:       vtxo.RoundID[:],
				BatchOutputIndex: sql.NullInt32{
					Int32: int32(vtxo.BatchOutputIndex),
					Valid: true,
				},
				Amount:   int64(vtxo.Descriptor.Amount),
				PkScript: vtxo.Descriptor.PkScript,
				PolicyTemplate: bytes.Clone(
					vtxo.Descriptor.PolicyTemplate,
				),
				CosignerKey: cosignerKeyBytes,
				Status:      string(vtxo.Status),
			})
			if err != nil {
				return fmt.Errorf("insert vtxo %v: %w",
					vtxo.Outpoint, err)
			}
		}

		return nil
	})
}

// MarkVTXOsLive updates the status of all VTXOs for a given round to "live"
// after the commitment transaction has been confirmed.
func (v *VTXOStoreDB) MarkVTXOsLive(ctx context.Context,
	roundID rounds.RoundID) error {

	return v.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.UpdateVTXOsLiveByRound(ctx, roundID[:])
	})
}

// MarkVTXOsExpired marks the given VTXOs as expired. This is called
// when the operator sweeps an expired batch, making all VTXOs in the
// presigned tree unspendable. Only transitions VTXOs in live, pending,
// or in_flight states — VTXOs already in a terminal state (forfeited,
// spent) are left unchanged.
func (v *VTXOStoreDB) MarkVTXOsExpired(ctx context.Context,
	outpoints []wire.OutPoint) error {

	if len(outpoints) == 0 {
		return nil
	}

	return v.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		for _, op := range outpoints {
			_, err := q.MarkVTXOExpired(
				ctx, sqlc.MarkVTXOExpiredParams{
					OutpointHash:  op.Hash[:],
					OutpointIndex: int32(op.Index),
				},
			)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// MarkVTXOForfeit marks a VTXO as forfeited and stores the forfeit metadata.
func (v *VTXOStoreDB) MarkVTXOForfeit(ctx context.Context,
	outpoint wire.OutPoint, info *rounds.ForfeitInfo) error {

	if info == nil {
		return fmt.Errorf("forfeit info is nil")
	}

	return v.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		// Mark the VTXO forfeited and clear lock metadata.
		// This keeps status/lock CHECK constraints satisfied.
		affected, err := qtx.MarkVTXOForfeited(ctx,
			sqlc.MarkVTXOForfeitedParams{
				OutpointHash:  outpoint.Hash[:],
				OutpointIndex: int32(outpoint.Index),
			})
		if err != nil {
			return fmt.Errorf("mark vtxo forfeited: %w", err)
		}
		if affected == 0 {
			return fmt.Errorf("vtxo %v not found", outpoint)
		}

		// Serialize forfeit transaction.
		var buf bytes.Buffer
		if err := info.ForfeitTx.Serialize(&buf); err != nil {
			return fmt.Errorf("serialize forfeit tx: %w", err)
		}
		forfeitTxBytes := buf.Bytes()

		err = qtx.UpsertRoundForfeitInfo(
			ctx, sqlc.UpsertRoundForfeitInfoParams{
				RoundID:       info.RoundID[:],
				OutpointHash:  outpoint.Hash[:],
				OutpointIndex: int32(outpoint.Index),
				ForfeitTx:     forfeitTxBytes,
				ConnectorOutputIndex: int32(
					info.ConnectorOutputIndex,
				),
				LeafIndex: int32(info.LeafIndex),
			},
		)
		if err != nil {
			return fmt.Errorf("insert forfeit info: %w", err)
		}

		return nil
	})
}

// MarkVTXOUnrolledByClient marks a live VTXO as revealed by a recognized
// client-owned on-chain path. Such a VTXO must no longer be used for future
// OOR or forfeit admission.
func (v *VTXOStoreDB) MarkVTXOUnrolledByClient(ctx context.Context,
	outpoint wire.OutPoint) error {

	return v.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		affected, err := qtx.MarkVTXOUnrolledByClient(ctx,
			sqlc.MarkVTXOUnrolledByClientParams{
				OutpointHash:  outpoint.Hash[:],
				OutpointIndex: int32(outpoint.Index),
			},
		)
		if err != nil {
			return fmt.Errorf("mark vtxo "+
				"unrolled_by_client: %w", err)
		}
		if affected == 0 {
			// The SQL WHERE clause requires both existence
			// and status = 'live'. A zero-row result means
			// the VTXO either does not exist or has already
			// transitioned to a terminal state.
			return fmt.Errorf("vtxo %v not live "+
				"(missing or already terminal)",
				outpoint)
		}

		return nil
	})
}

// GetVTXO retrieves a VTXO by its outpoint. Returns nil and no error if the
// VTXO doesn't exist.
func (v *VTXOStoreDB) GetVTXO(ctx context.Context,
	outpoint wire.OutPoint) (*rounds.VTXO, error) {

	var result *rounds.VTXO

	err := v.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		row, err := q.GetVTXO(ctx, sqlc.GetVTXOParams{
			OutpointHash:  outpoint.Hash[:],
			OutpointIndex: int32(outpoint.Index),
		})
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("get vtxo: %w", err)
		}

		// Reconstruct descriptor.
		cosignerKey, err := btcec.ParsePubKey(row.CosignerKey)
		if err != nil {
			return fmt.Errorf("parse cosigner key: %w", err)
		}

		descriptor := &tree.VTXODescriptor{
			PolicyTemplate: bytes.Clone(row.PolicyTemplate),
			PkScript:       row.PkScript,
			Amount:         btcutil.Amount(row.Amount),
			CoSignerKey:    cosignerKey,
		}

		// Reconstruct round ID.
		var roundID rounds.RoundID
		if len(row.RoundID) > 0 {
			copy(roundID[:], row.RoundID)
		}

		// Reconstruct outpoint.
		var vtxoOutpoint wire.OutPoint
		copy(vtxoOutpoint.Hash[:], row.OutpointHash)
		vtxoOutpoint.Index = uint32(row.OutpointIndex)

		result = &rounds.VTXO{
			Outpoint: vtxoOutpoint,
			RoundID:  roundID,
			BatchOutputIndex: func() int {
				if row.BatchOutputIndex.Valid {
					return int(row.BatchOutputIndex.Int32)
				}

				return 0
			}(),
			Descriptor: descriptor,
			Status:     rounds.VTXOStatus(row.Status),
		}

		return nil
	})

	return result, err
}

// GetForfeitInfo retrieves forfeit metadata for a VTXO. Returns nil and no
// error if the forfeit info doesn't exist.
func (v *VTXOStoreDB) GetForfeitInfo(ctx context.Context,
	outpoint wire.OutPoint) (*rounds.ForfeitInfo, error) {

	var result *rounds.ForfeitInfo

	err := v.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.GetRoundForfeitInfoByOutpoint(ctx,
			sqlc.GetRoundForfeitInfoByOutpointParams{
				OutpointHash:  outpoint.Hash[:],
				OutpointIndex: int32(outpoint.Index),
			})
		if err != nil {
			return fmt.Errorf("get forfeit info: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}
		if len(rows) > 1 {
			return fmt.Errorf("multiple forfeit infos for %v",
				outpoint)
		}

		row := rows[0]
		var roundID rounds.RoundID
		copy(roundID[:], row.RoundID)

		var forfeitTx *wire.MsgTx
		if len(row.ForfeitTx) > 0 {
			forfeitTx = &wire.MsgTx{}
			if err := forfeitTx.Deserialize(bytes.NewReader(
				row.ForfeitTx,
			)); err != nil {
				return fmt.Errorf("deserialize forfeit tx: %w",
					err)
			}
		}

		result = &rounds.ForfeitInfo{
			RoundID:              roundID,
			ConnectorOutputIndex: int(row.ConnectorOutputIndex),
			LeafIndex:            int(row.LeafIndex),
			ForfeitTx:            forfeitTx,
		}

		return nil
	})

	return result, err
}

// LockVTXO locks VTXOs for forfeit in the specified round. This prevents the
// VTXOs from being forfeited in another round concurrently. The call should
// fail if any outpoint is already locked by another round.
func (v *VTXOStoreDB) LockVTXO(ctx context.Context, roundID rounds.RoundID,
	outpoints ...wire.OutPoint) error {

	return v.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		ownerKind, ownerID, err := lockOwnerFromRoundID(roundID[:])
		if err != nil {
			return err
		}

		for _, outpoint := range outpoints {
			rowsAffected, err := qtx.LockVTXO(ctx,
				sqlc.LockVTXOParams{
					OutpointHash:  outpoint.Hash[:],
					OutpointIndex: int32(outpoint.Index),
					LockOwnerKind: sql.NullString{
						String: ownerKind,
						Valid:  true,
					},
					LockOwnerID: ownerID,
				})
			if err != nil {
				return fmt.Errorf("lock vtxo %v: %w",
					outpoint, err)
			}

			if rowsAffected == 0 {
				// To provide a more specific error, we query
				// the VTXO. This is outside the critical path,
				// so it's fine.
				opIdx := int32(outpoint.Index)
				vtxo, err := qtx.GetVTXO(
					ctx, sqlc.GetVTXOParams{
						OutpointHash:  outpoint.Hash[:],
						OutpointIndex: opIdx,
					},
				)
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("vtxo %v not found",
						outpoint)
				} else if err != nil {
					return fmt.Errorf(
						"failed to get vtxo %v after "+
							"lock failed: %w",
						outpoint, err,
					)
				}

				// Idempotent case: VTXO already locked by
				// this round.
				lockKindMatches := vtxo.LockOwnerKind.Valid &&
					vtxo.LockOwnerKind.String == ownerKind
				lockIDMatches := len(vtxo.LockOwnerID) > 0 &&
					bytes.Equal(vtxo.LockOwnerID, ownerID)
				if vtxo.Status ==
					string(vtxostate.StatusInFlight) &&
					lockKindMatches &&
					lockIDMatches {

					// Already locked by us, idempotent
					// success.
					continue
				}

				if vtxo.Status != string(vtxostate.StatusLive) {
					return fmt.Errorf(
						"vtxo %v not live, status: %s",
						outpoint, vtxo.Status,
					)
				}

				if len(vtxo.LockOwnerID) > 0 {
					return fmt.Errorf(
						"vtxo %v locked by another "+
							"round",
						outpoint,
					)
				}

				return fmt.Errorf(
					"failed to lock vtxo %v for unknown "+
						"reasons",
					outpoint,
				)
			}
		}

		return nil
	})
}

// UnlockVTXO releases the lock on VTXOs. Only the round that locked the VTXOs
// can unlock them.
func (v *VTXOStoreDB) UnlockVTXO(ctx context.Context, roundID rounds.RoundID,
	outpoints ...wire.OutPoint) error {

	return v.ExecTx(ctx, WriteTxOption(), func(qtx *sqlc.Queries) error {
		ownerKind, ownerID, err := lockOwnerFromRoundID(roundID[:])
		if err != nil {
			return err
		}

		for _, outpoint := range outpoints {
			rowsAffected, err := qtx.UnlockVTXO(ctx,
				sqlc.UnlockVTXOParams{
					OutpointHash:  outpoint.Hash[:],
					OutpointIndex: int32(outpoint.Index),
					LockOwnerKind: sql.NullString{
						String: ownerKind,
						Valid:  true,
					},
					LockOwnerID: ownerID,
				})
			if err != nil {
				return fmt.Errorf("unlock vtxo %v: %w",
					outpoint, err)
			}

			if rowsAffected == 0 {
				// To provide a more specific error, we query
				// the VTXO.
				opIdx := int32(outpoint.Index)
				vtxo, err := qtx.GetVTXO(
					ctx, sqlc.GetVTXOParams{
						OutpointHash:  outpoint.Hash[:],
						OutpointIndex: opIdx,
					},
				)
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("vtxo %v not found",
						outpoint)
				} else if err != nil {
					return fmt.Errorf(
						"failed to get vtxo %v after "+
							"unlock failed: %w",
						outpoint, err,
					)
				}

				if vtxo.Status !=
					string(vtxostate.StatusInFlight) {

					return fmt.Errorf(
						"vtxo %v not locked: %s",
						outpoint,
						vtxo.Status,
					)
				}

				if len(vtxo.LockOwnerID) == 0 ||
					!vtxo.LockOwnerKind.Valid {

					return fmt.Errorf(
						"vtxo %v has no "+
							"lock owner",
						outpoint,
					)
				}

				lockOwnerKindMatches :=
					vtxo.LockOwnerKind.String == ownerKind
				lockOwnerIDMatches := bytes.Equal(
					vtxo.LockOwnerID, ownerID,
				)
				lockOwnerMatches := lockOwnerKindMatches &&
					lockOwnerIDMatches
				if !lockOwnerMatches {
					return fmt.Errorf(
						"vtxo %v locked by different "+
							"round",
						outpoint,
					)
				}

				return fmt.Errorf(
					"failed to unlock vtxo %v for "+
						"unknown reasons",
					outpoint,
				)
			}
		}

		return nil
	})
}

// UnlockStaleVTXOs releases locks on VTXOs that are locked by rounds not in
// the active round list. This is used during startup to clean up locks from
// rounds that were abandoned before completion.
func (v *VTXOStoreDB) UnlockStaleVTXOs(ctx context.Context,
	activeRoundIDs []rounds.RoundID) error {

	return v.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		if len(activeRoundIDs) == 0 {
			_, err := q.UnlockAllLockedVTXOs(ctx)

			return err
		}

		// Convert RoundIDs to byte slices for the query.
		pendingRoundIds := make([][]byte, len(activeRoundIDs))
		for i, roundID := range activeRoundIDs {
			pendingRoundIds[i] = roundID[:]
		}

		switch q.Backend() {
		case sqlc.BackendTypeSqlite:
			_, err := q.UnlockStaleVTXOsSqlite(
				ctx, pendingRoundIds,
			)

			return err

		case sqlc.BackendTypePostgres:
			_, err := q.UnlockStaleVTXOsPostgres(
				ctx, pendingRoundIds,
			)

			return err

		default:
			return fmt.Errorf(
				"unknown backend: %v", q.Backend(),
			)
		}
	})
}

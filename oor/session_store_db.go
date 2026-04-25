package oor

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
)

// DBSessionStore is a DB-backed SessionStore implementation.
type DBSessionStore struct {
	tx *db.TransactionExecutor[*sqlc.Queries]

	clock clock.Clock

	log btclog.Logger

	operatorKey keychain.KeyDescriptor
}

// NewDBSessionStore creates a new DB-backed session store.
func NewDBSessionStore(dbq db.BatchedQuerier, clk clock.Clock,
	log btclog.Logger) *DBSessionStore {

	if log == nil {
		log = btclog.Disabled
	}

	txExec := db.NewTransactionExecutor[*sqlc.Queries](
		dbq,
		func(tx *sql.Tx) *sqlc.Queries {
			return sqlc.New(tx)
		},
		log,
	)

	return &DBSessionStore{
		tx:    txExec,
		clock: clk,
		log:   log,
	}
}

// SetOperatorKey configures the operator key descriptor used when finalized
// recipient outputs are materialized as real Ark VTXOs.
func (s *DBSessionStore) SetOperatorKey(operatorKey keychain.KeyDescriptor) {
	s.operatorKey = operatorKey
}

// UpsertCoSigned persists the CoSigned point-of-no-return snapshot.
//
// The session row stores the Ark PSBT directly (no opaque TLV blob), and
// checkpoint rows store per-input outpoints alongside co-signed PSBT bytes.
// The UNIQUE(input_txid, input_vout) constraint on oor_checkpoints prevents
// two sessions from claiming the same VTXO input.
func (s *DBSessionStore) UpsertCoSigned(ctx context.Context,
	sessionID SessionID, inputs []wire.OutPoint, arkPSBT *psbt.Packet,
	coSignedCheckpointPSBTs []*psbt.Packet, expiresAt time.Time) error {

	if sessionID == (SessionID{}) {
		return fmt.Errorf("session id must be provided")
	}

	if arkPSBT == nil {
		return fmt.Errorf("ark psbt must be provided")
	}

	arkBytes, err := serializePSBT(arkPSBT)
	if err != nil {
		return err
	}

	return s.tx.ExecTx(ctx, db.WriteTxOption(),
		func(q *sqlc.Queries) error {
			return s.upsertCoSignedSnapshot(
				ctx, q, sessionID, arkBytes, inputs,
				coSignedCheckpointPSBTs, expiresAt,
			)
		},
	)
}

// UpsertCoSignedAndMarkInFlight persists CoSigned state and marks all claimed
// inputs in-flight within one DB transaction.
func (s *DBSessionStore) UpsertCoSignedAndMarkInFlight(ctx context.Context,
	sessionID SessionID, inputs []wire.OutPoint, arkPSBT *psbt.Packet,
	coSignedCheckpointPSBTs []*psbt.Packet, expiresAt time.Time,
	owner vtxo.LockOwner) error {

	if owner == "" {
		return fmt.Errorf("owner must be provided")
	}

	if sessionID == (SessionID{}) {
		return fmt.Errorf("session id must be provided")
	}

	if arkPSBT == nil {
		return fmt.Errorf("ark psbt must be provided")
	}

	arkBytes, err := serializePSBT(arkPSBT)
	if err != nil {
		return err
	}

	ownerKind, ownerID, err := ownerColumns(owner)
	if err != nil {
		return err
	}

	return s.tx.ExecTx(ctx, db.WriteTxOption(),
		func(q *sqlc.Queries) error {
			err := s.upsertCoSignedSnapshot(
				ctx, q, sessionID, arkBytes, inputs,
				coSignedCheckpointPSBTs, expiresAt,
			)
			if err != nil {
				return err
			}

			return lockInputsInFlight(
				ctx, q, inputs, ownerKind, ownerID,
			)
		},
	)
}

// ApplyFinalize transitions the session from cosigned to awaiting_notify,
// updates checkpoint PSBTs with finalized bytes, and sets finalized_at.
//
// Idempotency semantics:
//   - If the session is cosigned, performs the transition.
//   - If the session is already awaiting_notify, verifies payload equality
//     (all checkpoint PSBTs must match) and returns success only if identical.
//   - If the session is finalized, returns success (past this stage).
//   - Otherwise returns an error.
func (s *DBSessionStore) ApplyFinalize(ctx context.Context,
	sessionID SessionID,
	finalCheckpointPSBTs []*psbt.Packet) error {

	if sessionID == (SessionID{}) {
		return fmt.Errorf("session id must be provided")
	}

	if len(finalCheckpointPSBTs) == 0 {
		return fmt.Errorf("final checkpoints must be provided")
	}

	return s.tx.ExecTx(ctx, db.WriteTxOption(),
		func(q *sqlc.Queries) error {
			_, err := s.applyFinalizeTx(
				ctx, q, sessionID, finalCheckpointPSBTs,
			)

			return err
		},
	)
}

// ApplyFinalizeAndMaterialize atomically persists the finalized checkpoint
// set, marks the consumed inputs spent, and materializes recipient output
// VTXOs.
func (s *DBSessionStore) ApplyFinalizeAndMaterialize(ctx context.Context,
	sessionID SessionID, inputs []wire.OutPoint,
	finalCheckpointPSBTs []*psbt.Packet,
	outputRecords []*vtxo.Record) error {

	if sessionID == (SessionID{}) {
		return fmt.Errorf("session id must be provided")
	}

	if len(finalCheckpointPSBTs) == 0 {
		return fmt.Errorf("final checkpoints must be provided")
	}

	return s.tx.ExecTx(ctx, db.WriteTxOption(),
		func(q *sqlc.Queries) error {
			state, err := s.applyFinalizeTx(
				ctx, q, sessionID, finalCheckpointPSBTs,
			)
			if err != nil {
				return err
			}

			switch state {
			case oorStateAwaitingNotify:
				err := db.MarkVTXORecordsSpentTx(
					ctx, q, inputs,
				)
				if err != nil {
					return err
				}

				// Compute the inherited batch-expiry every
				// materialized output row should carry. An
				// OOR output's spendable lifetime is bounded
				// by the shortest-lived input in its lineage,
				// so we take min(parent.batch_expiry) as
				// resolved by GetVTXOWithRoundExpiry (which
				// COALESCEs persisted + round-join sources).
				// If every parent reports 0 we fall back to
				// 0, matching the pre-fix behavior.
				inheritedExpiry, err := minParentBatchExpiry(
					ctx, q, inputs,
				)
				if err != nil {
					return err
				}

				for _, record := range outputRecords {
					err := db.CreateVTXORecordTx(
						ctx, q, record,
						s.clock.Now().Unix(),
						s.operatorKey,
						inheritedExpiry,
					)
					if err != nil {
						return err
					}
				}

				return nil

			case oorStateFinalized:
				return nil

			default:
				return fmt.Errorf("session %s in unexpected "+
					"state after finalize: %s", sessionID,
					state)
			}
		},
	)
}

// MarkNotified transitions the session from awaiting_notify to finalized.
// Idempotent: if already finalized, returns success.
func (s *DBSessionStore) MarkNotified(ctx context.Context,
	sessionID SessionID) error {

	if sessionID == (SessionID{}) {
		return fmt.Errorf("session id must be provided")
	}

	return s.tx.ExecTx(ctx, db.WriteTxOption(),
		func(q *sqlc.Queries) error {
			id := sessionIDBytes(sessionID)
			now := s.clock.Now().UnixNano()

			affected, err := q.MarkOORSessionNotified(ctx,
				sqlc.MarkOORSessionNotifiedParams{
					SessionID: id,
					UpdatedAt: now,
				},
			)
			if err != nil {
				return err
			}

			if affected > 0 {
				return nil
			}

			// Check if already finalized for idempotency.
			row, err := q.GetOORSession(ctx, id)
			if err != nil {
				return fmt.Errorf("session not found: %s",
					sessionID)
			}

			if sessionState(row.State) == oorStateFinalized {
				return nil
			}

			return fmt.Errorf("session %s in unexpected "+
				"state for notify: %s",
				sessionID, row.State)
		},
	)
}

// GetSessionState returns the persisted lifecycle state for a session.
//
// If no session row exists, found is false and err is nil.
func (s *DBSessionStore) GetSessionState(ctx context.Context,
	sessionID SessionID) (sessionState, bool, error) {

	if sessionID == (SessionID{}) {
		return "", false, fmt.Errorf("session id must be provided")
	}

	var out sessionState

	err := s.tx.ExecTx(ctx, db.ReadTxOption(),
		func(q *sqlc.Queries) error {
			row, err := q.GetOORSession(
				ctx, sessionIDBytes(sessionID),
			)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}

			out = sessionState(row.State)

			return nil
		},
	)
	if err != nil {
		return "", false, err
	}

	if out == "" {
		return "", false, nil
	}

	return out, true, nil
}

// LoadActiveSessions returns durable snapshots for sessions that require
// processing after restart (state = cosigned or awaiting_notify).
func (s *DBSessionStore) LoadActiveSessions(ctx context.Context) (
	[]*ActiveSession, error) {

	var sessions []*ActiveSession

	err := s.tx.ExecTx(ctx, db.ReadTxOption(),
		func(q *sqlc.Queries) error {
			rows, err := q.ListActiveOORSessions(ctx)
			if err != nil {
				return err
			}

			for _, row := range rows {
				session, err := s.loadActiveSession(
					ctx, q, row,
				)
				if err != nil {
					return err
				}

				sessions = append(sessions, session)
			}

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return sessions, nil
}

// LoadFinalizedPackage returns the canonical finalized package for a session.
func (s *DBSessionStore) LoadFinalizedPackage(ctx context.Context,
	sessionID SessionID) (*FinalizedPackage, error) {

	if sessionID == (SessionID{}) {
		return nil, fmt.Errorf("session id must be provided")
	}

	var out *FinalizedPackage

	err := s.tx.ExecTx(ctx, db.ReadTxOption(),
		func(q *sqlc.Queries) error {
			id := sessionIDBytes(sessionID)

			sessionRow, err := q.GetOORSession(ctx, id)
			if err != nil {
				return err
			}

			// Accept both awaiting_notify and finalized since
			// the caller may fetch the package before or after
			// notification.
			switch sessionState(sessionRow.State) {
			case oorStateAwaitingNotify, oorStateFinalized:

			default:
				return fmt.Errorf(
					"session not finalized: %s",
					sessionID,
				)
			}

			arkPSBT, err := deserializePSBT(sessionRow.ArkPsbt)
			if err != nil {
				return err
			}

			checkpointRows, err := q.ListOORCheckpoints(
				ctx, int32(sessionRow.ID),
			)
			if err != nil {
				return err
			}

			checkpoints := make(
				[]*psbt.Packet, 0, len(checkpointRows),
			)
			for i := range checkpointRows {
				pkt, err := deserializePSBT(
					checkpointRows[i].CheckpointPsbt,
				)
				if err != nil {
					return err
				}

				checkpoints = append(checkpoints, pkt)
			}

			out = &FinalizedPackage{
				ArkPSBT:              arkPSBT,
				FinalCheckpointPSBTs: checkpoints,
			}

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return out, nil
}

// LoadCheckpointTxByInput returns the broadcastable finalized checkpoint
// transaction that spends input, if one exists.
func (s *DBSessionStore) LoadCheckpointTxByInput(ctx context.Context,
	input wire.OutPoint) (*wire.MsgTx, bool, error) {

	var tx *wire.MsgTx

	err := s.tx.ExecTx(ctx, db.ReadTxOption(),
		func(q *sqlc.Queries) error {
			params := sqlc.GetBroadcastableOORCheckpointByInputParams{ //nolint:ll
				InputTxid: input.Hash[:],
				InputVout: int32(input.Index),
			}

			checkpointPSBT, err := q.GetBroadcastableOORCheckpointByInput( //nolint:ll
				ctx, params,
			)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}

			pkt, err := deserializePSBT(checkpointPSBT)
			if err != nil {
				return err
			}

			checkpointTx, err := psbt.Extract(pkt)
			if err != nil {
				return fmt.Errorf("extract "+
					"checkpoint tx for %s: %w",
					input, err)
			}

			tx = checkpointTx

			return nil
		},
	)
	if err != nil {
		return nil, false, err
	}

	if tx == nil {
		return nil, false, nil
	}

	return tx, true, nil
}

const (
	ownerKindRound = "round"
	ownerKindOOR   = "oor"
)

// upsertCoSignedSnapshot persists the session and checkpoint rows for a
// CoSigned state using the active transaction.
func (s *DBSessionStore) upsertCoSignedSnapshot(ctx context.Context,
	q *sqlc.Queries, sessionID SessionID, arkBytes []byte,
	inputs []wire.OutPoint, coSignedCheckpointPSBTs []*psbt.Packet,
	expiresAt time.Time) error {

	now := s.clock.Now()
	id := sessionIDBytes(sessionID)

	existing, err := q.GetOORSession(ctx, id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// New session row; continue with upsert.

	case err != nil:
		return err

	default:
		switch sessionState(existing.State) {
		case oorStateFinalized, oorStateAwaitingNotify:
			return fmt.Errorf("cannot upsert co-signed session %s "+
				"from state %s", sessionID, existing.State)

		case oorStateCoSigned:
			if !bytes.Equal(existing.ArkPsbt, arkBytes) {
				return fmt.Errorf("co-signed ark psbt mismatch")
			}

		default:
			return fmt.Errorf("session %s in unexpected state: %s",
				sessionID, existing.State)
		}
	}

	dbID, err := q.UpsertOORSession(ctx, sqlc.UpsertOORSessionParams{
		SessionID:   id,
		State:       string(oorStateCoSigned),
		ArkPsbt:     arkBytes,
		CreatedAt:   now.UnixNano(),
		UpdatedAt:   now.UnixNano(),
		ExpiresAt:   expiresAt.UnixNano(),
		FinalizedAt: sql.NullInt64{},
	})
	if err != nil {
		return err
	}

	// Delete existing checkpoints before re-inserting (idempotent
	// upsert).
	err = q.DeleteOORCheckpoints(ctx, int32(dbID))
	if err != nil {
		return err
	}

	for i, op := range inputs {
		var checkpointBytes []byte
		if i < len(coSignedCheckpointPSBTs) {
			checkpointBytes, err = serializePSBT(
				coSignedCheckpointPSBTs[i],
			)
			if err != nil {
				return err
			}
		}

		err = q.UpsertOORCheckpoint(ctx,
			sqlc.UpsertOORCheckpointParams{
				SessionDbID:     int32(dbID),
				CheckpointIndex: int32(i),
				InputTxid:       op.Hash[:],
				InputVout:       int32(op.Index),
				CheckpointPsbt:  checkpointBytes,
			},
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// applyFinalizeTx transitions the session to its post-finalize state using the
// active transaction.
func (s *DBSessionStore) applyFinalizeTx(ctx context.Context,
	q *sqlc.Queries, sessionID SessionID,
	finalCheckpointPSBTs []*psbt.Packet) (sessionState, error) {

	id := sessionIDBytes(sessionID)
	now := s.clock.Now().UnixNano()

	// Try the cosigned -> awaiting_notify transition first.
	affected, err := q.ApplyFinalizeOORSession(ctx,
		sqlc.ApplyFinalizeOORSessionParams{
			SessionID: id,
			UpdatedAt: now,
			FinalizedAt: sql.NullInt64{
				Int64: now,
				Valid: true,
			},
		},
	)
	if err != nil {
		return "", err
	}

	if affected > 0 {
		err := s.updateCheckpointPSBTs(
			ctx, q, id, finalCheckpointPSBTs,
		)
		if err != nil {
			return "", err
		}

		return oorStateAwaitingNotify, nil
	}

	row, err := q.GetOORSession(ctx, id)
	if err != nil {
		return "", fmt.Errorf("session not found: %s", sessionID)
	}

	switch state := sessionState(row.State); state {
	case oorStateAwaitingNotify:
		err := s.verifyCheckpointEquality(
			ctx, q, row.ID, finalCheckpointPSBTs,
		)
		if err != nil {
			return "", err
		}

		return state, nil

	case oorStateFinalized:
		return state, nil

	default:
		return "", fmt.Errorf("session %s in unexpected state: %s",
			sessionID, row.State)
	}
}

// updateCheckpointPSBTs overwrites checkpoint PSBT bytes for an existing
// session.
func (s *DBSessionStore) updateCheckpointPSBTs(ctx context.Context,
	q *sqlc.Queries, sessionIDBytes []byte,
	finalCheckpointPSBTs []*psbt.Packet) error {

	row, err := q.GetOORSession(ctx, sessionIDBytes)
	if err != nil {
		return err
	}

	checkpointRows, err := q.ListOORCheckpoints(
		ctx, int32(row.ID),
	)
	if err != nil {
		return err
	}

	if len(checkpointRows) != len(finalCheckpointPSBTs) {
		return fmt.Errorf(
			"checkpoint count mismatch: have %d, got %d",
			len(checkpointRows), len(finalCheckpointPSBTs),
		)
	}

	for i, cpRow := range checkpointRows {
		s.logCheckpointPersistSummary(
			ctx, "Persisting finalized checkpoint",
			i, finalCheckpointPSBTs[i],
		)

		checkpointBytes, err := serializePSBT(
			finalCheckpointPSBTs[i],
		)
		if err != nil {
			return err
		}

		err = q.UpsertOORCheckpoint(ctx,
			sqlc.UpsertOORCheckpointParams{
				SessionDbID:     cpRow.SessionDbID,
				CheckpointIndex: cpRow.CheckpointIndex,
				InputTxid:       cpRow.InputTxid,
				InputVout:       cpRow.InputVout,
				CheckpointPsbt:  checkpointBytes,
			},
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// logCheckpointPersistSummary emits a compact summary of the finalized
// checkpoint input metadata right before it is serialized into the operator DB.
func (s *DBSessionStore) logCheckpointPersistSummary(ctx context.Context,
	msg string, checkpointIndex int, checkpoint *psbt.Packet) {

	if s.log == nil || checkpoint == nil || len(checkpoint.Inputs) == 0 {
		return
	}

	in := checkpoint.Inputs[0]
	s.log.DebugS(ctx, msg,
		slog.Int("checkpoint_index", checkpointIndex),
		slog.Int("final_witness_len", len(in.FinalScriptWitness)),
		slog.Int("taproot_sig_count",
			len(in.TaprootScriptSpendSig)),
		slog.Int("taproot_leaf_count",
			len(in.TaprootLeafScript)),
		slog.Int("unknown_count", len(in.Unknowns)),
	)
}

// verifyCheckpointEquality checks that the given finalized checkpoint PSBTs
// match the already-persisted checkpoint PSBTs byte-for-byte.
func (s *DBSessionStore) verifyCheckpointEquality(ctx context.Context,
	q *sqlc.Queries, sessionDBID int64,
	finalCheckpointPSBTs []*psbt.Packet) error {

	existing, err := q.ListOORCheckpoints(ctx, int32(sessionDBID))
	if err != nil {
		return err
	}

	if len(existing) != len(finalCheckpointPSBTs) {
		return fmt.Errorf("idempotent finalize failed: checkpoint "+
			"count mismatch (have %d, got %d)",
			len(existing), len(finalCheckpointPSBTs))
	}

	for i, cp := range existing {
		newBytes, err := serializePSBT(finalCheckpointPSBTs[i])
		if err != nil {
			return err
		}

		if !bytes.Equal(cp.CheckpointPsbt, newBytes) {
			return fmt.Errorf("idempotent finalize failed: "+
				"checkpoint %d payload mismatch", i)
		}
	}

	return nil
}

// loadActiveSession converts a DB row into an ActiveSession by loading
// associated checkpoint rows and reconstructing input outpoints.
func (s *DBSessionStore) loadActiveSession(ctx context.Context,
	q *sqlc.Queries, row sqlc.OorSession) (*ActiveSession, error) {

	idHash, err := chainhash.NewHash(row.SessionID)
	if err != nil {
		return nil, fmt.Errorf("invalid session id: %w", err)
	}

	arkPSBT, err := deserializePSBT(row.ArkPsbt)
	if err != nil {
		return nil, err
	}

	checkpointRows, err := q.ListOORCheckpoints(
		ctx, int32(row.ID),
	)
	if err != nil {
		return nil, err
	}

	inputs := make([]wire.OutPoint, 0, len(checkpointRows))
	checkpoints := make([]*psbt.Packet, 0, len(checkpointRows))

	for _, cpRow := range checkpointRows {
		txid, err := chainhash.NewHash(cpRow.InputTxid)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid input txid: %w", err,
			)
		}

		inputs = append(inputs, wire.OutPoint{
			Hash:  *txid,
			Index: uint32(cpRow.InputVout),
		})

		pkt, err := deserializePSBT(cpRow.CheckpointPsbt)
		if err != nil {
			return nil, err
		}

		checkpoints = append(checkpoints, pkt)
	}

	return &ActiveSession{
		SessionID:       SessionID(*idHash),
		State:           sessionState(row.State),
		Inputs:          inputs,
		ArkPSBT:         arkPSBT,
		CheckpointPSBTs: checkpoints,
	}, nil
}

// ownerColumns converts a lock owner into DB columns.
func ownerColumns(owner vtxo.LockOwner) (sql.NullString, []byte, error) {
	ownerValue := string(owner)

	switch {
	case strings.HasPrefix(ownerValue, vtxo.LockOwnerRoundPrefix):
		id := strings.TrimPrefix(ownerValue, vtxo.LockOwnerRoundPrefix)
		if id == "" {
			return sql.NullString{}, nil, fmt.Errorf(
				"owner id must be set",
			)
		}

		return sql.NullString{
			String: ownerKindRound,
			Valid:  true,
		}, []byte(id), nil

	case strings.HasPrefix(ownerValue, vtxo.LockOwnerOORPrefix):
		id := strings.TrimPrefix(ownerValue, vtxo.LockOwnerOORPrefix)
		if id == "" {
			return sql.NullString{}, nil, fmt.Errorf(
				"owner id must be set",
			)
		}

		return sql.NullString{
			String: ownerKindOOR,
			Valid:  true,
		}, []byte(id), nil

	default:
		return sql.NullString{}, nil, fmt.Errorf(
			"unknown owner kind: %s", ownerValue,
		)
	}
}

// lockInputsInFlight marks each input in-flight for owner using the active
// transaction.
func lockInputsInFlight(ctx context.Context, q *sqlc.Queries,
	inputs []wire.OutPoint, ownerKind sql.NullString,
	ownerID []byte) error {

	for _, op := range inputs {
		params := sqlc.LockVTXOParams{
			OutpointHash:  op.Hash[:],
			OutpointIndex: int32(op.Index),
			LockOwnerKind: ownerKind,
			LockOwnerID:   ownerID,
		}

		affected, err := q.LockVTXO(ctx, params)
		if err != nil {
			return fmt.Errorf("lock vtxo %v: %w", op, err)
		}

		if affected > 0 {
			continue
		}

		row, err := q.GetVTXO(ctx, sqlc.GetVTXOParams{
			OutpointHash:  op.Hash[:],
			OutpointIndex: int32(op.Index),
		})
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("unknown vtxo: %v", op)
		}
		if err != nil {
			return fmt.Errorf("get vtxo %v after lock failed: %w",
				op, err)
		}

		// Lock-by-same-owner is idempotent.
		if row.Status == string(vtxo.StatusInFlight) &&
			row.LockOwnerKind == ownerKind &&
			bytes.Equal(row.LockOwnerID, ownerID) {

			continue
		}

		existingOwner := "<none>"
		if row.LockOwnerKind.Valid && len(row.LockOwnerID) > 0 {
			existingOwner = fmt.Sprintf("%s:%s",
				row.LockOwnerKind.String,
				string(row.LockOwnerID),
			)
		}

		return fmt.Errorf("vtxo %v not lockable (%s, owner=%s)",
			op, row.Status, existingOwner)
	}

	return nil
}

// minParentBatchExpiry returns the minimum effective batch-expiry height
// across the given parent outpoints, using GetVTXOWithRoundExpiry so the
// query naturally picks up each parent's own inherited expiry (for OOR
// chains) before falling back to the round-join derivation. Parents
// reporting a NULL effective expiry contribute 0 to the min, which is
// the same defensive fallback the adapter uses when a parent's source
// round row is missing or pre-confirmation. Returns 0 when every parent
// reports 0 or when the inputs slice is empty.
func minParentBatchExpiry(ctx context.Context, q *sqlc.Queries,
	inputs []wire.OutPoint) (uint32, error) {

	var (
		minExpiry uint32
		seen      bool
	)
	for _, op := range inputs {
		row, err := q.GetVTXOWithRoundExpiry(
			ctx, sqlc.GetVTXOWithRoundExpiryParams{
				OutpointHash:  op.Hash[:],
				OutpointIndex: int32(op.Index),
			},
		)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("parent vtxo %v missing at "+
				"materialize", op)
		}
		if err != nil {
			return 0, fmt.Errorf("load parent vtxo %v: %w", op,
				err)
		}

		var parentExpiry uint32
		if row.EffectiveBatchExpiry.Valid {
			parentExpiry = uint32(row.EffectiveBatchExpiry.Int32)
		}

		if !seen || parentExpiry < minExpiry {
			minExpiry = parentExpiry
			seen = true
		}
	}

	return minExpiry, nil
}

var _ SessionStore = (*DBSessionStore)(nil)
var _ CoSignedAtomicStore = (*DBSessionStore)(nil)
var _ FinalizeAtomicStore = (*DBSessionStore)(nil)

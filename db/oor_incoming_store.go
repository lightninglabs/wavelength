package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/oor"
	"github.com/lightningnetwork/lnd/clock"
)

// OORIncomingSessionStore persists incoming OOR snapshots in SQL.
type OORIncomingSessionStore struct {
	tx *TransactionExecutor[*sqlc.Queries]

	clock clock.Clock
}

// NewOORIncomingSessionStore creates a DB-backed incoming OOR snapshot store.
func NewOORIncomingSessionStore(dbq BatchedQuerier, clk clock.Clock,
	log btclog.Logger) *OORIncomingSessionStore {

	if clk == nil {
		clk = clock.NewDefaultClock()
	}

	if log == nil {
		log = btclog.Disabled
	}

	txExec := NewTransactionExecutor[*sqlc.Queries](
		dbq,
		func(tx *sql.Tx) *sqlc.Queries {
			return sqlc.New(tx)
		},
		log,
	)

	return &OORIncomingSessionStore{
		tx:    txExec,
		clock: clk,
	}
}

// UpsertIncoming stores or replaces a snapshot for a session id.
func (s *OORIncomingSessionStore) UpsertIncoming(ctx context.Context,
	snapshot *oor.IncomingSnapshot) error {

	if snapshot == nil {
		return fmt.Errorf("snapshot must be provided")
	}

	if snapshot.SessionID == (oor.SessionID{}) {
		return fmt.Errorf("snapshot session id must be provided")
	}

	blob, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode incoming snapshot: %w", err)
	}

	now := s.clock.Now().UnixNano()

	return s.tx.ExecTx(ctx, WriteTxOption(),
		func(q *sqlc.Queries) error {
			return q.UpsertOORIncomingSession(
				ctx, sqlc.UpsertOORIncomingSessionParams{
					SessionID:       sessionIDBytes(snapshot.SessionID),
					SnapshotVersion: int32(snapshot.Version),
					Phase:           string(snapshot.Phase),
					SnapshotBlob:    blob,
					CreatedAt:       now,
					UpdatedAt:       now,
				},
			)
		},
	)
}

// GetIncoming fetches the latest snapshot for a session id.
func (s *OORIncomingSessionStore) GetIncoming(ctx context.Context,
	sessionID oor.SessionID) (*oor.IncomingSnapshot, error) {

	if sessionID == (oor.SessionID{}) {
		return nil, fmt.Errorf("session id must be provided")
	}

	var out *oor.IncomingSnapshot

	err := s.tx.ExecTx(ctx, ReadTxOption(),
		func(q *sqlc.Queries) error {
			row, err := q.GetOORIncomingSession(
				ctx, sessionIDBytes(sessionID),
			)
			if err != nil {
				return err
			}

			var snap oor.IncomingSnapshot
			err = json.Unmarshal(row.SnapshotBlob, &snap)
			if err != nil {
				return fmt.Errorf("decode incoming snapshot: %w", err)
			}

			if snap.SessionID != sessionID {
				return fmt.Errorf("snapshot session id mismatch")
			}

			out = &snap

			return nil
		},
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s",
				oor.ErrIncomingSnapshotNotFound, sessionID,
			)
		}

		return nil, err
	}

	return out, nil
}

var _ oor.IncomingSessionStore = (*OORIncomingSessionStore)(nil)

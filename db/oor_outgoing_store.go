package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/oor"
	"github.com/lightningnetwork/lnd/clock"
)

// OOROutgoingSessionStore persists outgoing OOR snapshots in SQL.
type OOROutgoingSessionStore struct {
	tx *TransactionExecutor[*sqlc.Queries]

	clock clock.Clock
}

// NewOOROutgoingSessionStore creates a DB-backed outgoing OOR snapshot store.
func NewOOROutgoingSessionStore(dbq BatchedQuerier, clk clock.Clock,
	log btclog.Logger) *OOROutgoingSessionStore {

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

	return &OOROutgoingSessionStore{
		tx:    txExec,
		clock: clk,
	}
}

// UpsertOutgoing stores or replaces a snapshot for a session id.
func (s *OOROutgoingSessionStore) UpsertOutgoing(ctx context.Context,
	snapshot *oor.OutgoingSnapshot) error {

	if snapshot == nil {
		return fmt.Errorf("snapshot must be provided")
	}

	if snapshot.SessionID == (oor.SessionID{}) {
		return fmt.Errorf("snapshot session id must be provided")
	}

	blob, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode outgoing snapshot: %w", err)
	}

	now := s.clock.Now().UnixNano()

	return s.tx.ExecTx(ctx, WriteTxOption(),
		func(q *sqlc.Queries) error {
			return q.UpsertOOROutgoingSession(
				ctx, sqlc.UpsertOOROutgoingSessionParams{
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

// GetOutgoing fetches the latest snapshot for a session id.
func (s *OOROutgoingSessionStore) GetOutgoing(ctx context.Context,
	sessionID oor.SessionID) (*oor.OutgoingSnapshot, error) {

	if sessionID == (oor.SessionID{}) {
		return nil, fmt.Errorf("session id must be provided")
	}

	var out *oor.OutgoingSnapshot

	err := s.tx.ExecTx(ctx, ReadTxOption(),
		func(q *sqlc.Queries) error {
			row, err := q.GetOOROutgoingSession(
				ctx, sessionIDBytes(sessionID),
			)
			if err != nil {
				return err
			}

			var snap oor.OutgoingSnapshot
			err = json.Unmarshal(row.SnapshotBlob, &snap)
			if err != nil {
				return fmt.Errorf("decode outgoing snapshot: %w", err)
			}

			if snap.SessionID != sessionID {
				return fmt.Errorf("snapshot session id mismatch")
			}

			out = &snap

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return out, nil
}

// sessionIDBytes converts a session id to 32 raw bytes for DB storage.
func sessionIDBytes(id oor.SessionID) []byte {
	h := [32]byte(id)
	return h[:]
}

var _ oor.OutgoingSessionStore = (*OOROutgoingSessionStore)(nil)

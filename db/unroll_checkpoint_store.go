package db

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
)

// UnrollCheckpoint is the SQL persistence record for one per-target unroll
// FSM checkpoint.
type UnrollCheckpoint struct {
	// ActorID is the stable per-target actor/checkpoint identifier.
	ActorID string

	// StateType names the encoded state family.
	StateType string

	// StateData is the encoded domain checkpoint payload.
	StateData []byte

	// Version is the checkpoint payload schema version.
	Version int64

	// UpdatedAt records when the row was last written.
	UpdatedAt time.Time
}

// UnrollCheckpointStoreDB persists per-target unroll FSM checkpoints.
type UnrollCheckpointStoreDB struct {
	*TransactionExecutor[*sqlc.Queries]

	clk clock.Clock
}

// NewUnrollCheckpointStore creates a SQL-backed unroll checkpoint store.
func NewUnrollCheckpointStore(store *Store,
	clk clock.Clock) *UnrollCheckpointStoreDB {

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

	return &UnrollCheckpointStoreDB{
		TransactionExecutor: txExec,
		clk:                 clk,
	}
}

// SaveCheckpoint stores the latest checkpoint for an unroll actor.
func (s *UnrollCheckpointStoreDB) SaveCheckpoint(ctx context.Context,
	params UnrollCheckpoint) error {

	now := s.clk.Now().Unix()
	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.SaveUnrollCheckpoint(ctx, sqlc.SaveUnrollCheckpointParams{
			ActorID:   params.ActorID,
			StateType: params.StateType,
			StateData: params.StateData,
			Version:   params.Version,
			UpdatedAt: now,
		})
	})
}

// LoadCheckpoint loads the latest checkpoint for an unroll actor.
func (s *UnrollCheckpointStoreDB) LoadCheckpoint(ctx context.Context,
	actorID string) (*UnrollCheckpoint, error) {

	var checkpoint *UnrollCheckpoint
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		row, err := q.GetUnrollCheckpoint(ctx, actorID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}

		checkpoint = &UnrollCheckpoint{
			ActorID:   row.ActorID,
			StateType: row.StateType,
			StateData: row.StateData,
			Version:   row.Version,
			UpdatedAt: time.Unix(row.UpdatedAt, 0),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return checkpoint, nil
}

var _ interface {
	SaveCheckpoint(context.Context, UnrollCheckpoint) error
	LoadCheckpoint(context.Context, string) (*UnrollCheckpoint, error)
} = (*UnrollCheckpointStoreDB)(nil)

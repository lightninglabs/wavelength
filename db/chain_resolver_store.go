package db

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
)

// ChainResolverDB groups SQL methods needed by chain resolver persistence.
type ChainResolverDB interface {
	UpsertChainResolverState(ctx context.Context,
		arg sqlc.UpsertChainResolverStateParams) error

	GetChainResolverState(ctx context.Context,
		arg sqlc.GetChainResolverStateParams) (
		sqlc.ChainResolverState, error,
	)

	ListActiveChainResolverStates(
		ctx context.Context,
	) ([]sqlc.ChainResolverState, error)

	DeleteChainResolverState(ctx context.Context,
		arg sqlc.DeleteChainResolverStateParams) error
}

// BatchedChainResolverDB combines chain resolver queries with batched
// transaction execution.
type BatchedChainResolverDB interface {
	ChainResolverDB
	BatchedTx[ChainResolverDB]
}

// ChainResolverStore is the persistence layer for chain resolver state. It
// wraps sqlc-generated queries with outpoint encoding/decoding and timestamp
// management.
type ChainResolverStore struct {
	db    BatchedChainResolverDB
	clock clock.Clock
}

// NewChainResolverStore constructs a chain resolver store backed by batched
// SQL transactions.
func NewChainResolverStore(db BatchedChainResolverDB,
	c clock.Clock) *ChainResolverStore {

	if c == nil {
		c = clock.NewDefaultClock()
	}

	return &ChainResolverStore{
		db:    db,
		clock: c,
	}
}

// SaveResolverState persists the current state of a resolver identified by
// its VTXO outpoint.
func (s *ChainResolverStore) SaveResolverState(ctx context.Context,
	outpoint wire.OutPoint, state string, details []byte) error {

	now := s.clock.Now().Unix()
	writeTx := WriteTxOption()

	return s.db.ExecTx(
		ctx, writeTx, func(q ChainResolverDB) error {
			return q.UpsertChainResolverState(
				ctx, sqlc.UpsertChainResolverStateParams{
					OutpointHash:  outpoint.Hash[:],
					OutpointIndex: int32(outpoint.Index),
					State:         state,
					StateDetails:  details,
					CreatedAt:     now,
					UpdatedAt:     now,
				},
			)
		},
	)
}

// GetResolverState loads the persisted state for a resolver. Returns the
// state name and opaque details blob.
func (s *ChainResolverStore) GetResolverState(ctx context.Context,
	outpoint wire.OutPoint) (string, []byte, error) {

	var (
		stateName string
		details   []byte
	)

	readTx := ReadTxOption()

	err := s.db.ExecTx(
		ctx, readTx, func(q ChainResolverDB) error {
			row, err := q.GetChainResolverState(
				ctx, sqlc.GetChainResolverStateParams{
					OutpointHash:  outpoint.Hash[:],
					OutpointIndex: int32(outpoint.Index),
				},
			)
			if err != nil {
				return err
			}

			stateName = row.State
			details = row.StateDetails

			return nil
		},
	)
	if err != nil {
		return "", nil, fmt.Errorf("get resolver state: %w", err)
	}

	return stateName, details, nil
}

// ListActiveResolvers returns the outpoints of all resolvers that have
// non-terminal persisted state.
func (s *ChainResolverStore) ListActiveResolvers(
	ctx context.Context) ([]wire.OutPoint, error) {

	var outpoints []wire.OutPoint
	readTx := ReadTxOption()

	err := s.db.ExecTx(
		ctx, readTx, func(q ChainResolverDB) error {
			rows, err := q.ListActiveChainResolverStates(ctx)
			if err != nil {
				return err
			}

			outpoints = make([]wire.OutPoint, len(rows))
			for i, row := range rows {
				outpoints[i] = rowToOutpoint(
					row.OutpointHash,
					row.OutpointIndex,
				)
			}

			return nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("list active resolvers: %w", err)
	}

	return outpoints, nil
}

// DeleteResolverState removes the persisted state for a resolver. Called
// when a resolver reaches a terminal state.
func (s *ChainResolverStore) DeleteResolverState(ctx context.Context,
	outpoint wire.OutPoint) error {

	writeTx := WriteTxOption()

	return s.db.ExecTx(
		ctx, writeTx, func(q ChainResolverDB) error {
			return q.DeleteChainResolverState(
				ctx, sqlc.DeleteChainResolverStateParams{
					OutpointHash:  outpoint.Hash[:],
					OutpointIndex: int32(outpoint.Index),
				},
			)
		},
	)
}

// rowToOutpoint converts a raw outpoint hash and index from a database row
// into a wire.OutPoint.
func rowToOutpoint(hash []byte, index int32) wire.OutPoint {
	var op wire.OutPoint
	copy(op.Hash[:], hash)
	op.Index = uint32(index)

	return op
}

package db

import (
	"context"
	"errors"

	"github.com/lightninglabs/wavelength/db/sqlc"
)

// ErrExitBatchIDUsed prevents a duplicate or fenced submission from queuing
// work.
var ErrExitBatchIDUsed = errors.New("exit batch request ID already used")

// BeginExitBatch reserves the unique request ID before the caller touches
// any VTXOs. Recovery can close this ID until the round turn commits.
func (s *RoundPersistenceStore) BeginExitBatch(ctx context.Context,
	id string) error {

	return s.db.ExecTx(ctx, WriteTxOption(), func(q RoundStore) error {
		n, err := q.BeginExitBatchRequest(ctx, id)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrExitBatchIDUsed
		}

		return nil
	})
}

// CommitExitBatch records successful admission inside the durable round turn.
// A closed request fails the entire turn, including its checkpoint and effects.
func (s *RoundPersistenceStore) CommitExitBatch(ctx context.Context, id string,
	receipt []byte) error {

	if len(receipt) == 0 {
		return errors.New("missing exit receipt")
	}

	return s.db.ExecTx(ctx, WriteTxOption(), func(q RoundStore) error {
		n, err := q.AdmitExitBatchRequest(
			ctx, sqlc.AdmitExitBatchRequestParams{
				RequestID: id,
				Receipt:   receipt,
			},
		)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrExitBatchIDUsed
		}

		return nil
	})
}

// ResolveExitBatch returns the admission receipt or atomically closes the ID.
// Once an empty result is returned, even an already delayed submit cannot win.
func (s *RoundPersistenceStore) ResolveExitBatch(ctx context.Context,
	id string) ([]byte, error) {

	var receipt []byte
	err := s.db.ExecTx(ctx, WriteTxOption(), func(q RoundStore) error {
		var err error
		receipt, err = q.ResolveExitBatchRequest(
			ctx, sqlc.ResolveExitBatchRequestParams{
				RequestID: id,
				Receipt:   []byte{},
			},
		)

		return err
	})

	return receipt, err
}

package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/round"
)

// ServiceOperationStore retains cold-restart ownership without nonce material.
type ServiceOperationStore struct {
	*TransactionExecutor[*sqlc.Queries]
}

// NewServiceOperationStore uses the shared database and transaction policy.
func (s *Store) NewServiceOperationStore() *ServiceOperationStore {
	return &ServiceOperationStore{NewTransactionExecutor(
		s.BaseDB(), func(tx *sql.Tx) *sqlc.Queries {
			return s.queries.WithTx(tx)
		}, s.log,
	)}
}

// SaveServiceOperation claims all inputs before a signed join leaves the
// client.
func (s *ServiceOperationStore) SaveServiceOperation(ctx context.Context,
	op round.DeferredServiceOperation) error {

	encoded, err := op.Service.Encode()
	if err != nil {
		return err
	}

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		prior, err := q.GetServiceOperation(
			ctx, op.Service.OperationID[:],
		)
		switch {
		case err == nil:
			request, err := types.DecodeServiceRequest(
				prior.AuthorizationBlob,
			)
			if err != nil {
				return err
			}
			auth := op.Service
			if !prior.Active ||
				auth.FeeLimitSat > request.FeeLimitSat ||
				auth.ExpiresAtUnix > request.ExpiresAtUnix ||
				(auth.AllowFallback && !request.AllowFallback) {
				return fmt.Errorf("service authorization " +
					"conflict")
			}
			inputs, err := q.ListServiceOperationInputs(
				ctx, op.Service.OperationID[:],
			)
			if err != nil {
				return err
			}
			if len(inputs) != len(op.Inputs) {
				return fmt.Errorf("service input set changed")
			}
			for _, input := range inputs {
				found := false
				for _, expected := range op.Inputs {
					found = found || (bytes.Equal(
						input.Txid, expected.Hash[:],
					) &&
						input.OutputIndex == int64(
							expected.Index,
						))
				}
				if !found {
					return fmt.Errorf("service input set " +
						"changed")
				}
			}

		case errors.Is(err, sql.ErrNoRows):
		default:
			return err
		}
		if err := q.SaveServiceOperation(
			ctx, sqlc.SaveServiceOperationParams{
				OperationID:       op.Service.OperationID[:],
				AuthorizationBlob: encoded,
			},
		); err != nil {
			return err
		}
		for _, input := range op.Inputs {
			owners, err := q.OtherServiceInputOwners(
				ctx, sqlc.OtherServiceInputOwnersParams{
					Txid: input.Hash[:],
					OutputIndex: int64(
						input.Index,
					),
					OperationID: op.Service.OperationID[:],
				},
			)
			if err != nil {
				return err
			}
			if len(owners) != 0 {
				return fmt.Errorf("input belongs to a " +
					"deferred service operation")
			}
			if err := q.InsertServiceOperationInput(
				ctx, sqlc.InsertServiceOperationInputParams{
					OperationID: op.Service.OperationID[:],
					IsForfeit: slices.Contains(
						op.Forfeits, input,
					),
					Txid: input.Hash[:],
					OutputIndex: int64(
						input.Index,
					),
				},
			); err != nil {
				return err
			}
		}

		return nil
	})
}

// BindServiceOperation records admission before further client participation.
func (s *ServiceOperationStore) BindServiceOperation(ctx context.Context,
	id [32]byte, roundID round.RoundID, deadline time.Time) error {

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		count, err := q.BindServiceOperation(
			ctx, sqlc.BindServiceOperationParams{
				OperationID: id[:],
				RoundID: sql.NullString{
					String: roundID.String(), Valid: true,
				},
				DeadlineUnix: deadline.Unix(),
			},
		)
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("service operation is not active")
		}

		return nil
	})
}

// ListDeferredServiceOperations returns owners without a signing checkpoint.
func (s *ServiceOperationStore) ListDeferredServiceOperations(
	ctx context.Context) ([]round.DeferredServiceOperation, error) {

	var result []round.DeferredServiceOperation
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.ListDeferredServiceOperations(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			request, err := types.DecodeServiceRequest(
				row.AuthorizationBlob,
			)
			if err != nil {
				return err
			}
			op := round.DeferredServiceOperation{Service: *request}
			if row.RoundID.Valid {
				op.RoundID, err = round.ParseRoundID(
					row.RoundID.String,
				)
				if err != nil {
					return err
				}
				op.Deadline = time.Unix(row.DeadlineUnix, 0)
			}
			inputs, err := q.ListServiceOperationInputs(
				ctx, row.OperationID,
			)
			if err != nil {
				return err
			}
			for _, input := range inputs {
				outpoint := wire.OutPoint{
					Index: uint32(input.OutputIndex),
				}
				copy(outpoint.Hash[:], input.Txid)
				op.Inputs = append(op.Inputs, outpoint)
				if input.IsForfeit {
					op.Forfeits = append(
						op.Forfeits, outpoint,
					)
				}
			}
			result = append(result, op)
		}

		return nil
	})

	return result, err
}

// CompleteServiceOperation retires input claims after an authoritative outcome.
func (s *ServiceOperationStore) CompleteServiceOperation(ctx context.Context,
	id [32]byte) error {

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.CompleteServiceOperation(ctx, id[:])
	})
}

// HasServiceInputOwner keeps startup cleanup from releasing deferred inputs.
func (s *ServiceOperationStore) HasServiceInputOwner(ctx context.Context,
	op wire.OutPoint) (bool, error) {

	var held bool
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		var err error
		held, err = q.HasServiceInputOwner(
			ctx, sqlc.HasServiceInputOwnerParams{
				Txid:        op.Hash[:],
				OutputIndex: int64(op.Index),
			},
		)

		return err
	})

	return held, err
}

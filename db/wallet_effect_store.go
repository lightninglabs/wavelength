package db

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightningnetwork/lnd/clock"
)

// WalletEffectStoreDB persists wallet-owned durable effects.
type WalletEffectStoreDB struct {
	*TransactionExecutor[*sqlc.Queries]

	clk clock.Clock
}

// NewWalletEffectStore creates a wallet effect store from the shared DB.
func NewWalletEffectStore(store *Store, clk clock.Clock) *WalletEffectStoreDB {
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

	return &WalletEffectStoreDB{
		TransactionExecutor: txExec,
		clk:                 clk,
	}
}

// InsertWalletEffect persists a pending effect. Duplicate idempotency keys are
// treated as success.
func (s *WalletEffectStoreDB) InsertWalletEffect(ctx context.Context,
	effect wallet.WalletEffectInsert) error {

	now := s.clk.Now().Unix()
	if effect.ID == "" {
		effect.ID = effect.IdempotencyKey
	}
	if effect.MaxAttempts == 0 {
		effect.MaxAttempts = 10
	}

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.InsertWalletEffect(ctx, sqlc.InsertWalletEffectParams{
			ID:             effect.ID,
			EffectType:     effect.EffectType,
			IdempotencyKey: effect.IdempotencyKey,
			OutpointHash:   effect.OutpointHash,
			OutpointIndex:  effect.OutpointIndex,
			Txid:           effect.Txid,
			AmountSat:      effect.AmountSat,
			FeeSat:         effect.FeeSat,
			BlockHeight:    effect.BlockHeight,
			Classification: effect.Classification,
			MaxAttempts:    effect.MaxAttempts,
			NextAttemptAt:  now,
			CreatedAt:      now,
		})
	})
}

// ClaimDueWalletEffects claims a batch of due effects.
func (s *WalletEffectStoreDB) ClaimDueWalletEffects(ctx context.Context,
	owner string, limit int, lease time.Duration) ([]wallet.WalletEffect,
	error) {

	if limit <= 0 {
		return nil, nil
	}

	var claimed []wallet.WalletEffect
	now := s.clk.Now().Unix()
	claimUntil := s.clk.Now().Add(lease).Unix()

	err := s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		ids, err := q.ListDueWalletEffectIDs(
			ctx, sqlc.ListDueWalletEffectIDsParams{
				NextAttemptAt: now,
				Limit:         int32(limit),
			},
		)
		if err != nil {
			return err
		}

		claimed = make([]wallet.WalletEffect, 0, len(ids))
		for _, id := range ids {
			token := uuid.NewString()
			row, err := q.ClaimWalletEffect(
				ctx, sqlc.ClaimWalletEffectParams{
					ID: id,
					ClaimOwner: sql.NullString{
						String: owner,
						Valid:  true,
					},
					ClaimToken: sql.NullString{
						String: token,
						Valid:  true,
					},
					ClaimUntil: sql.NullInt64{
						Int64: claimUntil,
						Valid: true,
					},
					UpdatedAt: now,
				},
			)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}

				return err
			}

			claimed = append(claimed, walletEffectFromRow(row))
		}

		return nil
	})

	return claimed, err
}

// MarkWalletEffectDone marks a claimed effect done.
func (s *WalletEffectStoreDB) MarkWalletEffectDone(ctx context.Context, id,
	claimToken string) error {

	now := s.clk.Now().Unix()

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.MarkWalletEffectDone(
			ctx, sqlc.MarkWalletEffectDoneParams{
				ID: id,
				ClaimToken: sql.NullString{
					String: claimToken,
					Valid:  true,
				},
				DoneAt: sql.NullInt64{
					Int64: now,
					Valid: true,
				},
			},
		)
	})
}

// ReleaseWalletEffectForRetry releases a failed claim back to pending, or dead
// after max attempts.
func (s *WalletEffectStoreDB) ReleaseWalletEffectForRetry(ctx context.Context,
	id, claimToken string, retryAfter time.Duration, failure error) error {

	now := s.clk.Now()
	var errText string
	if failure != nil {
		errText = failure.Error()
	}

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.ReleaseWalletEffectForRetry(
			ctx, sqlc.ReleaseWalletEffectForRetryParams{
				ID: id,
				ClaimToken: sql.NullString{
					String: claimToken,
					Valid:  true,
				},
				NextAttemptAt: now.Add(retryAfter).Unix(),
				UpdatedAt:     now.Unix(),
				LastError: sql.NullString{
					String: errText,
					Valid:  errText != "",
				},
			},
		)
	})
}

// ReleaseExpiredWalletEffectClaims releases stale claims for retry.
func (s *WalletEffectStoreDB) ReleaseExpiredWalletEffectClaims(
	ctx context.Context) error {

	now := s.clk.Now().Unix()

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.ReleaseExpiredWalletEffectClaims(
			ctx, sqlc.ReleaseExpiredWalletEffectClaimsParams{
				ClaimUntil: sql.NullInt64{
					Int64: now,
					Valid: true,
				},
				UpdatedAt: now,
			},
		)
	})
}

func walletEffectFromRow(row sqlc.WalletEffect) wallet.WalletEffect {
	return wallet.WalletEffect{
		ID:             row.ID,
		EffectType:     row.EffectType,
		IdempotencyKey: row.IdempotencyKey,
		OutpointHash:   row.OutpointHash,
		OutpointIndex:  row.OutpointIndex,
		Txid:           row.Txid,
		AmountSat:      row.AmountSat,
		FeeSat:         row.FeeSat,
		BlockHeight:    row.BlockHeight,
		Classification: row.Classification,
		ClaimToken:     row.ClaimToken,
		Attempts:       row.Attempts,
	}
}

var _ wallet.WalletEffectStore = (*WalletEffectStoreDB)(nil)

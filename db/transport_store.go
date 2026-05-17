package db

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightningnetwork/lnd/clock"
)

// TransportStoreDB persists mailbox transport cursors and egress effects.
type TransportStoreDB struct {
	*TransactionExecutor[*sqlc.Queries]

	clk clock.Clock
}

func NewTransportStore(store *Store, clk clock.Clock) *TransportStoreDB {
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

	return &TransportStoreDB{
		TransactionExecutor: txExec,
		clk:                 clk,
	}
}

func (s *TransportStoreDB) LoadIngressCursor(ctx context.Context,
	localMailboxID, remoteMailboxID string) (serverconn.AckState, error) {

	var state serverconn.AckState
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		row, err := q.GetMailboxIngressCursor(ctx, localMailboxID)
		if errors.Is(err, sql.ErrNoRows) {
			now := s.clk.Now().Unix()

			return q.UpsertMailboxIngressCursor(
				ctx, sqlc.UpsertMailboxIngressCursorParams{
					LocalMailboxID:  localMailboxID,
					RemoteMailboxID: remoteMailboxID,
					CreatedAt:       now,
					UpdatedAt:       now,
				},
			)
		}
		if err != nil {
			return err
		}

		state = ackStateFromIngressRow(row)

		return nil
	})

	return state, err
}

func (s *TransportStoreDB) SaveIngressCursor(ctx context.Context,
	localMailboxID, remoteMailboxID string,
	state serverconn.AckState) error {

	now := s.clk.Now().Unix()

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.UpsertMailboxIngressCursor(
			ctx, sqlc.UpsertMailboxIngressCursorParams{
				LocalMailboxID:  localMailboxID,
				RemoteMailboxID: remoteMailboxID,
				PullCursor:      int64(state.PullCursor),
				DispatchCommittedTo: int64(
					state.DispatchCommittedTo,
				),
				AckTarget:      int64(state.AckTarget),
				AckCommittedTo: int64(state.AckCommittedTo),
				LastPullAt: sql.NullInt64{
					Int64: now,
					Valid: true,
				},
				LastDispatchAt: sql.NullInt64{
					Int64: now,
					Valid: state.DispatchCommittedTo > 0,
				},
				LastAckAt: sql.NullInt64{
					Int64: now,
					Valid: state.AckCommittedTo > 0,
				},
				CreatedAt: now,
				UpdatedAt: now,
			},
		)
	})
}

func (s *TransportStoreDB) InsertEgress(ctx context.Context,
	env serverconn.EgressEnvelope) error {

	now := s.clk.Now().Unix()
	if env.ID == "" {
		env.ID = env.IdempotencyKey
	}
	if env.ID == "" {
		env.ID = uuid.NewString()
	}

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.InsertMailboxEgress(
			ctx, sqlc.InsertMailboxEgressParams{
				ID:              env.ID,
				Connector:       env.Connector,
				LocalMailboxID:  env.LocalMailboxID,
				RemoteMailboxID: env.RemoteMailboxID,
				RpcKind:         env.RPCKind,
				Service:         env.Service,
				Method:          env.Method,
				CorrelationID: sql.NullString{
					String: env.CorrelationID,
					Valid:  env.CorrelationID != "",
				},
				ReplyTo: sql.NullString{
					String: env.ReplyTo,
					Valid:  env.ReplyTo != "",
				},
				MsgID:          env.MsgID,
				IdempotencyKey: env.IdempotencyKey,
				Envelope:       env.Envelope,
				MaxAttempts:    10,
				NextAttemptAt:  now,
				CreatedAt:      now,
			},
		)
	})
}

func (s *TransportStoreDB) ClaimDueEgress(ctx context.Context, owner string,
	limit int, lease time.Duration) ([]serverconn.EgressEnvelope, error) {

	if limit <= 0 {
		return nil, nil
	}

	now := s.clk.Now().Unix()
	claimUntil := s.clk.Now().Add(lease).Unix()
	claimed := make([]serverconn.EgressEnvelope, 0, limit)

	err := s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		ids, err := q.ListDueMailboxEgressIDs(
			ctx, sqlc.ListDueMailboxEgressIDsParams{
				NextAttemptAt: now,
				Limit:         int32(limit),
			},
		)
		if err != nil {
			return err
		}

		for _, id := range ids {
			token := uuid.NewString()
			row, err := q.ClaimMailboxEgress(
				ctx, sqlc.ClaimMailboxEgressParams{
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
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}

			claimed = append(claimed, egressFromRow(row))
		}

		return nil
	})

	return claimed, err
}

func (s *TransportStoreDB) MarkEgressSent(ctx context.Context, id,
	claimToken string) error {

	now := s.clk.Now().Unix()

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.MarkMailboxEgressSent(
			ctx, sqlc.MarkMailboxEgressSentParams{
				ID: id,
				ClaimToken: sql.NullString{
					String: claimToken,
					Valid:  true,
				},
				SentAt: sql.NullInt64{
					Int64: now,
					Valid: true,
				},
			},
		)
	})
}

func (s *TransportStoreDB) ReleaseEgressForRetry(ctx context.Context, id,
	claimToken string, retryAfter time.Duration, failure error) error {

	now := s.clk.Now()
	errText := ""
	if failure != nil {
		errText = failure.Error()
	}

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.ReleaseMailboxEgressForRetry(
			ctx, sqlc.ReleaseMailboxEgressForRetryParams{
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

func (s *TransportStoreDB) ReleaseExpiredEgressClaims(
	ctx context.Context) error {

	now := s.clk.Now().Unix()

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.ReleaseExpiredMailboxEgressClaims(
			ctx, sqlc.ReleaseExpiredMailboxEgressClaimsParams{
				ClaimUntil: sql.NullInt64{
					Int64: now,
					Valid: true,
				},
				UpdatedAt: now,
			},
		)
	})
}

func ackStateFromIngressRow(row sqlc.MailboxIngressCursor) serverconn.AckState {
	return serverconn.AckState{
		PullCursor:          uint64(row.PullCursor),
		DispatchCommittedTo: uint64(row.DispatchCommittedTo),
		AckTarget:           uint64(row.AckTarget),
		AckCommittedTo:      uint64(row.AckCommittedTo),
	}
}

func egressFromRow(row sqlc.MailboxEgress) serverconn.EgressEnvelope {
	return serverconn.EgressEnvelope{
		ID:              row.ID,
		Connector:       row.Connector,
		LocalMailboxID:  row.LocalMailboxID,
		RemoteMailboxID: row.RemoteMailboxID,
		RPCKind:         row.RpcKind,
		Service:         row.Service,
		Method:          row.Method,
		CorrelationID:   row.CorrelationID.String,
		ReplyTo:         row.ReplyTo.String,
		MsgID:           row.MsgID,
		IdempotencyKey:  row.IdempotencyKey,
		Envelope:        row.Envelope,
		ClaimToken:      row.ClaimToken.String,
		Attempts:        row.Attempts,
	}
}

var _ serverconn.TransportStore = (*TransportStoreDB)(nil)

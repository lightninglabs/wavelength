package actordelivery

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/db"
	adsqlc "github.com/lightninglabs/darepo-client/db/actordelivery/sqlc"
	"github.com/lightningnetwork/lnd/clock"
)

// Type aliases for SQLC-generated types to reduce import noise.
type (
	MailboxMsgRow          = adsqlc.MailboxMessage
	OutboxMsgRow           = adsqlc.OutboxMessage
	AskResultRow           = adsqlc.AskResult
	FsmCheckpointRow       = adsqlc.FsmCheckpoint
	DeadLetterRow          = adsqlc.DeadLetter
	EnqueueMailboxParams   = adsqlc.EnqueueMailboxMessageParams
	EnqueueOutboxParams    = adsqlc.EnqueueOutboxMessageParams
	LeaseMailboxParams     = adsqlc.LeaseNextMailboxMessageParams
	AckMailboxParams       = adsqlc.AckMailboxMessageParams
	NackMailboxParams      = adsqlc.NackMailboxMessageParams
	ExtendMailboxParams    = adsqlc.ExtendMailboxLeaseParams
	InsertAskResultParams  = adsqlc.InsertAskResultParams
	ClaimOutboxBatchParams = adsqlc.ClaimOutboxBatchParams
	CompleteOutboxParams   = adsqlc.CompleteOutboxMessageParams
	FailOutboxParams       = adsqlc.FailOutboxMessageParams
	MarkProcessedParams    = adsqlc.MarkMessageProcessedParams
	SaveCheckpointParams   = adsqlc.SaveFSMCheckpointParams
	DeadLetterInsertParams = adsqlc.MoveMailboxToDeadLetterParams
	ListDeadLettersParams  = adsqlc.ListDeadLettersByActorParams
)

// ActorDeliveryQueries is the interface that groups all actor delivery-related
// database queries.
//
// ActorDeliveryQueries is intentionally wide because it is implemented by
// SQLC-generated query sets. Keeping it as a single interface simplifies
// transactional usage without excessive adapter boilerplate.
//
//nolint:interfacebloat
type ActorDeliveryQueries interface {
	// Mailbox operations.
	EnqueueMailboxMessage(ctx context.Context,
		arg EnqueueMailboxParams) error
	LeaseNextMailboxMessage(
		ctx context.Context, arg LeaseMailboxParams,
	) (MailboxMsgRow, error)
	AckMailboxMessage(ctx context.Context,
		arg AckMailboxParams) (int64, error)
	NackMailboxMessage(
		ctx context.Context, arg NackMailboxParams,
	) (int64, error)
	ExtendMailboxLease(
		ctx context.Context, arg ExtendMailboxParams,
	) (int64, error)
	DeleteMailboxMessage(ctx context.Context, id string) error
	ExpireMailboxLeases(ctx context.Context, leaseUntil sql.NullInt64) error

	// Ask result operations.
	InsertAskResult(ctx context.Context, arg InsertAskResultParams) error
	GetAskResult(ctx context.Context,
		promiseID string) (AskResultRow, error)
	DeleteAskResult(ctx context.Context, promiseID string) error

	// Outbox operations.
	EnqueueOutboxMessage(ctx context.Context, arg EnqueueOutboxParams) error
	ClaimOutboxBatch(ctx context.Context,
		arg ClaimOutboxBatchParams) ([]OutboxMsgRow, error)
	CompleteOutboxMessage(ctx context.Context,
		arg CompleteOutboxParams) error
	FailOutboxMessage(ctx context.Context, arg FailOutboxParams) error

	// Deduplication operations.
	IsMessageProcessed(ctx context.Context, id string) (bool, error)
	MarkMessageProcessed(ctx context.Context, arg MarkProcessedParams) error

	// FSM checkpoint operations.
	SaveFSMCheckpoint(ctx context.Context, arg SaveCheckpointParams) error
	GetFSMCheckpoint(
		ctx context.Context, actorID string,
	) (FsmCheckpointRow, error)
	DeleteFSMCheckpoint(ctx context.Context, actorID string) error

	// Dead letter operations.
	MoveMailboxToDeadLetter(
		ctx context.Context, arg DeadLetterInsertParams,
	) error
	GetDeadLetter(ctx context.Context, id string) (DeadLetterRow, error)
	ListDeadLettersByActor(
		ctx context.Context, arg ListDeadLettersParams,
	) ([]DeadLetterRow, error)
	DeleteDeadLetter(ctx context.Context, id string) error

	// Cleanup operations.
	CleanupExpiredProcessedMessages(ctx context.Context,
		expiresAt int64) error
	CleanupExpiredAskResults(ctx context.Context, expiresAt int64) error
}

// BatchedActorDeliveryQueries provides transactional execution for actor
// delivery operations via the BatchedTx generic interface.
type BatchedActorDeliveryQueries interface {
	db.BatchedTx[ActorDeliveryQueries]
}

// Store implements the actor.DeliveryStore interface using the
// BatchedTx pattern for transaction-safe operations. All methods execute within
// database transactions with automatic retry on serialization errors.
type Store struct {
	db    BatchedActorDeliveryQueries
	clock clock.Clock
}

// NewStore creates a new actor delivery store using the
// transaction executor pattern.
func NewStore(
	db BatchedActorDeliveryQueries, clock clock.Clock,
) *Store {

	return &Store{
		db:    db,
		clock: clock,
	}
}

// EnqueueMessage persists a new message to an actor's mailbox.
func (s *Store) EnqueueMessage(
	ctx context.Context, params actor.EnqueueParams,
) error {

	writeTxOpts := db.WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts,
		func(q ActorDeliveryQueries) error {
			createdAt := s.clock.Now().Unix()

			return q.EnqueueMailboxMessage(
				ctx,
				EnqueueMailboxParams{
					ID:          params.ID,
					MailboxID:   params.MailboxID,
					MessageType: params.MessageType,
					Payload:     params.Payload,
					PromiseID: toNullString(
						params.PromiseID,
					),
					CallbackActorID: toNullString(
						params.CallbackActorID,
					),
					CorrelationID: toNullString(
						params.CorrelationID,
					),
					Priority:    int32(params.Priority),
					AvailableAt: params.AvailableAt.Unix(),
					MaxAttempts: int32(params.MaxAttempts),
					CreatedAt:   createdAt,
				},
			)
		})
}

// LeaseNextMessage atomically claims the next available message for processing.
func (s *Store) LeaseNextMessage(
	ctx context.Context,
	mailboxID string,
	leaseToken string,
	leaseDuration time.Duration,
) (*actor.LeasedMessage, error) {

	writeTxOpts := db.WriteTxOption()

	var result *actor.LeasedMessage

	err := s.db.ExecTx(ctx, writeTxOpts,
		func(q ActorDeliveryQueries) error {
			now := s.clock.Now()
			leaseUntil := now.Add(leaseDuration)

			msg, err := q.LeaseNextMailboxMessage(
				ctx,
				LeaseMailboxParams{
					MailboxID: mailboxID,
					LeaseToken: toNullString(
						leaseToken,
					),
					LeaseUntil: toNullInt64(
						leaseUntil.Unix(),
					),
					AvailableAt: now.Unix(),
				},
			)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return nil
				}

				return err
			}

			callbackActorID := fromNullString(msg.CallbackActorID)
			correlationID := fromNullString(msg.CorrelationID)
			leaseUntilTime := fromNullInt64Time(msg.LeaseUntil)
			createdAt := time.Unix(msg.CreatedAt, 0)

			result = &actor.LeasedMessage{
				ID:              msg.ID,
				MailboxID:       msg.MailboxID,
				MessageType:     msg.MessageType,
				Payload:         msg.Payload,
				PromiseID:       fromNullString(msg.PromiseID),
				CallbackActorID: callbackActorID,
				CorrelationID:   correlationID,
				Priority:        int(msg.Priority),
				LeaseToken:      fromNullString(msg.LeaseToken),
				LeaseUntil:      leaseUntilTime,
				Attempts:        int(msg.Attempts),
				MaxAttempts:     int(msg.MaxAttempts),
				CreatedAt:       createdAt,
			}

			return nil
		})

	return result, err
}

// AckMessage acknowledges successful processing of a message.
func (s *Store) AckMessage(
	ctx context.Context, id, leaseToken string,
) (int64, error) {

	writeTxOpts := db.WriteTxOption()

	var rows int64

	err := s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			var err error
			rows, err = q.AckMailboxMessage(ctx, AckMailboxParams{
				ID:         id,
				LeaseToken: toNullString(leaseToken),
			})

			return err
		},
	)

	return rows, err
}

// NackMessage releases a message for redelivery after the specified delay.
func (s *Store) NackMessage(
	ctx context.Context,
	id, leaseToken string,
	retryAfter time.Duration,
) (int64, error) {

	writeTxOpts := db.WriteTxOption()

	var rows int64

	err := s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			availableAt := s.clock.Now().Add(retryAfter)

			var err error
			rows, err = q.NackMailboxMessage(ctx, NackMailboxParams{
				ID:          id,
				LeaseToken:  toNullString(leaseToken),
				AvailableAt: availableAt.Unix(),
			})

			return err
		},
	)

	return rows, err
}

// ExtendLease extends the lease for long-running message processing.
func (s *Store) ExtendLease(
	ctx context.Context,
	id, leaseToken string,
	extension time.Duration,
) (int64, error) {

	writeTxOpts := db.WriteTxOption()

	var rows int64

	err := s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			leaseUntil := s.clock.Now().Add(extension)

			var err error
			rows, err = q.ExtendMailboxLease(
				ctx,
				ExtendMailboxParams{
					ID:         id,
					LeaseToken: toNullString(leaseToken),
					LeaseUntil: toNullInt64(
						leaseUntil.Unix(),
					),
				},
			)

			return err
		},
	)

	return rows, err
}

// MoveToDeadLetter moves a failed message to the dead letter queue.
func (s *Store) MoveToDeadLetter(
	ctx context.Context, id, reason string,
) error {

	writeTxOpts := db.WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			createdAt := s.clock.Now().Unix()

			// First, move to dead letter.
			err := q.MoveMailboxToDeadLetter(
				ctx,
				DeadLetterInsertParams{
					ID:            id,
					FailureReason: reason,
					CreatedAt:     createdAt,
				},
			)
			if err != nil {
				return err
			}

			// Then delete from mailbox.
			return q.DeleteMailboxMessage(ctx, id)
		},
	)
}

// DeleteMessage removes a message from the mailbox.
func (s *Store) DeleteMessage(
	ctx context.Context, id string,
) error {

	writeTxOpts := db.WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			return q.DeleteMailboxMessage(ctx, id)
		},
	)
}

// SaveAskResult persists the result of an Ask message for caller retrieval.
func (s *Store) SaveAskResult(
	ctx context.Context, params actor.AskResultParams,
) error {

	writeTxOpts := db.WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			return q.InsertAskResult(ctx, InsertAskResultParams{
				PromiseID:  params.PromiseID,
				ResultBlob: params.ResultBlob,
				ErrorText:  toNullString(params.ErrorText),
				CreatedAt:  s.clock.Now().Unix(),
				ExpiresAt:  params.ExpiresAt.Unix(),
			})
		},
	)
}

// GetAskResult retrieves the result of an Ask message.
func (s *Store) GetAskResult(
	ctx context.Context, promiseID string,
) (*actor.AskResult, error) {

	readTxOpts := db.ReadTxOption()

	var result *actor.AskResult

	err := s.db.ExecTx(ctx, readTxOpts, func(q ActorDeliveryQueries) error {
		row, err := q.GetAskResult(ctx, promiseID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}

			return err
		}

		result = &actor.AskResult{
			PromiseID:  row.PromiseID,
			ResultBlob: row.ResultBlob,
			ErrorText:  fromNullString(row.ErrorText),
			CreatedAt:  time.Unix(row.CreatedAt, 0),
			ExpiresAt:  time.Unix(row.ExpiresAt, 0),
		}

		return nil
	})

	return result, err
}

// DeleteAskResult removes an Ask result after retrieval.
func (s *Store) DeleteAskResult(
	ctx context.Context, promiseID string,
) error {

	writeTxOpts := db.WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			return q.DeleteAskResult(ctx, promiseID)
		},
	)
}

// EnqueueOutbox adds a message to the transactional outbox.
func (s *Store) EnqueueOutbox(
	ctx context.Context, params actor.OutboxParams,
) error {

	writeTxOpts := db.WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			return q.EnqueueOutboxMessage(ctx, EnqueueOutboxParams{
				ID:            params.ID,
				SourceActorID: params.SourceActorID,
				TargetActorID: params.TargetActorID,
				MessageType:   params.MessageType,
				Payload:       params.Payload,
				DomainKey:     toNullString(params.DomainKey),
				Version:       int32(params.Version),
				CreatedAt:     s.clock.Now().Unix(),
			})
		},
	)
}

// ClaimOutboxBatch claims a batch of pending outbox messages for delivery.
func (s *Store) ClaimOutboxBatch(
	ctx context.Context, params actor.OutboxClaimParams,
) ([]actor.OutboxMessage, error) {

	writeTxOpts := db.WriteTxOption()

	var result []actor.OutboxMessage

	err := s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			now := s.clock.Now()
			claimedUntil := now.Add(params.ClaimDuration)

			rows, err := q.ClaimOutboxBatch(
				ctx, ClaimOutboxBatchParams{
					Limit: int32(params.Limit),
					ClaimToken: toNullString(
						params.ClaimToken,
					),
					ClaimedUntil: toNullInt64(
						claimedUntil.Unix(),
					),
					ClaimedUntil_2: toNullInt64(
						now.Unix(),
					),
				},
			)
			if err != nil {
				return err
			}

			result = make([]actor.OutboxMessage, len(rows))
			for i, row := range rows {
				result[i] = actor.OutboxMessage{
					ID:            row.ID,
					SourceActorID: row.SourceActorID,
					TargetActorID: row.TargetActorID,
					MessageType:   row.MessageType,
					Payload:       row.Payload,
					DomainKey: fromNullString(
						row.DomainKey,
					),
					Version: int64(row.Version),
					Status:  row.Status,
					DeliveryAttempts: int(
						row.DeliveryAttempts,
					),
					ClaimToken: fromNullString(
						row.ClaimToken,
					),
					CreatedAt: time.Unix(row.CreatedAt, 0),
				}
			}

			return nil
		},
	)

	return result, err
}

// CompleteOutbox marks an outbox message as successfully delivered. The
// claim token must match the token set during ClaimOutboxBatch.
func (s *Store) CompleteOutbox(
	ctx context.Context, id, claimToken string,
) error {

	writeTxOpts := db.WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			return q.CompleteOutboxMessage(
				ctx,
				CompleteOutboxParams{
					ID: id,
					CompletedAt: toNullInt64(
						s.clock.Now().Unix(),
					),
					ClaimToken: toNullString(
						claimToken,
					),
				},
			)
		},
	)
}

// FailOutbox marks an outbox message as failed (dead letter). The claim
// token must match the token set during ClaimOutboxBatch.
func (s *Store) FailOutbox(
	ctx context.Context, id, claimToken string,
) error {

	writeTxOpts := db.WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			return q.FailOutboxMessage(ctx, FailOutboxParams{
				ID: id,
				CompletedAt: toNullInt64(
					s.clock.Now().Unix(),
				),
				ClaimToken: toNullString(claimToken),
			})
		},
	)
}

// IsProcessed checks if a message has already been processed.
func (s *Store) IsProcessed(
	ctx context.Context, id string,
) (bool, error) {

	readTxOpts := db.ReadTxOption()

	var processed bool

	err := s.db.ExecTx(ctx, readTxOpts, func(q ActorDeliveryQueries) error {
		var err error
		processed, err = q.IsMessageProcessed(ctx, id)

		return err
	})

	return processed, err
}

// MarkProcessed records that a message has been processed.
func (s *Store) MarkProcessed(
	ctx context.Context,
	id, actorID string,
	ttl time.Duration,
) error {

	writeTxOpts := db.WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			now := s.clock.Now()
			expiresAt := now.Add(ttl)

			return q.MarkMessageProcessed(ctx, MarkProcessedParams{
				ID:          id,
				ActorID:     actorID,
				ProcessedAt: now.Unix(),
				ExpiresAt:   expiresAt.Unix(),
			})
		},
	)
}

// SaveCheckpoint saves or updates an FSM state checkpoint.
func (s *Store) SaveCheckpoint(
	ctx context.Context, params actor.CheckpointParams,
) error {

	writeTxOpts := db.WriteTxOption()

	err := s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			return q.SaveFSMCheckpoint(ctx, SaveCheckpointParams{
				ActorID:   params.ActorID,
				StateType: params.StateType,
				StateData: params.StateData,
				Version:   int32(params.Version),
				UpdatedAt: s.clock.Now().Unix(),
			})
		},
	)

	return err
}

// LoadCheckpoint loads an FSM checkpoint for an actor.
func (s *Store) LoadCheckpoint(
	ctx context.Context, actorID string,
) (*actor.Checkpoint, error) {

	readTxOpts := db.ReadTxOption()

	var result *actor.Checkpoint

	err := s.db.ExecTx(ctx, readTxOpts, func(q ActorDeliveryQueries) error {
		row, err := q.GetFSMCheckpoint(ctx, actorID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}

			return err
		}

		result = &actor.Checkpoint{
			ActorID:   row.ActorID,
			StateType: row.StateType,
			StateData: row.StateData,
			Version:   int64(row.Version),
			UpdatedAt: time.Unix(row.UpdatedAt, 0),
		}

		return nil
	})

	return result, err
}

// DeleteCheckpoint removes an FSM checkpoint.
func (s *Store) DeleteCheckpoint(
	ctx context.Context, actorID string,
) error {

	writeTxOpts := db.WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			return q.DeleteFSMCheckpoint(ctx, actorID)
		},
	)
}

// GetDeadLetter retrieves a specific dead letter message.
func (s *Store) GetDeadLetter(
	ctx context.Context, id string,
) (*actor.DeadLetter, error) {

	readTxOpts := db.ReadTxOption()

	var result *actor.DeadLetter

	err := s.db.ExecTx(ctx, readTxOpts, func(q ActorDeliveryQueries) error {
		row, err := q.GetDeadLetter(ctx, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}

			return err
		}

		result = &actor.DeadLetter{
			ID:            row.ID,
			Source:        row.Source,
			ActorID:       row.ActorID,
			MessageType:   row.MessageType,
			Payload:       row.Payload,
			FailureReason: row.FailureReason,
			Attempts:      int(row.Attempts),
			CreatedAt:     time.Unix(row.CreatedAt, 0),
		}

		return nil
	})

	return result, err
}

// ListDeadLetters lists dead letters for an actor with pagination.
func (s *Store) ListDeadLetters(
	ctx context.Context, actorID string, limit int,
) ([]actor.DeadLetter, error) {

	readTxOpts := db.ReadTxOption()

	var result []actor.DeadLetter

	err := s.db.ExecTx(ctx, readTxOpts, func(q ActorDeliveryQueries) error {
		rows, err := q.ListDeadLettersByActor(
			ctx,
			ListDeadLettersParams{
				ActorID: actorID,
				Limit:   int32(limit),
			},
		)
		if err != nil {
			return err
		}

		result = make([]actor.DeadLetter, len(rows))
		for i, row := range rows {
			createdAt := time.Unix(row.CreatedAt, 0)

			result[i] = actor.DeadLetter{
				ID:            row.ID,
				Source:        row.Source,
				ActorID:       row.ActorID,
				MessageType:   row.MessageType,
				Payload:       row.Payload,
				FailureReason: row.FailureReason,
				Attempts:      int(row.Attempts),
				CreatedAt:     createdAt,
			}
		}

		return nil
	})

	return result, err
}

// DeleteDeadLetter removes a dead letter after manual processing.
func (s *Store) DeleteDeadLetter(
	ctx context.Context, id string,
) error {

	writeTxOpts := db.WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			return q.DeleteDeadLetter(ctx, id)
		},
	)
}

// ExpireLeases releases all expired leases so messages can be redelivered.
func (s *Store) ExpireLeases(ctx context.Context) error {
	writeTxOpts := db.WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			return q.ExpireMailboxLeases(
				ctx, toNullInt64(s.clock.Now().Unix()),
			)
		},
	)
}

// CleanupExpired removes expired deduplication entries and ask results.
func (s *Store) CleanupExpired(ctx context.Context) error {
	writeTxOpts := db.WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			now := s.clock.Now().Unix()

			// Cleanup expired deduplication entries.
			if err := q.CleanupExpiredProcessedMessages(
				ctx, now,
			); err != nil {
				return err
			}

			// Cleanup expired Ask results.
			return q.CleanupExpiredAskResults(ctx, now)
		},
	)
}

// Helper functions for SQL type conversions.

// toNullString converts a string to sql.NullString.
func toNullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{Valid: false}
	}

	return sql.NullString{String: s, Valid: true}
}

// fromNullString converts sql.NullString to string.
func fromNullString(ns sql.NullString) string {
	if !ns.Valid {
		return ""
	}

	return ns.String
}

// toNullInt64 converts an int64 to sql.NullInt64.
func toNullInt64(i int64) sql.NullInt64 {
	return sql.NullInt64{Int64: i, Valid: true}
}

// fromNullInt64Time converts sql.NullInt64 (Unix timestamp) to time.Time.
func fromNullInt64Time(ni sql.NullInt64) time.Time {
	if !ni.Valid {
		return time.Time{}
	}

	return time.Unix(ni.Int64, 0)
}

// TxActorDeliveryStore is a transaction-scoped version of Store.
// It wraps a specific transaction and provides DeliveryStore operations within
// that transaction scope.
type TxActorDeliveryStore struct {
	querier ActorDeliveryQueries
	clock   clock.Clock
	tx      *sql.Tx
}

// newTxActorDeliveryStore creates a new transaction-scoped delivery store.
func newTxActorDeliveryStore(
	querier ActorDeliveryQueries, clock clock.Clock, tx *sql.Tx,
) *TxActorDeliveryStore {

	return &TxActorDeliveryStore{
		querier: querier,
		clock:   clock,
		tx:      tx,
	}
}

// Tx returns the underlying database transaction.
func (s *TxActorDeliveryStore) Tx() *sql.Tx {
	return s.tx
}

// EnqueueMessage persists a new message to an actor's mailbox.
func (s *TxActorDeliveryStore) EnqueueMessage(
	ctx context.Context, params actor.EnqueueParams,
) error {

	return s.querier.EnqueueMailboxMessage(ctx, EnqueueMailboxParams{
		ID:              params.ID,
		MailboxID:       params.MailboxID,
		MessageType:     params.MessageType,
		Payload:         params.Payload,
		PromiseID:       toNullString(params.PromiseID),
		CallbackActorID: toNullString(params.CallbackActorID),
		CorrelationID:   toNullString(params.CorrelationID),
		Priority:        int32(params.Priority),
		AvailableAt:     params.AvailableAt.Unix(),
		MaxAttempts:     int32(params.MaxAttempts),
		CreatedAt:       s.clock.Now().Unix(),
	})
}

// LeaseNextMessage atomically claims the next available message for processing.
func (s *TxActorDeliveryStore) LeaseNextMessage(
	ctx context.Context,
	mailboxID string,
	leaseToken string,
	leaseDuration time.Duration,
) (*actor.LeasedMessage, error) {

	now := s.clock.Now()
	leaseUntil := now.Add(leaseDuration)

	msg, err := s.querier.LeaseNextMailboxMessage(ctx, LeaseMailboxParams{
		MailboxID:   mailboxID,
		LeaseToken:  toNullString(leaseToken),
		LeaseUntil:  toNullInt64(leaseUntil.Unix()),
		AvailableAt: now.Unix(),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}

		return nil, err
	}

	return &actor.LeasedMessage{
		ID:              msg.ID,
		MailboxID:       msg.MailboxID,
		MessageType:     msg.MessageType,
		Payload:         msg.Payload,
		PromiseID:       fromNullString(msg.PromiseID),
		CallbackActorID: fromNullString(msg.CallbackActorID),
		CorrelationID:   fromNullString(msg.CorrelationID),
		Priority:        int(msg.Priority),
		LeaseToken:      fromNullString(msg.LeaseToken),
		LeaseUntil:      fromNullInt64Time(msg.LeaseUntil),
		Attempts:        int(msg.Attempts),
		MaxAttempts:     int(msg.MaxAttempts),
		CreatedAt:       time.Unix(msg.CreatedAt, 0),
	}, nil
}

// AckMessage acknowledges successful processing of a message.
func (s *TxActorDeliveryStore) AckMessage(
	ctx context.Context, id, leaseToken string,
) (int64, error) {

	return s.querier.AckMailboxMessage(ctx, AckMailboxParams{
		ID:         id,
		LeaseToken: toNullString(leaseToken),
	})
}

// NackMessage releases a message for redelivery after the specified delay.
func (s *TxActorDeliveryStore) NackMessage(
	ctx context.Context,
	id, leaseToken string,
	retryAfter time.Duration,
) (int64, error) {

	availableAt := s.clock.Now().Add(retryAfter)

	return s.querier.NackMailboxMessage(ctx, NackMailboxParams{
		ID:          id,
		LeaseToken:  toNullString(leaseToken),
		AvailableAt: availableAt.Unix(),
	})
}

// ExtendLease extends the lease for long-running message processing.
func (s *TxActorDeliveryStore) ExtendLease(
	ctx context.Context,
	id, leaseToken string,
	extension time.Duration,
) (int64, error) {

	leaseUntil := s.clock.Now().Add(extension)

	return s.querier.ExtendMailboxLease(ctx, ExtendMailboxParams{
		ID:         id,
		LeaseToken: toNullString(leaseToken),
		LeaseUntil: toNullInt64(leaseUntil.Unix()),
	})
}

// MoveToDeadLetter moves a failed message to the dead letter queue.
func (s *TxActorDeliveryStore) MoveToDeadLetter(
	ctx context.Context, id, reason string,
) error {

	err := s.querier.MoveMailboxToDeadLetter(ctx, DeadLetterInsertParams{
		ID:            id,
		FailureReason: reason,
		CreatedAt:     s.clock.Now().Unix(),
	})
	if err != nil {
		return err
	}

	return s.querier.DeleteMailboxMessage(ctx, id)
}

// DeleteMessage removes a message from the mailbox.
func (s *TxActorDeliveryStore) DeleteMessage(
	ctx context.Context, id string,
) error {

	return s.querier.DeleteMailboxMessage(ctx, id)
}

// SaveAskResult persists the result of an Ask message.
func (s *TxActorDeliveryStore) SaveAskResult(
	ctx context.Context, params actor.AskResultParams,
) error {

	return s.querier.InsertAskResult(ctx, InsertAskResultParams{
		PromiseID:  params.PromiseID,
		ResultBlob: params.ResultBlob,
		ErrorText:  toNullString(params.ErrorText),
		CreatedAt:  s.clock.Now().Unix(),
		ExpiresAt:  params.ExpiresAt.Unix(),
	})
}

// GetAskResult retrieves the result of an Ask message.
func (s *TxActorDeliveryStore) GetAskResult(
	ctx context.Context, promiseID string,
) (*actor.AskResult, error) {

	row, err := s.querier.GetAskResult(ctx, promiseID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}

		return nil, err
	}

	return &actor.AskResult{
		PromiseID:  row.PromiseID,
		ResultBlob: row.ResultBlob,
		ErrorText:  fromNullString(row.ErrorText),
		CreatedAt:  time.Unix(row.CreatedAt, 0),
		ExpiresAt:  time.Unix(row.ExpiresAt, 0),
	}, nil
}

// DeleteAskResult removes an Ask result after retrieval.
func (s *TxActorDeliveryStore) DeleteAskResult(
	ctx context.Context, promiseID string,
) error {

	return s.querier.DeleteAskResult(ctx, promiseID)
}

// EnqueueOutbox adds a message to the transactional outbox.
func (s *TxActorDeliveryStore) EnqueueOutbox(
	ctx context.Context, params actor.OutboxParams,
) error {

	return s.querier.EnqueueOutboxMessage(ctx, EnqueueOutboxParams{
		ID:            params.ID,
		SourceActorID: params.SourceActorID,
		TargetActorID: params.TargetActorID,
		MessageType:   params.MessageType,
		Payload:       params.Payload,
		DomainKey:     toNullString(params.DomainKey),
		Version:       int32(params.Version),
		CreatedAt:     s.clock.Now().Unix(),
	})
}

// ClaimOutboxBatch claims a batch of pending outbox messages for delivery.
func (s *TxActorDeliveryStore) ClaimOutboxBatch(
	ctx context.Context, params actor.OutboxClaimParams,
) ([]actor.OutboxMessage, error) {

	now := s.clock.Now()
	claimedUntil := now.Add(params.ClaimDuration)

	rows, err := s.querier.ClaimOutboxBatch(
		ctx, ClaimOutboxBatchParams{
			Limit:          int32(params.Limit),
			ClaimToken:     toNullString(params.ClaimToken),
			ClaimedUntil:   toNullInt64(claimedUntil.Unix()),
			ClaimedUntil_2: toNullInt64(now.Unix()),
		},
	)
	if err != nil {
		return nil, err
	}

	result := make([]actor.OutboxMessage, len(rows))
	for i, row := range rows {
		result[i] = actor.OutboxMessage{
			ID:               row.ID,
			SourceActorID:    row.SourceActorID,
			TargetActorID:    row.TargetActorID,
			MessageType:      row.MessageType,
			Payload:          row.Payload,
			DomainKey:        fromNullString(row.DomainKey),
			Version:          int64(row.Version),
			Status:           row.Status,
			DeliveryAttempts: int(row.DeliveryAttempts),
			ClaimToken:       fromNullString(row.ClaimToken),
			CreatedAt:        time.Unix(row.CreatedAt, 0),
		}
	}

	return result, nil
}

// CompleteOutbox marks an outbox message as successfully delivered. The
// claim token must match the token set during ClaimOutboxBatch.
func (s *TxActorDeliveryStore) CompleteOutbox(
	ctx context.Context, id, claimToken string,
) error {

	return s.querier.CompleteOutboxMessage(ctx, CompleteOutboxParams{
		ID:          id,
		CompletedAt: toNullInt64(s.clock.Now().Unix()),
		ClaimToken:  toNullString(claimToken),
	})
}

// FailOutbox marks an outbox message as failed.
func (s *TxActorDeliveryStore) FailOutbox(
	ctx context.Context, id, claimToken string,
) error {

	return s.querier.FailOutboxMessage(ctx, FailOutboxParams{
		ID:          id,
		CompletedAt: toNullInt64(s.clock.Now().Unix()),
		ClaimToken:  toNullString(claimToken),
	})
}

// IsProcessed checks if a message has already been processed.
func (s *TxActorDeliveryStore) IsProcessed(
	ctx context.Context, id string,
) (bool, error) {

	return s.querier.IsMessageProcessed(ctx, id)
}

// MarkProcessed records that a message has been processed.
func (s *TxActorDeliveryStore) MarkProcessed(
	ctx context.Context,
	id, actorID string,
	ttl time.Duration,
) error {

	now := s.clock.Now()
	expiresAt := now.Add(ttl)

	return s.querier.MarkMessageProcessed(ctx, MarkProcessedParams{
		ID:          id,
		ActorID:     actorID,
		ProcessedAt: now.Unix(),
		ExpiresAt:   expiresAt.Unix(),
	})
}

// SaveCheckpoint saves or updates an FSM state checkpoint.
func (s *TxActorDeliveryStore) SaveCheckpoint(
	ctx context.Context, params actor.CheckpointParams,
) error {

	return s.querier.SaveFSMCheckpoint(ctx, SaveCheckpointParams{
		ActorID:   params.ActorID,
		StateType: params.StateType,
		StateData: params.StateData,
		Version:   int32(params.Version),
		UpdatedAt: s.clock.Now().Unix(),
	})
}

// LoadCheckpoint loads an FSM checkpoint for an actor.
func (s *TxActorDeliveryStore) LoadCheckpoint(
	ctx context.Context, actorID string,
) (*actor.Checkpoint, error) {

	row, err := s.querier.GetFSMCheckpoint(ctx, actorID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}

		return nil, err
	}

	return &actor.Checkpoint{
		ActorID:   row.ActorID,
		StateType: row.StateType,
		StateData: row.StateData,
		Version:   int64(row.Version),
		UpdatedAt: time.Unix(row.UpdatedAt, 0),
	}, nil
}

// DeleteCheckpoint removes an FSM checkpoint.
func (s *TxActorDeliveryStore) DeleteCheckpoint(
	ctx context.Context, actorID string,
) error {

	return s.querier.DeleteFSMCheckpoint(ctx, actorID)
}

// GetDeadLetter retrieves a specific dead letter message.
func (s *TxActorDeliveryStore) GetDeadLetter(
	ctx context.Context, id string,
) (*actor.DeadLetter, error) {

	row, err := s.querier.GetDeadLetter(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}

		return nil, err
	}

	return &actor.DeadLetter{
		ID:            row.ID,
		Source:        row.Source,
		ActorID:       row.ActorID,
		MessageType:   row.MessageType,
		Payload:       row.Payload,
		FailureReason: row.FailureReason,
		Attempts:      int(row.Attempts),
		CreatedAt:     time.Unix(row.CreatedAt, 0),
	}, nil
}

// ListDeadLetters lists dead letters for an actor with pagination.
func (s *TxActorDeliveryStore) ListDeadLetters(
	ctx context.Context, actorID string, limit int,
) ([]actor.DeadLetter, error) {

	rows, err := s.querier.ListDeadLettersByActor(
		ctx,
		ListDeadLettersParams{
			ActorID: actorID,
			Limit:   int32(limit),
		},
	)
	if err != nil {
		return nil, err
	}

	result := make([]actor.DeadLetter, len(rows))
	for i, row := range rows {
		result[i] = actor.DeadLetter{
			ID:            row.ID,
			Source:        row.Source,
			ActorID:       row.ActorID,
			MessageType:   row.MessageType,
			Payload:       row.Payload,
			FailureReason: row.FailureReason,
			Attempts:      int(row.Attempts),
			CreatedAt:     time.Unix(row.CreatedAt, 0),
		}
	}

	return result, nil
}

// DeleteDeadLetter removes a dead letter after manual processing.
func (s *TxActorDeliveryStore) DeleteDeadLetter(
	ctx context.Context, id string,
) error {

	return s.querier.DeleteDeadLetter(ctx, id)
}

// ExpireLeases releases all expired leases so messages can be redelivered.
func (s *TxActorDeliveryStore) ExpireLeases(ctx context.Context) error {
	return s.querier.ExpireMailboxLeases(
		ctx, toNullInt64(s.clock.Now().Unix()),
	)
}

// CleanupExpired removes expired deduplication entries and ask results.
func (s *TxActorDeliveryStore) CleanupExpired(ctx context.Context) error {
	now := s.clock.Now().Unix()

	err := s.querier.CleanupExpiredProcessedMessages(ctx, now)
	if err != nil {
		return err
	}

	return s.querier.CleanupExpiredAskResults(ctx, now)
}

// Compile-time check that TxActorDeliveryStore implements actor.DeliveryStore.
var _ actor.DeliveryStore = (*TxActorDeliveryStore)(nil)

// TxAwareActorDeliveryStore extends Store with transaction
// execution support for atomic multi-operation workflows.
type TxAwareActorDeliveryStore struct {
	*Store
	querier db.BatchedQuerier
}

// NewTxAwareActorDeliveryStore creates a new transaction-aware delivery store.
func NewTxAwareActorDeliveryStore(
	db BatchedActorDeliveryQueries,
	querier db.BatchedQuerier,
	clock clock.Clock,
) *TxAwareActorDeliveryStore {

	return &TxAwareActorDeliveryStore{
		Store:   NewStore(db, clock),
		querier: querier,
	}
}

// ExecTx executes a function within a database transaction. The TxFunc receives
// a context with the transaction attached (via WithTx) and a transaction-scoped
// DeliveryStore. All operations within the function participate in the same
// transaction.
func (s *TxAwareActorDeliveryStore) ExecTx(
	ctx context.Context, readOnly bool, fn actor.TxFunc,
) error {

	var txOpts db.TxOptions
	if readOnly {
		txOpts = db.ReadTxOption()
	} else {
		txOpts = db.WriteTxOption()
	}

	tx, err := s.querier.BeginTx(ctx, txOpts)
	if err != nil {
		return err
	}

	defer func() {
		_ = tx.Rollback()
	}()

	// Create a transaction-scoped queries object.
	txQuerier := adsqlc.New(tx)
	txStore := newTxActorDeliveryStore(txQuerier, s.clock, tx)

	// Attach transaction to context.
	txCtx := actor.WithTx(ctx, tx)

	// Execute the function with the transaction-scoped store.
	if err := fn(txCtx, txStore); err != nil {
		return err
	}

	return tx.Commit()
}

// Compile-time check that Store implements actor.DeliveryStore.
var _ actor.DeliveryStore = (*Store)(nil)

// Compile-time check that TxAwareActorDeliveryStore implements
// actor.TxAwareDeliveryStore.
var _ actor.TxAwareDeliveryStore = (*TxAwareActorDeliveryStore)(nil)

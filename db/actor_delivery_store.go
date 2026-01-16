package db

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
)

// Type aliases for SQLC-generated types to reduce import noise.
type (
	MailboxMsgRow          = sqlc.MailboxMessage
	OutboxMsgRow           = sqlc.OutboxMessage
	AskResultRow           = sqlc.AskResult
	FsmCheckpointRow       = sqlc.FsmCheckpoint
	DeadLetterRow          = sqlc.DeadLetter
	EnqueueMailboxParams   = sqlc.EnqueueMailboxMessageParams
	EnqueueOutboxParams    = sqlc.EnqueueOutboxMessageParams
	LeaseMailboxParams     = sqlc.LeaseNextMailboxMessageParams
	AckMailboxParams       = sqlc.AckMailboxMessageParams
	NackMailboxParams      = sqlc.NackMailboxMessageParams
	ExtendMailboxParams    = sqlc.ExtendMailboxLeaseParams
	InsertAskResultParams  = sqlc.InsertAskResultParams
	CompleteOutboxParams   = sqlc.CompleteOutboxMessageParams
	FailOutboxParams       = sqlc.FailOutboxMessageParams
	MarkProcessedParams    = sqlc.MarkMessageProcessedParams
	SaveCheckpointParams   = sqlc.SaveFSMCheckpointParams
	DeadLetterInsertParams = sqlc.MoveMailboxToDeadLetterParams
	ListDeadLettersParams  = sqlc.ListDeadLettersByActorParams
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
	ExpireMailboxLeases(ctx context.Context, leaseUntil sql.NullInt32) error

	// Ask result operations.
	InsertAskResult(ctx context.Context, arg InsertAskResultParams) error
	GetAskResult(ctx context.Context,
		promiseID string) (AskResultRow, error)
	DeleteAskResult(ctx context.Context, promiseID string) error

	// Outbox operations.
	EnqueueOutboxMessage(ctx context.Context, arg EnqueueOutboxParams) error
	ClaimOutboxBatch(ctx context.Context,
		limit int32) ([]OutboxMsgRow, error)
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
		expiresAt int32) error
	CleanupExpiredAskResults(ctx context.Context, expiresAt int32) error
}

// BatchedActorDeliveryQueries combines ActorDeliveryQueries with transaction
// support via the BatchedTx generic interface. This enables atomic operations
// across multiple queries.
type BatchedActorDeliveryQueries interface {
	ActorDeliveryQueries
	BatchedTx[ActorDeliveryQueries]
}

// ActorDeliveryStore implements the actor.DeliveryStore interface using the
// BatchedTx pattern for transaction-safe operations. All methods execute within
// database transactions with automatic retry on serialization errors.
type ActorDeliveryStore struct {
	db    BatchedActorDeliveryQueries
	clock clock.Clock
}

// NewActorDeliveryStore creates a new actor delivery store using the
// transaction executor pattern.
func NewActorDeliveryStore(
	db BatchedActorDeliveryQueries, clock clock.Clock,
) *ActorDeliveryStore {

	return &ActorDeliveryStore{
		db:    db,
		clock: clock,
	}
}

// EnqueueMessage persists a new message to an actor's mailbox.
func (s *ActorDeliveryStore) EnqueueMessage(
	ctx context.Context, params actor.EnqueueParams,
) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts,
		func(q ActorDeliveryQueries) error {
			createdAt := int32(s.clock.Now().Unix())

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
					Priority: int32(params.Priority),
					AvailableAt: int32(
						params.AvailableAt.Unix(),
					),
					MaxAttempts: int32(params.MaxAttempts),
					CreatedAt:   createdAt,
				},
			)
		})
}

// LeaseNextMessage atomically claims the next available message for processing.
func (s *ActorDeliveryStore) LeaseNextMessage(
	ctx context.Context,
	mailboxID string,
	leaseToken string,
	leaseDuration time.Duration,
) (*actor.LeasedMessage, error) {

	writeTxOpts := WriteTxOption()

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
					LeaseUntil: toNullInt32(
						int32(leaseUntil.Unix()),
					),
					AvailableAt: int32(now.Unix()),
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
			leaseUntilTime := fromNullInt32Time(msg.LeaseUntil)
			createdAt := time.Unix(int64(msg.CreatedAt), 0)

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
func (s *ActorDeliveryStore) AckMessage(
	ctx context.Context, id, leaseToken string,
) (int64, error) {

	writeTxOpts := WriteTxOption()

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
func (s *ActorDeliveryStore) NackMessage(
	ctx context.Context,
	id, leaseToken string,
	retryAfter time.Duration,
) (int64, error) {

	writeTxOpts := WriteTxOption()

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
				AvailableAt: int32(availableAt.Unix()),
			})

			return err
		},
	)

	return rows, err
}

// ExtendLease extends the lease for long-running message processing.
func (s *ActorDeliveryStore) ExtendLease(
	ctx context.Context,
	id, leaseToken string,
	extension time.Duration,
) (int64, error) {

	writeTxOpts := WriteTxOption()

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
					LeaseUntil: toNullInt32(
						int32(leaseUntil.Unix()),
					),
				},
			)

			return err
		},
	)

	return rows, err
}

// MoveToDeadLetter moves a failed message to the dead letter queue.
func (s *ActorDeliveryStore) MoveToDeadLetter(
	ctx context.Context, id, reason string,
) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			createdAt := int32(s.clock.Now().Unix())

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
func (s *ActorDeliveryStore) DeleteMessage(
	ctx context.Context, id string,
) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			return q.DeleteMailboxMessage(ctx, id)
		},
	)
}

// SaveAskResult persists the result of an Ask message for caller retrieval.
func (s *ActorDeliveryStore) SaveAskResult(
	ctx context.Context, params actor.AskResultParams,
) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			return q.InsertAskResult(ctx, InsertAskResultParams{
				PromiseID:  params.PromiseID,
				ResultBlob: params.ResultBlob,
				ErrorText:  toNullString(params.ErrorText),
				CreatedAt:  int32(s.clock.Now().Unix()),
				ExpiresAt:  int32(params.ExpiresAt.Unix()),
			})
		},
	)
}

// GetAskResult retrieves the result of an Ask message.
func (s *ActorDeliveryStore) GetAskResult(
	ctx context.Context, promiseID string,
) (*actor.AskResult, error) {

	readTxOpts := ReadTxOption()

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
			CreatedAt:  time.Unix(int64(row.CreatedAt), 0),
			ExpiresAt:  time.Unix(int64(row.ExpiresAt), 0),
		}

		return nil
	})

	return result, err
}

// DeleteAskResult removes an Ask result after retrieval.
func (s *ActorDeliveryStore) DeleteAskResult(
	ctx context.Context, promiseID string,
) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			return q.DeleteAskResult(ctx, promiseID)
		},
	)
}

// EnqueueOutbox adds a message to the transactional outbox.
func (s *ActorDeliveryStore) EnqueueOutbox(
	ctx context.Context, params actor.OutboxParams,
) error {

	writeTxOpts := WriteTxOption()

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
				CreatedAt:     int32(s.clock.Now().Unix()),
			})
		},
	)
}

// ClaimOutboxBatch claims a batch of pending outbox messages for delivery.
func (s *ActorDeliveryStore) ClaimOutboxBatch(
	ctx context.Context, limit int,
) ([]actor.OutboxMessage, error) {

	writeTxOpts := WriteTxOption()

	var result []actor.OutboxMessage

	err := s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			rows, err := q.ClaimOutboxBatch(ctx, int32(limit))
			if err != nil {
				return err
			}

			result = make([]actor.OutboxMessage, len(rows))
			for i, row := range rows {
				createdAt := time.Unix(int64(row.CreatedAt), 0)
				domainKey := fromNullString(row.DomainKey)
				deliveryAttempts := int(row.DeliveryAttempts)

				result[i] = actor.OutboxMessage{
					ID:               row.ID,
					SourceActorID:    row.SourceActorID,
					TargetActorID:    row.TargetActorID,
					MessageType:      row.MessageType,
					Payload:          row.Payload,
					DomainKey:        domainKey,
					Version:          int64(row.Version),
					Status:           row.Status,
					DeliveryAttempts: deliveryAttempts,
					CreatedAt:        createdAt,
				}
			}

			return nil
		},
	)

	return result, err
}

// CompleteOutbox marks an outbox message as successfully delivered.
func (s *ActorDeliveryStore) CompleteOutbox(
	ctx context.Context, id string,
) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			return q.CompleteOutboxMessage(
				ctx,
				CompleteOutboxParams{
					ID: id,
					CompletedAt: toNullInt32(
						int32(s.clock.Now().Unix()),
					),
				},
			)
		},
	)
}

// FailOutbox marks an outbox message as failed (dead letter).
func (s *ActorDeliveryStore) FailOutbox(
	ctx context.Context, id string,
) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			return q.FailOutboxMessage(ctx, FailOutboxParams{
				ID: id,
				CompletedAt: toNullInt32(
					int32(s.clock.Now().Unix()),
				),
			})
		},
	)
}

// IsProcessed checks if a message has already been processed.
func (s *ActorDeliveryStore) IsProcessed(
	ctx context.Context, id string,
) (bool, error) {

	readTxOpts := ReadTxOption()

	var processed bool

	err := s.db.ExecTx(ctx, readTxOpts, func(q ActorDeliveryQueries) error {
		var err error
		processed, err = q.IsMessageProcessed(ctx, id)

		return err
	})

	return processed, err
}

// MarkProcessed records that a message has been processed.
func (s *ActorDeliveryStore) MarkProcessed(
	ctx context.Context,
	id, actorID string,
	ttl time.Duration,
) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			now := s.clock.Now()
			expiresAt := now.Add(ttl)

			return q.MarkMessageProcessed(ctx, MarkProcessedParams{
				ID:          id,
				ActorID:     actorID,
				ProcessedAt: int32(now.Unix()),
				ExpiresAt:   int32(expiresAt.Unix()),
			})
		},
	)
}

// SaveCheckpoint saves or updates an FSM state checkpoint.
func (s *ActorDeliveryStore) SaveCheckpoint(
	ctx context.Context, params actor.CheckpointParams,
) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			return q.SaveFSMCheckpoint(ctx, SaveCheckpointParams{
				ActorID:   params.ActorID,
				StateType: params.StateType,
				StateData: params.StateData,
				Version:   int32(params.Version),
				UpdatedAt: int32(s.clock.Now().Unix()),
			})
		},
	)
}

// LoadCheckpoint loads an FSM checkpoint for an actor.
func (s *ActorDeliveryStore) LoadCheckpoint(
	ctx context.Context, actorID string,
) (*actor.Checkpoint, error) {

	readTxOpts := ReadTxOption()

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
			UpdatedAt: time.Unix(int64(row.UpdatedAt), 0),
		}

		return nil
	})

	return result, err
}

// DeleteCheckpoint removes an FSM checkpoint.
func (s *ActorDeliveryStore) DeleteCheckpoint(
	ctx context.Context, actorID string,
) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			return q.DeleteFSMCheckpoint(ctx, actorID)
		},
	)
}

// GetDeadLetter retrieves a specific dead letter message.
func (s *ActorDeliveryStore) GetDeadLetter(
	ctx context.Context, id string,
) (*actor.DeadLetter, error) {

	readTxOpts := ReadTxOption()

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
			CreatedAt:     time.Unix(int64(row.CreatedAt), 0),
		}

		return nil
	})

	return result, err
}

// ListDeadLetters lists dead letters for an actor with pagination.
func (s *ActorDeliveryStore) ListDeadLetters(
	ctx context.Context, actorID string, limit int,
) ([]actor.DeadLetter, error) {

	readTxOpts := ReadTxOption()

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
			createdAt := time.Unix(int64(row.CreatedAt), 0)

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
func (s *ActorDeliveryStore) DeleteDeadLetter(
	ctx context.Context, id string,
) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			return q.DeleteDeadLetter(ctx, id)
		},
	)
}

// ExpireLeases releases all expired leases so messages can be redelivered.
func (s *ActorDeliveryStore) ExpireLeases(ctx context.Context) error {
	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			return q.ExpireMailboxLeases(
				ctx, toNullInt32(int32(s.clock.Now().Unix())),
			)
		},
	)
}

// CleanupExpired removes expired deduplication entries and ask results.
func (s *ActorDeliveryStore) CleanupExpired(ctx context.Context) error {
	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			now := int32(s.clock.Now().Unix())

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

// toNullInt32 converts an int32 to sql.NullInt32.
func toNullInt32(i int32) sql.NullInt32 {
	return sql.NullInt32{Int32: i, Valid: true}
}

// fromNullInt32Time converts sql.NullInt32 (Unix timestamp) to time.Time.
func fromNullInt32Time(ni sql.NullInt32) time.Time {
	if !ni.Valid {
		return time.Time{}
	}

	return time.Unix(int64(ni.Int32), 0)
}

// TxActorDeliveryStore is a transaction-scoped version of ActorDeliveryStore.
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
		AvailableAt:     int32(params.AvailableAt.Unix()),
		MaxAttempts:     int32(params.MaxAttempts),
		CreatedAt:       int32(s.clock.Now().Unix()),
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
		LeaseUntil:  toNullInt32(int32(leaseUntil.Unix())),
		AvailableAt: int32(now.Unix()),
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
		LeaseUntil:      fromNullInt32Time(msg.LeaseUntil),
		Attempts:        int(msg.Attempts),
		MaxAttempts:     int(msg.MaxAttempts),
		CreatedAt:       time.Unix(int64(msg.CreatedAt), 0),
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
		AvailableAt: int32(availableAt.Unix()),
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
		LeaseUntil: toNullInt32(int32(leaseUntil.Unix())),
	})
}

// MoveToDeadLetter moves a failed message to the dead letter queue.
func (s *TxActorDeliveryStore) MoveToDeadLetter(
	ctx context.Context, id, reason string,
) error {

	err := s.querier.MoveMailboxToDeadLetter(ctx, DeadLetterInsertParams{
		ID:            id,
		FailureReason: reason,
		CreatedAt:     int32(s.clock.Now().Unix()),
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
		CreatedAt:  int32(s.clock.Now().Unix()),
		ExpiresAt:  int32(params.ExpiresAt.Unix()),
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
		CreatedAt:  time.Unix(int64(row.CreatedAt), 0),
		ExpiresAt:  time.Unix(int64(row.ExpiresAt), 0),
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
		CreatedAt:     int32(s.clock.Now().Unix()),
	})
}

// ClaimOutboxBatch claims a batch of pending outbox messages for delivery.
func (s *TxActorDeliveryStore) ClaimOutboxBatch(
	ctx context.Context, limit int,
) ([]actor.OutboxMessage, error) {

	rows, err := s.querier.ClaimOutboxBatch(ctx, int32(limit))
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
			CreatedAt:        time.Unix(int64(row.CreatedAt), 0),
		}
	}

	return result, nil
}

// CompleteOutbox marks an outbox message as successfully delivered.
func (s *TxActorDeliveryStore) CompleteOutbox(
	ctx context.Context, id string,
) error {

	return s.querier.CompleteOutboxMessage(ctx, CompleteOutboxParams{
		ID:          id,
		CompletedAt: toNullInt32(int32(s.clock.Now().Unix())),
	})
}

// FailOutbox marks an outbox message as failed.
func (s *TxActorDeliveryStore) FailOutbox(
	ctx context.Context, id string,
) error {

	return s.querier.FailOutboxMessage(ctx, FailOutboxParams{
		ID:          id,
		CompletedAt: toNullInt32(int32(s.clock.Now().Unix())),
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
		ProcessedAt: int32(now.Unix()),
		ExpiresAt:   int32(expiresAt.Unix()),
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
		UpdatedAt: int32(s.clock.Now().Unix()),
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
		UpdatedAt: time.Unix(int64(row.UpdatedAt), 0),
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
		CreatedAt:     time.Unix(int64(row.CreatedAt), 0),
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
			CreatedAt:     time.Unix(int64(row.CreatedAt), 0),
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
		ctx, toNullInt32(int32(s.clock.Now().Unix())),
	)
}

// CleanupExpired removes expired deduplication entries and ask results.
func (s *TxActorDeliveryStore) CleanupExpired(ctx context.Context) error {
	now := int32(s.clock.Now().Unix())

	err := s.querier.CleanupExpiredProcessedMessages(ctx, now)
	if err != nil {
		return err
	}

	return s.querier.CleanupExpiredAskResults(ctx, now)
}

// Compile-time check that TxActorDeliveryStore implements actor.DeliveryStore.
var _ actor.DeliveryStore = (*TxActorDeliveryStore)(nil)

// TxAwareActorDeliveryStore extends ActorDeliveryStore with transaction
// execution support for atomic multi-operation workflows.
type TxAwareActorDeliveryStore struct {
	*ActorDeliveryStore
	querier BatchedQuerier
}

// NewTxAwareActorDeliveryStore creates a new transaction-aware delivery store.
func NewTxAwareActorDeliveryStore(
	db BatchedActorDeliveryQueries,
	querier BatchedQuerier,
	clock clock.Clock,
) *TxAwareActorDeliveryStore {

	return &TxAwareActorDeliveryStore{
		ActorDeliveryStore: NewActorDeliveryStore(db, clock),
		querier:            querier,
	}
}

// ExecTx executes a function within a database transaction. The TxFunc receives
// a context with the transaction attached (via WithTx) and a transaction-scoped
// DeliveryStore. All operations within the function participate in the same
// transaction.
func (s *TxAwareActorDeliveryStore) ExecTx(
	ctx context.Context, readOnly bool, fn actor.TxFunc,
) error {

	var txOpts TxOptions
	if readOnly {
		txOpts = ReadTxOption()
	} else {
		txOpts = WriteTxOption()
	}

	tx, err := s.querier.BeginTx(ctx, txOpts)
	if err != nil {
		return err
	}

	defer func() {
		_ = tx.Rollback()
	}()

	// Create a transaction-scoped queries object.
	txQuerier := sqlc.New(tx)
	txStore := newTxActorDeliveryStore(txQuerier, s.clock, tx)

	// Attach transaction to context.
	txCtx := actor.WithTx(ctx, tx)

	// Execute the function with the transaction-scoped store.
	if err := fn(txCtx, txStore); err != nil {
		return err
	}

	return tx.Commit()
}

// Compile-time check that ActorDeliveryStore implements actor.DeliveryStore.
var _ actor.DeliveryStore = (*ActorDeliveryStore)(nil)

// Compile-time check that TxAwareActorDeliveryStore implements
// actor.TxAwareDeliveryStore.
var _ actor.TxAwareDeliveryStore = (*TxAwareActorDeliveryStore)(nil)

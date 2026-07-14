package actordelivery

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
	adsqlc "github.com/lightninglabs/wavelength/db/actordelivery/sqlc"
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
	PeekMailboxParams      = adsqlc.PeekNextMailboxMessageParams
	AckMailboxParams       = adsqlc.AckMailboxMessageParams
	NackMailboxParams      = adsqlc.NackMailboxMessageParams
	NackMailboxByIDParams  = adsqlc.NackMailboxMessageByIDParams
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

	LeaseNextMailboxMessage(ctx context.Context,
		arg LeaseMailboxParams) (MailboxMsgRow, error)

	PeekNextMailboxMessage(ctx context.Context,
		arg PeekMailboxParams) (MailboxMsgRow, error)

	AckMailboxMessage(ctx context.Context,
		arg AckMailboxParams) (int64, error)

	AckMailboxMessageByID(ctx context.Context, id string) (int64, error)

	NackMailboxMessage(ctx context.Context,
		arg NackMailboxParams) (int64, error)

	NackMailboxMessageByID(ctx context.Context,
		arg NackMailboxByIDParams) (int64, error)

	ExtendMailboxLease(ctx context.Context,
		arg ExtendMailboxParams) (int64, error)

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

	GetFSMCheckpoint(ctx context.Context,
		actorID string) (FsmCheckpointRow, error)

	DeleteFSMCheckpoint(ctx context.Context, actorID string) error

	// Dead letter operations.
	MoveMailboxToDeadLetter(
		ctx context.Context, arg DeadLetterInsertParams,
	) error

	GetDeadLetter(ctx context.Context, id string) (DeadLetterRow, error)

	ListDeadLettersByActor(ctx context.Context,
		arg ListDeadLettersParams) ([]DeadLetterRow, error)

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

	outboxWakeMu sync.Mutex
	outboxWakes  []func()

	mailboxWakeMu     sync.Mutex
	mailboxWakeNextID uint64

	// mailboxWakes holds the live post-commit mailbox wakes, keyed first by
	// target mailbox ID and then by a unique registration handle.
	// notifyMailboxWake fires only the wakes registered for the mailboxes
	// named in a committed transaction's enqueue set, so an idle mailbox is
	// never roused by an unrelated enqueue. The inner handle map keeps a
	// restart that reuses a durable mailbox ID from clobbering a still-live
	// registration: each RegisterMailboxWake adds its own handle, and its
	// cancel deletes exactly that handle, pruning the mailbox's entry when
	// its last wake is removed so a stopped mailbox leaves nothing behind.
	mailboxWakes map[string]map[uint64]func()
}

// NewStore creates a new actor delivery store using the
// transaction executor pattern.
func NewStore(
	db BatchedActorDeliveryQueries, clock clock.Clock,
) *Store {

	return &Store{
		db:           db,
		clock:        clock,
		mailboxWakes: make(map[string]map[uint64]func()),
	}
}

// EnqueueMessage persists a new message to an actor's mailbox.
func (s *Store) EnqueueMessage(
	ctx context.Context, params actor.EnqueueParams,
) error {

	writeTxOpts := db.WriteTxOption()

	err := s.db.ExecTx(ctx, writeTxOpts,
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
					CorrelationKey: toNullString(
						params.CorrelationKey,
					),
				},
			)
		})
	if err != nil {
		return err
	}

	// When this enqueue joined an ambient ExecTx transaction (the folded
	// outbox-delivery path, where the target actor's store enqueues through
	// the shared tx via TransactionExecutor.ExecTx), record the target
	// mailbox in the transaction's enqueue set so ExecTx fires a
	// post-commit wake at exactly that mailbox. The tx-scoped
	// TxActorDeliveryStore records the same set directly; this covers the
	// join path that bypasses it. Outside an ExecTx this is a no-op.
	noteMailboxEnqueued(ctx, params.MailboxID)

	return nil
}

// LeaseNextMessage atomically claims the next available message for processing.
func (s *Store) LeaseNextMessage(ctx context.Context, mailboxID string,
	leaseToken string, leaseDuration time.Duration) (*actor.LeasedMessage,
	error) {

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

// leasedMessageFromRow maps a SQLC mailbox row to an actor.LeasedMessage.
func leasedMessageFromRow(msg MailboxMsgRow) *actor.LeasedMessage {
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
	}
}

// leaselessMessageFromRow maps a SQLC mailbox row to a peeked delivery. A
// peeked row may still carry stale, expired lease metadata from an older leased
// claim; the actor-layer contract for PeekNextMessage is always an empty token,
// which routes ack/nack to the by-ID leaseless operations.
func leaselessMessageFromRow(msg MailboxMsgRow) *actor.LeasedMessage {
	leased := leasedMessageFromRow(msg)
	leased.LeaseToken = ""
	leased.LeaseUntil = time.Time{}

	return leased
}

// PeekNextMessage claims the next available message with a read-only query and
// takes no lease. The returned LeasedMessage carries an empty LeaseToken, which
// signals the single-worker consume path to ack/nack via the unfenced by-ID
// operations. It runs under a read transaction (no fsync), eliminating the
// write transaction that LeaseNextMessage performs.
func (s *Store) PeekNextMessage(ctx context.Context, mailboxID string) (
	*actor.LeasedMessage, error) {

	readTxOpts := db.ReadTxOption()

	var result *actor.LeasedMessage

	err := s.db.ExecTx(ctx, readTxOpts,
		func(q ActorDeliveryQueries) error {
			now := s.clock.Now()

			msg, err := q.PeekNextMailboxMessage(
				ctx,
				PeekMailboxParams{
					MailboxID:   mailboxID,
					AvailableAt: now.Unix(),
				},
			)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return nil
				}

				return err
			}

			result = leaselessMessageFromRow(msg)

			return nil
		})

	return result, err
}

// AckMessage acknowledges successful processing of a message.
func (s *Store) AckMessage(ctx context.Context, id, leaseToken string) (int64,
	error) {

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

// AckMessageByID acknowledges successful processing of a message by ID without
// validating a lease token. It is the leaseless single-worker counterpart to
// AckMessage.
func (s *Store) AckMessageByID(ctx context.Context, id string) (int64, error) {
	writeTxOpts := db.WriteTxOption()

	var rows int64

	err := s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			var err error
			rows, err = q.AckMailboxMessageByID(ctx, id)

			return err
		},
	)

	return rows, err
}

// NackMessage releases a message for redelivery after the specified delay.
func (s *Store) NackMessage(ctx context.Context, id, leaseToken string,
	retryAfter time.Duration) (int64, error) {

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

// NackMessageByID releases a message for redelivery by ID without validating a
// lease token, and increments attempts. It is the leaseless single-worker
// counterpart to NackMessage. The attempts bump preserves dead-lettering on
// max attempts because the leaseless peek does not increment attempts.
func (s *Store) NackMessageByID(ctx context.Context, id string,
	retryAfter time.Duration) (int64, error) {

	writeTxOpts := db.WriteTxOption()

	var rows int64

	err := s.db.ExecTx(
		ctx,
		writeTxOpts,
		func(q ActorDeliveryQueries) error {
			availableAt := s.clock.Now().Add(retryAfter)

			var err error
			rows, err = q.NackMailboxMessageByID(
				ctx,
				NackMailboxByIDParams{
					ID:          id,
					AvailableAt: availableAt.Unix(),
				},
			)

			return err
		},
	)

	return rows, err
}

// ExtendLease extends the lease for long-running message processing.
func (s *Store) ExtendLease(ctx context.Context, id, leaseToken string,
	extension time.Duration) (int64, error) {

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
func (s *Store) GetAskResult(ctx context.Context, promiseID string) (
	*actor.AskResult, error) {

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

	err := s.db.ExecTx(
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
	if err != nil {
		return err
	}

	s.notifyOutboxWake()

	return nil
}

// RegisterOutboxWake registers a same-process callback that runs after outbox
// work commits. The publisher still polls as the durable fallback.
func (s *Store) RegisterOutboxWake(wake func()) {
	if wake == nil {
		return
	}

	s.outboxWakeMu.Lock()
	defer s.outboxWakeMu.Unlock()

	s.outboxWakes = append(s.outboxWakes, wake)
}

func (s *Store) notifyOutboxWake() {
	s.outboxWakeMu.Lock()
	wakes := append([]func(){}, s.outboxWakes...)
	s.outboxWakeMu.Unlock()

	for _, wake := range wakes {
		wake()
	}
}

// RegisterMailboxWake registers a same-process callback for mailboxID that runs
// after an enqueue to that mailbox commits inside an ExecTx, and returns a
// cancel function that deregisters it. It restores immediate delivery for the
// folded outbox path, where the enqueued row is invisible to a consumer until
// the publisher's transaction commits. The wake is targeted: notifyMailboxWake
// rouses only the mailboxes named in the committed transaction's enqueue set,
// so an idle mailbox is never woken by an unrelated enqueue. The mailbox still
// polls as the durable, cross-process, and restart fallback.
func (s *Store) RegisterMailboxWake(mailboxID string, wake func()) func() {
	if wake == nil {
		return func() {}
	}

	s.mailboxWakeMu.Lock()
	id := s.mailboxWakeNextID
	s.mailboxWakeNextID++
	wakes := s.mailboxWakes[mailboxID]
	if wakes == nil {
		wakes = make(map[uint64]func())
		s.mailboxWakes[mailboxID] = wakes
	}
	wakes[id] = wake
	s.mailboxWakeMu.Unlock()

	// The cancel deletes exactly this registration's handle and prunes the
	// mailbox's entry once its last wake is gone, so a stopped mailbox
	// (DurableMailbox.Close) leaves no closure behind even when a later
	// mailbox reuses the same durable ID.
	return func() {
		s.mailboxWakeMu.Lock()
		if wakes, ok := s.mailboxWakes[mailboxID]; ok {
			delete(wakes, id)
			if len(wakes) == 0 {
				delete(s.mailboxWakes, mailboxID)
			}
		}
		s.mailboxWakeMu.Unlock()
	}
}

// notifyMailboxWake fires the post-commit wakes for exactly the mailboxes named
// in mailboxIDs (a committed transaction's enqueue set). Mailboxes that
// received nothing in the transaction are left untouched.
func (s *Store) notifyMailboxWake(mailboxIDs map[string]struct{}) {
	s.mailboxWakeMu.Lock()
	var wakes []func()
	for mailboxID := range mailboxIDs {
		for _, wake := range s.mailboxWakes[mailboxID] {
			wakes = append(wakes, wake)
		}
	}
	s.mailboxWakeMu.Unlock()

	for _, wake := range wakes {
		wake()
	}
}

// ClaimOutboxBatch claims a batch of pending outbox messages for delivery.
func (s *Store) ClaimOutboxBatch(ctx context.Context,
	params actor.OutboxClaimParams) ([]actor.OutboxMessage, error) {

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
func (s *Store) IsProcessed(ctx context.Context, id string) (bool, error) {
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
func (s *Store) LoadCheckpoint(ctx context.Context, actorID string) (
	*actor.Checkpoint, error) {

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
func (s *Store) GetDeadLetter(ctx context.Context, id string) (
	*actor.DeadLetter, error) {

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
func (s *Store) ListDeadLetters(ctx context.Context, actorID string,
	limit int) ([]actor.DeadLetter, error) {

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
				ctx,
				toNullInt64(
					s.clock.Now().Unix(),
				),
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

	outboxEnqueued *bool
}

// newTxActorDeliveryStore creates a new transaction-scoped delivery store.
func newTxActorDeliveryStore(
	querier ActorDeliveryQueries, clock clock.Clock, tx *sql.Tx,
	outboxEnqueued *bool,
) *TxActorDeliveryStore {

	return &TxActorDeliveryStore{
		querier:        querier,
		clock:          clock,
		tx:             tx,
		outboxEnqueued: outboxEnqueued,
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

	if err := s.querier.EnqueueMailboxMessage(ctx, EnqueueMailboxParams{
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
		CorrelationKey:  toNullString(params.CorrelationKey),
	}); err != nil {
		return err
	}

	// Record the target mailbox in the transaction's enqueue set so ExecTx
	// fires a post-commit wake at exactly that mailbox. The enqueued row is
	// invisible to the consumer until commit, so the in-process wake from
	// DurableMailbox.Send cannot rouse it; this is the signal that restores
	// immediate same-process delivery.
	noteMailboxEnqueued(ctx, params.MailboxID)

	return nil
}

// LeaseNextMessage atomically claims the next available message for processing.
func (s *TxActorDeliveryStore) LeaseNextMessage(ctx context.Context,
	mailboxID string, leaseToken string, leaseDuration time.Duration) (
	*actor.LeasedMessage, error) {

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

// PeekNextMessage claims the next available message with a read-only query and
// takes no lease, within the current transaction. The returned LeasedMessage
// carries an empty LeaseToken, signalling the unfenced by-ID ack/nack path.
func (s *TxActorDeliveryStore) PeekNextMessage(ctx context.Context,
	mailboxID string) (*actor.LeasedMessage, error) {

	now := s.clock.Now()

	msg, err := s.querier.PeekNextMailboxMessage(ctx, PeekMailboxParams{
		MailboxID:   mailboxID,
		AvailableAt: now.Unix(),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}

		return nil, err
	}

	return leaselessMessageFromRow(msg), nil
}

// AckMessage acknowledges successful processing of a message.
func (s *TxActorDeliveryStore) AckMessage(ctx context.Context, id,
	leaseToken string) (int64, error) {

	return s.querier.AckMailboxMessage(ctx, AckMailboxParams{
		ID:         id,
		LeaseToken: toNullString(leaseToken),
	})
}

// AckMessageByID acknowledges successful processing of a message by ID without
// validating a lease token, within the current transaction. It is the leaseless
// single-worker counterpart to AckMessage.
func (s *TxActorDeliveryStore) AckMessageByID(ctx context.Context, id string) (
	int64, error) {

	return s.querier.AckMailboxMessageByID(ctx, id)
}

// NackMessage releases a message for redelivery after the specified delay.
func (s *TxActorDeliveryStore) NackMessage(ctx context.Context, id,
	leaseToken string, retryAfter time.Duration) (int64, error) {

	availableAt := s.clock.Now().Add(retryAfter)

	return s.querier.NackMailboxMessage(ctx, NackMailboxParams{
		ID:          id,
		LeaseToken:  toNullString(leaseToken),
		AvailableAt: availableAt.Unix(),
	})
}

// NackMessageByID releases a message for redelivery by ID without validating a
// lease token, and increments attempts, within the current transaction. It is
// the leaseless single-worker counterpart to NackMessage.
func (s *TxActorDeliveryStore) NackMessageByID(ctx context.Context, id string,
	retryAfter time.Duration) (int64, error) {

	availableAt := s.clock.Now().Add(retryAfter)

	return s.querier.NackMailboxMessageByID(ctx, NackMailboxByIDParams{
		ID:          id,
		AvailableAt: availableAt.Unix(),
	})
}

// ExtendLease extends the lease for long-running message processing.
func (s *TxActorDeliveryStore) ExtendLease(ctx context.Context, id,
	leaseToken string, extension time.Duration) (int64, error) {

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
func (s *TxActorDeliveryStore) GetAskResult(ctx context.Context,
	promiseID string) (*actor.AskResult, error) {

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

	err := s.querier.EnqueueOutboxMessage(ctx, EnqueueOutboxParams{
		ID:            params.ID,
		SourceActorID: params.SourceActorID,
		TargetActorID: params.TargetActorID,
		MessageType:   params.MessageType,
		Payload:       params.Payload,
		DomainKey:     toNullString(params.DomainKey),
		Version:       int32(params.Version),
		CreatedAt:     s.clock.Now().Unix(),
	})
	if err != nil {
		return err
	}

	if s.outboxEnqueued != nil {
		*s.outboxEnqueued = true
	}

	return nil
}

// ClaimOutboxBatch claims a batch of pending outbox messages for delivery.
func (s *TxActorDeliveryStore) ClaimOutboxBatch(ctx context.Context,
	params actor.OutboxClaimParams) ([]actor.OutboxMessage, error) {

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
func (s *TxActorDeliveryStore) IsProcessed(ctx context.Context, id string) (
	bool, error) {

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
func (s *TxActorDeliveryStore) LoadCheckpoint(ctx context.Context,
	actorID string) (*actor.Checkpoint, error) {

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
func (s *TxActorDeliveryStore) GetDeadLetter(ctx context.Context, id string) (
	*actor.DeadLetter, error) {

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
func (s *TxActorDeliveryStore) ListDeadLetters(ctx context.Context,
	actorID string, limit int) ([]actor.DeadLetter, error) {

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
		ctx,
		toNullInt64(
			s.clock.Now().Unix(),
		),
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
	outboxEnqueued := false
	txStore := newTxActorDeliveryStore(
		txQuerier, s.clock, tx, &outboxEnqueued,
	)

	// Attach the transaction and a fresh mailbox-enqueue set to the
	// context. The set captures the target mailbox ID of every enqueue
	// regardless of store path, including those that join the ambient tx
	// via TransactionExecutor.ExecTx (the folded outbox-delivery path,
	// where the target actor's store enqueues through
	// (*Store).EnqueueMessage rather than the tx-scoped
	// TxActorDeliveryStore).
	txCtx := actor.WithTx(ctx, tx)
	txCtx, mailboxEnqueued := withMailboxEnqueueSet(txCtx)

	// Execute the function with the transaction-scoped store.
	if err := fn(txCtx, txStore); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	if outboxEnqueued {
		s.notifyOutboxWake()
	}

	// Fire a targeted post-commit wake at exactly the mailboxes that
	// received a message in this transaction. The enqueued rows only became
	// visible at commit, so this rouses their consumers immediately instead
	// of leaving them to wait out a poll interval. Only the named mailboxes
	// re-poll, so the ~K idle durable actors are no longer roused on every
	// unrelated commit (the dominant poll cost under load). This restores
	// the same-process delivery latency that the folded outbox path
	// otherwise regresses.
	if len(mailboxEnqueued) > 0 {
		s.notifyMailboxWake(mailboxEnqueued)
	}

	return nil
}

// Compile-time check that Store implements actor.DeliveryStore.
var _ actor.DeliveryStore = (*Store)(nil)

// Compile-time check that Store can wake same-process outbox publishers.
var _ actor.OutboxWakeRegistrar = (*Store)(nil)

// Compile-time check that Store can wake same-process mailbox receive loops
// after a folded outbox enqueue commits.
var _ actor.MailboxWakeRegistrar = (*Store)(nil)

// Compile-time check that TxAwareActorDeliveryStore implements
// actor.TxAwareDeliveryStore.
var _ actor.TxAwareDeliveryStore = (*TxAwareActorDeliveryStore)(nil)

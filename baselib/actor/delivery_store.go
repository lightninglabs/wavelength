package actor

import (
	"context"
	"time"
)

// DeliveryStore defines the persistence operations for durable mailboxes.
// Implementations should ensure all operations are atomic and handle
// concurrent access safely. Operations that accept a context should use
// any transaction present via TxFromContext.
type DeliveryStore interface {
	// ===== Mailbox Operations =====

	// EnqueueMessage persists a new message to an actor's mailbox.
	EnqueueMessage(ctx context.Context, params EnqueueParams) error

	// LeaseNextMessage atomically claims the next available message for
	// processing. Sets the lease token and expiry, increments attempts.
	// Returns nil if no messages are available.
	LeaseNextMessage(ctx context.Context, mailboxID string,
		leaseToken string,
		leaseDuration time.Duration) (*LeasedMessage, error)

	// PeekNextMessage claims the next available message with a READ-only
	// query and takes NO lease: it does not set a lease token or expiry and
	// does not increment attempts. It is the leaseless fast path for
	// single-worker (NumWorkers == 1) actors, which have no competing
	// consumer to fence the ack against. The returned LeasedMessage carries
	// an EMPTY LeaseToken, which signals the consume path to ack/nack via
	// the unfenced by-ID operations. Eligibility and ordering match
	// LeaseNextMessage exactly. Returns nil if no messages are available.
	PeekNextMessage(ctx context.Context,
		mailboxID string) (*LeasedMessage, error)

	// AckMessage acknowledges successful processing of a message.
	// Validates the lease token to prevent stale acks. Returns the number
	// of rows affected (0 if token mismatch, 1 if success).
	AckMessage(ctx context.Context, id, leaseToken string) (int64, error)

	// AckMessageByID acknowledges successful processing of a message by ID
	// WITHOUT validating a lease token. It is the leaseless single-worker
	// counterpart to AckMessage. Returns the number of rows affected (1 on
	// success, 0 if the row was already gone). MUST NOT be used by the
	// multi-worker path, which relies on lease-token fencing.
	AckMessageByID(ctx context.Context, id string) (int64, error)

	// NackMessage releases a message for redelivery after the specified
	// delay. Clears the lease and sets a new available_at time.
	// Validates the lease token to prevent stale nacks.
	NackMessage(ctx context.Context, id, leaseToken string,
		retryAfter time.Duration) (int64, error)

	// NackMessageByID releases a message for redelivery by ID WITHOUT
	// validating a lease token, and increments attempts. It is the
	// leaseless single-worker counterpart to NackMessage. The attempts bump
	// is required because the leaseless peek does not increment attempts,
	// so the failure path must, to preserve dead-lettering on max attempts.
	// Returns the number of rows affected.
	NackMessageByID(ctx context.Context, id string,
		retryAfter time.Duration) (int64, error)

	// ExtendLease extends the lease for long-running message processing.
	// Validates the lease token to prevent stale extensions.
	ExtendLease(ctx context.Context, id, leaseToken string,
		extension time.Duration) (int64, error)

	// MoveToDeadLetter moves a failed message to the dead letter queue.
	MoveToDeadLetter(ctx context.Context, id, reason string) error

	// DeleteMessage removes a message from the mailbox (cleanup).
	DeleteMessage(ctx context.Context, id string) error

	// ===== Ask Result Operations =====

	// SaveAskResult persists the result of an Ask message for caller
	// retrieval.
	SaveAskResult(ctx context.Context, params AskResultParams) error

	// GetAskResult retrieves the result of an Ask message.
	GetAskResult(ctx context.Context, promiseID string) (*AskResult, error)

	// DeleteAskResult removes an Ask result after retrieval.
	DeleteAskResult(ctx context.Context, promiseID string) error

	// ===== Outbox Operations (CDC) =====

	// EnqueueOutbox adds a message to the transactional outbox.
	// Should be called within the same transaction as FSM state changes.
	EnqueueOutbox(ctx context.Context, params OutboxParams) error

	// ClaimOutboxBatch claims a batch of pending outbox messages for
	// delivery. Sets a claim token and lease duration to prevent
	// concurrent publishers from processing the same messages.
	ClaimOutboxBatch(ctx context.Context,
		params OutboxClaimParams) ([]OutboxMessage, error)

	// CompleteOutbox marks an outbox message as successfully delivered.
	// The claim token must match the token set during ClaimOutboxBatch.
	CompleteOutbox(ctx context.Context, id, claimToken string) error

	// FailOutbox marks an outbox message as failed (dead letter).
	// The claim token must match the token set during ClaimOutboxBatch.
	FailOutbox(ctx context.Context, id, claimToken string) error

	// ===== Deduplication Operations =====

	// IsProcessed checks if a message has already been processed.
	IsProcessed(ctx context.Context, id string) (bool, error)

	// MarkProcessed records that a message has been processed.
	MarkProcessed(
		ctx context.Context,
		id, actorID string,
		ttl time.Duration,
	) error

	// ===== Checkpoint Operations =====

	// SaveCheckpoint saves or updates an FSM state checkpoint.
	SaveCheckpoint(ctx context.Context, params CheckpointParams) error

	// LoadCheckpoint loads an FSM checkpoint for an actor.
	LoadCheckpoint(ctx context.Context, actorID string) (*Checkpoint, error)

	// DeleteCheckpoint removes an FSM checkpoint.
	DeleteCheckpoint(ctx context.Context, actorID string) error

	// ===== Dead Letter Operations =====

	// GetDeadLetter retrieves a specific dead letter message.
	GetDeadLetter(ctx context.Context, id string) (*DeadLetter, error)

	// ListDeadLetters lists dead letters for an actor with pagination.
	ListDeadLetters(ctx context.Context, actorID string,
		limit int) ([]DeadLetter, error)

	// DeleteDeadLetter removes a dead letter after manual processing.
	DeleteDeadLetter(ctx context.Context, id string) error

	// ===== Maintenance Operations =====

	// ExpireLeases releases all expired leases so messages can be
	// redelivered.
	ExpireLeases(ctx context.Context) error

	// CleanupExpired removes expired deduplication entries and ask results.
	CleanupExpired(ctx context.Context) error
}

// ackMessage acknowledges a delivery, picking the fenced or unfenced store
// operation by whether a lease token is present. A leaseless (peeked) delivery
// carries an empty token and acks by ID; a leased delivery acks under its lease
// fence. Centralizing the choice here keeps every ack call site -- the commit
// fold, the non-tx tail, the classic tx path, and the duplicate-skip path --
// consistent without each branching on the token.
func ackMessage(ctx context.Context, store DeliveryStore, id,
	leaseToken string) (int64, error) {

	if leaseToken == "" {
		return store.AckMessageByID(ctx, id)
	}

	return store.AckMessage(ctx, id, leaseToken)
}

// nackMessage releases a delivery for redelivery, picking the fenced or
// unfenced store operation by whether a lease token is present. The unfenced
// by-ID path additionally increments attempts, which the leaseless peek does
// not do, so dead-lettering on max attempts is preserved.
func nackMessage(ctx context.Context, store DeliveryStore, id,
	leaseToken string, retryAfter time.Duration) (int64, error) {

	if leaseToken == "" {
		return store.NackMessageByID(ctx, id, retryAfter)
	}

	return store.NackMessage(ctx, id, leaseToken, retryAfter)
}

// OutboxWakeRegistrar is optionally implemented by stores that can notify a
// same-process publisher after new outbox work commits. Polling remains the
// cross-process and restart fallback.
type OutboxWakeRegistrar interface {
	RegisterOutboxWake(func())
}

// EnqueueParams contains parameters for enqueueing a mailbox message.
type EnqueueParams struct {
	// ID is the unique message identifier (ULID recommended).
	ID string

	// MailboxID identifies the target actor's mailbox.
	MailboxID string

	// MessageType is the type name for deserialization.
	MessageType string

	// Payload contains the TLV-encoded message data.
	Payload []byte

	// PromiseID is set for Ask messages (nil for Tell).
	PromiseID string

	// CallbackActorID is set for DurableAsk messages to route the response.
	// The response will be delivered to this actor's mailbox via outbox.
	// Empty for regular Ask/Tell messages.
	CallbackActorID string

	// CorrelationID links DurableAsk requests to their responses.
	// The response message will include this ID for matching.
	// Empty for regular Ask/Tell messages.
	CorrelationID string

	// Priority determines processing order (higher = more important).
	Priority int

	// AvailableAt is when the message becomes available for delivery.
	AvailableAt time.Time

	// MaxAttempts is the maximum delivery attempts before dead-lettering.
	MaxAttempts int

	// CorrelationKey is an optional per-message tag that participates in
	// the durable mailbox's per-key FIFO claim ordering. Non-empty keys
	// cause the claim path to refuse to return a message when an
	// earlier-enqueued same-key message is still in the queue, even if
	// the earlier message is in retry backoff. Empty means the message
	// is unkeyed and uses the existing global available_at order.
	CorrelationKey string
}

// LeasedMessage represents a message claimed from the mailbox.
type LeasedMessage struct {
	// ID is the unique message identifier.
	ID string

	// MailboxID identifies the actor's mailbox.
	MailboxID string

	// MessageType is the type name for deserialization.
	MessageType string

	// Payload contains the TLV-encoded message data.
	Payload []byte

	// PromiseID is set for Ask messages.
	PromiseID string

	// CallbackActorID is set for DurableAsk messages to route the response.
	CallbackActorID string

	// CorrelationID links DurableAsk requests to their responses.
	CorrelationID string

	// Priority is the message priority.
	Priority int

	// LeaseToken is the opaque token for ack/nack validation.
	LeaseToken string

	// LeaseUntil is when the lease expires.
	LeaseUntil time.Time

	// Attempts is the number of delivery attempts so far.
	Attempts int

	// MaxAttempts is the maximum allowed attempts.
	MaxAttempts int

	// CreatedAt is when the message was enqueued.
	CreatedAt time.Time
}

// AskResultParams contains parameters for saving an Ask result.
type AskResultParams struct {
	// PromiseID links to the original Ask message.
	PromiseID string

	// ResultBlob contains the TLV-encoded successful result (nil on error).
	ResultBlob []byte

	// ErrorText contains the error message if the request failed.
	ErrorText string

	// ExpiresAt is when this result can be garbage collected.
	ExpiresAt time.Time
}

// AskResult represents a persisted Ask result.
type AskResult struct {
	// PromiseID links to the original Ask message.
	PromiseID string

	// ResultBlob contains the TLV-encoded successful result.
	ResultBlob []byte

	// ErrorText contains the error message if failed.
	ErrorText string

	// CreatedAt is when the result was persisted.
	CreatedAt time.Time

	// ExpiresAt is when this result expires.
	ExpiresAt time.Time
}

// OutboxParams contains parameters for enqueueing an outbox message.
type OutboxParams struct {
	// ID is the unique message identifier (ULID recommended).
	ID string

	// SourceActorID identifies the actor that created this message.
	SourceActorID string

	// TargetActorID identifies the destination actor.
	TargetActorID string

	// MessageType is the type name for deserialization.
	MessageType string

	// Payload contains the TLV-encoded message data.
	Payload []byte

	// DomainKey is an optional natural idempotency key.
	DomainKey string

	// Version is a monotonic counter for ordering within a domain.
	Version int64
}

// OutboxClaimParams contains parameters for claiming outbox messages.
type OutboxClaimParams struct {
	// Limit is the maximum number of messages to claim.
	Limit int

	// ClaimToken is an opaque token identifying this publisher's claim.
	// CompleteOutbox/FailOutbox must present a matching token.
	ClaimToken string

	// ClaimDuration is how long the claim is valid. After expiry, the
	// messages become available for reclaim by another publisher.
	ClaimDuration time.Duration
}

// OutboxMessage represents a message in the transactional outbox.
type OutboxMessage struct {
	// ID is the unique message identifier.
	ID string

	// SourceActorID identifies the actor that created this message.
	SourceActorID string

	// TargetActorID identifies the destination actor.
	TargetActorID string

	// MessageType is the type name for deserialization.
	MessageType string

	// Payload contains the TLV-encoded message data.
	Payload []byte

	// DomainKey is the natural idempotency key.
	DomainKey string

	// Version is the monotonic version number.
	Version int64

	// Status is the delivery status (pending, completed, dead_letter).
	Status string

	// DeliveryAttempts is the number of delivery attempts.
	DeliveryAttempts int

	// ClaimToken is the opaque token set during claim.
	ClaimToken string

	// CreatedAt is when the message was enqueued.
	CreatedAt time.Time
}

// CheckpointParams contains parameters for saving an FSM checkpoint.
type CheckpointParams struct {
	// ActorID identifies the actor whose FSM state is checkpointed.
	ActorID string

	// StateType is the name of the current FSM state.
	StateType string

	// StateData contains the TLV-encoded state snapshot.
	StateData []byte

	// Version is a monotonic counter for conflict detection.
	Version int64
}

// Checkpoint represents a persisted FSM state checkpoint.
type Checkpoint struct {
	// ActorID identifies the actor.
	ActorID string

	// StateType is the name of the current FSM state.
	StateType string

	// StateData contains the TLV-encoded state snapshot.
	StateData []byte

	// Version is the checkpoint version.
	Version int64

	// UpdatedAt is when the checkpoint was last updated.
	UpdatedAt time.Time
}

// DeadLetter represents a failed message in the dead letter queue.
type DeadLetter struct {
	// ID is the original message identifier.
	ID string

	// Source indicates where the message originated: 'mailbox' or 'outbox'.
	Source string

	// ActorID identifies the target actor (mailbox) or source actor
	// (outbox).
	ActorID string

	// MessageType is the type name.
	MessageType string

	// Payload contains the original message data.
	Payload []byte

	// FailureReason describes why the message was dead-lettered.
	FailureReason string

	// Attempts is the number of delivery attempts.
	Attempts int

	// CreatedAt is when the message was dead-lettered.
	CreatedAt time.Time
}

// TxAwareDeliveryStore extends DeliveryStore with transaction execution
// support. This enables the DurableActor to wrap message processing in a
// database transaction and pass it to the behavior via context for atomic FSM
// updates.
type TxAwareDeliveryStore interface {
	DeliveryStore

	// ExecTx executes a function within a database transaction. The
	// provided TxFunc receives the transaction that should be attached to
	// the context via WithTx. If the function returns an error, the
	// transaction is rolled back; otherwise it is committed.
	//
	// The readOnly flag indicates whether the transaction should be
	// read-only.
	ExecTx(ctx context.Context, readOnly bool, fn TxFunc) error
}

// TxFunc is a function that executes within a database transaction.
// The provided DeliveryStore operates within that transaction.
type TxFunc func(ctx context.Context, store DeliveryStore) error

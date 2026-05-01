package actor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// ErrLeaseExpired indicates that an ack/nack/extend operation failed because
// the lease has expired or been claimed by another consumer.
var ErrLeaseExpired = fmt.Errorf("lease expired or claimed by another consumer")

// ErrAlreadyAcked indicates that a delivery has already been acknowledged.
var ErrAlreadyAcked = fmt.Errorf("delivery already acknowledged")

// Delivery wraps a message with lease-based acknowledgment semantics. The
// receiver must call Ack or Nack before the lease expires, otherwise the
// message will be redelivered to another consumer.
//
// This pattern ensures exactly-once processing semantics on top of at-least-once
// delivery. The lease token prevents stale acks from a previous consumer that
// crashed after processing but before acknowledging.
type Delivery[M TLVMessage, R any] struct {
	// ID is the unique identifier for this delivery.
	ID string

	// Message is the delivered message.
	Message M

	// Promise is set for Ask messages to complete with the response.
	// Nil for Tell (fire-and-forget) messages.
	Promise Promise[R]

	// CallerCtx preserves the original caller's context for deadline
	// propagation. Used when completing Ask promises.
	CallerCtx context.Context

	// CallbackActorID is set for DurableAsk to route the response.
	// The response will be delivered to this actor's mailbox via outbox.
	// Empty for regular Ask/Tell messages.
	CallbackActorID string

	// CorrelationID links DurableAsk requests to their responses.
	// The caller uses this to match responses to original requests.
	// Empty for regular Ask/Tell messages.
	CorrelationID string

	// LeaseToken is the opaque token that must match for ack/nack to succeed.
	LeaseToken string

	// LeaseUntil is the deadline by which Ack/Nack must be called.
	LeaseUntil time.Time

	// CreatedAt is when this delivery was first enqueued.
	CreatedAt time.Time

	// Attempts is the number of delivery attempts for this message.
	Attempts int

	// MaxAttempts is the maximum allowed attempts before dead-lettering.
	MaxAttempts int

	// store is the backing store for persisting ack/nack operations.
	store DeliveryStore

	// mu guards mutable fields (acked, LeaseUntil) that may be accessed
	// concurrently by the heartbeat goroutine (Extend) and the main
	// processing goroutine (Ack/Nack).
	mu sync.Mutex

	// acked tracks whether this delivery has been acknowledged.
	acked bool

	// deferPromise suppresses in-Ack promise completion when set. This
	// is used by the transaction path to defer promise completion until
	// after the transaction commits successfully.
	deferPromise bool
}

// IsAsk returns true if this delivery is for an Ask message (has a promise).
func (d *Delivery[M, R]) IsAsk() bool {
	return d.Promise != nil
}

// IsDurableAsk returns true if this is a DurableAsk message (has callback info).
// DurableAsk responses are delivered via the outbox to the callback actor.
func (d *Delivery[M, R]) IsDurableAsk() bool {
	return d.CallbackActorID != "" && d.CorrelationID != ""
}

// IsTell returns true if this delivery is for a Tell message (no promise).
func (d *Delivery[M, R]) IsTell() bool {
	return d.Promise == nil
}

// LeaseRemaining returns the time remaining on the lease.
func (d *Delivery[M, R]) LeaseRemaining() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()

	return time.Until(d.LeaseUntil)
}

// IsLeaseExpired returns true if the lease has expired.
func (d *Delivery[M, R]) IsLeaseExpired() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	return time.Now().After(d.LeaseUntil)
}

// ShouldDeadLetter returns true if this message should be moved to the dead
// letter queue (max attempts reached).
func (d *Delivery[M, R]) ShouldDeadLetter() bool {
	return d.Attempts >= d.MaxAttempts
}

// Ack marks the message as successfully processed.
//
// For Ask messages, the result is used to complete the in-memory promise. The
// error (if any) is persisted for crash recovery, but the successful result
// value is not currently persisted. Callers that require crash-safe
// request/response semantics should use the DurableAsk pattern.
// Returns an error if the lease has expired or been claimed by another consumer.
//
// The context should contain any transaction needed for atomic operations.
// If a transaction is present (via WithTx), the ack will be part of that
// transaction.
func (d *Delivery[M, R]) Ack(ctx context.Context, result fn.Result[R]) error {
	d.mu.Lock()
	if d.acked {
		d.mu.Unlock()

		return ErrAlreadyAcked
	}
	d.mu.Unlock()

	// Validate lease ownership by acking the mailbox message first.
	// This must happen before SaveAskResult to prevent stale lease
	// holders from persisting results: ask_results uses ON CONFLICT
	// DO NOTHING, so a stale write would silently block the valid
	// worker's result.
	rowsAffected, err := d.store.AckMessage(ctx, d.ID, d.LeaseToken)
	if err != nil {
		return fmt.Errorf("ack message: %w", err)
	}

	if rowsAffected == 0 {
		return ErrLeaseExpired
	}

	// For Ask messages, persist the result for crash recovery. This
	// runs after AckMessage so only the valid lease holder writes the
	// result.
	if d.IsAsk() && d.Promise != nil {
		var resultBlob []byte
		var errorText string

		if err := result.Err(); err != nil {
			errorText = err.Error()
		} else {
			// For standard Ask, only the success status is
			// persisted, not the result value itself. See the
			// doc comment above.
			resultBlob = nil
		}

		saveErr := d.store.SaveAskResult(ctx, AskResultParams{
			PromiseID:  d.ID,
			ResultBlob: resultBlob,
			ErrorText:  errorText,
			ExpiresAt:  time.Now().Add(24 * time.Hour),
		})
		if saveErr != nil {
			return fmt.Errorf("save ask result: %w", saveErr)
		}
	}

	d.mu.Lock()
	d.acked = true
	d.mu.Unlock()

	// Complete the in-memory promise only after the durable ack has
	// succeeded. This ensures callers never observe success for an
	// operation that was not durably committed. When deferPromise is
	// set, the caller (tx path) handles completion after commit.
	if d.IsAsk() && d.Promise != nil && !d.deferPromise {
		d.Promise.Complete(result)
	}

	return nil
}

// Nack releases the message back to the queue for redelivery. The retryAfter
// duration controls when the message becomes available again. Use this for
// transient failures that may succeed on retry.
//
// If the message has reached max attempts, it will be moved to the dead letter
// queue instead of being requeued.
func (d *Delivery[M, R]) Nack(
	ctx context.Context,
	err error,
	retryAfter time.Duration,
) error {

	d.mu.Lock()
	if d.acked {
		d.mu.Unlock()

		return ErrAlreadyAcked
	}
	d.mu.Unlock()

	// Check if we should dead-letter instead of retry.
	if d.ShouldDeadLetter() {
		reason := "max attempts reached"
		if err != nil {
			reason = fmt.Sprintf("max attempts reached: %v", err)
		}

		if dlErr := d.store.MoveToDeadLetter(ctx, d.ID, reason); dlErr != nil {
			return fmt.Errorf("move to dead letter: %w", dlErr)
		}

		if delErr := d.store.DeleteMessage(ctx, d.ID); delErr != nil {
			return fmt.Errorf("delete message after dead letter: %w", delErr)
		}

		d.mu.Lock()
		d.acked = true
		d.mu.Unlock()

		return nil
	}

	// Release the message for redelivery.
	rowsAffected, nackErr := d.store.NackMessage(
		ctx, d.ID, d.LeaseToken, retryAfter,
	)
	if nackErr != nil {
		return fmt.Errorf("nack message: %w", nackErr)
	}

	if rowsAffected == 0 {
		return ErrLeaseExpired
	}

	d.mu.Lock()
	d.acked = true
	d.mu.Unlock()

	return nil
}

// Extend prolongs the lease for long-running message processing. This should
// be called periodically for messages that take longer than the default lease
// duration. Returns an error if the lease has already expired.
func (d *Delivery[M, R]) Extend(ctx context.Context, extension time.Duration) error {
	d.mu.Lock()
	if d.acked {
		d.mu.Unlock()

		return ErrAlreadyAcked
	}
	d.mu.Unlock()

	rowsAffected, err := d.store.ExtendLease(
		ctx, d.ID, d.LeaseToken, extension,
	)
	if err != nil {
		return fmt.Errorf("extend lease: %w", err)
	}

	if rowsAffected == 0 {
		return ErrLeaseExpired
	}

	// Update local state under the lock since the heartbeat goroutine
	// may read LeaseUntil concurrently.
	d.mu.Lock()
	d.LeaseUntil = time.Now().Add(extension)
	d.mu.Unlock()

	return nil
}

// newDelivery creates a new Delivery from a LeasedMessage.
func newDelivery[M TLVMessage, R any](
	msg *LeasedMessage,
	decoded M,
	promise Promise[R],
	callerCtx context.Context,
	store DeliveryStore,
) *Delivery[M, R] {

	return &Delivery[M, R]{
		ID:              msg.ID,
		Message:         decoded,
		Promise:         promise,
		CallerCtx:       callerCtx,
		CallbackActorID: msg.CallbackActorID,
		CorrelationID:   msg.CorrelationID,
		LeaseToken:      msg.LeaseToken,
		LeaseUntil:      msg.LeaseUntil,
		CreatedAt:       msg.CreatedAt,
		Attempts:        msg.Attempts,
		MaxAttempts:     msg.MaxAttempts,
		store:           store,
		acked:           false,
	}
}

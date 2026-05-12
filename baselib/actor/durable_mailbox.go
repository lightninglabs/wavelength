package actor

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// isExpectedShutdownErr reports whether err is one of the well-known
// transient errors emitted by the durable-store / lease pipeline once
// teardown has begun (closed DB handle, cancelled ctx). The lease loop
// uses this to demote such errors to debug instead of warn-flooding test
// artifacts at the tail of every itest. The check is text-based because
// the underlying store is one of several backends and we deliberately
// avoid importing each driver here.
func isExpectedShutdownErr(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	msg := err.Error()
	hints := []string{
		"sql: database is closed",
		"database is closed",
		"sql: connection is already closed",
		"use of closed network connection",
	}
	for _, h := range hints {
		if strings.Contains(msg, h) {
			return true
		}
	}

	return false
}

// generateID generates a UUIDv7 which provides both uniqueness and
// time-ordering. UUIDv7 embeds a Unix timestamp in milliseconds in the most
// significant bits, ensuring that IDs generated later sort after IDs generated
// earlier. This is important for message ordering when priority and
// available_at are equal.
func generateID() string {
	return uuid.Must(uuid.NewV7()).String()
}

// DurableMailboxConfig contains configuration options for a DurableMailbox.
type DurableMailboxConfig struct {
	// MailboxID uniquely identifies this mailbox (typically the actor ID).
	MailboxID string

	// Store is the persistence layer for mailbox operations.
	Store DeliveryStore

	// Codec handles message serialization/deserialization.
	Codec *MessageCodec

	// Clock provides time for message timestamps. If None, uses
	// DefaultClock.
	Clock fn.Option[clock.Clock]

	// LeaseDuration is how long a message is leased to a consumer.
	// Default: 30 seconds.
	LeaseDuration time.Duration

	// PollInterval is how often to poll for new messages when empty.
	// Same-process sends wake the mailbox immediately, so polling is only
	// the fallback for missed wakes, restarts, and external enqueues.
	// Default: 1s.
	PollInterval time.Duration

	// MaxAttempts is the default maximum delivery attempts.
	// Default: 10.
	MaxAttempts int
}

// DefaultDurableMailboxConfig returns a config with sensible defaults.
func DefaultDurableMailboxConfig(mailboxID string, store DeliveryStore,
	codec *MessageCodec) DurableMailboxConfig {

	return DurableMailboxConfig{
		MailboxID:     mailboxID,
		Store:         store,
		Codec:         codec,
		LeaseDuration: 30 * time.Second,
		PollInterval:  time.Second,
		MaxAttempts:   10,
	}
}

// DurableMailbox implements the Mailbox interface with SQLite-backed
// persistence. It provides durable message storage with lease-based delivery
// semantics.
type DurableMailbox[M TLVMessage, R any] struct {
	cfg DurableMailboxConfig

	// clock is used for message timestamps. Stored separately to avoid
	// nil checks on every call.
	clock clock.Clock

	// closed indicates whether the mailbox has been closed.
	closed atomic.Bool

	// closeMu protects close operations.
	closeMu sync.RWMutex

	// wake signals the receive loop to poll immediately.
	wake chan struct{}

	// actorCtx is the actor's lifecycle context.
	actorCtx context.Context

	// promiseRegistry maps message IDs to in-flight promises for Ask
	// messages. This allows the delivery to complete the promise after
	// processing.
	promiseRegistry   map[string]any
	promiseRegistryMu sync.RWMutex
}

// NewDurableMailbox creates a new durable mailbox with the given configuration.
func NewDurableMailbox[M TLVMessage, R any](
	actorCtx context.Context,
	cfg DurableMailboxConfig,
) *DurableMailbox[M, R] {

	return &DurableMailbox[M, R]{
		cfg:             cfg,
		clock:           cfg.Clock.UnwrapOr(clock.NewDefaultClock()),
		wake:            make(chan struct{}, 1),
		actorCtx:        actorCtx,
		promiseRegistry: make(map[string]any),
	}
}

// Send attempts to send an envelope to the mailbox, blocking until either the
// envelope is accepted, the provided context is cancelled, or the actor's
// context is cancelled.
//
// The sender's database transaction (if any) is preserved in the context
// passed to EnqueueMessage. When both actors share the same SQLite
// database, ExecTx in the delivery store joins the outer transaction,
// making the enqueue atomic with the sender's state change. This
// eliminates the window where a crash could commit the sender's state
// but lose the enqueued message.
func (m *DurableMailbox[M, R]) Send(ctx context.Context,
	env envelope[M, R]) error {

	m.closeMu.RLock()
	defer m.closeMu.RUnlock()

	// Check lifecycle contexts before the closed flag so actor shutdown and
	// caller cancellation keep precedence over local mailbox state.
	select {
	case <-ctx.Done():
		return ctx.Err()

	case <-m.actorCtx.Done():
		return ErrActorTerminated

	default:
	}

	if m.closed.Load() {
		return ErrMailboxClosed
	}

	payload, err := m.cfg.Codec.Encode(env.message)
	if err != nil {
		return fmt.Errorf("encode mailbox message: %w", err)
	}

	// Use the outbox-propagated ID for receiver-side deduplication when
	// present, otherwise generate a fresh UUIDv7. The OutboxPublisher
	// injects the outbox row ID so that retry deliveries (when
	// CompleteOutbox fails after a successful Tell) produce the same
	// inbox message ID. The ON CONFLICT (id) DO NOTHING clause on
	// EnqueueMailboxMessage makes the duplicate insert a silent no-op.
	id, ok := OutboxIDFromContext(ctx)
	if !ok {
		id = generateID()
	}

	// Determine promise ID for Ask messages and register the promise.
	var promiseID string
	if env.promise != nil {
		promiseID = id

		// Register the promise for later retrieval when the message is
		// received from the database.
		m.promiseRegistryMu.Lock()
		m.promiseRegistry[id] = env.promise
		m.promiseRegistryMu.Unlock()
	}

	// Determine priority.
	priority := 0
	if pm, ok := any(env.message).(PriorityMessage); ok {
		priority = pm.Priority()
	}

	// Enqueue the message.
	params := EnqueueParams{
		ID:              id,
		MailboxID:       m.cfg.MailboxID,
		MessageType:     env.message.MessageType(),
		Payload:         payload,
		PromiseID:       promiseID,
		CallbackActorID: env.callbackActorID,
		CorrelationID:   env.correlationID,
		Priority:        priority,
		AvailableAt:     m.clock.Now(),
		MaxAttempts:     m.cfg.MaxAttempts,
	}

	// Allow the sender's transaction (if any) to flow through so
	// same-DB actors share the tx and the enqueue is atomic with
	// the sender's state change. ExecTx in the delivery store
	// joins the outer tx when one exists in the context.
	if err := m.cfg.Store.EnqueueMessage(ctx, params); err != nil {
		// Clean up the promise registry entry to prevent unbounded
		// stale entries from accumulating on repeated enqueue failures.
		if promiseID != "" {
			m.promiseRegistryMu.Lock()
			delete(m.promiseRegistry, promiseID)
			m.promiseRegistryMu.Unlock()
		}

		return fmt.Errorf("enqueue mailbox message: %w", err)
	}

	// Signal the receive loop to wake up.
	select {
	case m.wake <- struct{}{}:
	default:
	}

	return nil
}

// TrySend attempts to send an envelope to the mailbox without blocking.
// It returns nil if the envelope was successfully sent, or an error if the
// mailbox could not accept it.
func (m *DurableMailbox[M, R]) TrySend(env envelope[M, R]) error {
	if m.actorCtx.Err() != nil {
		return ErrActorTerminated
	}

	if m.closed.Load() {
		return ErrMailboxClosed
	}

	// Use a short caller timeout context. Keep it separate from actorCtx so
	// Send can report actor shutdown as ErrActorTerminated instead of as a
	// caller context cancellation.
	ctx, cancel := context.WithTimeout(
		context.Background(), 100*time.Millisecond,
	)
	defer cancel()

	return m.Send(ctx, env)
}

// Receive returns an iterator over Delivery objects from the mailbox. The
// iterator will block when the mailbox is empty and yield deliveries as they
// become available. The iterator stops when the context is cancelled or the
// mailbox is closed.
func (m *DurableMailbox[M, R]) Receive(
	ctx context.Context) iter.Seq[envelope[M, R]] {

	return func(yield func(envelope[M, R]) bool) {
		ticker := time.NewTicker(m.cfg.PollInterval)
		defer ticker.Stop()

		for {
			// Check for cancellation.
			select {
			case <-ctx.Done():
				return

			case <-m.actorCtx.Done():
				return

			default:
			}

			if m.closed.Load() {
				return
			}

			// Try to lease a message.
			leaseToken := generateID()
			leased, err := m.cfg.Store.LeaseNextMessage(
				ctx, m.cfg.MailboxID, leaseToken,
				m.cfg.LeaseDuration,
			)

			if err != nil {
				// During teardown, the durable store is closed
				// before the lease loop's outer ctx fires. The
				// resulting "sql: database is closed" /
				// context-cancelled errors are expected: log
				// at debug level instead of warn-flooding test
				// artifacts at the tail of every itest. Real
				// lease errors during normal operation still
				// warn loudly because neither ctx is done and
				// the err message does not name the closed
				// store.
				ctxDone := ctx.Err() != nil ||
					m.actorCtx.Err() != nil ||
					m.closed.Load()
				if ctxDone || isExpectedShutdownErr(err) {
					logger(ctx).DebugS(ctx,
						"Lease loop exiting on "+
							"shutdown",
						"mailbox_id", m.cfg.MailboxID,
						"err", err)

					return
				}

				logger(ctx).WarnS(ctx, "Failed to lease "+
					"message from mailbox",
					err, "mailbox_id", m.cfg.MailboxID)

				select {
				case <-ticker.C:
					continue

				case <-m.wake:
					continue

				case <-ctx.Done():
					return

				case <-m.actorCtx.Done():
					return
				}
			}

			if leased == nil {
				// No messages available, wait for poll interval
				// or wake signal.
				select {
				case <-ticker.C:
					continue

				case <-m.wake:
					continue

				case <-ctx.Done():
					return

				case <-m.actorCtx.Done():
					return
				}
			}

			// Decode the message.
			decoded, err := m.cfg.Codec.Decode(leased.Payload)
			if err != nil {
				// Decode error - nack with backoff, or
				// dead-letter if max attempts exhausted.
				logger(ctx).WarnS(ctx, "Failed to decode "+
					"message payload",
					err,
					"mailbox_id", m.cfg.MailboxID,
					"message_id", leased.ID,
					"attempts", leased.Attempts,
					"max_attempts", leased.MaxAttempts)

				m.handlePoisonMessage(
					ctx, leased,
					fmt.Sprintf("decode error: %v", err),
				)

				continue
			}

			// Cast to the expected message type.
			msg, ok := decoded.(M)
			if !ok {
				// Type mismatch - nack with backoff, or
				// dead-letter if max attempts exhausted.
				logger(ctx).WarnS(ctx, "Message type mismatch",
					nil,
					"mailbox_id", m.cfg.MailboxID,
					"message_id", leased.ID,
					"attempts", leased.Attempts,
					"max_attempts", leased.MaxAttempts)

				m.handlePoisonMessage(
					ctx, leased, "type mismatch: "+
						"cannot cast decoded "+
						"message to expected type",
				)

				continue
			}

			// Retrieve the promise from the registry if this is an
			// Ask.
			var promise Promise[R]
			if leased.PromiseID != "" {
				m.promiseRegistryMu.Lock()
				if p, ok := m.promiseRegistry[leased.PromiseID]; ok {
					if typedPromise, ok := p.(Promise[R]); ok {
						promise = typedPromise
					}

					// Remove from registry - each promise
					// is used once.
					delete(
						m.promiseRegistry,
						leased.PromiseID,
					)
				}
				m.promiseRegistryMu.Unlock()
			}

			// Create the delivery with the promise attached.
			delivery := newDelivery[M, R](
				leased, msg, promise, ctx, m.cfg.Store,
			)

			// Wrap in envelope for compatibility with the Mailbox
			// interface. The Delivery is passed directly via
			// env.delivery, eliminating the need for a global map.
			// The DurableActor reads env.delivery and type-asserts
			// it to *Delivery[M, R].
			env := envelope[M, R]{
				message:   msg,
				promise:   promise,
				callerCtx: ctx,
				delivery:  delivery,
			}

			if !yield(env) {
				return
			}
		}
	}
}

// handlePoisonMessage handles a message that cannot be decoded or cast to the
// expected type. If the message has exhausted its max delivery attempts, it is
// moved to the dead letter queue. Otherwise it is nacked with a backoff delay
// for retry (in case the failure is due to a transient codec issue or version
// mismatch that a restart could resolve).
func (m *DurableMailbox[M, R]) handlePoisonMessage(ctx context.Context,
	leased *LeasedMessage, reason string) {

	if leased.Attempts >= leased.MaxAttempts {
		// Exhausted attempts -- dead-letter the message so it
		// doesn't stay stranded in the mailbox forever.
		dlReason := fmt.Sprintf("poison message (attempts %d/%d): %s",
			leased.Attempts, leased.MaxAttempts, reason)

		if dlErr := m.cfg.Store.MoveToDeadLetter(
			ctx, leased.ID, dlReason,
		); dlErr != nil {

			logger(ctx).WarnS(ctx,
				"Failed to dead-letter poison message",
				dlErr,
				"mailbox_id", m.cfg.MailboxID,
				"message_id", leased.ID)

			return
		}

		if delErr := m.cfg.Store.DeleteMessage(
			ctx, leased.ID,
		); delErr != nil {

			logger(ctx).WarnS(ctx,
				"Failed to delete dead-lettered poison "+
					"message",
				delErr,
				"mailbox_id", m.cfg.MailboxID,
				"message_id", leased.ID)
		}

		logger(ctx).InfoS(
			ctx,
			"Poison message moved to dead letter queue",
			"mailbox_id", m.cfg.MailboxID,
			"message_id", leased.ID,
			"reason", dlReason,
		)

		return
	}

	// Not yet exhausted -- nack for retry with backoff.
	_, _ = m.cfg.Store.NackMessage(
		ctx, leased.ID, leased.LeaseToken, 60*time.Second,
	)
}

// Close closes the mailbox, preventing any further sends. After closing,
// Receive will yield any remaining envelopes and then stop.
func (m *DurableMailbox[M, R]) Close() {
	m.closeMu.Lock()
	defer m.closeMu.Unlock()

	if m.closed.CompareAndSwap(false, true) {
		close(m.wake)
	}
}

// IsClosed returns true if the mailbox has been closed.
func (m *DurableMailbox[M, R]) IsClosed() bool {
	return m.closed.Load()
}

// Drain returns an iterator over any remaining envelopes in the mailbox after
// it has been closed. This is useful for cleanup logic during actor shutdown.
func (m *DurableMailbox[M, R]) Drain() iter.Seq[envelope[M, R]] {
	return func(yield func(envelope[M, R]) bool) {
		// For durable mailbox, messages remain in the database for
		// potential recovery. We don't actually drain them here.
		// The actor can restart and continue processing.
	}
}

// Ensure DurableMailbox implements Mailbox interface.
// Note: Interface check is done via explicit type assertion in tests
// since TLVMessage has complex generic constraints.

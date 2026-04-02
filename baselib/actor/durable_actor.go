package actor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// TellRetryPolicy determines whether a failed Tell message should be retried
// and how long to wait before the next attempt.
type TellRetryPolicy func(err error, attempts int) (retry bool, delay time.Duration)

// DefaultTellRetryPolicy provides exponential backoff for transient errors.
// It gives up after 5 attempts with a maximum delay of 60 seconds.
func DefaultTellRetryPolicy(err error, attempts int) (bool, time.Duration) {
	if attempts >= 5 {
		return false, 0
	}

	// Exponential backoff: 1s, 2s, 4s, 8s, 16s (capped at 60s).
	delay := time.Duration(1<<uint(attempts)) * time.Second
	if delay > 60*time.Second {
		delay = 60 * time.Second
	}

	return true, delay
}

// DurableActorConfig holds the configuration parameters for a DurableActor.
type DurableActorConfig[M TLVMessage, R any] struct {
	// ID is the unique identifier for the actor.
	ID string

	// Log is the logger attached to the durable actor runtime context.
	// When unset, the runtime falls back to btclog.Disabled.
	Log fn.Option[btclog.Logger]

	// Behavior defines how the actor responds to messages.
	// The runtime handles ack/nack automatically based on the result.
	Behavior ActorBehavior[M, R]

	// Store is the persistence layer for mailbox operations. If the store
	// implements TxAwareDeliveryStore, message processing will be wrapped
	// in a database transaction for atomic FSM updates.
	Store DeliveryStore

	// Codec handles message serialization/deserialization.
	Codec *MessageCodec

	// Clock provides time for message timestamps and lease calculations.
	// If None, uses DefaultClock.
	Clock fn.Option[clock.Clock]

	// DLO is a reference to the dead letter office for this actor system.
	// If nil, undeliverable messages during shutdown may be dropped.
	DLO ActorRef[Message, any]

	// Wg is an optional WaitGroup for tracking actor lifecycle.
	Wg *sync.WaitGroup

	// TellRetryPolicy determines retry behavior for failed Tell messages.
	// If nil, DefaultTellRetryPolicy is used.
	TellRetryPolicy TellRetryPolicy

	// LeaseDuration is how long a message is leased to the actor.
	// Default: 30 seconds.
	LeaseDuration time.Duration

	// HeartbeatInterval is how often to extend leases for long operations.
	// Should be less than half the LeaseDuration.
	// Default: LeaseDuration / 3.
	HeartbeatInterval time.Duration

	// PollInterval is how often to poll for new messages when empty.
	// Default: 100ms.
	PollInterval time.Duration

	// MaxAttempts is the default maximum delivery attempts.
	// Default: 10.
	MaxAttempts int

	// CleanupTimeout specifies the maximum duration for OnStop cleanup.
	// Default: 5 seconds.
	CleanupTimeout time.Duration

	// DeduplicationTTL is how long to keep processed message IDs for
	// deduplication. Should exceed the maximum possible redelivery window.
	// Default: 24 hours.
	DeduplicationTTL time.Duration
}

// DefaultDurableActorConfig returns a config with sensible defaults.
func DefaultDurableActorConfig[M TLVMessage, R any](
	id string,
	behavior ActorBehavior[M, R],
	store DeliveryStore,
	codec *MessageCodec,
) DurableActorConfig[M, R] {

	leaseDuration := 30 * time.Second

	return DurableActorConfig[M, R]{
		ID:                id,
		Log:               fn.None[btclog.Logger](),
		Behavior:          behavior,
		Store:             store,
		Codec:             codec,
		TellRetryPolicy:   DefaultTellRetryPolicy,
		LeaseDuration:     leaseDuration,
		HeartbeatInterval: leaseDuration / 3,
		PollInterval:      100 * time.Millisecond,
		MaxAttempts:       10,
		CleanupTimeout:    5 * time.Second,
		DeduplicationTTL:  24 * time.Hour,
	}
}

// DurableActor is an actor implementation that provides crash-resilient message
// processing using a durable mailbox. Messages are persisted before delivery
// and acknowledged automatically after processing, ensuring no message loss
// even on actor crashes.
//
// The actor runtime automatically handles:
//   - Ack on successful processing (or Ask with error result)
//   - Nack with retry on failed Tell messages (per TellRetryPolicy)
//   - Panic recovery with automatic Nack
//   - Lease heartbeating for long-running operations
//   - Dead letter handling when max attempts exceeded
//   - Deduplication via processed message tracking
//   - Transaction wrapping for atomic FSM updates (if store supports it)
//
// This provides exactly-once processing on top of at-least-once delivery.
type DurableActor[M TLVMessage, R any] struct {
	// id is the unique identifier for the actor.
	id string

	// behavior defines how the actor responds to messages.
	behavior ActorBehavior[M, R]

	// mailbox is the durable incoming message queue.
	mailbox *DurableMailbox[M, R]

	// ctx is the context governing the actor's lifecycle.
	ctx context.Context

	// cancel is the function to cancel the actor's context.
	cancel context.CancelFunc

	// store is the persistence layer for mailbox operations.
	store DeliveryStore

	// txAwareStore is the transaction-aware store, if available.
	// When non-nil, message processing is wrapped in a database transaction.
	txAwareStore TxAwareDeliveryStore

	// dlo is a reference to the dead letter office.
	dlo ActorRef[Message, any]

	// wg is an optional WaitGroup for tracking this actor's lifecycle.
	wg *sync.WaitGroup

	// tellRetryPolicy determines retry behavior for failed Tell messages.
	tellRetryPolicy TellRetryPolicy

	// leaseDuration is how long a message is leased.
	leaseDuration time.Duration

	// heartbeatInterval is how often to extend leases.
	heartbeatInterval time.Duration

	// cleanupTimeout is the maximum duration for OnStop cleanup.
	cleanupTimeout time.Duration

	// deduplicationTTL is how long to keep processed message IDs.
	deduplicationTTL time.Duration

	// startOnce ensures the actor's processing loop starts only once.
	startOnce sync.Once

	// stopOnce ensures the actor's processing loop stops only once.
	stopOnce sync.Once

	// ref is the cached ActorRef for this actor.
	ref ActorRef[M, R]
}

// NewDurableActor creates a new durable actor instance.
func NewDurableActor[M TLVMessage, R any](
	cfg DurableActorConfig[M, R],
) *DurableActor[M, R] {

	baseCtx := build.ContextWithLogger(
		context.Background(), cfg.Log.UnwrapOr(btclog.Disabled),
	)
	ctx, cancel := context.WithCancel(baseCtx)

	mailboxCfg := DurableMailboxConfig{
		MailboxID:     cfg.ID,
		Store:         cfg.Store,
		Codec:         cfg.Codec,
		Clock:         cfg.Clock,
		LeaseDuration: cfg.LeaseDuration,
		PollInterval:  cfg.PollInterval,
		MaxAttempts:   cfg.MaxAttempts,
	}

	tellPolicy := cfg.TellRetryPolicy
	if tellPolicy == nil {
		tellPolicy = DefaultTellRetryPolicy
	}

	deduplicationTTL := cfg.DeduplicationTTL
	if deduplicationTTL == 0 {
		deduplicationTTL = 24 * time.Hour
	}

	// Check if the store supports transaction awareness.
	var txAwareStore TxAwareDeliveryStore
	if txStore, ok := cfg.Store.(TxAwareDeliveryStore); ok {
		txAwareStore = txStore
	}

	actor := &DurableActor[M, R]{
		id:                cfg.ID,
		behavior:          cfg.Behavior,
		mailbox:           NewDurableMailbox[M, R](ctx, mailboxCfg),
		ctx:               ctx,
		cancel:            cancel,
		store:             cfg.Store,
		txAwareStore:      txAwareStore,
		dlo:               cfg.DLO,
		wg:                cfg.Wg,
		tellRetryPolicy:   tellPolicy,
		leaseDuration:     cfg.LeaseDuration,
		heartbeatInterval: cfg.HeartbeatInterval,
		cleanupTimeout:    cfg.CleanupTimeout,
		deduplicationTTL:  deduplicationTTL,
	}

	// Create and cache the actor's reference.
	actor.ref = &durableActorRefImpl[M, R]{
		actor: actor,
	}

	return actor
}

// Start initiates the actor's message processing loop.
func (a *DurableActor[M, R]) Start() {
	a.startOnce.Do(func() {
		logger(a.ctx).DebugS(a.ctx, "Starting durable actor", "actor_id", a.id)

		if a.wg != nil {
			a.wg.Add(1)
		}

		go a.process()
	})
}

// process is the main event loop for durable message processing.
func (a *DurableActor[M, R]) process() {
	if a.wg != nil {
		defer a.wg.Done()
	}

	// Process messages from the durable mailbox.
	for env := range a.mailbox.Receive(a.ctx) {
		// Extract the Delivery from the envelope. For DurableMailbox,
		// the delivery is passed directly in env.delivery, eliminating
		// the need for a global map lookup.
		delivery, ok := env.delivery.(*Delivery[M, R])
		if !ok || delivery == nil {
			// This shouldn't happen for properly configured durable
			// actors, but handle gracefully.
			logger(a.ctx).WarnS(a.ctx, "No delivery found in envelope", nil,
				"actor_id", a.id,
				"msg_type", env.message.MessageType())

			continue
		}

		a.processDelivery(delivery)
	}

	// The actor's context has been cancelled. Close the mailbox.
	a.mailbox.Close()

	// For durable mailboxes, we don't drain to DLO since messages persist
	// in the database and will be picked up on restart.

	// If the behavior implements Stoppable, call OnStop.
	if stoppable, ok := a.behavior.(Stoppable); ok {
		cleanupCtx, cancel := context.WithTimeout(
			context.Background(), a.cleanupTimeout,
		)
		defer cancel()

		if err := stoppable.OnStop(cleanupCtx); err != nil {
			logger(a.ctx).WarnS(a.ctx, "Durable actor cleanup error",
				err, "actor_id", a.id)
		}
	}

	logger(a.ctx).DebugS(a.ctx, "Durable actor terminated", "actor_id", a.id)
}

// processDelivery handles a single message delivery with deduplication,
// transaction wrapping, panic recovery, lease heartbeating, and automatic
// ack/nack based on result.
func (a *DurableActor[M, R]) processDelivery(delivery *Delivery[M, R]) {
	// Create a context that merges actor and caller contexts.
	var processCtx context.Context
	var cancel context.CancelFunc

	if delivery.CallerCtx != nil {
		processCtx, cancel = mergeContexts(a.ctx, delivery.CallerCtx)
	} else {
		processCtx = a.ctx
		cancel = func() {}
	}
	defer cancel()

	logger(processCtx).TraceS(processCtx, "Durable actor processing message",
		"actor_id", a.id,
		"msg_type", delivery.Message.MessageType(),
		"delivery_id", delivery.ID,
		"attempts", delivery.Attempts,
		"is_ask", delivery.IsAsk())

	// Check deduplication - skip if already processed.
	processed, err := a.store.IsProcessed(processCtx, delivery.ID)
	if err != nil {
		logger(processCtx).WarnS(processCtx, "Failed to check deduplication", err,
			"actor_id", a.id,
			"delivery_id", delivery.ID)
		// Continue processing on error - idempotent handling should be safe.
	} else if processed {
		logger(processCtx).DebugS(processCtx, "Skipping duplicate message",
			"actor_id", a.id,
			"delivery_id", delivery.ID)

		// Already processed - attempt to ack the mailbox message without
		// re-running behavior or re-persisting any Ask results.
		rows, err := delivery.store.AckMessage(
			processCtx, delivery.ID, delivery.LeaseToken,
		)
		if err != nil {
			logger(processCtx).WarnS(processCtx, "Failed to ack duplicate", err,
				"delivery_id", delivery.ID)
			return
		}
		if rows == 0 {
			logger(processCtx).WarnS(processCtx, "Duplicate ack failed (lease expired)",
				ErrLeaseExpired,
				"delivery_id", delivery.ID)
		}

		return
	}

	// If we have a transaction-aware store, wrap processing in a transaction.
	if a.txAwareStore != nil {
		a.processInTransaction(processCtx, delivery)
	} else {
		a.processWithoutTransaction(processCtx, delivery)
	}
}

// processInTransaction wraps message processing in a database transaction.
// All FSM state changes, outbox writes, and deduplication marks happen
// atomically within this transaction.
func (a *DurableActor[M, R]) processInTransaction(
	ctx context.Context,
	delivery *Delivery[M, R],
) {

	// Capture the behavior result so we can complete the in-memory
	// promise only after the transaction commits successfully. This
	// prevents callers from observing success for an operation that
	// was not durably committed.
	var behaviorResult fn.Result[R]

	// Suppress in-Ack promise completion during the tx -- we'll
	// complete the promise ourselves after commit succeeds.
	delivery.deferPromise = true

	err := a.txAwareStore.ExecTx(ctx, false, func(
		txCtx context.Context, store DeliveryStore,
	) error {

		// Execute behavior with panic recovery.
		behaviorResult = a.executeBehaviorSafely(txCtx, delivery)

		// Handle the result within the transaction. This determines
		// whether to ack, nack for retry, or dead-letter. We only mark
		// as processed if we're not going to retry - otherwise the
		// redelivered message would be incorrectly skipped by dedup.
		return a.handleResultInTx(
			txCtx, delivery, behaviorResult, store,
		)
	})

	if err != nil {
		logger(ctx).WarnS(ctx,
			"Transaction failed, nacking message",
			err,
			"actor_id", a.id,
			"delivery_id", delivery.ID,
			"msg_type", delivery.Message.MessageType(),
		)

		// Transaction failed - Nack for retry.
		if nackErr := delivery.Nack(ctx, err, 10*time.Second); nackErr != nil {
			logger(ctx).WarnS(ctx, "Failed to nack after tx failure",
				nackErr,
				"delivery_id", delivery.ID)
		}

		return
	}

	// Transaction committed -- now it is safe to complete the
	// in-memory promise so the caller observes the result.
	if delivery.IsAsk() && delivery.Promise != nil {
		delivery.Promise.Complete(behaviorResult)
	}
}

// processWithoutTransaction handles message processing when no transaction
// support is available.
func (a *DurableActor[M, R]) processWithoutTransaction(
	ctx context.Context,
	delivery *Delivery[M, R],
) {

	// Start the heartbeat goroutine for lease extension.
	heartbeatDone := make(chan struct{})
	go a.heartbeat(ctx, delivery, heartbeatDone)
	defer close(heartbeatDone)

	// Execute behavior with panic recovery.
	result := a.executeBehaviorSafely(ctx, delivery)

	// For Ask messages, avoid marking as processed until after ack has
	// succeeded. This prevents a crash between MarkProcessed and Ack from
	// turning into a permanent "processed" flag while the mailbox message
	// (and Ask result) is still pending.
	if delivery.IsAsk() {
		// For DurableAsk, the outbox write is the critical durable
		// output. If it fails, we must nack for retry rather than
		// acking (which would permanently drop the response while
		// the request appears "done").
		if delivery.IsDurableAsk() {
			if err := a.writeAskResponseToOutbox(
				ctx, delivery, result, a.store,
			); err != nil {
				logger(ctx).WarnS(ctx,
					"Failed to write ask response to "+
						"outbox, nacking for retry",
					err,
					"actor_id", a.id,
					"delivery_id", delivery.ID,
					"callback_actor_id",
					delivery.CallbackActorID)

				if nackErr := delivery.Nack(
					ctx, err, 5*time.Second,
				); nackErr != nil {
					logger(ctx).WarnS(ctx,
						"Failed to nack after "+
							"outbox write failure",
						nackErr,
						"delivery_id", delivery.ID)
				}

				return
			}
		}

		if err := delivery.Ack(ctx, result); err != nil {
			logger(ctx).WarnS(ctx, "Failed to ack Ask message", err,
				"actor_id", a.id,
				"delivery_id", delivery.ID)

			return
		}

		if err := a.store.MarkProcessed(
			ctx, delivery.ID, a.id, a.deduplicationTTL,
		); err != nil {
			logger(ctx).WarnS(ctx, "Failed to mark processed", err,
				"actor_id", a.id,
				"delivery_id", delivery.ID)
		}

		return
	}

	// Only mark as processed if we're not going to retry.
	// For Tell messages that fail, we may want to retry.
	// For DurableAsk messages, defer marking processed until after the
	// outbox write succeeds in handleResult (the outbox write is the
	// critical durable output, and marking processed before it succeeds
	// would permanently drop the response on outbox failure).
	shouldMarkProcessed := true
	if delivery.IsDurableAsk() {
		// DurableAsk: mark processed only after outbox write in
		// handleResult.
		shouldMarkProcessed = false
	} else if delivery.IsTell() && result.Err() != nil {
		retry, _ := a.tellRetryPolicy(result.Err(), delivery.Attempts)
		if retry {
			shouldMarkProcessed = false
		}
	}

	if shouldMarkProcessed {
		if err := a.store.MarkProcessed(
			ctx, delivery.ID, a.id, a.deduplicationTTL,
		); err != nil {
			logger(ctx).WarnS(ctx, "Failed to mark processed", err,
				"actor_id", a.id,
				"delivery_id", delivery.ID)
			// Continue anyway - dedup is defense in depth.
		}
	}

	// Handle the result.
	a.handleResult(ctx, delivery, result)
}

// executeBehaviorSafely runs the behavior with panic recovery.
func (a *DurableActor[M, R]) executeBehaviorSafely(
	ctx context.Context,
	delivery *Delivery[M, R],
) (result fn.Result[R]) {

	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("panic: %v", r)

			logger(ctx).ErrorS(ctx, "Panic during message processing",
				err,
				"actor_id", a.id,
				"delivery_id", delivery.ID)

			result = fn.Err[R](err)
		}
	}()

	return a.behavior.Receive(ctx, delivery.Message)
}

// handleResultInTx handles the result within a transaction.
// It determines whether to ack, nack for retry, or dead-letter, and only
// marks the message as processed when we won't retry (to avoid dedup issues).
func (a *DurableActor[M, R]) handleResultInTx(
	ctx context.Context,
	delivery *Delivery[M, R],
	result fn.Result[R],
	store DeliveryStore,
) error {

	// Create a delivery that uses the tx-scoped store. We must
	// propagate deferPromise so the in-Ack promise completion is
	// suppressed -- the caller (processInTransaction) handles
	// promise completion after ExecTx returns.
	txDelivery := &Delivery[M, R]{
		ID:              delivery.ID,
		Message:         delivery.Message,
		Promise:         delivery.Promise,
		CallerCtx:       delivery.CallerCtx,
		CallbackActorID: delivery.CallbackActorID,
		CorrelationID:   delivery.CorrelationID,
		LeaseToken:      delivery.LeaseToken,
		LeaseUntil:      delivery.LeaseUntil,
		Attempts:        delivery.Attempts,
		MaxAttempts:     delivery.MaxAttempts,
		store:           store,
		deferPromise:    delivery.deferPromise,
	}

	// For DurableAsk messages, write the response to the outbox,
	// mark as processed, and ack within the transaction. DurableAsk
	// messages are never retried via the Tell retry policy because
	// the outbox write IS the durable output. Retrying after a
	// successful outbox write would produce duplicate responses for
	// the same correlation ID.
	if delivery.IsDurableAsk() {
		if err := a.writeAskResponseToOutbox(
			ctx, delivery, result, store,
		); err != nil {
			return fmt.Errorf("write ask response: %w", err)
		}

		if err := store.MarkProcessed(
			ctx, delivery.ID, a.id, a.deduplicationTTL,
		); err != nil {
			return fmt.Errorf("mark processed: %w", err)
		}

		return txDelivery.Ack(ctx, result)
	}

	// For Ask messages, always Ack (even with error result). Mark as
	// processed since Ask messages are never retried.
	if delivery.IsAsk() {
		if err := store.MarkProcessed(
			ctx, delivery.ID, a.id, a.deduplicationTTL,
		); err != nil {
			return fmt.Errorf("mark processed: %w", err)
		}

		return txDelivery.Ack(ctx, result)
	}

	// For Tell messages, handle based on success/error.
	if err := result.Err(); err != nil {
		logger(ctx).WarnS(ctx,
			"Durable actor Tell message failed",
			err,
			"actor_id", a.id,
			"delivery_id", delivery.ID,
			"msg_type", delivery.Message.MessageType(),
			"attempts", delivery.Attempts,
		)

		// Apply Tell retry policy.
		retry, delay := a.tellRetryPolicy(err, delivery.Attempts)
		if retry {
			// Don't mark as processed - we want retry to work.
			_, nackErr := store.NackMessage(
				ctx, delivery.ID, delivery.LeaseToken, delay,
			)

			return nackErr
		}

		// Max retries exceeded - dead letter. Mark as processed since
		// we won't retry.
		if err := store.MarkProcessed(
			ctx, delivery.ID, a.id, a.deduplicationTTL,
		); err != nil {
			return fmt.Errorf("mark processed: %w", err)
		}

		return store.MoveToDeadLetter(ctx, delivery.ID, err.Error())
	}

	// Success - mark as processed and Ack the message.
	if err := store.MarkProcessed(
		ctx, delivery.ID, a.id, a.deduplicationTTL,
	); err != nil {
		return fmt.Errorf("mark processed: %w", err)
	}

	return txDelivery.Ack(ctx, result)
}

// handleResult processes the behavior result and automatically acks/nacks.
func (a *DurableActor[M, R]) handleResult(
	ctx context.Context,
	delivery *Delivery[M, R],
	result fn.Result[R],
) {

	// For DurableAsk messages, write response to outbox. If the write
	// fails, nack for retry rather than dropping the response. On
	// success, mark as processed and ack immediately since the outbox
	// write is the critical durable output.
	if delivery.IsDurableAsk() {
		if err := a.writeAskResponseToOutbox(
			ctx, delivery, result, a.store,
		); err != nil {
			logger(ctx).WarnS(ctx,
				"Failed to write ask response to outbox, "+
					"nacking for retry",
				err,
				"actor_id", a.id,
				"delivery_id", delivery.ID,
				"callback_actor_id", delivery.CallbackActorID)

			if nackErr := delivery.Nack(
				ctx, err, 5*time.Second,
			); nackErr != nil {
				logger(ctx).WarnS(ctx,
					"Failed to nack after outbox write "+
						"failure",
					nackErr,
					"delivery_id", delivery.ID)
			}

			return
		}

		// Outbox write succeeded -- mark processed and ack.
		if err := a.store.MarkProcessed(
			ctx, delivery.ID, a.id, a.deduplicationTTL,
		); err != nil {
			logger(ctx).WarnS(ctx, "Failed to mark DurableAsk processed",
				err,
				"actor_id", a.id,
				"delivery_id", delivery.ID)
		}

		if err := delivery.Ack(ctx, result); err != nil {
			logger(ctx).WarnS(ctx, "Failed to ack DurableAsk message",
				err,
				"actor_id", a.id,
				"delivery_id", delivery.ID)
		}

		return
	}

	// For Ask messages, always Ack (even with error result).
	// The error is persisted as part of the result.
	if delivery.IsAsk() {
		if err := delivery.Ack(ctx, result); err != nil {
			logger(ctx).WarnS(ctx, "Failed to ack Ask message", err,
				"actor_id", a.id,
				"delivery_id", delivery.ID)
		}

		return
	}

	// For Tell messages, handle based on success/error.
	if err := result.Err(); err != nil {
		logger(ctx).WarnS(ctx,
			"Durable actor Tell message failed",
			err,
			"actor_id", a.id,
			"delivery_id", delivery.ID,
			"msg_type", delivery.Message.MessageType(),
			"attempts", delivery.Attempts,
		)

		// Apply Tell retry policy.
		retry, delay := a.tellRetryPolicy(err, delivery.Attempts)
		if retry {
			if nackErr := delivery.Nack(ctx, err, delay); nackErr != nil {
				logger(ctx).WarnS(ctx, "Failed to nack Tell message",
					nackErr,
					"actor_id", a.id,
					"delivery_id", delivery.ID)
			}
		} else {
			// Max retries exceeded or policy says don't retry.
			// Explicitly move to dead letter queue.
			reason := fmt.Sprintf("retry policy exhausted: %v", err)
			if dlErr := a.store.MoveToDeadLetter(
				ctx, delivery.ID, reason,
			); dlErr != nil {
				logger(ctx).WarnS(ctx, "Failed to dead-letter Tell message",
					dlErr,
					"actor_id", a.id,
					"delivery_id", delivery.ID)
			}

			// Delete from mailbox after dead-lettering.
			if delErr := a.store.DeleteMessage(
				ctx, delivery.ID,
			); delErr != nil {
				logger(ctx).WarnS(ctx, "Failed to delete dead-lettered message",
					delErr,
					"actor_id", a.id,
					"delivery_id", delivery.ID)
			}
		}

		return
	}

	// Success - Ack the message.
	if err := delivery.Ack(ctx, result); err != nil {
		logger(ctx).WarnS(ctx, "Failed to ack Tell message", err,
			"actor_id", a.id,
			"delivery_id", delivery.ID)
	}
}

// heartbeat extends the lease periodically for long-running operations.
func (a *DurableActor[M, R]) heartbeat(
	ctx context.Context,
	delivery *Delivery[M, R],
	done <-chan struct{},
) {

	ticker := time.NewTicker(a.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return

		case <-ctx.Done():
			return

		case <-ticker.C:
			// Extend the lease.
			if err := delivery.Extend(ctx, a.leaseDuration); err != nil {
				logger(ctx).WarnS(ctx, "Failed to extend lease",
					err,
					"actor_id", a.id,
					"delivery_id", delivery.ID)

				return
			}

			logger(ctx).TraceS(ctx, "Extended lease for delivery",
				"actor_id", a.id,
				"delivery_id", delivery.ID,
				"new_expiry", delivery.LeaseUntil)
		}
	}
}

// writeAskResponseToOutbox creates an AskResponse and writes it to the outbox
// for delivery to the callback actor. This is called for DurableAsk messages.
func (a *DurableActor[M, R]) writeAskResponseToOutbox(
	ctx context.Context,
	delivery *Delivery[M, R],
	result fn.Result[R],
	store DeliveryStore,
) error {

	var response *AskResponse

	if err := result.Err(); err != nil {
		// Error response - no result blob, just error text.
		response = NewAskResponseError(delivery.CorrelationID, err.Error())
	} else {
		// Success response - try to encode the result.
		resultValue, _ := result.Unpack()

		// Check if the result is a TLVMessage.
		if tlvResult, ok := any(resultValue).(TLVMessage); ok {
			// Encode using the actor's codec.
			resultBlob, encErr := a.mailbox.cfg.Codec.Encode(tlvResult)
			if encErr != nil {
				return fmt.Errorf("encode result: %w", encErr)
			}

			response = NewAskResponseSuccess(
				delivery.CorrelationID, resultBlob,
			)
		} else {
			// Result is not a TLVMessage (e.g., primitive like int64).
			// Store an empty blob - the caller can use the correlation ID
			// to look up the result via other means if needed.
			// For the generic case, we just acknowledge completion.
			response = NewAskResponseSuccess(delivery.CorrelationID, nil)
		}
	}

	// Encode the AskResponse for the outbox.
	responsePayload, err := a.mailbox.cfg.Codec.Encode(response)
	if err != nil {
		return fmt.Errorf("encode ask response: %w", err)
	}

	// Write to outbox, targeting the callback actor.
	outboxParams := OutboxParams{
		ID:            generateID(),
		SourceActorID: a.id,
		TargetActorID: delivery.CallbackActorID,
		MessageType:   response.MessageType(),
		Payload:       responsePayload,
	}

	if err := store.EnqueueOutbox(ctx, outboxParams); err != nil {
		return fmt.Errorf("enqueue outbox: %w", err)
	}

	logger(ctx).DebugS(ctx, "Wrote DurableAsk response to outbox",
		"actor_id", a.id,
		"delivery_id", delivery.ID,
		"callback_actor_id", delivery.CallbackActorID,
		"correlation_id", delivery.CorrelationID,
		"is_error", response.IsError())

	return nil
}

// Stop signals the actor to terminate.
func (a *DurableActor[M, R]) Stop() {
	a.stopOnce.Do(func() {
		a.cancel()
	})
}

// Ref returns an ActorRef for this actor.
func (a *DurableActor[M, R]) Ref() ActorRef[M, R] {
	return a.ref
}

// TellRef returns a TellOnlyRef for this actor.
func (a *DurableActor[M, R]) TellRef() TellOnlyRef[M] {
	return a.ref
}

// durableActorRefImpl provides an ActorRef implementation for DurableActor.
type durableActorRefImpl[M TLVMessage, R any] struct {
	actor *DurableActor[M, R]
}

// ID returns the unique identifier for this actor.
func (ref *durableActorRefImpl[M, R]) ID() string {
	return ref.actor.id
}

// Tell sends a message without waiting for a response. Returns an error if
// the message could not be durably enqueued.
func (ref *durableActorRefImpl[M, R]) Tell(ctx context.Context, msg M) error {
	logger(ctx).TraceS(ctx, "Sending Tell to durable actor",
		"actor_id", ref.actor.id,
		"msg_type", msg.MessageType())

	env := envelope[M, R]{
		message:   msg,
		promise:   nil,
		callerCtx: ctx,
	}

	ok := ref.actor.mailbox.Send(ctx, env)
	if !ok {
		// Check if actor is terminated.
		if ref.actor.ctx.Err() != nil {
			logger(ctx).DebugS(ctx, "Tell failed, routing to DLO",
				"actor_id", ref.actor.id,
				"msg_type", msg.MessageType())

			// Use context.Background() since the actor is terminated and
			// the original context might be done or cancelled.
			ref.trySendToDLO(context.Background(), msg)

			return ErrActorTerminated
		}

		// Check if caller's context was cancelled.
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Mailbox full or other failure.
		return ErrMailboxFull
	}

	return nil
}

// Ask sends a message and returns a Future for the response.
func (ref *durableActorRefImpl[M, R]) Ask(ctx context.Context, msg M) Future[R] {
	logger(ctx).TraceS(ctx, "Sending Ask to durable actor",
		"actor_id", ref.actor.id,
		"msg_type", msg.MessageType())

	promise := NewPromise[R]()

	// Check if actor is already terminated.
	if ref.actor.ctx.Err() != nil {
		logger(ctx).DebugS(ctx, "Ask failed, actor already terminated",
			"actor_id", ref.actor.id,
			"msg_type", msg.MessageType())

		promise.Complete(fn.Err[R](ErrActorTerminated))

		return promise.Future()
	}

	env := envelope[M, R]{
		message:   msg,
		promise:   promise,
		callerCtx: ctx,
	}

	ok := ref.actor.mailbox.Send(ctx, env)
	if !ok {
		if ref.actor.ctx.Err() != nil {
			promise.Complete(fn.Err[R](ErrActorTerminated))
		} else {
			err := ctx.Err()
			if err == nil {
				err = ErrActorTerminated
			}

			promise.Complete(fn.Err[R](err))
		}
	}

	return promise.Future()
}

// trySendToDLO attempts to send a message to the dead letter office.
// The context is accepted as a parameter to give the caller control, but
// callers should typically pass context.Background() since the original
// context might already be done (we're in an error path where the actor is
// terminated or the original context was cancelled). This is a fire-and-forget
// operation for diagnostic purposes.
func (ref *durableActorRefImpl[M, R]) trySendToDLO(ctx context.Context, msg M) {
	if ref.actor.dlo != nil {
		ref.actor.dlo.Tell(ctx, msg)
	}
}

// DurableAskParams specifies parameters for a durable Ask request.
type DurableAskParams struct {
	// CallbackActorID is the actor that will receive the response.
	// The response will be delivered to this actor's durable mailbox.
	CallbackActorID string

	// CorrelationID is used to match the response to the original request.
	// The response will include this ID for the caller to match.
	CorrelationID string
}

// DurableAsk sends a message and arranges for the response to be delivered
// to the callback actor's durable mailbox. Unlike Ask, which returns an
// in-memory Future, DurableAsk persists the callback metadata with the message.
// When the target actor processes the message, it writes an AskResponse to its
// outbox, which the OutboxPublisher then delivers to the callback actor.
//
// This provides crash-safe Ask semantics: if the caller crashes before receiving
// the response, the response will still be delivered when the caller restarts
// and resumes processing its mailbox.
//
// Returns an error if the message could not be durably enqueued.
func (ref *durableActorRefImpl[M, R]) DurableAsk(
	ctx context.Context,
	msg M,
	params DurableAskParams,
) error {

	if params.CallbackActorID == "" {
		return fmt.Errorf("callback actor ID is required for DurableAsk")
	}

	if params.CorrelationID == "" {
		return fmt.Errorf("correlation ID is required for DurableAsk")
	}

	logger(ctx).TraceS(ctx, "Sending DurableAsk to durable actor",
		"actor_id", ref.actor.id,
		"msg_type", msg.MessageType(),
		"callback_actor_id", params.CallbackActorID,
		"correlation_id", params.CorrelationID)

	env := envelope[M, R]{
		message:         msg,
		promise:         nil, // No in-memory promise - response via outbox.
		callerCtx:       ctx,
		callbackActorID: params.CallbackActorID,
		correlationID:   params.CorrelationID,
	}

	ok := ref.actor.mailbox.Send(ctx, env)
	if !ok {
		if ref.actor.ctx.Err() != nil {
			logger(ctx).DebugS(ctx, "DurableAsk failed, actor terminated",
				"actor_id", ref.actor.id,
				"msg_type", msg.MessageType())

			return ErrActorTerminated
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		return ErrMailboxFull
	}

	return nil
}

// DurableActorRef extends ActorRef with durable Ask semantics.
// This interface is implemented by DurableActor references.
type DurableActorRef[M TLVMessage, R any] interface {
	ActorRef[M, R]

	// DurableAsk sends a message with callback metadata for durable response
	// delivery. The response will be delivered to the callback actor's
	// mailbox via the outbox.
	DurableAsk(ctx context.Context, msg M, params DurableAskParams) error
}

// Compile-time interface checks.
var (
	_ ActorRef[TLVMessage, any]        = (*durableActorRefImpl[TLVMessage, any])(nil)
	_ DurableActorRef[TLVMessage, any] = (*durableActorRefImpl[TLVMessage, any])(nil)
)

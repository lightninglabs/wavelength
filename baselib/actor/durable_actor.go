package actor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/build"
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

	// Behavior defines how the actor responds to messages. It is one of two
	// shapes:
	//
	//   - Left: a classic ActorBehavior. The runtime wraps the whole
	//     Receive in one transaction (processInTransaction) when the store
	//     is tx-aware, else runs the short-slice path. This is the safe
	//     default.
	//
	//   - Right: a BoundTxBehavior built via NewTxBehavior. The runtime
	//     drives the behavior through the Read/Commit execution path
	//     (processWithExec) so it can do side-effect IO outside the writer
	//     transaction. This requires a TxAwareDeliveryStore.
	//
	// DefaultDurableActorConfig populates the Left case from its behavior
	// argument; tx-aware actors overwrite this field with fn.NewRight.
	Behavior fn.Either[ActorBehavior[M, R], BoundTxBehavior[M, R]]

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
	// Same-process sends wake the mailbox immediately, so polling is only
	// the fallback for missed wakes, restarts, and external enqueues.
	// Default: 1s.
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

	// NumWorkers is how many concurrent worker loops drain the actor's
	// mailbox. The default (and a value <= 0) is 1, which preserves the
	// strictly-sequential per-actor processing other actors rely on. A
	// value greater than 1 turns the actor into a competing-consumer pool:
	// that many goroutines each lease distinct messages from the one shared
	// mailbox via LeaseNextMailboxMessage, so independent messages are
	// processed in parallel while the per-correlation-key FIFO claim still
	// keeps same-key messages strictly ordered. Use it only for behaviors
	// whose handlers are safe to run concurrently (e.g. the serverconn
	// egress sender, which holds no writer across its Edge.Send). The
	// behavior must not assume sequential delivery across keys.
	NumWorkers int

	// allowConcurrentClassic disables the construction guard that rejects
	// NumWorkers > 1 on a classic ActorBehavior. It is a test-only escape
	// hatch with no exported struct access, flippable only via
	// AllowConcurrentClassicBehavior, so production configs cannot set it
	// even by accident. See that method for why production must never use
	// it.
	allowConcurrentClassic bool
}

// AllowConcurrentClassicBehavior is a TEST-ONLY escape hatch that disables the
// construction guard which otherwise rejects NumWorkers > 1 on a classic
// ActorBehavior. It exists solely so the egress benchmark can build the
// production-forbidden classic-path-with-pool config in order to measure that
// pooling the classic path gains nothing (the writer held across the side
// effect serializes the workers regardless). Production code must NEVER call
// this: a classic behavior wraps its entire Receive in one write transaction
// and assumes strictly-sequential delivery, so fanning it out across workers
// delivers concurrent Receive calls and can silently corrupt in-memory state.
// The field it sets is unexported, so this method is the only way to flip it.
func (c DurableActorConfig[
	M,
	R,
]) AllowConcurrentClassicBehavior() DurableActorConfig[M, R] {

	c.allowConcurrentClassic = true

	return c
}

// NewClassicBehavior wraps a classic ActorBehavior as the Left case of the
// DurableActorConfig.Behavior either. DefaultDurableActorConfig applies it for
// you; use it directly only when assigning Behavior on a hand-built config.
func NewClassicBehavior[M TLVMessage, R any](
	behavior ActorBehavior[M, R],
) fn.Either[ActorBehavior[M, R], BoundTxBehavior[M, R]] {

	return fn.NewLeft[ActorBehavior[M, R], BoundTxBehavior[M, R]](behavior)
}

// NewTxBehaviorEither binds a TxBehavior and its StoreFactory and wraps the
// result as the Right case of the DurableActorConfig.Behavior either.
// DefaultDurableTxActorConfig applies it for you; use it directly only when
// assigning Behavior on a hand-built config.
func NewTxBehaviorEither[M TLVMessage, R any, S any](
	behavior TxBehavior[M, R, S],
	factory StoreFactory[S],
) fn.Either[ActorBehavior[M, R], BoundTxBehavior[M, R]] {

	return fn.NewRight[ActorBehavior[M, R]](
		NewTxBehavior(behavior, factory),
	)
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
		Behavior:          NewClassicBehavior(behavior),
		Store:             store,
		Codec:             codec,
		TellRetryPolicy:   DefaultTellRetryPolicy,
		LeaseDuration:     leaseDuration,
		HeartbeatInterval: leaseDuration / 3,
		PollInterval:      time.Second,
		MaxAttempts:       10,
		CleanupTimeout:    5 * time.Second,
		DeduplicationTTL:  24 * time.Hour,
		NumWorkers:        1,
	}
}

// DefaultDurableTxActorConfig returns a config wired for the Read/Commit
// execution path. It is the tx-aware counterpart to DefaultDurableActorConfig:
// rather than taking a classic ActorBehavior, it takes a TxBehavior and its
// StoreFactory and binds them into the config's Right case, hiding the
// NewTxBehavior / fn.NewRight boilerplate from callers. The store must be a
// TxAwareDeliveryStore (enforced by NewDurableActor).
func DefaultDurableTxActorConfig[M TLVMessage, R any, S any](
	id string,
	behavior TxBehavior[M, R, S],
	factory StoreFactory[S],
	store DeliveryStore,
	codec *MessageCodec,
) DurableActorConfig[M, R] {

	cfg := DefaultDurableActorConfig[M, R](id, nil, store, codec)
	cfg.Behavior = NewTxBehaviorEither[M, R, S](behavior, factory)

	return cfg
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

	// behavior selects how the actor processes messages. The Left case is a
	// classic ActorBehavior the runtime wraps in a transaction; the Right
	// case is a BoundTxBehavior driven through the Read/Commit execution
	// path. It mirrors DurableActorConfig.Behavior directly.
	behavior fn.Either[ActorBehavior[M, R], BoundTxBehavior[M, R]]

	// mailbox is the durable incoming message queue.
	mailbox *DurableMailbox[M, R]

	// ctx is the context governing the actor's lifecycle.
	ctx context.Context

	// cancel is the function to cancel the actor's context.
	cancel context.CancelFunc

	// store is the persistence layer for mailbox operations.
	store DeliveryStore

	// txAwareStore is the transaction-aware store, if available. When
	// non-nil, message processing is wrapped in a database transaction.
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

	// numWorkers is how many concurrent lease loops drain the shared
	// mailbox. It is always >= 1; values <= 0 from the config are clamped
	// at construction.
	numWorkers int

	// startOnce ensures the actor's processing loop starts only once.
	startOnce sync.Once

	// stopOnce ensures the actor's processing loop stops only once.
	stopOnce sync.Once

	// started records whether Start has launched the processing loop.
	started atomic.Bool

	// done closes once the processing loop has exited.
	done chan struct{}

	// ref is the cached ActorRef for this actor.
	ref ActorRef[M, R]
}

// ErrTxBehaviorNeedsTxStore indicates that a config requested the Read/Commit
// execution path (a Right BoundTxBehavior) without a TxAwareDeliveryStore,
// which that path requires to open its short transactions.
var ErrTxBehaviorNeedsTxStore = fmt.Errorf("durable actor: TxBehavior " +
	"requires a TxAwareDeliveryStore")

// ErrNoBehavior indicates that a config carries no usable behavior: either a
// nil classic ActorBehavior (the fn.Either zero value is a Left holding nil, so
// a forgotten Behavior field lands here) or a zero-value BoundTxBehavior whose
// run hook was never bound (e.g. fn.NewRight(BoundTxBehavior{}) instead of
// NewTxBehavior/DefaultDurableTxActorConfig). Surfacing it at construction
// keeps the misconfiguration from panicking at first dispatch.
var ErrNoBehavior = fmt.Errorf("durable actor: a behavior must be set (use " +
	"DefaultDurableActorConfig or DefaultDurableTxActorConfig)")

// ErrConcurrentClassicBehavior indicates that a config requested a
// competing-consumer pool (NumWorkers > 1) for a classic ActorBehavior (the
// Left case). The classic path wraps the entire Receive in one write
// transaction and relies on strictly-sequential, one-message-at-a-time
// processing; running it across N workers would deliver concurrent Receive
// calls and silently corrupt any actor that keeps in-memory state or assumes
// serial execution. Pools are only sound for the Read/Commit (TxBehavior) path,
// whose handlers run their side effects outside the writer and are required to
// be concurrency-safe, so we reject the combination at construction rather than
// let a future caller trip over it at runtime.
var ErrConcurrentClassicBehavior = fmt.Errorf("durable actor: NumWorkers > 1 " +
	"requires a TxBehavior (Read/Commit) behavior; a classic " +
	"ActorBehavior must be processed sequentially")

// ErrDurableAskUnsupported indicates that a DurableAsk was delivered to an
// actor running on the Read/Commit (TxBehavior) execution path. On that path
// the message is acked inside the behavior's own Commit, so the framework
// cannot fold the crash-safe callback response into the consuming transaction
// (it does not have the behavior's result at Commit time). The request is
// rejected with this error delivered to the caller as the response. Full
// DurableAsk support is planned alongside the OOR effect-actor adopter.
var ErrDurableAskUnsupported = fmt.Errorf("durable actor: DurableAsk is not " +
	"supported on the Read/Commit execution path")

// NewDurableActor creates a new durable actor instance. It returns an error
// result when the configuration is invalid -- currently when a TxBehavior is
// paired with a store that is not transaction-aware.
func NewDurableActor[M TLVMessage, R any](
	cfg DurableActorConfig[M, R],
) fn.Result[*DurableActor[M, R]] {

	baseCtx := build.ContextWithLogger(
		context.Background(), cfg.Log.UnwrapOr(btclog.Disabled),
	)
	ctx, cancel := context.WithCancel(baseCtx)

	// Clamp the worker count to at least one. A single worker preserves the
	// strictly-sequential per-actor processing semantics; more than one
	// turns the actor into a competing-consumer pool over its single
	// mailbox.
	numWorkers := cfg.NumWorkers
	if numWorkers < 1 {
		numWorkers = 1
	}

	mailboxCfg := DurableMailboxConfig{
		MailboxID:     cfg.ID,
		Store:         cfg.Store,
		Codec:         cfg.Codec,
		Clock:         cfg.Clock,
		LeaseDuration: cfg.LeaseDuration,
		PollInterval:  cfg.PollInterval,
		MaxAttempts:   cfg.MaxAttempts,

		// Size the wake channel to the worker count so a burst of
		// enqueues can rouse every idle worker at once.
		WakeBuffer: numWorkers,
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

	// Validate the configured behavior up front so a misconfigured actor
	// fails closed at construction rather than panicking at first dispatch.
	// The fn.Either zero value is a Left holding a nil ActorBehavior, so a
	// forgotten Behavior field is caught here too.
	if cfg.Behavior.IsRight() {
		txb := cfg.Behavior.RightToSome().UnsafeFromSome()
		switch {
		// A zero-value BoundTxBehavior has no run hook and would panic
		// at dispatch.
		case !txb.isSet():
			cancel()

			return fn.Err[*DurableActor[M, R]](ErrNoBehavior)

		// The Read/Commit execution path opens its own short
		// transactions, so it requires a transaction-aware store.
		case txAwareStore == nil:
			cancel()

			return fn.Err[*DurableActor[M, R]](
				ErrTxBehaviorNeedsTxStore,
			)
		}
	} else if cfg.Behavior.LeftToSome().UnwrapOr(nil) == nil {
		cancel()

		return fn.Err[*DurableActor[M, R]](ErrNoBehavior)
	}

	// A competing-consumer pool is only sound for the Read/Commit path,
	// whose handlers run outside the writer and must be concurrency-safe.
	// Reject NumWorkers > 1 on a classic ActorBehavior here so a stateful
	// actor can never be silently fanned out into concurrent Receive calls.
	// The test-only AllowConcurrentClassicBehavior escape hatch bypasses
	// this so the egress benchmark can measure the forbidden config.
	if numWorkers > 1 && cfg.Behavior.IsLeft() &&
		!cfg.allowConcurrentClassic {

		cancel()

		return fn.Err[*DurableActor[M, R]](ErrConcurrentClassicBehavior)
	}

	// Enable the leaseless peek consume path strictly for a single-worker
	// Read/Commit (Right / TxBehavior) actor. A single worker has no
	// competing consumer, so the lease token's only purpose -- fencing the
	// ack -- is unnecessary, and peeking with a read-only query eliminates
	// one write transaction per consumed message. We additionally require
	// the Read/Commit path because that is where the consume ack is folded
	// into the behavior's own short transaction (where the by-ID ack
	// belongs atomically); the classic path keeps the lease so its
	// lease-bound attempts and retry-budget semantics are unchanged. With
	// numWorkers > 1 the flag stays false, so the multi-worker
	// competing-consumer path keeps LeaseNextMessage and the lease-fenced
	// ack byte-for-byte.
	mailboxCfg.SingleWorkerLeaseless = numWorkers == 1 &&
		cfg.Behavior.IsRight()

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
		numWorkers:        numWorkers,
		done:              make(chan struct{}),
	}

	// Create and cache the actor's reference.
	actor.ref = &durableActorRefImpl[M, R]{
		actor: actor,
	}

	return fn.Ok(actor)
}

// Start initiates the actor's message processing loops.
func (a *DurableActor[M, R]) Start() {
	a.startOnce.Do(func() {
		a.started.Store(true)

		logger(a.ctx).DebugS(a.ctx, "Starting durable actor",
			"actor_id", a.id,
			"num_workers", a.numWorkers,
		)

		if a.wg != nil {
			a.wg.Add(1)
		}

		// Launch numWorkers competing lease loops over the one shared
		// mailbox. With numWorkers == 1 this is the historical
		// single-loop behavior. A supervisor goroutine joins them and
		// runs teardown once, so the actor's done / Wg / Stoppable
		// semantics are unchanged regardless of the worker count.
		var workers sync.WaitGroup
		for i := 0; i < a.numWorkers; i++ {
			workers.Add(1)

			go a.worker(&workers)
		}

		go func() {
			workers.Wait()
			a.teardown()

			if a.wg != nil {
				a.wg.Done()
			}

			close(a.done)
		}()
	})
}

// worker runs a single lease loop, draining deliveries from the shared mailbox
// until the actor context is cancelled. When the actor runs more than one
// worker they compete for distinct messages via the store's lease, so
// independent messages process in parallel; the per-correlation-key FIFO claim
// keeps same-key messages ordered across workers.
func (a *DurableActor[M, R]) worker(wg *sync.WaitGroup) {
	defer wg.Done()

	// Process messages from the durable mailbox.
	for env := range a.mailbox.Receive(a.ctx) {
		// Extract the Delivery from the envelope. For DurableMailbox,
		// the delivery is passed directly in env.delivery, eliminating
		// the need for a global map lookup.
		delivery, ok := env.delivery.(*Delivery[M, R])
		if !ok || delivery == nil {
			// This shouldn't happen for properly configured durable
			// actors, but handle gracefully.
			logger(a.ctx).WarnS(a.ctx, "No delivery found in "+
				"envelope", nil,
				"actor_id", a.id,
				"msg_type", env.message.MessageType())

			continue
		}

		a.processDelivery(delivery)
	}
}

// teardown closes the mailbox and runs the Stoppable cleanup hook exactly once,
// after every worker loop has exited. The supervisor goroutine started in Start
// invokes it before signaling done.
func (a *DurableActor[M, R]) teardown() {
	// The actor's context has been cancelled and all workers have exited.
	// Close the mailbox.
	a.mailbox.Close()

	// For durable mailboxes, we don't drain to DLO since messages persist
	// in the database and will be picked up on restart.

	// If a classic behavior implements Stoppable, call OnStop. The
	// Read/Commit (Right) path has no Stoppable hook of its own; its owner
	// manages cleanup.
	a.behavior.WhenLeft(func(b ActorBehavior[M, R]) {
		stoppable, ok := b.(Stoppable)
		if !ok {
			return
		}

		cleanupCtx, cancel := context.WithTimeout(
			context.Background(), a.cleanupTimeout,
		)
		defer cancel()

		if err := stoppable.OnStop(cleanupCtx); err != nil {
			logger(a.ctx).WarnS(a.ctx, "Durable actor cleanup error",
				err, "actor_id", a.id)
		}
	})

	logger(a.ctx).DebugS(a.ctx, "Durable actor terminated",
		"actor_id", a.id,
	)
}

// processDelivery handles a single message delivery with deduplication,
// transaction wrapping, panic recovery, lease heartbeating, and automatic
// ack/nack based on result.
func (a *DurableActor[M, R]) processDelivery(delivery *Delivery[M, R]) {
	// Create a context for processing. Ask/DurableAsk messages merge the
	// actor and caller contexts so request deadlines can still interrupt
	// synchronous work. Tell messages use only the actor context, matching
	// non-durable actor semantics: once a fire-and-forget message is
	// durably enqueued, later caller cancellation must not cancel
	// processing.
	var processCtx context.Context
	var cancel context.CancelFunc

	if delivery.CallerCtx != nil &&
		(delivery.IsAsk() || delivery.IsDurableAsk()) {

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
		logger(processCtx).WarnS(processCtx, "Failed to check "+
			"deduplication", err,
			"actor_id", a.id,
			"delivery_id", delivery.ID)
		// Continue processing on error - idempotent handling should be
		// safe.
	} else if processed {
		logger(processCtx).DebugS(
			processCtx,
			"Skipping duplicate message",
			"actor_id", a.id,
			"delivery_id", delivery.ID,
		)

		// Already processed - attempt to ack the mailbox message
		// without re-running behavior or re-persisting any Ask results.
		// ackMessage routes a leaseless (empty-token) delivery to the
		// unfenced by-ID ack.
		rows, err := ackMessage(
			processCtx, delivery.store, delivery.ID,
			delivery.LeaseToken,
		)
		if err != nil {
			logger(processCtx).WarnS(processCtx, "Failed to ack "+
				"duplicate", err,
				"delivery_id", delivery.ID)

			return
		}
		if rows == 0 {
			// A zero-row ack means the row was not deleted. On the
			// leased path that signals the lease expired or was
			// claimed by another consumer. On the leaseless
			// (empty-token) path there is no fence: the row is
			// simply already gone (the original processing consumed
			// it and this is a benign duplicate redelivery), so it
			// is not a lease failure and warrants no warning.
			if delivery.LeaseToken == "" {
				logger(processCtx).DebugS(
					processCtx,
					"Duplicate ack found row already gone",
					"delivery_id", delivery.ID,
				)
			} else {
				logger(processCtx).WarnS(processCtx,
					"Duplicate ack failed (lease expired)",
					ErrLeaseExpired,
					"delivery_id", delivery.ID)
			}
		}

		return
	}

	// If the actor opted into the Read/Commit execution path (a Right
	// behavior), drive it through the Exec handle. Construction guarantees
	// a tx-aware store is present in this case.
	if a.behavior.IsRight() {
		a.processWithExec(
			processCtx, delivery,
			a.behavior.RightToSome().UnsafeFromSome(),
		)

		return
	}

	// If we have a transaction-aware store, wrap processing in a
	// transaction.
	if a.txAwareStore != nil {
		a.processInTransaction(processCtx, delivery)
	} else {
		a.processWithoutTransaction(processCtx, delivery)
	}
}

// processInTransaction wraps message processing in a database transaction.
// All FSM state changes, outbox writes, and deduplication marks happen
// atomically within this transaction.
func (a *DurableActor[M, R]) processInTransaction(ctx context.Context,
	delivery *Delivery[M, R]) {

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
		if nackErr := delivery.Nack(
			ctx, err, 10*time.Second,
		); nackErr != nil {

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
func (a *DurableActor[M, R]) processWithoutTransaction(ctx context.Context,
	delivery *Delivery[M, R]) {

	// Start the heartbeat goroutine for lease extension.
	heartbeatDone := make(chan struct{})
	go a.heartbeat(ctx, delivery, heartbeatDone)
	defer close(heartbeatDone)

	// Execute behavior with panic recovery.
	result := a.executeBehaviorSafely(ctx, delivery)

	// Hand the result to the shared non-transactional ack/nack bookkeeping.
	a.finishNonTx(ctx, delivery, result)
}

// finishNonTx applies ack/nack/dead-letter bookkeeping for a result that was
// produced outside a framework transaction (the processWithoutTransaction path
// and the uncommitted tail of processWithExec). It runs the durable writes
// under a detached, bounded context so they finish even if Stop cancels the
// actor context mid-commit.
func (a *DurableActor[M, R]) finishNonTx(ctx context.Context,
	delivery *Delivery[M, R], result fn.Result[R]) {

	// The message result is now known. Durable bookkeeping must be allowed
	// to finish even if Stop cancels the actor context while ack/processed
	// markers are being committed. In particular, SQLite's driver can race
	// internally when database/sql auto-rolls back a context-bound
	// transaction while an ExecContext is still in flight.
	bookkeepingCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), a.cleanupTimeout,
	)
	defer cancel()

	// For Ask messages, avoid marking as processed until after ack has
	// succeeded. This prevents a crash between MarkProcessed and Ack from
	// turning into a permanent "processed" flag while the mailbox message
	// (and Ask result) is still pending.
	if delivery.IsAsk() {
		if err := delivery.Ack(bookkeepingCtx, result); err != nil {
			logger(ctx).WarnS(ctx, "Failed to ack Ask message", err,
				"actor_id", a.id,
				"delivery_id", delivery.ID)

			return
		}

		if err := a.store.MarkProcessed(
			bookkeepingCtx, delivery.ID, a.id, a.deduplicationTTL,
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
		retry, _ := a.tellRetryPolicy(
			result.Err(), delivery.EffectiveAttempts(),
		)
		if retry {
			shouldMarkProcessed = false
		}
	}

	if shouldMarkProcessed {
		if err := a.store.MarkProcessed(
			bookkeepingCtx, delivery.ID, a.id, a.deduplicationTTL,
		); err != nil {

			logger(ctx).WarnS(ctx, "Failed to mark processed", err,
				"actor_id", a.id,
				"delivery_id", delivery.ID)
			// Continue anyway - dedup is defense in depth.
		}
	}

	// Handle the result.
	a.handleResult(bookkeepingCtx, delivery, result)
}

// processWithExec drives a TxBehavior through its Read/Commit execution path.
// The behavior does any slow side-effect IO without holding the writer, then
// commits state plus the lease-fenced ack in one short transaction. A lease
// heartbeat runs for the duration so a long IO middle does not let the lease
// expire underneath an in-progress Commit.
func (a *DurableActor[M, R]) processWithExec(ctx context.Context,
	delivery *Delivery[M, R], tb BoundTxBehavior[M, R]) {

	// The Read/Commit execution path does not yet support DurableAsk. On
	// this path the message is acked inside the behavior's own Commit, so
	// the framework cannot fold the crash-safe callback response into the
	// consuming transaction (the behavior owns the tx and its result is not
	// known until after Commit). Rather than silently consume the request
	// and leave the caller hanging, reject it with an explicit error
	// response. Full support is planned with the OOR effect-actor adopter.
	if delivery.IsDurableAsk() {
		a.rejectDurableAskOnExecPath(ctx, delivery)

		return
	}

	// Extend the lease while the behavior does IO outside the writer tx.
	// A leaseless (peeked) delivery holds no lease, so there is nothing to
	// heartbeat -- skip it to avoid a spurious "Failed to extend lease"
	// warning loop against an empty token. stop stays a safe no-op then.
	heartbeatDone := make(chan struct{})
	var stopHeartbeat sync.Once
	stop := func() {
		stopHeartbeat.Do(func() { close(heartbeatDone) })
	}
	if !delivery.leaseless {
		go a.heartbeat(ctx, delivery, heartbeatDone)
	}
	defer stop()

	core := &execCore{
		store:         a.txAwareStore,
		msgID:         delivery.ID,
		leaseToken:    delivery.LeaseToken,
		actorID:       a.id,
		dedupTTL:      a.deduplicationTTL,
		leaseDuration: a.leaseDuration,
	}

	// Offer the behavior a detachable promise so a pure-routing turn can
	// complete the Ask from a downstream future's continuation instead of
	// parking this goroutine on Await.
	var detachBox *askDetachBox
	if delivery.IsAsk() && !delivery.IsDurableAsk() {
		ctx, detachBox = withDetachableAskPromise(
			ctx, delivery.Promise, delivery.CallerCtx,
		)
	}

	result := a.runExecSafely(ctx, delivery, tb, core)

	// Stop the heartbeat as soon as the behavior returns. On the committed
	// path the message is already acked and deleted inside Commit, so a
	// late heartbeat tick would try to extend a gone lease and log a
	// spurious "Failed to extend lease" warning.
	stop()

	// A detached promise belongs to the behavior's continuation -- but only
	// for a successful turn. A failed turn may have errored before the
	// continuation was wired, so the framework still completes with the
	// error; promise completion is first-wins, so a racing continuation is
	// harmless.
	detached := detachBox != nil && detachBox.detached && result.IsOk()
	if detached {
		delivery.deferPromise = true
	}

	// If the behavior committed, the state write, dedup mark, and
	// lease-fenced ack are already durable in a single transaction. Only
	// the in-memory promise for an Ask caller remains.
	if core.committed {
		if delivery.IsAsk() && delivery.Promise != nil && !detached {
			delivery.Promise.Complete(result)
		}

		return
	}

	// The behavior returned without committing: it either failed before
	// Commit (e.g. the side-effect IO errored, so we nack for retry) or it
	// intentionally consumed the message without persisting state (e.g. a
	// RestartMessage). Either way, fall back to the framework's standard
	// non-transactional ack/nack handling.
	//
	// WARNING for TxBehavior authors: a handler that returns fn.Ok WITHOUT
	// calling ax.Commit lands here too, and finishNonTx then acks the
	// message via the non-lease-fenced path. On a single worker that is
	// merely a loss of crash-safety, but under an EgressWorkers/NumWorkers
	// pool it can double-process: if this worker's lease was reclaimed by a
	// competing worker mid-handling, the non-fenced ack deletes the row
	// that the other worker is now also processing. The framework cannot
	// distinguish "forgot to commit" from "intentionally consumed", so on
	// the success path you MUST call ax.Commit (even with an empty closure,
	// as the serverconn egress sender does) to get the lease fence.
	a.finishNonTx(ctx, delivery, result)
}

// rejectDurableAskOnExecPath fails a DurableAsk delivered to a Read/Commit
// (TxBehavior) actor. It writes an error AskResponse to the outbox so the
// caller receives a definitive failure instead of hanging, then consumes the
// message under the lease fence -- the error response, dedup mark, and ack all
// commit in one transaction. On any failure it nacks so the request is retried
// (and re-rejected) rather than lost. See ErrDurableAskUnsupported.
func (a *DurableActor[M, R]) rejectDurableAskOnExecPath(ctx context.Context,
	delivery *Delivery[M, R]) {

	// Use a detached, bounded context so the rejection bookkeeping finishes
	// even if Stop cancels the actor context mid-commit.
	bookkeepingCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), a.cleanupTimeout,
	)
	defer cancel()

	errResult := fn.Err[R](ErrDurableAskUnsupported)

	err := a.txAwareStore.ExecTx(bookkeepingCtx, false, func(
		txCtx context.Context, store DeliveryStore) error {

		if err := a.writeAskResponseToOutbox(
			txCtx, delivery, errResult, store,
		); err != nil {
			return err
		}

		if err := store.MarkProcessed(
			txCtx, delivery.ID, a.id, a.deduplicationTTL,
		); err != nil {
			return err
		}

		rows, err := ackMessage(
			txCtx, store, delivery.ID, delivery.LeaseToken,
		)
		if err != nil {
			return err
		}
		if rows == 0 {
			return ErrLeaseLost
		}

		return nil
	})
	if err == nil {
		logger(ctx).WarnS(ctx, "Rejected DurableAsk on Read/Commit "+
			"path with error response",
			ErrDurableAskUnsupported,
			"actor_id", a.id,
			"delivery_id", delivery.ID)

		return
	}

	logger(ctx).WarnS(ctx, "Failed to reject DurableAsk on Read/Commit "+
		"path, nacking for retry",
		err,
		"actor_id", a.id,
		"delivery_id", delivery.ID)

	if nackErr := delivery.Nack(
		bookkeepingCtx, err, 10*time.Second,
	); nackErr != nil {

		logger(ctx).WarnS(ctx, "Failed to nack after DurableAsk "+
			"reject failure",
			nackErr,
			"actor_id", a.id,
			"delivery_id", delivery.ID)
	}
}

// runExecSafely runs a TxBehavior with panic recovery, converting a panic into
// an error result so the caller treats it as a non-committed failure.
func (a *DurableActor[M, R]) runExecSafely(ctx context.Context,
	delivery *Delivery[M, R], tb BoundTxBehavior[M, R], core *execCore) (
	result fn.Result[R]) {

	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("panic: %v", r)

			logger(ctx).ErrorS(ctx, "Panic during tx message "+
				"processing",
				err,
				"actor_id", a.id,
				"delivery_id", delivery.ID)

			result = fn.Err[R](err)
		}
	}()

	return tb.run(ctx, core, delivery.Message)
}

// executeBehaviorSafely runs the behavior with panic recovery.
func (a *DurableActor[M, R]) executeBehaviorSafely(ctx context.Context,
	delivery *Delivery[M, R]) (result fn.Result[R]) {

	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("panic: %v", r)

			logger(ctx).ErrorS(ctx, "Panic during message "+
				"processing",
				err,
				"actor_id", a.id,
				"delivery_id", delivery.ID)

			result = fn.Err[R](err)
		}
	}()

	// This path only runs for a classic (Left) behavior; the Right path is
	// handled by processWithExec.
	classic := a.behavior.LeftToSome().UnwrapOr(nil)

	return classic.Receive(ctx, delivery.Message)
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
		effectiveAttempts := delivery.EffectiveAttempts()
		logger(ctx).WarnS(ctx,
			"Durable actor Tell message failed",
			err,
			"actor_id", a.id,
			"delivery_id", delivery.ID,
			"msg_type", delivery.Message.MessageType(),
			"attempts", delivery.Attempts,
			"effective_attempts", effectiveAttempts,
		)

		// Apply Tell retry policy.
		retry, delay := a.tellRetryPolicy(err, effectiveAttempts)
		if retry {
			// Don't mark as processed - we want retry to work.
			// nackMessage routes a leaseless (empty-token) delivery
			// to the by-ID nack, which increments attempts.
			_, nackErr := nackMessage(
				ctx, store, delivery.ID, delivery.LeaseToken,
				delay,
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
func (a *DurableActor[M, R]) handleResult(ctx context.Context,
	delivery *Delivery[M, R], result fn.Result[R]) {

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

			logger(ctx).WarnS(ctx, "Failed to mark DurableAsk "+
				"processed",
				err,
				"actor_id", a.id,
				"delivery_id", delivery.ID)
		}

		if err := delivery.Ack(ctx, result); err != nil {
			logger(ctx).WarnS(ctx, "Failed to ack DurableAsk "+
				"message",
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
		effectiveAttempts := delivery.EffectiveAttempts()
		logger(ctx).WarnS(ctx,
			"Durable actor Tell message failed",
			err,
			"actor_id", a.id,
			"delivery_id", delivery.ID,
			"msg_type", delivery.Message.MessageType(),
			"attempts", delivery.Attempts,
			"effective_attempts", effectiveAttempts,
		)

		// Apply Tell retry policy.
		retry, delay := a.tellRetryPolicy(err, effectiveAttempts)
		if retry {
			if nackErr := delivery.Nack(
				ctx, err, delay,
			); nackErr != nil {

				logger(ctx).WarnS(ctx, "Failed to nack Tell "+
					"message",
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

				logger(ctx).WarnS(ctx, "Failed to dead-letter "+
					"Tell message",
					dlErr,
					"actor_id", a.id,
					"delivery_id", delivery.ID)
			}

			// Delete from mailbox after dead-lettering.
			if delErr := a.store.DeleteMessage(
				ctx, delivery.ID,
			); delErr != nil {

				logger(ctx).WarnS(ctx, "Failed to delete "+
					"dead-lettered message",
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
			if err := delivery.Extend(
				ctx, a.leaseDuration,
			); err != nil {

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
		response = NewAskResponseError(
			delivery.CorrelationID, err.Error(),
		)
	} else {
		// Success response - try to encode the result.
		resultValue, _ := result.Unpack()

		// Check if the result is a TLVMessage.
		if tlvResult, ok := any(resultValue).(TLVMessage); ok {
			// Encode using the actor's codec.
			resultBlob, encErr := a.mailbox.cfg.Codec.Encode(
				tlvResult,
			)
			if encErr != nil {
				return fmt.Errorf("encode result: %w", encErr)
			}

			response = NewAskResponseSuccess(
				delivery.CorrelationID, resultBlob,
			)
		} else {
			// Result is not a TLVMessage (e.g., primitive like
			// int64). Store an empty blob - the caller can use the
			// correlation ID to look up the result via other means
			// if needed. For the generic case, we just acknowledge
			// completion.
			response = NewAskResponseSuccess(
				delivery.CorrelationID, nil,
			)
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
		"is_error", response.IsError(),
	)

	return nil
}

// Stop signals the actor to terminate.
func (a *DurableActor[M, R]) Stop() {
	a.stopOnce.Do(func() {
		a.cancel()
	})
}

// Wait blocks until the actor's processing loop has exited.
func (a *DurableActor[M, R]) Wait(ctx context.Context) error {
	if !a.started.Load() {
		return nil
	}

	select {
	case <-a.done:
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

// StopAndWait signals the actor to terminate and waits for shutdown.
func (a *DurableActor[M, R]) StopAndWait(ctx context.Context) error {
	a.Stop()

	return a.Wait(ctx)
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

	if err := ref.actor.mailbox.Send(ctx, env); err != nil {
		if errors.Is(err, ErrActorTerminated) {
			logger(ctx).DebugS(ctx, "Tell failed, routing to DLO",
				"actor_id", ref.actor.id,
				"msg_type", msg.MessageType(),
			)

			// Use context.Background() since the actor is
			// terminated and the original context might be done or
			// cancelled. Return the original mailbox error below so
			// callers receive the exact failure.
			ref.trySendToDLO(context.Background(), msg)
		}

		return err
	}

	return nil
}

// Ask sends a message and returns a Future for the response.
func (ref *durableActorRefImpl[M, R]) Ask(ctx context.Context,
	msg M) Future[R] {

	logger(ctx).TraceS(ctx, "Sending Ask to durable actor",
		"actor_id", ref.actor.id,
		"msg_type", msg.MessageType())

	promise := NewPromise[R]()

	// Check if actor is already terminated.
	if ref.actor.ctx.Err() != nil {
		logger(ctx).DebugS(ctx, "Ask failed, actor already terminated",
			"actor_id", ref.actor.id,
			"msg_type", msg.MessageType(),
		)

		promise.Complete(fn.Err[R](ErrActorTerminated))

		return promise.Future()
	}

	env := envelope[M, R]{
		message:   msg,
		promise:   promise,
		callerCtx: ctx,
	}

	if err := ref.actor.mailbox.Send(ctx, env); err != nil {
		promise.Complete(fn.Err[R](err))
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
// This provides crash-safe Ask semantics: if the caller crashes before
// receiving the response, the response will still be delivered when the caller
// restarts and resumes processing its mailbox.
//
// Returns an error if the message could not be durably enqueued.
func (ref *durableActorRefImpl[M, R]) DurableAsk(
	ctx context.Context,
	msg M,
	params DurableAskParams,
) error {

	if params.CallbackActorID == "" {
		return fmt.Errorf("callback actor ID is required for " +
			"DurableAsk")
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
		message: msg,
		// No in-memory promise - response via outbox.
		promise:         nil,
		callerCtx:       ctx,
		callbackActorID: params.CallbackActorID,
		correlationID:   params.CorrelationID,
	}

	if err := ref.actor.mailbox.Send(ctx, env); err != nil {
		if errors.Is(err, ErrActorTerminated) {
			logger(ctx).DebugS(
				ctx,
				"DurableAsk failed, actor terminated",
				"actor_id", ref.actor.id,
				"msg_type", msg.MessageType(),
			)
		}

		return err
	}

	return nil
}

// DurableActorRef extends ActorRef with durable Ask semantics.
// This interface is implemented by DurableActor references.
type DurableActorRef[M TLVMessage, R any] interface {
	ActorRef[M, R]

	// DurableAsk sends a message with callback metadata for durable
	// response delivery. The response will be delivered to the callback
	// actor's mailbox via the outbox.
	DurableAsk(ctx context.Context, msg M, params DurableAskParams) error
}

// Compile-time interface checks.
var (
	_ ActorRef[TLVMessage, any] = (*durableActorRefImpl[
		TLVMessage,
		any])(nil)
	_ DurableActorRef[TLVMessage, any] = (*durableActorRefImpl[
		TLVMessage,
		any])(nil)
)

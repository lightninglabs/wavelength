package actor

import (
	"context"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// mergeContexts creates a new context that cancels when either parent context
// cancels, enabling actors to respect both system shutdown and caller deadlines
// simultaneously. It preserves the shortest deadline between the two contexts
// to ensure the most restrictive timeout is honored.
//
// A background goroutine monitors both parent contexts and cancels the merged
// context when either parent cancels. The goroutine exits as soon as any
// cancellation is detected, preventing goroutine leaks. Callers must call the
// returned cancel function to release resources when the merged context is no
// longer needed.
//
// Performance note: This function spawns a goroutine for each call. For
// high-throughput actors processing thousands of messages per second, this
// overhead should be measured. The goroutine is short-lived and exits when
// message processing completes.
func mergeContexts(ctx1, ctx2 context.Context) (context.Context, context.CancelFunc) {
	// Get deadlines from both contexts.
	deadline1, hasDeadline1 := ctx1.Deadline()
	deadline2, hasDeadline2 := ctx2.Deadline()

	// Determine which context has the earliest deadline. By default, we'll
	// use ctx1 and only switch to ctx2 if it has an earlier deadline.
	baseCtx := ctx1
	if hasDeadline2 {
		if !hasDeadline1 || deadline2.Before(deadline1) {
			baseCtx = ctx2
		}
	}

	// Create a new context that will be cancelled explicitly.
	mergedCtx, cancel := context.WithCancel(baseCtx)

	// Watch both parent contexts and cancel the merged one when either
	// parent cancels.
	go func() {
		select {
		case <-ctx1.Done():
			cancel()
		case <-ctx2.Done():
			cancel()
		case <-mergedCtx.Done():
			// Already cancelled.
		}
	}()

	return mergedCtx, cancel
}

// ActorConfig holds the configuration parameters for creating a new Actor.
// It is generic over M (Message type) and R (Response type) to accommodate
// the actor's specific behavior.
type ActorConfig[M Message, R any] struct {
	// ID is the unique identifier for the actor.
	ID string

	// Behavior defines how the actor responds to messages.
	Behavior ActorBehavior[M, R]

	// DLO is a reference to the dead letter office for this actor system.
	// If nil, undeliverable messages during shutdown or due to a full
	// mailbox (if such logic were added) might be dropped.
	DLO ActorRef[Message, any]

	// MailboxSize defines the buffer capacity of the actor's mailbox.
	MailboxSize int

	// Wg is an optional WaitGroup for tracking actor lifecycle. If
	// non-nil, the actor will call Add(1) when starting and Done() when
	// its process loop exits. This enables deterministic shutdown.
	Wg *sync.WaitGroup

	// CleanupTimeout specifies the maximum duration for OnStop cleanup.
	// If None, a default of 5 seconds is used.
	CleanupTimeout fn.Option[time.Duration]
}

// envelope wraps a message with its associated promise and caller context. This
// allows the sender of an "ask" message to await a response. If the promise is
// nil, it signifies a "tell" operation (fire-and-forget). The callerCtx allows
// actors to respect request-scoped deadlines and cancellation.
type envelope[M Message, R any] struct {
	message   M
	promise   Promise[R]
	callerCtx context.Context
}

// Actor represents a concrete actor implementation. It encapsulates a behavior,
// manages its internal state implicitly through that behavior, and processes
// messages from its mailbox sequentially in its own goroutine.
type Actor[M Message, R any] struct {
	// id is the unique identifier for the actor.
	id string

	// behavior defines how the actor responds to messages.
	behavior ActorBehavior[M, R]

	// mailbox is the incoming message queue for the actor.
	mailbox Mailbox[M, R]

	// ctx is the context governing the actor's lifecycle.
	ctx context.Context

	// cancel is the function to cancel the actor's context.
	cancel context.CancelFunc

	// dlo is a reference to the dead letter office for this actor system.
	dlo ActorRef[Message, any]

	// wg is an optional WaitGroup for tracking this actor's lifecycle. If
	// non-nil, Done() is called when the process loop exits.
	wg *sync.WaitGroup

	// cleanupTimeout is the maximum duration for OnStop cleanup.
	cleanupTimeout time.Duration

	// startOnce ensures the actor's processing loop is started only once.
	startOnce sync.Once

	// stopOnce ensures the actor's processing loop is stopped only once.
	stopOnce sync.Once

	// ref is the cached ActorRef for this actor.
	ref ActorRef[M, R]
}

// NewActor creates a new actor instance with the given ID and behavior.
// It initializes the actor's internal structures but does not start its
// message processing goroutine. The Start() method must be called to begin
// processing messages.
func NewActor[M Message, R any](cfg ActorConfig[M, R]) *Actor[M, R] {
	ctx, cancel := context.WithCancel(context.Background())

	// Ensure MailboxSize has a sane default if not specified or zero.
	mailboxCapacity := cfg.MailboxSize
	if mailboxCapacity <= 0 {
		mailboxCapacity = 1
	}

	actor := &Actor[M, R]{
		id:             cfg.ID,
		behavior:       cfg.Behavior,
		mailbox:        NewChannelMailbox[M, R](ctx, mailboxCapacity),
		ctx:            ctx,
		cancel:         cancel,
		dlo:            cfg.DLO,
		wg:             cfg.Wg,
		cleanupTimeout: cfg.CleanupTimeout.UnwrapOr(5 * time.Second),
	}

	// Create and cache the actor's own reference.
	actor.ref = &actorRefImpl[M, R]{
		actor: actor,
	}

	return actor
}

// Start initiates the actor's message processing loop in a new goroutine.
// This method should be called exactly once after actor creation; repeated
// calls are safe but have no effect (enforced via startOnce). When a WaitGroup
// is configured, we increment it here to enable deterministic shutdown—the
// system can block on wg.Wait() to ensure all actor goroutines have fully
// exited before proceeding with resource cleanup.
func (a *Actor[M, R]) Start() {
	a.startOnce.Do(func() {
		log.DebugS(a.ctx, "Starting actor", "actor_id", a.id)

		if a.wg != nil {
			a.wg.Add(1)
		}
		go a.process()
	})
}

// process is the main event loop that drives actor message handling. We iterate
// over the mailbox using the receive iterator pattern, which automatically stops
// when the actor's context is cancelled during shutdown. The deferred Done()
// call (when wg is non-nil) ensures the WaitGroup counter is decremented even if
// the behavior panics, enabling the system to detect when all actors have
// terminated.
func (a *Actor[M, R]) process() {
	// Decrement the WaitGroup counter when this goroutine exits. Using defer
	// ensures this runs even if the behavior panics.
	if a.wg != nil {
		defer a.wg.Done()
	}

	// Process messages from the mailbox using the iterator pattern. The
	// iterator will stop when the actor's context is cancelled.
	for env := range a.mailbox.Receive(a.ctx) {
		// For Ask messages, merge the actor's context with the
		// caller's context so the behavior can detect both actor
		// shutdown and caller deadline expiration. For Tell messages,
		// use only the actor's context to preserve fire-and-forget
		// semantics. Once a Tell message is enqueued, it should not be
		// cancelled by the caller's context.
		var processCtx context.Context
		var cancel context.CancelFunc
		if env.promise != nil {
			processCtx, cancel = mergeContexts(a.ctx, env.callerCtx)
		} else {
			processCtx = a.ctx
			cancel = func() {}
		}

		log.TraceS(processCtx, "Actor processing message",
			"actor_id", a.id,
			"msg_type", env.message.MessageType(),
			"is_ask", env.promise != nil)

		result := a.behavior.Receive(processCtx, env.message)

		cancel()

		// If a promise was provided (i.e., it was an "ask" operation),
		// complete the promise with the result from the behavior.
		if env.promise != nil {
			env.promise.Complete(result)
		}
	}

	// The actor's context has been cancelled. Close the mailbox to prevent
	// new messages from being enqueued, then drain any remaining messages
	// to the DLO.
	a.mailbox.Close()

	// Drain any remaining messages that were enqueued before the mailbox
	// was closed.
	drainedCount := 0
	for env := range a.mailbox.Drain() {
		drainedCount++

		log.TraceS(a.ctx, "Draining message from terminated actor",
			"actor_id", a.id,
			"msg_type", env.message.MessageType(),
			"has_dlo", a.dlo != nil)

		// If a DLO is configured, send the original message there for
		// auditing or potential manual reprocessing.
		if a.dlo != nil {
			a.dlo.Tell(context.Background(), env.message)
		}

		// If it was an Ask, complete the promise with an error
		// indicating the actor terminated.
		if env.promise != nil {
			env.promise.Complete(fn.Err[R](ErrActorTerminated))
		}
	}

	// If the behavior implements the Stoppable interface, call its OnStop
	// hook to allow cleanup of external resources. Use a timeout to ensure
	// cleanup doesn't hang indefinitely.
	if stoppable, ok := a.behavior.(Stoppable); ok {
		cleanupCtx, cancel := context.WithTimeout(
			context.Background(), a.cleanupTimeout,
		)
		defer cancel()

		if err := stoppable.OnStop(cleanupCtx); err != nil {
			log.WarnS(a.ctx, "Actor cleanup error during shutdown",
				err, "actor_id", a.id)
		}
	}

	log.DebugS(a.ctx, "Actor terminated",
		"actor_id", a.id,
		"drained_messages", drainedCount)
}

// Stop signals the actor to terminate its processing loop and shut down.
// This is achieved by cancelling the actor's internal context. The actor's
// goroutine will exit once it detects the context cancellation, then close
// the mailbox and drain remaining messages to the DLO.
//
// Note: Messages cannot be lost between Receive() exiting and Close() being
// called because Send() checks actorCtx.Err() first, failing fast after
// context cancellation. Any message that passes the actorCtx check before
// cancellation will either complete its send or see actorCtx.Done() in the
// select and return false.
func (a *Actor[M, R]) Stop() {
	a.stopOnce.Do(func() {
		a.cancel()
	})
}

// actorRefImpl provides a concrete implementation of the ActorRef interface. It
// holds a reference to the target Actor instance, enabling message sending.
type actorRefImpl[M Message, R any] struct {
	actor *Actor[M, R]
}

// Tell sends a message without waiting for a response. If the context is
// cancelled before the message can be sent to the actor's mailbox, the message
// may be dropped.
//
//nolint:lll
func (ref *actorRefImpl[M, R]) Tell(ctx context.Context, msg M) {
	log.TraceS(ctx, "Sending Tell message",
		"actor_id", ref.actor.id,
		"msg_type", msg.MessageType())

	// Attempt to send the message to the mailbox. The mailbox's Send
	// method handles context cancellation and actor termination internally.
	env := envelope[M, R]{
		message:   msg,
		promise:   nil,
		callerCtx: ctx,
	}
	ok := ref.actor.mailbox.Send(ctx, env)

	// If the send failed, determine whether to route to DLO. We only send
	// to the DLO when the failure was due to actor termination or mailbox
	// closure (actor-side failures). If the caller's context was cancelled,
	// the message is intentionally dropped to preserve prior semantics
	// where caller-aborted messages are not revived via the DLO.
	if !ok {
		if ctx.Err() == nil || ref.actor.ctx.Err() != nil {
			log.DebugS(ctx, "Tell failed, routing to DLO",
				"actor_id", ref.actor.id,
				"msg_type", msg.MessageType())

			ref.trySendToDLO(msg)
		} else {
			log.TraceS(ctx, "Tell failed, caller cancelled",
				"actor_id", ref.actor.id,
				"msg_type", msg.MessageType())
		}
	}
}

// Ask sends a message and returns a Future for the response. The Future will be
// completed with the actor's reply or an error if the operation fails (e.g.,
// context cancellation before send).
//
//nolint:lll
func (ref *actorRefImpl[M, R]) Ask(ctx context.Context, msg M) Future[R] {
	log.TraceS(ctx, "Sending Ask message",
		"actor_id", ref.actor.id,
		"msg_type", msg.MessageType())

	// Create a new promise that will be fulfilled with the actor's
	// response.
	promise := NewPromise[R]()

	// If the actor's own context is already done, complete the promise with
	// ErrActorTerminated and return immediately. This is the primary guard
	// against trying to send to a stopped actor.
	if ref.actor.ctx.Err() != nil {
		log.DebugS(ctx, "Ask failed, actor already terminated",
			"actor_id", ref.actor.id,
			"msg_type", msg.MessageType())

		promise.Complete(fn.Err[R](ErrActorTerminated))
		return promise.Future()
	}

	// Attempt to send the message with the promise to the mailbox. The
	// mailbox's Send method handles context cancellation and actor
	// termination internally.
	env := envelope[M, R]{
		message:   msg,
		promise:   promise,
		callerCtx: ctx,
	}
	ok := ref.actor.mailbox.Send(ctx, env)

	// If the send failed (mailbox closed, context cancelled, or actor
	// terminated), complete the promise with an appropriate error.
	if !ok {
		// Determine the appropriate error based on the state. Check
		// the actor context first as actor termination takes
		// precedence over caller context cancellation.
		if ref.actor.ctx.Err() != nil {
			promise.Complete(fn.Err[R](ErrActorTerminated))
		} else {
			err := ctx.Err()
			if err == nil {
				// This indicates an unexpected state: the send
				// failed, but neither the actor nor the caller
				// context appears to be done. Default to
				// ErrActorTerminated as the most likely cause
				// (e.g., mailbox was closed directly).
				err = ErrActorTerminated
			}

			promise.Complete(fn.Err[R](err))
		}
	}

	// Return the future associated with the promise, allowing the caller to
	// await the response.
	return promise.Future()
}

// trySendToDLO attempts to send the message to the actor's DLO if configured.
func (ref *actorRefImpl[M, R]) trySendToDLO(msg M) {
	if ref.actor.dlo != nil {
		// Use context.Background() for sending to DLO as the
		// original context might be done or the operation
		// should not be bound by it.
		// This Tell to DLO is fire-and-forget.
		ref.actor.dlo.Tell(context.Background(), msg)
	}
}

// ID returns the unique identifier for this actor.
func (ref *actorRefImpl[M, R]) ID() string {
	return ref.actor.id
}

// Ref returns an ActorRef for this actor. This allows clients to interact with
// the actor (send messages) without having direct access to the Actor struct
// itself, promoting encapsulation and location transparency.
func (a *Actor[M, R]) Ref() ActorRef[M, R] {
	return a.ref
}

// TellRef returns a TellOnlyRef for this actor. This allows clients to send
// messages to the actor using only the "tell" pattern (fire-and-forget),
// without having access to "ask" capabilities.
func (a *Actor[M, R]) TellRef() TellOnlyRef[M] {
	return a.ref
}

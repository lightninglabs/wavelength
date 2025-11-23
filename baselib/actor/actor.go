package actor

import (
	"context"
	"sync"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// mergeContexts creates a new context that cancels when either parent context
// cancels. It preserves the shortest deadline between the two contexts.
func mergeContexts(ctx1, ctx2 context.Context) (context.Context, context.CancelFunc) {
	// Get deadlines from both contexts.
	deadline1, hasDeadline1 := ctx1.Deadline()
	deadline2, hasDeadline2 := ctx2.Deadline()

	// Determine which context has the earliest deadline.
	var baseCtx context.Context
	if hasDeadline1 && hasDeadline2 {
		if deadline1.Before(deadline2) {
			baseCtx = ctx1
		} else {
			baseCtx = ctx2
		}
	} else if hasDeadline1 {
		baseCtx = ctx1
	} else if hasDeadline2 {
		baseCtx = ctx2
	} else {
		// Neither has a deadline, use ctx1 as base.
		baseCtx = ctx1
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
		id:       cfg.ID,
		behavior: cfg.Behavior,
		mailbox:  NewChannelMailbox[M, R](ctx, mailboxCapacity),
		ctx:      ctx,
		cancel:   cancel,
		dlo:      cfg.DLO,
		wg:       cfg.Wg,
	}

	// Create and cache the actor's own reference.
	actor.ref = &actorRefImpl[M, R]{
		actor: actor,
	}

	return actor
}

// Start initiates the actor's message processing loop in a new goroutine. This
// method should be called once after the actor is created. If the actor was
// configured with a WaitGroup, it increments the counter to track the running
// goroutine.
func (a *Actor[M, R]) Start() {
	a.startOnce.Do(func() {
		if a.wg != nil {
			a.wg.Add(1)
		}
		go a.process()
	})
}

// process is the main event loop for the actor. It receives messages from the
// mailbox and processes them using the actor's behavior. If the actor was
// configured with a WaitGroup, it signals completion when the loop exits.
func (a *Actor[M, R]) process() {
	// Decrement the WaitGroup counter when this goroutine exits, enabling
	// deterministic shutdown.
	if a.wg != nil {
		defer a.wg.Done()
	}

	// Process messages from the mailbox using the iterator pattern. The
	// iterator will stop when the actor's context is cancelled.
	for env := range a.mailbox.Receive(a.ctx) {
		// Merge the actor's context with the caller's context. This
		// allows the behavior to detect both actor shutdown and
		// caller deadline expiration.
		mergedCtx, cancel := mergeContexts(a.ctx, env.callerCtx)

		result := a.behavior.Receive(mergedCtx, env.message)

		// Cancel the merged context to free resources.
		cancel()

		// If a promise was provided (i.e., it was an "ask" operation),
		// complete the promise with the result from the behavior.
		if env.promise != nil {
			env.promise.Complete(result)
		}
	}

	// The actor's context has been cancelled. Close the mailbox to prevent
	// new messages and drain any remaining messages.
	a.mailbox.Close()

	// Drain any remaining messages that were enqueued before the mailbox
	// was closed.
	for env := range a.mailbox.Drain() {
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
}

// Stop signals the actor to terminate its processing loop and shut down.
// This is achieved by cancelling the actor's internal context. The actor's
// goroutine will exit once it detects the context cancellation.
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
	// Attempt to send the message to the mailbox. The mailbox's Send
	// method handles context cancellation and actor termination internally.
	env := envelope[M, R]{
		message:   msg,
		promise:   nil,
		callerCtx: ctx,
	}
	ok := ref.actor.mailbox.Send(ctx, env)

	// If the send failed (mailbox closed, context cancelled, or actor
	// terminated), try to route the message to the DLO if available.
	if !ok {
		ref.trySendToDLO(msg)
	}
}

// Ask sends a message and returns a Future for the response. The Future will be
// completed with the actor's reply or an error if the operation fails (e.g.,
// context cancellation before send).
//
//nolint:lll
func (ref *actorRefImpl[M, R]) Ask(ctx context.Context, msg M) Future[R] {
	// Create a new promise that will be fulfilled with the actor's
	// response.
	promise := NewPromise[R]()

	// If the actor's own context is already done, complete the promise with
	// ErrActorTerminated and return immediately. This is the primary guard
	// against trying to send to a stopped actor.
	if ref.actor.ctx.Err() != nil {
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
		// Determine the appropriate error based on the state.
		if ref.actor.ctx.Err() != nil {
			promise.Complete(fn.Err[R](ErrActorTerminated))
		} else {
			promise.Complete(fn.Err[R](ctx.Err()))
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

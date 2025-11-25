package actor

import (
	"context"
	"fmt"
	"iter"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// ErrActorTerminated indicates that an operation failed because the target
// actor was terminated or in the process of shutting down.
var ErrActorTerminated = fmt.Errorf("actor terminated")

// ErrServiceKeyTypeMismatch indicates that a registration attempt failed
// because the service key name is already registered with a different message
// or response type.
var ErrServiceKeyTypeMismatch = fmt.Errorf("service key type mismatch")

// BaseMessage is a helper struct that can be embedded in message types defined
// outside the actor package to satisfy the Message interface's unexported
// messageMarker method.
type BaseMessage struct{}

// messageMarker implements the unexported method for the Message interface,
// allowing types that embed BaseMessage to satisfy the Message interface.
func (BaseMessage) messageMarker() {}

// Message is a sealed interface for actor messages. Actors will receive
// messages conforming to this interface. The interface is "sealed" by the
// unexported messageMarker method, meaning only types that can satisfy it
// (e.g., by embedding BaseMessage or being in the same package) can be Messages.
type Message interface {
	// messageMarker is a private method that makes this a sealed interface
	// (see BaseMessage for embedding).
	messageMarker()

	// MessageType returns the type name of the message for
	// routing/filtering.
	MessageType() string
}

// PriorityMessage is an extension of the Message interface for messages that
// carry a priority level. This can be used by actor mailboxes or schedulers
// to prioritize message processing.
type PriorityMessage interface {
	Message

	// Priority returns the processing priority of this message (higher =
	// more important).
	Priority() int
}

// Future represents the result of an asynchronous computation. It allows
// consumers to wait for the result (Await), apply transformations upon
// completion (ThenApply), or register a callback to be executed when the
// result is available (OnComplete).
type Future[T any] interface {
	// Await blocks until the result is available or the context is
	// cancelled, then returns it.
	Await(ctx context.Context) fn.Result[T]

	// ThenApply registers a function to transform the result of a future.
	// The original future is not modified, a new instance of the future is
	// returned. If the passed context is cancelled while waiting for the
	// original future to complete, the new future will complete with the
	// context's error.
	ThenApply(ctx context.Context, fn func(T) T) Future[T]

	// OnComplete registers a function to be called when the result of the
	// future is ready. If the passed context is cancelled before the future
	// completes, the callback function will be invoked with the context's
	// error.
	OnComplete(ctx context.Context, fn func(fn.Result[T]))
}

// Promise is an interface that allows for the completion of an associated
// Future. It provides a way to set the result of an asynchronous operation.
// The producer of an asynchronous result uses a Promise to set the outcome,
// while consumers use the associated Future to retrieve it.
type Promise[T any] interface {
	// Future returns the Future interface associated with this Promise.
	// Consumers can use this to Await the result or register callbacks.
	Future() Future[T]

	// Complete attempts to set the result of the future. It returns true if
	// this call successfully set the result (i.e., it was the first to
	// complete it), and false if the future had already been completed.
	Complete(result fn.Result[T]) bool
}

// BaseActorRef is a non-generic base interface for all actor references. This
// enables stronger typing in data structures that store heterogeneous actor
// references, such as the Receptionist's registration map. All ActorRef
// instances implement this interface.
//
// Type safety is enforced through generic type parameters on TellOnlyRef and
// ActorRef, plus the Receptionist's type registry which validates that service
// keys with the same name always have matching message and response types.
// External packages can implement this interface for testing purposes.
type BaseActorRef interface {
	// ID returns the unique identifier for this actor.
	ID() string
}

// TellOnlyRef is a reference to an actor that only supports "tell" operations.
// This is useful for scenarios where only fire-and-forget message passing is
// needed, or to restrict capabilities.
type TellOnlyRef[M Message] interface {
	BaseActorRef

	// Tell sends a message without waiting for a response. If the
	// context is cancelled before the message can be sent to the actor's
	// mailbox, the message may be dropped.
	Tell(ctx context.Context, msg M)
}

// ActorRef is a reference to an actor that supports both "tell" and "ask"
// operations. It embeds TellOnlyRef and adds the Ask method for
// request-response interactions.
type ActorRef[M Message, R any] interface {
	TellOnlyRef[M]

	// Ask sends a message and returns a Future for the response.
	// The Future will be completed with the actor's reply or an error
	// if the operation fails (e.g., context cancellation before send).
	Ask(ctx context.Context, msg M) Future[R]
}

// ActorBehavior defines the logic for how an actor processes incoming messages.
// It is a strategy interface that encapsulates the actor's reaction to messages.
type ActorBehavior[M Message, R any] interface {
	// Receive processes a message and returns a Result. The provided
	// context merges the actor's lifecycle context with the caller's
	// request context. It cancels when either the actor shuts down OR the
	// caller's deadline expires, allowing actors to respect request-scoped
	// timeouts while also detecting system shutdown.
	Receive(ctx context.Context, msg M) fn.Result[R]
}

// Stoppable is an optional interface that ActorBehavior implementations can
// implement to perform cleanup when the actor is stopping. This is useful for
// releasing external resources such as database connections, file handles, or
// network listeners that the behavior manages.
type Stoppable interface {
	// OnStop is called during actor shutdown, after the message processing
	// loop exits but before the actor's goroutine terminates. The provided
	// context has a deadline for cleanup operations. Implementations should
	// release resources and return promptly, respecting the context
	// deadline to avoid blocking system shutdown.
	OnStop(ctx context.Context) error
}

// SystemContext defines the minimal interface for system capabilities needed
// by actors and service keys. This narrow interface enables dependency
// injection and unit testing without requiring a full ActorSystem instance.
type SystemContext interface {
	// Receptionist returns the system's receptionist for actor discovery.
	Receptionist() *Receptionist

	// DeadLetters returns a reference to the dead letter actor for
	// undeliverable messages.
	DeadLetters() ActorRef[Message, any]
}

// Mailbox defines the interface for an actor's message queue. This abstraction
// allows different mailbox strategies to be plugged in, such as priority
// queues, durable on-disk queues, or backpressure-aware mailboxes, without
// changing the actor implementation.
//
// Thread Safety:
//   - Send and TrySend may be called concurrently from multiple goroutines.
//   - Receive should only be called from a single goroutine (the actor's
//     process loop).
//   - Close may be called concurrently with Send/TrySend and is idempotent.
//   - IsClosed may be called concurrently from any goroutine.
//   - Drain should only be called after Close and from a single goroutine.
//   - Send and TrySend return false after Close has been called.
type Mailbox[M Message, R any] interface {
	// Send attempts to send an envelope to the mailbox, blocking until
	// either the envelope is accepted, the provided context is cancelled,
	// or the actor's context is cancelled. It returns true if the envelope
	// was successfully sent, false otherwise.
	Send(ctx context.Context, env envelope[M, R]) bool

	// TrySend attempts to send an envelope to the mailbox without
	// blocking. It returns true if the envelope was successfully sent,
	// false if the mailbox is full or closed.
	TrySend(env envelope[M, R]) bool

	// Receive returns an iterator over envelopes in the mailbox. The
	// iterator will block when the mailbox is empty and yield envelopes as
	// they arrive. The iterator will stop when the provided context is
	// cancelled or when the mailbox is closed.
	Receive(ctx context.Context) iter.Seq[envelope[M, R]]

	// Close closes the mailbox, preventing any further sends. After
	// closing, Receive will yield any remaining envelopes and then stop.
	Close()

	// IsClosed returns true if the mailbox has been closed.
	IsClosed() bool

	// Drain returns an iterator over any remaining envelopes in the
	// mailbox after it has been closed. This is useful for cleanup logic
	// during actor shutdown.
	Drain() iter.Seq[envelope[M, R]]
}

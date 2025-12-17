package actor

import (
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// MapRef is a message-transforming wrapper around an ActorRef. It implements
// ActorRef[In, OutR] and forwards transformed messages to an ActorRef[Out, InR].
// This enables type-erased lookups (e.g., ServiceKey[Message, any]) to work
// with actors registered with concrete types.
//
// This is useful for adapters like OutboxPublisher which discover actors at
// runtime via ServiceKey[Message, any] but need to interact with actors that
// have specific message types.
type MapRef[In Message, Out Message, InR any, OutR any] struct {
	// targetRef is the underlying ActorRef that receives transformed messages.
	targetRef ActorRef[Out, InR]

	// mapInput transforms incoming messages from type In to type Out.
	mapInput func(In) (Out, error)

	// mapOutput transforms response from type InR to type OutR.
	mapOutput func(InR) OutR
}

// NewMapRef creates a new message-transforming wrapper around an ActorRef.
// The mapInput function transforms incoming messages; mapOutput transforms
// responses.
func NewMapRef[In Message, Out Message, InR any, OutR any](
	targetRef ActorRef[Out, InR],
	mapInput func(In) (Out, error),
	mapOutput func(InR) OutR,
) *MapRef[In, Out, InR, OutR] {

	return &MapRef[In, Out, InR, OutR]{
		targetRef: targetRef,
		mapInput:  mapInput,
		mapOutput: mapOutput,
	}
}

// TypeAssertingRef creates a MapRef that uses type assertion to convert
// messages. This is useful when the input type is a supertype of the target
// type (e.g., Message -> ConcreteMsg).
func TypeAssertingRef[In Message, Out Message, R any](
	targetRef ActorRef[Out, R],
) *MapRef[In, Out, R, any] {

	return NewMapRef(
		targetRef,
		func(in In) (Out, error) {
			out, ok := any(in).(Out)
			if !ok {
				var zero Out
				return zero, fmt.Errorf(
					"type assertion failed: expected %T, got %T",
					zero, in,
				)
			}

			return out, nil
		},
		func(r R) any { return r },
	)
}

// Tell transforms the incoming message using mapInput and forwards it to the
// target reference. Returns an error if transformation fails or the message
// could not be enqueued.
func (m *MapRef[In, Out, InR, OutR]) Tell(ctx context.Context, msg In) error {
	transformed, err := m.mapInput(msg)
	if err != nil {
		return fmt.Errorf("map input: %w", err)
	}

	return m.targetRef.Tell(ctx, transformed)
}

// Ask transforms the incoming message using mapInput, forwards it to the
// target reference, and transforms the response using mapOutput.
func (m *MapRef[In, Out, InR, OutR]) Ask(
	ctx context.Context, msg In,
) Future[OutR] {

	promise := NewPromise[OutR]()

	transformed, err := m.mapInput(msg)
	if err != nil {
		promise.Complete(fn.Err[OutR](fmt.Errorf("map input: %w", err)))

		return promise.Future()
	}

	// Call the inner Ask and transform the result.
	innerFuture := m.targetRef.Ask(ctx, transformed)

	go func() {
		result := innerFuture.Await(ctx)
		val, err := result.Unpack()
		if err != nil {
			promise.Complete(fn.Err[OutR](err))
		} else {
			promise.Complete(fn.Ok(m.mapOutput(val)))
		}
	}()

	return promise.Future()
}

// ID returns the target actor's identifier.
func (m *MapRef[In, Out, InR, OutR]) ID() string {
	return m.targetRef.ID()
}

// baseActorRefMarker implements the BaseActorRef sealed interface marker.
func (m *MapRef[In, Out, InR, OutR]) baseActorRefMarker() {}

// Compile-time check that MapRef implements ActorRef.
var _ ActorRef[Message, any] = (*MapRef[Message, Message, any, any])(nil)

package actor

import (
	"context"
	"fmt"
)

// MapInputRef is a message-transforming wrapper around a TellOnlyRef. It
// implements TellOnlyRef[In] and forwards transformed messages to a
// TellOnlyRef[Out]. This enables actors expecting one message type to receive
// notifications from sources producing a different (but relatable) type.
//
// This is useful for adapting notification sources to actor-specific event
// types. For example, a chain notification system might produce generic
// ConfirmationEvent messages, but a specific actor might expect its own
// ConfirmedTx type. MapInputRef bridges this gap.
type MapInputRef[In Message, Out Message] struct {
	// targetRef is the underlying TellOnlyRef that receives transformed
	// messages.
	targetRef TellOnlyRef[Out]

	// mapFn transforms incoming messages from type In to type Out.
	mapFn func(In) Out
}

// NewMapInputRef creates a new message-transforming wrapper around a
// TellOnlyRef. The mapFn function is called for each message to transform it
// from type In to type Out before forwarding to targetRef.
func NewMapInputRef[In Message, Out Message](targetRef TellOnlyRef[Out],
	mapFn func(In) Out) *MapInputRef[In, Out] {

	return &MapInputRef[In, Out]{
		targetRef: targetRef,
		mapFn:     mapFn,
	}
}

// Tell transforms the incoming message using mapFn and forwards it to the
// target reference. Returns an error if the message could not be enqueued.
func (m *MapInputRef[In, Out]) Tell(ctx context.Context, msg In) error {
	transformed := m.mapFn(msg)

	return m.targetRef.Tell(ctx, transformed)
}

// ID returns a composite identifier incorporating the target's ID.
func (m *MapInputRef[In, Out]) ID() string {
	return fmt.Sprintf("map-input->%s", m.targetRef.ID())
}

// baseActorRefMarker implements the BaseActorRef sealed interface marker.
func (m *MapInputRef[In, Out]) baseActorRefMarker() {}

// Compile-time check that MapInputRef implements TellOnlyRef.
//
//nolint:forcetypeassert
var _ TellOnlyRef[Message] = (*MapInputRef[Message, Message])(nil)

// FilterMapInputRef is a MapInputRef whose transform may also DROP a
// message: when mapFn reports false, Tell succeeds without forwarding
// anything. Use it to adapt a notification source whose lifecycle
// includes events the target actor has no message for (e.g. best-effort
// events that a later reliable event supersedes) — mapping such events
// to a sentinel target message would force every target to grow a
// no-op arm, and failing on them turns a benign notification into a
// spurious error.
type FilterMapInputRef[In Message, Out Message] struct {
	// targetRef is the underlying TellOnlyRef that receives transformed
	// messages.
	targetRef TellOnlyRef[Out]

	// mapFn transforms incoming messages from type In to type Out. A
	// false second return drops the message.
	mapFn func(In) (Out, bool)
}

// NewFilterMapInputRef creates a message-transforming wrapper around a
// TellOnlyRef whose transform may drop messages. mapFn is called for
// each message; when it reports false the message is dropped and Tell
// returns nil.
func NewFilterMapInputRef[In Message, Out Message](targetRef TellOnlyRef[Out],
	mapFn func(In) (Out, bool)) *FilterMapInputRef[In, Out] {

	return &FilterMapInputRef[In, Out]{
		targetRef: targetRef,
		mapFn:     mapFn,
	}
}

// Tell transforms the incoming message using mapFn and forwards it to
// the target reference, or silently drops it when mapFn reports false.
func (m *FilterMapInputRef[In, Out]) Tell(ctx context.Context, msg In) error {
	transformed, ok := m.mapFn(msg)
	if !ok {
		return nil
	}

	return m.targetRef.Tell(ctx, transformed)
}

// ID returns a composite identifier incorporating the target's ID.
func (m *FilterMapInputRef[In, Out]) ID() string {
	return fmt.Sprintf("filter-map-input->%s", m.targetRef.ID())
}

// baseActorRefMarker implements the BaseActorRef sealed interface marker.
func (m *FilterMapInputRef[In, Out]) baseActorRefMarker() {}

// Compile-time check that FilterMapInputRef implements TellOnlyRef.
//
//nolint:forcetypeassert
var _ TellOnlyRef[Message] = (*FilterMapInputRef[Message, Message])(nil)

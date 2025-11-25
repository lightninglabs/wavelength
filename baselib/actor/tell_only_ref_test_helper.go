package actor

import (
	"context"
	"time"
)

// ChannelTellOnlyRef is a TellOnlyRef implementation that sends messages to a
// channel. This is useful for testing actors that accept TellOnlyRef
// parameters.
type ChannelTellOnlyRef[M Message] struct {
	id   string
	msgs chan M
}

// NewChannelTellOnlyRef creates a new ChannelTellOnlyRef with the given ID and
// buffer size for the message channel.
func NewChannelTellOnlyRef[M Message](id string, bufSize int) *ChannelTellOnlyRef[M] {
	return &ChannelTellOnlyRef[M]{
		id:   id,
		msgs: make(chan M, bufSize),
	}
}

// Tell sends the message to the internal channel.
func (c *ChannelTellOnlyRef[M]) Tell(ctx context.Context, msg M) {
	select {
	case c.msgs <- msg:
	case <-ctx.Done():
	}
}

// ID returns the reference ID.
func (c *ChannelTellOnlyRef[M]) ID() string {
	return c.id
}

// baseActorRefMarker implements the BaseActorRef sealed interface marker.
func (c *ChannelTellOnlyRef[M]) baseActorRefMarker() {}

// Messages returns the underlying channel for receiving messages.
func (c *ChannelTellOnlyRef[M]) Messages() <-chan M {
	return c.msgs
}

// AwaitMessage waits for a message with a timeout. Returns the message and true
// if received, or the zero value and false if the timeout expires.
func (c *ChannelTellOnlyRef[M]) AwaitMessage(
	timeout time.Duration) (M, bool) {

	select {
	case msg := <-c.msgs:
		return msg, true
	case <-time.After(timeout):
		var zero M
		return zero, false
	}
}

// Compile-time check that ChannelTellOnlyRef implements TellOnlyRef.
var _ TellOnlyRef[Message] = (*ChannelTellOnlyRef[Message])(nil)

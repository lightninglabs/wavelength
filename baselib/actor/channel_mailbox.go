package actor

import (
	"context"
	"iter"
	"sync"
	"sync/atomic"
)

// ChannelMailbox is a Mailbox implementation backed by a Go channel. It
// provides thread-safe send and receive operations with support for context
// cancellation.
type ChannelMailbox[M Message, R any] struct {
	// ch is the underlying channel used to store envelopes.
	ch chan envelope[M, R]

	// closed indicates whether the mailbox has been closed. Uses atomic
	// operations for lock-free reads.
	closed atomic.Bool

	// mu protects send operations to prevent sending to a closed channel.
	mu sync.RWMutex

	// closeOnce ensures Close() is executed exactly once.
	closeOnce sync.Once

	// actorCtx is the context governing the actor's lifecycle. When this
	// context is cancelled, receive operations will terminate.
	actorCtx context.Context
}

// NewChannelMailbox creates a new channel-based mailbox with the given
// capacity and actor context. If capacity is 0 or negative, it defaults to 1
// to ensure the mailbox is buffered.
func NewChannelMailbox[M Message, R any](actorCtx context.Context,
	capacity int) *ChannelMailbox[M, R] {

	if capacity <= 0 {
		capacity = 1
	}

	return &ChannelMailbox[M, R]{
		ch:       make(chan envelope[M, R], capacity),
		actorCtx: actorCtx,
	}
}

// Send attempts to send an envelope to the mailbox. It blocks until either the
// envelope is accepted, the caller's context is cancelled, or the actor's
// context is cancelled. It returns nil if the envelope was successfully sent,
// or an error describing why the send failed.
func (m *ChannelMailbox[M, R]) Send(ctx context.Context,
	env envelope[M, R]) error {

	// Check contexts before acquiring the lock as an optimization. This
	// allows fast-path rejection when contexts are already cancelled,
	// avoiding unnecessary lock acquisition. The select statement below
	// still handles the case where contexts are cancelled after this check.
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.actorCtx.Err() != nil {
		return ErrActorTerminated
	}

	// Hold the read lock for the entire send operation to prevent
	// send-on-closed-channel panics. The read lock allows concurrent sends
	// but blocks when Close() acquires the write lock.
	//
	// Safety: The channel send in the select below cannot panic because:
	// 1. We hold the read lock for the entire operation
	// 2. Close() must acquire the write lock before closing the channel
	// 3. The write lock cannot be acquired while any read lock is held
	// 4. Therefore, the channel cannot be closed while we're in this block
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed.Load() {
		return ErrMailboxClosed
	}

	// Attempt to send the envelope, respecting both the caller's context
	// and the actor's context for cancellation.
	select {
	case m.ch <- env:
		logger(ctx).TraceS(ctx, "Mailbox send succeeded",
			"msg_type", env.message.MessageType(),
			"queue_len", len(m.ch))

		return nil

	case <-ctx.Done():
		logger(ctx).TraceS(ctx, "Mailbox send failed, caller context "+
			"cancelled",
			"msg_type", env.message.MessageType())

		return ctx.Err()

	case <-m.actorCtx.Done():
		logger(ctx).TraceS(ctx, "Mailbox send failed, actor context "+
			"cancelled",
			"msg_type", env.message.MessageType())

		return ErrActorTerminated
	}
}

// TrySend attempts to send an envelope to the mailbox without blocking. It
// returns nil if the envelope was successfully sent, or an error if the
// mailbox is full, closed, or the actor has been terminated.
func (m *ChannelMailbox[M, R]) TrySend(env envelope[M, R]) error {
	// Check if the actor has been terminated before attempting to send.
	// This ensures TrySend respects the actor's lifecycle consistently
	// with Send.
	if m.actorCtx.Err() != nil {
		return ErrActorTerminated
	}

	// Hold the read lock for the entire send operation to prevent
	// send-on-closed-channel panics.
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed.Load() {
		return ErrMailboxClosed
	}

	select {
	case m.ch <- env:
		return nil

	default:
		return ErrMailboxFull
	}
}

// Receive returns an iterator over envelopes in the mailbox. The iterator will
// yield envelopes as they arrive and will stop when the provided context is
// cancelled or when the mailbox is closed and drained.
//
// Context cancellation is checked before each receive attempt to ensure
// deterministic shutdown behavior. This prevents the select statement from
// racing between a ready channel and cancelled context.
func (m *ChannelMailbox[M, R]) Receive(
	ctx context.Context) iter.Seq[envelope[M, R]] {

	return func(yield func(envelope[M, R]) bool) {
		for {
			// Check context first for deterministic shutdown. This
			// ensures we stop receiving as soon as the context is
			// cancelled, rather than racing in the select.
			if ctx.Err() != nil {
				return
			}

			select {
			case env, ok := <-m.ch:
				if !ok {
					return
				}

				if !yield(env) {
					return
				}

			case <-ctx.Done():
				return
			}
		}
	}
}

// Close closes the mailbox, preventing any further sends. This method is safe
// to call multiple times; only the first call will have an effect. The write
// lock blocks concurrent sends, preventing send-on-closed-channel panics.
func (m *ChannelMailbox[M, R]) Close() {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		remainingMsgs := len(m.ch)
		logger(m.actorCtx).DebugS(m.actorCtx, "Mailbox closing",
			"remaining_messages", remainingMsgs,
		)

		m.closed.Store(true)
		close(m.ch)
	})
}

// IsClosed returns true if the mailbox has been closed. This method performs a
// lock-free read using atomic operations.
func (m *ChannelMailbox[M, R]) IsClosed() bool {
	return m.closed.Load()
}

// Drain returns an iterator over any remaining envelopes in the mailbox. This
// should only be called after Close() has been invoked. The iterator will
// yield all remaining envelopes and then stop. If the mailbox is not closed,
// it returns immediately without draining.
func (m *ChannelMailbox[M, R]) Drain() iter.Seq[envelope[M, R]] {
	return func(yield func(envelope[M, R]) bool) {
		// Only drain if the mailbox has been closed.
		if !m.IsClosed() {
			return
		}

		// Drain remaining messages using a non-blocking select to avoid
		// hanging if the channel is empty.
		for {
			select {
			case env, ok := <-m.ch:
				// Channel was closed and fully drained.
				if !ok {
					return
				}

				// Yield the envelope. If yield returns false,
				// the consumer wants to stop early.
				if !yield(env) {
					return
				}

			default:
				// No more messages available, return.
				return
			}
		}
	}
}

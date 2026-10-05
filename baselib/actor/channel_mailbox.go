package actor

import (
	"context"
	"fmt"
	"iter"
	"sync"
	"sync/atomic"
)

// ErrMailboxOverflow is returned by a send made from inside a receive turn
// when the mailbox's overflow queue is already at its hard cap. The send had
// no effect: nothing was enqueued and the mailbox state is unchanged, so the
// caller may retry once the receiver has drained below the cap. The error
// returned by the mailbox wraps this value with the mailbox ID, so test for
// it with errors.Is.
//
// A durable sender that returns the error from its behavior gets natural
// backpressure: the delivery is nacked and redelivered later, by which time
// the receiver has usually caught up.
var ErrMailboxOverflow = fmt.Errorf("mailbox overflow limit reached")

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

	// id names the mailbox in logs. It is normally the owning actor's ID.
	id string

	// overflowMu guards the overflow queue and the episode bookkeeping
	// below. Lock order is always mu (read side) first, then overflowMu.
	overflowMu sync.Mutex

	// overflow holds envelopes sent from inside a receive turn that did
	// not fit in ch, oldest first.
	overflow []envelope[M, R]

	// overflowLen mirrors len(overflow). It is written under overflowMu
	// (or, once closed, by the lone draining goroutine) and read without
	// any lock, so OverflowLen never contends with senders or the receive
	// loop.
	overflowLen atomic.Int64

	// overflowLimit is the hard cap on the number of queued overflow
	// envelopes. A send that would exceed it fails with
	// ErrMailboxOverflow, see enqueueOverflow.
	overflowLimit int

	// overflowCapLogged is set once the hard cap has been reported at
	// critical severity in the current overflow episode, so a sender that
	// keeps retrying against a full queue does not log per message. It is
	// cleared together with overflowWarned when the overflow drains.
	overflowCapLogged bool

	// overflowWarned is set once the soft threshold has been logged for
	// the current overflow episode. An episode runs from the first append
	// to an empty overflow until the overflow drains back to empty.
	overflowWarned bool
}

// DefaultMailboxOverflowLimit is the default hard cap on envelopes queued in
// a ChannelMailbox's overflow queue by sends made from inside a receive turn.
// A send that would exceed it fails with ErrMailboxOverflow.
const DefaultMailboxOverflowLimit = 100_000

// ChannelMailboxOption configures optional ChannelMailbox behavior.
type ChannelMailboxOption func(*mailboxOptions)

// mailboxOptions collects the optional settings of a ChannelMailbox.
type mailboxOptions struct {
	overflowLimit int
	id            string
}

// WithOverflowLimit sets the hard cap on the in-turn overflow queue. A send
// that would exceed it fails with ErrMailboxOverflow. A value of zero or less
// selects DefaultMailboxOverflowLimit.
func WithOverflowLimit(limit int) ChannelMailboxOption {
	return func(o *mailboxOptions) {
		o.overflowLimit = limit
	}
}

// WithMailboxID sets the identifier the mailbox includes in its logs.
func WithMailboxID(id string) ChannelMailboxOption {
	return func(o *mailboxOptions) {
		o.id = id
	}
}

// NewChannelMailbox creates a new channel-based mailbox with the given
// capacity and actor context. If capacity is 0 or negative, it defaults to 1
// to ensure the mailbox is buffered.
func NewChannelMailbox[M Message, R any](actorCtx context.Context, capacity int,
	opts ...ChannelMailboxOption) *ChannelMailbox[M, R] {

	if capacity <= 0 {
		capacity = 1
	}

	var o mailboxOptions
	for _, opt := range opts {
		opt(&o)
	}
	if o.overflowLimit <= 0 {
		o.overflowLimit = DefaultMailboxOverflowLimit
	}

	return &ChannelMailbox[M, R]{
		ch:            make(chan envelope[M, R], capacity),
		actorCtx:      actorCtx,
		id:            o.id,
		overflowLimit: o.overflowLimit,
	}
}

// OverflowLen returns the number of envelopes currently parked in the
// overflow queue behind the channel.
func (m *ChannelMailbox[M, R]) OverflowLen() int {
	return int(m.overflowLen.Load())
}

// popOverflowLocked removes and returns the oldest overflow envelope. The
// caller must hold overflowMu, or be the single draining goroutine once the
// mailbox is closed, and must have checked the queue is non-empty.
// Resetting the episode state when the queue empties lives here so every
// consumer of the queue shares it.
func (m *ChannelMailbox[M, R]) popOverflowLocked() envelope[M, R] {
	env := m.overflow[0]

	// Zero the slot so the envelope can be collected, then reslice past
	// it. The dead prefix can't grow without bound: once append exhausts
	// the shrunken capacity it reallocates and copies only live entries.
	m.overflow[0] = envelope[M, R]{}
	m.overflow = m.overflow[1:]
	m.overflowLen.Add(-1)

	if len(m.overflow) == 0 {
		m.overflow = nil

		if m.overflowWarned {
			logger(m.actorCtx).InfoS(m.actorCtx, "Mailbox "+
				"overflow drained", "mailbox_id", m.id)
		}
		m.overflowWarned = false
		m.overflowCapLogged = false
	}

	return env
}

// enqueueOverflow is the in-turn send path. It never blocks: the envelope
// goes straight into the channel when there is room and nothing is queued
// ahead of it, and otherwise is appended to the overflow queue.
//
// Appending whenever the overflow is non-empty, even if the channel has since
// gained room, is what keeps a single sender's messages in order: a later
// message must not jump ahead of an earlier one still sitting in the overflow.
//
// If the queue would exceed the hard cap the send is rejected with
// ErrMailboxOverflow, wrapped with the mailbox ID, and nothing is enqueued.
// The first rejection of an overflow episode is logged at critical severity,
// since a consumer that cannot catch up needs a human; later rejections in
// the same episode are not logged, so a retrying sender cannot flood the log.
// Returning an error rather than panicking matters because durable actors
// recover behavior panics, which would swallow the failure, whereas an error
// reaches the sender's own error handling and, for a durable sender, turns
// into a nack and a later retry. Parking the sender instead would recreate
// the silent deadlock this path exists to remove.
func (m *ChannelMailbox[M, R]) enqueueOverflow(env envelope[M, R]) error {
	// Lock order: mu (read side) then overflowMu. The read lock is the
	// close guard that makes the channel send below safe.
	m.mu.RLock()
	defer m.mu.RUnlock()

	m.overflowMu.Lock()
	defer m.overflowMu.Unlock()

	if m.closed.Load() {
		return ErrMailboxClosed
	}

	queued := len(m.overflow)
	if queued == 0 {
		select {
		case m.ch <- env:
			return nil

		default:
		}
	}

	if queued+1 > m.overflowLimit {
		if !m.overflowCapLogged {
			m.overflowCapLogged = true

			logger(m.actorCtx).CriticalS(m.actorCtx, "Mailbox "+
				"overflow limit reached, rejecting sends", nil,
				"mailbox_id", m.id,
				"overflow_len", queued,
				"overflow_limit", m.overflowLimit,
				"msg_type", env.message.MessageType())
		}

		return fmt.Errorf("%w: mailbox %q at %d queued messages",
			ErrMailboxOverflow, m.id, m.overflowLimit)
	}

	m.overflow = append(m.overflow, env)
	m.overflowLen.Add(1)
	queued++

	softLimit := max(m.overflowLimit/10, 1)
	if queued >= softLimit && !m.overflowWarned {
		m.overflowWarned = true

		logger(m.actorCtx).WarnS(m.actorCtx, "Mailbox overflow "+
			"growing", nil,
			"mailbox_id", m.id,
			"overflow_len", queued,
			"overflow_limit", m.overflowLimit)
	}

	return nil
}

// refillFromOverflow moves queued overflow envelopes into the channel, oldest
// first, for as long as the channel has room. Only the receive loop calls it,
// right after taking an envelope off the channel.
//
// This maintains the invariant that a non-empty overflow implies a non-empty
// channel, so the receiver never sleeps on an empty channel while overflow
// envelopes wait. After a refill either the overflow is empty or a
// non-blocking send just failed, meaning the channel was full. Nothing but
// the receiver removes envelopes from the channel, so it stays non-empty
// until the receiver's next receive, which refills again. An in-turn sender
// only appends under overflowMu when the channel is full or the overflow is
// already non-empty. If the receiver frees a slot between that sender's
// failed channel send and its append, the receiver's refill needs overflowMu,
// so it runs after the append and sees the new envelope. External senders
// write to the channel directly and can only make it fuller.
//
// Fairness caveat: Go hands a freed slot straight to a sender parked on the
// full channel, as part of the receive itself. So while external producers
// stay parked on this mailbox, refill finds the channel full and the overflow
// does not drain. Under sustained external saturation in-turn traffic keeps
// accumulating until the hard cap rejects it with ErrMailboxOverflow. That
// trades a slow mailbox for a loud failure under overload, which is the
// intended direction; external producers are expected to be paced by their
// own ingress limits.
func (m *ChannelMailbox[M, R]) refillFromOverflow() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	m.overflowMu.Lock()
	defer m.overflowMu.Unlock()

	// After Close the channel is closed, so sending would panic. Drain
	// picks up whatever is left in the overflow.
	if m.closed.Load() {
		return
	}

	for len(m.overflow) > 0 {
		select {
		case m.ch <- m.overflow[0]:
			m.popOverflowLocked()

		default:
			return
		}
	}
}

// Send attempts to send an envelope to the mailbox. A send made with the
// context of an active receive turn never blocks: it enqueues, or fails with
// ErrMailboxOverflow once the overflow queue is at its hard cap, see
// enqueueOverflow. Any other send blocks until either the envelope is
// accepted, the caller's context is cancelled, or the actor's context is
// cancelled. It returns nil if the envelope was successfully sent, or an
// error describing why the send failed. A failed send leaves the mailbox
// unchanged.
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

	// A send made with an active receive turn's context comes from an
	// actor's own goroutine. Parking it on a full mailbox can deadlock a
	// cycle of actors, so it never blocks and spills into the overflow.
	// Any other producer keeps the blocking backpressure below.
	if _, ok := activeTurn(ctx); ok {
		return m.enqueueOverflow(env)
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

	// Hold overflowMu across the check and the send. Reporting full while
	// the overflow is non-empty keeps a TrySend from overtaking an earlier
	// in-turn Send from the same sender, which is still queued there.
	m.overflowMu.Lock()
	defer m.overflowMu.Unlock()

	if len(m.overflow) > 0 {
		return ErrMailboxFull
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

				// Top the channel back up from the overflow
				// before running the behavior, which may take
				// arbitrarily long.
				m.refillFromOverflow()

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

		// The write lock excludes every sender and the refill, which
		// all touch the overflow under the read lock, so the length can
		// be read directly here.
		remainingMsgs := len(m.ch) + m.OverflowLen()

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
// should only be called after Close() has been invoked. The iterator yields
// the remaining channel envelopes and then the overflow envelopes, which
// preserves send order since the overflow always trails the channel. If the
// mailbox is not closed, it returns immediately without draining. Only one
// goroutine may drain, which is the actor loop that closed the mailbox.
func (m *ChannelMailbox[M, R]) Drain() iter.Seq[envelope[M, R]] {
	return func(yield func(envelope[M, R]) bool) {
		// Only drain if the mailbox has been closed.
		if !m.IsClosed() {
			return
		}

		// Drain remaining messages using a non-blocking select to avoid
		// hanging if the channel is empty.
	chanLoop:
		for {
			select {
			case env, ok := <-m.ch:
				// Channel was closed and fully drained.
				if !ok {
					break chanLoop
				}

				// Yield the envelope. If yield returns false,
				// the consumer wants to stop early.
				if !yield(env) {
					return
				}

			default:
				break chanLoop
			}
		}

		// No lock is needed for the overflow here. Close set closed
		// under the write lock, and every writer (in-turn sends and the
		// refill) checks closed under the read lock before touching the
		// queue, so nothing appends or pops once Close has returned.
		// Drain runs on the actor loop right after Close, which makes
		// it the queue's only user; OverflowLen reads the atomic
		// mirror.
		for len(m.overflow) > 0 {
			env := m.popOverflowLocked()

			if !yield(env) {
				return
			}
		}
	}
}

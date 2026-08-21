package actor

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightningnetwork/lnd/clock"
)

const (
	// UnlimitedRestarts is the DurableActorConfig.MaxRestarts value that
	// disables the intensity budget entirely, letting the actor restart
	// from its checkpoint for as long as it keeps panicking. It is the
	// default, because restarting forever is strictly no worse than the
	// nack-and-continue loop supervision replaces (both are rate-limited
	// by the nack backoff), while a finite budget introduces a genuinely
	// new failure mode: silent permanent death.
	UnlimitedRestarts = -1

	// DefaultMaxRestarts is the restart budget a durable actor gets when
	// its config does not choose one. It is UnlimitedRestarts: a finite
	// intensity budget kills the actor for good, which is only a safe
	// trade where the actor's owner is watching for that event, so it has
	// to be chosen rather than inherited. See
	// DurableActorConfig.MaxRestarts.
	DefaultMaxRestarts = UnlimitedRestarts

	// RecommendedMaxRestarts is the intensity an owner that does wire a
	// Watch observer should reach for. It matches the BEAM's default
	// one_for_one supervisor intensity, counted over
	// DefaultRestartWindow.
	RecommendedMaxRestarts = 5

	// DefaultRestartWindow is the width of the sliding window a finite
	// MaxRestarts is counted over when the config does not set one.
	DefaultRestartWindow = 60 * time.Second
)

// TerminationReason classifies why a durable actor's supervision loop exited.
// Exactly one reason is reported per actor, carried on the single
// TerminationInfo delivered to every registered watcher.
type TerminationReason uint8

const (
	// TerminationStopped means the actor exited because Stop (or
	// StopAndWait) was called. This is the graceful path: every worker
	// drained out, the mailbox was closed, and the behavior's OnStop hook
	// ran.
	TerminationStopped TerminationReason = iota

	// TerminationContextCancelled means the actor's lifetime context was
	// cancelled without Stop having been called. The actor's context is
	// currently rooted at context.Background, so this reason is reserved
	// for a future construction path that accepts an externally owned
	// lifetime context.
	TerminationContextCancelled

	// TerminationRestartIntensityExceeded means the behavior panicked more
	// often than the configured MaxRestarts / RestartWindow budget allows,
	// so supervision gave up rather than restarting the actor again. Err
	// carries the panic that broke the budget.
	TerminationRestartIntensityExceeded

	// TerminationRestartFailed means a restart was within budget but could
	// not be carried out: the FSM checkpoint would not load, or the
	// RestartMessage would not enqueue. Restarting anyway would hand the
	// behavior a blank slate in place of its persisted state, so the actor
	// terminates instead. Err carries the failure.
	TerminationRestartFailed
)

// String returns a human readable name for the termination reason.
func (r TerminationReason) String() string {
	switch r {
	case TerminationStopped:
		return "stopped"

	case TerminationContextCancelled:
		return "context_cancelled"

	case TerminationRestartIntensityExceeded:
		return "restart_intensity_exceeded"

	case TerminationRestartFailed:
		return "restart_failed"

	default:
		return fmt.Sprintf("unknown(%d)", uint8(r))
	}
}

// TerminationInfo describes how and why a durable actor stopped. It is the
// single value delivered on every channel handed out by
// (*DurableActor).Watch.
type TerminationInfo struct {
	// ActorID is the ID of the actor that terminated.
	ActorID string

	// Reason classifies the termination.
	Reason TerminationReason

	// Err carries the failure behind a terminal-failure reason: the panic
	// for TerminationRestartIntensityExceeded, the bookkeeping error for
	// TerminationRestartFailed. It is nil for the graceful reasons.
	Err error

	// Restarts is how many times the actor was restarted from its
	// checkpoint over its whole lifetime, counting the restart that broke
	// the intensity budget.
	Restarts int

	// RestartsExhausted reports whether the actor died because it ran out
	// of restart budget, as opposed to being stopped or failing to
	// restart.
	RestartsExhausted bool
}

// behaviorPanic is the error a recovered behavior panic is converted into. It
// is what separates "the behavior returned an error" (an ordinary, retryable
// message failure) from "the behavior panicked" (its in-memory state is now
// suspect, so the actor must be restarted from its checkpoint). Both the
// recovered value and the stack captured at the recover site are retained so
// the termination notification carries something an operator can act on.
type behaviorPanic struct {
	// value is the value that was passed to panic.
	value any

	// stack is the goroutine stack captured at the recover site.
	stack []byte
}

// newBehaviorPanic wraps a recovered panic value along with the current stack.
func newBehaviorPanic(value any) *behaviorPanic {
	return &behaviorPanic{
		value: value,
		stack: debug.Stack(),
	}
}

// Error implements the error interface. The rendering matches the message the
// runtime produced before supervision existed ("panic: <value>"), so the
// nack, dead-letter reason, and log strings a panicking behavior generates are
// unchanged.
func (p *behaviorPanic) Error() string {
	return fmt.Sprintf("panic: %v", p.value)
}

// Stack returns the goroutine stack captured where the panic was recovered.
func (p *behaviorPanic) Stack() []byte {
	return p.stack
}

// isBehaviorPanic reports whether err came from a panicking behavior rather
// than from a behavior that returned a failed result.
func isBehaviorPanic(err error) bool {
	var bp *behaviorPanic

	return errors.As(err, &bp)
}

// restartTracker enforces a BEAM-style restart intensity budget: at most max
// restarts inside a sliding window of the configured width. It is only ever
// touched from the supervision goroutine, so it carries no lock of its own.
type restartTracker struct {
	// max is how many restarts are allowed inside window. A negative value
	// disables the budget.
	max int

	// window is the width of the sliding window.
	window time.Duration

	// clock supplies the current time so tests can drive the window
	// deterministically.
	clock clock.Clock

	// stamps holds the times of the restarts still inside the window, in
	// ascending order. It is written and read only by the supervision
	// goroutine.
	stamps []time.Time

	// total counts every restart the actor has ever taken, including the
	// ones that have since aged out of the window. It is atomic because it
	// is the one field observers outside supervision read.
	total atomic.Int64
}

// newRestartTracker builds a tracker over the given budget. A non-positive
// window falls back to DefaultRestartWindow.
func newRestartTracker(max int, window time.Duration,
	clk clock.Clock) *restartTracker {

	if window <= 0 {
		window = DefaultRestartWindow
	}

	return &restartTracker{
		max:    max,
		window: window,
		clock:  clk,
	}
}

// record registers one restart at the current time and reports whether the
// actor is still inside its intensity budget. It returns false when the
// restart being recorded is the one that breaks the budget, which is the
// signal for supervision to terminate the actor permanently.
func (r *restartTracker) record() bool {
	now := r.clock.Now()
	r.total.Add(1)

	// Drop the restarts that have aged out of the sliding window, reusing
	// the backing array so a long-lived actor that restarts occasionally
	// does not grow the slice without bound.
	cutoff := now.Add(-r.window)
	kept := r.stamps[:0]
	for _, ts := range r.stamps {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	r.stamps = append(kept, now)

	// A negative budget is the explicit "restart forever" opt-in.
	if r.max < 0 {
		return true
	}

	return len(r.stamps) <= r.max
}

// count returns how many restarts the actor has taken over its whole lifetime.
// It is safe to call from any goroutine.
func (r *restartTracker) count() int {
	return int(r.total.Load())
}

// watcherRegistry holds the termination watchers registered against a durable
// actor. Delivery is one buffered value per watcher followed by a close, so
// notifying watchers can never park the actor's shutdown path no matter how
// slowly a watcher reads.
type watcherRegistry struct {
	// mu guards every field below. It is held across the notification
	// sends, which is safe precisely because those sends cannot block.
	mu sync.Mutex

	// watchers maps a registration handle to the channel to notify. A
	// handle is removed as soon as it has been notified or its watching
	// context was cancelled.
	watchers map[uint64]chan TerminationInfo

	// nextID is the next registration handle to hand out.
	nextID uint64

	// terminated records whether the termination notification has already
	// been published, so a watcher that registers afterwards is served
	// immediately from info instead of waiting forever.
	terminated bool

	// info is the published termination notification. It is only
	// meaningful once terminated is set.
	info TerminationInfo

	// published closes once the terminal notification has been delivered.
	// A watcher's cleanup goroutine parks on it rather than on the actor's
	// done channel, which never closes for an actor that was stopped
	// without ever being started.
	published chan struct{}
}

// newWatcherRegistry builds an empty registry.
func newWatcherRegistry() *watcherRegistry {
	return &watcherRegistry{
		watchers:  make(map[uint64]chan TerminationInfo),
		published: make(chan struct{}),
	}
}

// done returns a channel that closes once the terminal notification has been
// published.
func (w *watcherRegistry) done() <-chan struct{} {
	return w.published
}

// add registers a new watcher. It returns the channel to hand to the caller,
// the registration handle, and whether the actor had already terminated. In
// that last case the channel comes back already loaded with the notification
// and closed, and the handle is not registered.
func (w *watcherRegistry) add() (chan TerminationInfo, uint64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// A buffer of one is what makes the eventual send unconditionally
	// non-blocking: exactly one value is ever sent per channel.
	ch := make(chan TerminationInfo, 1)

	if w.terminated {
		ch <- w.info
		close(ch)

		return ch, 0, true
	}

	id := w.nextID
	w.nextID++
	w.watchers[id] = ch

	return ch, id, false
}

// remove deregisters a watcher that is no longer interested and closes its
// channel, so a caller ranging over it observes the end of the stream. It is a
// no-op once the watcher has been notified.
func (w *watcherRegistry) remove(id uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	ch, ok := w.watchers[id]
	if !ok {
		return
	}

	delete(w.watchers, id)
	close(ch)
}

// publish records the terminal notification and delivers it to every
// registered watcher exactly once. Each send targets a single-use buffered
// channel, so no watcher can park the actor's shutdown path. It reports
// whether this call was the one that published; later calls are no-ops.
func (w *watcherRegistry) publish(info TerminationInfo) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.terminated {
		return false
	}

	w.terminated = true
	w.info = info

	for id, ch := range w.watchers {
		// The channel has a buffer of one and is written exactly once,
		// so this send always succeeds. The default arm exists so a
		// future change cannot quietly reintroduce a shutdown path
		// that blocks on a watcher.
		select {
		case ch <- info:
		default:
		}

		close(ch)
		delete(w.watchers, id)
	}

	close(w.published)

	return true
}

// warmupBarrier holds the rest of a competing-consumer pool behind the first
// message of a worker generation, so a RestartMessage hand-off is not raced by
// a sibling worker claiming the next row.
//
// The problem it solves is specific to NumWorkers > 1. A RestartMessage
// carries RestartPriority so the claim query hands it out first, but "first"
// only orders the claims, not the turns: launching the whole pool at once lets
// one worker take the restart while a sibling immediately takes the row behind
// it, and a normal turn then runs against the same behavior instance while the
// restart handler is still rebuilding it from the checkpoint. The documented
// guarantee that a restart message is processed before all other messages
// needs the pool to warm up one worker at a time to actually hold.
//
// The release rule is deliberately shaped so that it cannot wedge a pool that
// has no hand-off waiting for it. The warm-up worker holds the barrier only
// across a restart turn; the first claim of anything else opens it before that
// message is even processed, and a generation whose mailbox turns out to be
// empty opens it on the first idle tick. Whatever ends the warm-up worker
// opens it too, so a panic, a dead-lettered restore, or a closed mailbox all
// release the pool rather than stranding it at one worker.
type warmupBarrier struct {
	// released closes when the pool may fan out.
	released chan struct{}

	// openOnce keeps the close idempotent, since several paths race to
	// open the barrier (the warm-up worker's claim, its exit, and the
	// idle tick).
	openOnce sync.Once

	// claimed records that the warm-up worker has taken at least one
	// message. It is what separates "the restart turn is still running"
	// from "there was never a hand-off here", which the idle tick would
	// otherwise be unable to tell apart.
	claimed atomic.Bool

	// required records that supervision KNOWS a restart row is waiting for
	// this generation, because it enqueued that row itself. It disables
	// the idle tick, which is what makes the guarantee exact rather than
	// timing-dependent on the path this kernel creates: without it a tick
	// that fired before the warm-up worker got its first claim would fan
	// the pool out into the very race the barrier exists to prevent.
	required atomic.Bool
}

// newWarmupBarrier returns a barrier when one is wanted, and nil otherwise. A
// nil barrier is a working no-op through every method below, so a
// single-worker actor (which is already strictly sequential and needs no
// barrier at all) runs the identical code path with nothing to pay for.
//
// A required barrier is one supervision enqueued the restart row for, so it
// waits for that row unconditionally. A barrier that is merely wanted covers
// the boot hand-off, which an owner enqueues before Start and which the actor
// therefore cannot see: there the barrier waits for the first claim and lets
// an idle tick release it, which orders the common case without being able to
// prove a row was ever there.
func newWarmupBarrier(wanted, required bool) *warmupBarrier {
	if !wanted {
		return nil
	}

	b := &warmupBarrier{
		released: make(chan struct{}),
	}
	b.required.Store(required)

	return b
}

// noteClaim records that the warm-up worker has taken a message.
func (b *warmupBarrier) noteClaim() {
	if b == nil {
		return
	}

	b.claimed.Store(true)
}

// open releases the pool. It is safe to call from any goroutine and any number
// of times.
func (b *warmupBarrier) open() {
	if b == nil {
		return
	}

	b.openOnce.Do(func() {
		close(b.released)
	})
}

// wait blocks until the pool may fan out: until the warm-up worker resolves
// the generation's restart hand-off, until the generation is cancelled, or
// until an idle tick proves there was no hand-off waiting in the first place.
//
// The idle tick only opens the barrier while nothing has been claimed AND the
// barrier is not required. Once the warm-up worker has taken a message the
// tick is inert, so a restore that takes longer than one idle period is waited
// out rather than raced; and a required barrier ignores the tick entirely,
// because supervision knows the row is there and a tick that beat the worker
// to its first claim would fan the pool out into the race the barrier exists
// to prevent.
func (b *warmupBarrier) wait(ctx context.Context, idle time.Duration) {
	if b == nil {
		return
	}

	if idle <= 0 {
		idle = defaultPollInterval
	}

	ticker := time.NewTicker(idle)
	defer ticker.Stop()

	for {
		select {
		case <-b.released:
			return

		case <-ctx.Done():
			return

		case <-ticker.C:
			if !b.required.Load() && !b.claimed.Load() {
				b.open()

				return
			}
		}
	}
}

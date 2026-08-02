package timeout

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/stretchr/testify/require"
)

// wedgeableCallbackRef stands in for a requester whose mailbox has filled up.
// While wedged, every non-blocking send is refused; releasing it makes the
// next attempt succeed.
type wedgeableCallbackRef struct {
	mu sync.Mutex

	id       string
	wedged   bool
	err      error
	attempts int
	msgs     []ExpiredMsg
}

// newWedgeableCallbackRef returns a callback that refuses deliveries with err
// until release is called.
func newWedgeableCallbackRef(id string, err error) *wedgeableCallbackRef {
	return &wedgeableCallbackRef{
		id:     id,
		wedged: true,
		err:    err,
	}
}

// ID returns the callback identifier.
func (w *wedgeableCallbackRef) ID() string { return w.id }

// Tell records the message unconditionally. The timeout actor must never
// reach for this path, so a test that sees a message arrive here has caught a
// regression back to blocking delivery.
func (w *wedgeableCallbackRef) Tell(_ context.Context, msg *ExpiredMsg) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.msgs = append(w.msgs, *msg)

	return nil
}

// TryTell refuses the delivery while wedged and records it once released.
func (w *wedgeableCallbackRef) TryTell(_ context.Context,
	msg *ExpiredMsg) error {

	w.mu.Lock()
	defer w.mu.Unlock()

	w.attempts++

	if w.wedged {
		return w.err
	}

	w.msgs = append(w.msgs, *msg)

	return nil
}

// release lets subsequent deliveries through.
func (w *wedgeableCallbackRef) release() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.wedged = false
}

// snapshot returns the messages delivered so far and the number of delivery
// attempts made.
func (w *wedgeableCallbackRef) snapshot() ([]ExpiredMsg, int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]ExpiredMsg{}, w.msgs...), w.attempts
}

// wedgeableTickRef is the recurring-tick counterpart of
// wedgeableCallbackRef.
type wedgeableTickRef struct {
	mu sync.Mutex

	id       string
	wedged   bool
	attempts int
	msgs     []TickFiredMsg
}

// newWedgeableTickRef returns a tick callback that refuses deliveries until
// release is called.
func newWedgeableTickRef(id string) *wedgeableTickRef {
	return &wedgeableTickRef{
		id:     id,
		wedged: true,
	}
}

// ID returns the callback identifier.
func (w *wedgeableTickRef) ID() string { return w.id }

// Tell records the tick unconditionally; the actor must not use this path.
func (w *wedgeableTickRef) Tell(_ context.Context, msg *TickFiredMsg) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.msgs = append(w.msgs, *msg)

	return nil
}

// TryTell refuses the tick while wedged and records it once released.
func (w *wedgeableTickRef) TryTell(_ context.Context, msg *TickFiredMsg) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.attempts++

	if w.wedged {
		return actor.ErrMailboxFull
	}

	w.msgs = append(w.msgs, *msg)

	return nil
}

// release lets subsequent ticks through.
func (w *wedgeableTickRef) release() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.wedged = false
}

// snapshot returns the ticks delivered so far and the attempt count.
func (w *wedgeableTickRef) snapshot() ([]TickFiredMsg, int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]TickFiredMsg{}, w.msgs...), w.attempts
}

// TestRetryDelayBackoff verifies the backoff doubles from the base delay and
// then flattens at the cap, including for attempt counts large enough to
// overflow a naive shift.
func TestRetryDelayBackoff(t *testing.T) {
	t.Parallel()

	require.Equal(t, retryBaseDelay, retryDelay(0))
	require.Equal(t, time.Second, retryDelay(1))
	require.Equal(t, 2*time.Second, retryDelay(2))
	require.Equal(t, 4*time.Second, retryDelay(3))
	require.Equal(t, 8*time.Second, retryDelay(4))
	require.Equal(t, 16*time.Second, retryDelay(5))
	require.Equal(t, retryMaxDelay, retryDelay(6))
	require.Equal(t, retryMaxDelay, retryDelay(1000))
}

// TestTimeoutRetriesWedgedCallback verifies an expiry that cannot be
// delivered is held and re-armed on a growing backoff, then delivered intact
// once the requester drains. Nothing is lost and nothing is duplicated.
func TestTimeoutRetriesWedgedCallback(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	cb := newWedgeableCallbackRef("wedged", actor.ErrMailboxFull)

	res := a.Receive(ctx, &ScheduleTimeoutRequest{
		ID:       "retried",
		Duration: 50 * time.Millisecond,
		Callback: cb,
	})
	require.True(t, res.IsOk())

	// First fire: the requester has no room, so the entry survives with a
	// retry armed one second out.
	clock.Advance(50 * time.Millisecond)

	msgs, attempts := cb.snapshot()
	require.Empty(t, msgs)
	require.Equal(t, 1, attempts)
	require.Len(t, a.oneshots, 1)

	// Nothing happens before the backoff elapses.
	clock.Advance(500 * time.Millisecond)

	_, attempts = cb.snapshot()
	require.Equal(t, 1, attempts)

	// Second attempt, still wedged. The next retry doubles to two
	// seconds, so a one second advance is not enough to trigger a third.
	clock.Advance(500 * time.Millisecond)
	clock.Advance(time.Second)

	msgs, attempts = cb.snapshot()
	require.Empty(t, msgs)
	require.Equal(t, 2, attempts)

	// Once the requester drains, the held expiry lands on the next retry.
	cb.release()
	clock.Advance(2 * time.Second)

	msgs, attempts = cb.snapshot()
	require.Len(t, msgs, 1)
	require.Equal(t, ID("retried"), msgs[0].ID)
	require.Equal(t, 3, attempts)

	// The reminder is discharged, so no entry and no timer outlive it.
	require.Empty(t, a.oneshots)

	clock.Advance(time.Minute)

	msgs, _ = cb.snapshot()
	require.Len(t, msgs, 1)
}

// TestTimeoutRetryStaysCancellable verifies a held expiry is still owned by
// its ID: cancelling it stops the retry chain instead of leaving a reminder
// that fires long after the requester asked for it to go away.
func TestTimeoutRetryStaysCancellable(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	cb := newWedgeableCallbackRef("wedged", actor.ErrMailboxFull)

	res := a.Receive(ctx, &ScheduleTimeoutRequest{
		ID:       "cancelled",
		Duration: 50 * time.Millisecond,
		Callback: cb,
	})
	require.True(t, res.IsOk())

	clock.Advance(50 * time.Millisecond)
	require.Len(t, a.oneshots, 1)

	res = a.Receive(ctx, &CancelTimeoutRequest{ID: "cancelled"})
	require.True(t, res.IsOk())
	require.Empty(t, a.oneshots)

	// Even a healthy requester hears nothing more about a cancelled
	// timeout.
	cb.release()
	clock.Advance(time.Minute)

	msgs, attempts := cb.snapshot()
	require.Empty(t, msgs)
	require.Equal(t, 1, attempts)
}

// TestTimeoutDropsUnreachableCallback verifies a permanently gone requester
// does not earn a retry chain: there is nothing a backoff could fix, so the
// entry is released.
func TestTimeoutDropsUnreachableCallback(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	cb := newWedgeableCallbackRef("dead", actor.ErrActorTerminated)

	res := a.Receive(ctx, &ScheduleTimeoutRequest{
		ID:       "dropped",
		Duration: 50 * time.Millisecond,
		Callback: cb,
	})
	require.True(t, res.IsOk())

	clock.Advance(50 * time.Millisecond)
	require.Empty(t, a.oneshots)

	clock.Advance(time.Minute)

	msgs, attempts := cb.snapshot()
	require.Empty(t, msgs)
	require.Equal(t, 1, attempts)
}

// TestTickRetryPreservesFireInstant verifies a tick the consumer could not
// take is held rather than dropped, and that the retry reports when the timer
// actually fired instead of when delivery finally succeeded.
func TestTickRetryPreservesFireInstant(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	clock := newFakeClock(startEpoch)
	a := newTestActor(clock)
	cb := newWedgeableTickRef("wedged-tick")

	res := a.Receive(ctx, &ScheduleRecurringTickRequest{
		ID:       "tick",
		Interval: 100 * time.Millisecond,
		Callback: cb,
	})
	require.True(t, res.IsOk())

	firedAt := startEpoch.Add(100 * time.Millisecond)

	clock.Advance(100 * time.Millisecond)

	msgs, attempts := cb.snapshot()
	require.Empty(t, msgs)
	require.Equal(t, 1, attempts)

	// The retry replaces the cadence while the consumer is behind, so no
	// second tick piles up during the backoff window.
	cb.release()
	clock.Advance(time.Second)

	msgs, attempts = cb.snapshot()
	require.Len(t, msgs, 1)
	require.Equal(t, 2, attempts)
	require.Equal(t, firedAt, msgs[0].FiredAt)

	// With the consumer drained, the ticker resumes its normal interval.
	clock.Advance(100 * time.Millisecond)

	msgs, _ = cb.snapshot()
	require.Len(t, msgs, 2)
	require.True(t, msgs[1].FiredAt.After(msgs[0].FiredAt))
}

// blockingCallbackRef parks any blocking send until the test releases it,
// while reporting a full mailbox to the non-blocking one. It models the
// production shape that wedged the daemon: a requester whose mailbox is full
// and stays full.
type blockingCallbackRef struct {
	id      string
	release chan struct{}
	once    sync.Once

	mu       sync.Mutex
	attempts int
}

// newBlockingCallbackRef creates a callback whose Tell never returns until
// released.
func newBlockingCallbackRef(id string) *blockingCallbackRef {
	return &blockingCallbackRef{
		id:      id,
		release: make(chan struct{}),
	}
}

// ID returns the callback identifier.
func (b *blockingCallbackRef) ID() string { return b.id }

// Tell parks the caller until the test releases it.
func (b *blockingCallbackRef) Tell(ctx context.Context, _ *ExpiredMsg) error {
	b.count()

	select {
	case <-b.release:
	case <-ctx.Done():
	}

	return nil
}

// TryTell reports the mailbox as full without ever parking.
func (b *blockingCallbackRef) TryTell(_ context.Context, _ *ExpiredMsg) error {
	b.count()

	return actor.ErrMailboxFull
}

// count records a delivery attempt.
func (b *blockingCallbackRef) count() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.attempts++
}

// attemptCount returns how many deliveries have been attempted.
func (b *blockingCallbackRef) attemptCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.attempts
}

// stop releases any parked sender.
func (b *blockingCallbackRef) stop() {
	b.once.Do(func() {
		close(b.release)
	})
}

// TestTimeoutActorStaysResponsiveWhileCallbackWedged is the regression test
// for the daemon-wide timer freeze. The timeout actor is a process-wide
// singleton, so if one wedged requester can park its receive goroutine, every
// other subsystem's timers stop with it. Here a wedged callback must not stop
// the actor from accepting and firing an unrelated timeout.
func TestTimeoutActorStaysResponsiveWhileCallbackWedged(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	behavior := NewActor()
	host := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "timeout-responsive",
		Behavior:    behavior,
		MailboxSize: 8,
	})
	host.Start()
	t.Cleanup(host.Stop)

	behavior.Start(host.Ref())

	wedged := newBlockingCallbackRef("wedged")
	t.Cleanup(wedged.stop)

	require.NoError(
		t,
		host.Ref().Tell(ctx, &ScheduleTimeoutRequest{
			ID:       "wedged",
			Duration: 10 * time.Millisecond,
			Callback: wedged,
		},
		),
	)

	// Wait until the actor has tried to hand the expiry over. From this
	// point on, a blocking delivery would have the receive goroutine
	// parked for good.
	require.Eventually(t, func() bool {
		return wedged.attemptCount() > 0
	}, 5*time.Second, 5*time.Millisecond)

	// The actor must still accept new work.
	askCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	healthy := actor.NewChannelTellOnlyRef[*ExpiredMsg]("healthy", 1)
	schedule := &ScheduleTimeoutRequest{
		ID:       "healthy",
		Duration: 10 * time.Millisecond,
		Callback: healthy,
	}

	res := host.Ref().Ask(askCtx, schedule).Await(askCtx)
	require.True(t, res.IsOk(), "schedule was not acknowledged")

	// And it must still fire the timers it accepted.
	_, ok := healthy.AwaitMessage(5 * time.Second)
	require.True(t, ok, "timer never fired while a callback was wedged")
}

package actor

import (
	"context"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// wedgeBehavior parks inside Receive until it is released, which lets a test
// hold an actor's only processing goroutine hostage and fill its mailbox.
type wedgeBehavior struct {
	started chan struct{}
	release chan struct{}
}

// newWedgeBehavior creates a behavior that signals on started when its first
// message lands and stays inside Receive until release is closed.
func newWedgeBehavior() *wedgeBehavior {
	return &wedgeBehavior{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
}

// Receive signals that processing began and blocks until the test releases it
// or the actor shuts down.
func (b *wedgeBehavior) Receive(actorCtx context.Context,
	_ *testMsg) fn.Result[string] {

	select {
	case b.started <- struct{}{}:
	default:
	}

	select {
	case <-b.release:
	case <-actorCtx.Done():
	}

	return fn.Ok("wedged")
}

// wedgedActor returns a started actor whose behavior is parked inside Receive
// with its mailbox filled to capacity, so the next send has nowhere to go.
func wedgedActor(t *testing.T, h *actorTestHarness) (*Actor[*testMsg, string],
	*wedgeBehavior) {

	t.Helper()

	beh := newWedgeBehavior()
	a := h.newActor("wedged-"+t.Name(), beh, 1)
	ref := a.Ref()

	// The first message is pulled off the channel and parks the process
	// loop, which frees the single buffer slot again.
	require.NoError(t, ref.Tell(t.Context(), newTestMsg("park")))
	select {
	case <-beh.started:
	case <-time.After(time.Second):
		t.Fatal("behavior never started processing")
	}

	// This second message occupies the buffer. Nothing will drain it while
	// the behavior stays parked.
	require.NoError(t, ref.Tell(t.Context(), newTestMsg("fill")))

	t.Cleanup(func() {
		close(beh.release)
	})

	return a, beh
}

// TestTryTellFullMailboxFailsFast verifies that a send into a mailbox at
// capacity reports ErrMailboxFull immediately instead of parking the caller,
// which is the whole point of the method: a receive goroutine may call it
// without risking a deadlock against a wedged peer.
func TestTryTellFullMailboxFailsFast(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	a, _ := wedgedActor(t, h)

	start := time.Now()
	err := a.Ref().TryTell(t.Context(), newTestMsg("overflow"))
	elapsed := time.Since(start)

	require.ErrorIs(t, err, ErrMailboxFull)

	// The bound is deliberately far looser than anything TryTell should
	// need, because a loaded CI box can deschedule us mid-call and a
	// tight bound would flake on that rather than on the behavior. It
	// still catches the regression this method exists to prevent: a
	// blocking send here would never return at all, since nothing drains
	// the mailbox until the test ends.
	require.Less(t, elapsed, time.Second)
}

// TestTryTellDoesNotBurnCallerDeadline contrasts the two sends against the
// same wedged actor. Tell spends the caller's entire deadline waiting for
// room; TryTell gives the message straight back. This is the difference that
// let a full peer mailbox stall a sender's receive goroutine.
func TestTryTellDoesNotBurnCallerDeadline(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	a, _ := wedgedActor(t, h)

	const deadline = 200 * time.Millisecond

	ctx, cancel := context.WithTimeout(t.Context(), deadline)
	defer cancel()

	start := time.Now()
	err := a.Ref().Tell(ctx, newTestMsg("blocking"))
	blockingElapsed := time.Since(start)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.GreaterOrEqual(t, blockingElapsed, deadline)

	start = time.Now()
	err = a.Ref().TryTell(t.Context(), newTestMsg("non-blocking"))
	tryElapsed := time.Since(start)

	require.ErrorIs(t, err, ErrMailboxFull)
	require.Less(t, tryElapsed, blockingElapsed)
}

// TestTryTellDelivers verifies the success path: a mailbox with room accepts
// the message and the behavior processes it.
func TestTryTellDelivers(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	beh := newEchoBehavior(t, 0)
	a := h.newActor("try-tell-delivers", beh, 4)

	replies := make(chan string, 1)
	msg := newTestMsgWithReply("payload", replies)

	require.NoError(t, a.Ref().TryTell(t.Context(), msg))

	select {
	case got := <-replies:
		require.Equal(t, "payload", got)

	case <-time.After(time.Second):
		t.Fatal("message was never processed")
	}
}

// TestTryTellTerminatedActor verifies a stopped actor reports
// ErrActorTerminated and that the undeliverable message still reaches the dead
// letter office, matching what Tell does on the same path.
func TestTryTellTerminatedActor(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	beh := newEchoBehavior(t, 0)
	a := h.newActor("try-tell-terminated", beh, 4)

	a.Stop()

	// Stop only cancels the actor's context; wait until the mailbox
	// actually rejects sends before asserting on the error.
	msg := newTestMsg("after-stop")
	require.Eventually(t, func() bool {
		return a.Ref().TryTell(t.Context(), msg) != nil
	}, time.Second, 10*time.Millisecond)

	require.ErrorIs(
		t,
		a.Ref().TryTell(t.Context(), msg),
		ErrActorTerminated,
	)
	h.assertDLOMessage(msg)
}

// TestTryTellClosedMailbox verifies a closed mailbox reports ErrMailboxClosed
// rather than the full or terminated sentinels, so callers can tell a
// transient backlog apart from a permanently gone recipient.
func TestTryTellClosedMailbox(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	beh := newEchoBehavior(t, 0)
	a := h.newActor("try-tell-closed", beh, 4)

	a.mailbox.Close()

	err := a.Ref().TryTell(t.Context(), newTestMsg("after-close"))
	require.ErrorIs(t, err, ErrMailboxClosed)
}

// TestTryTellCancelledCaller verifies the context is honoured as an immediate
// cancellation check: a caller that already gave up does not get its message
// enqueued.
func TestTryTellCancelledCaller(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	beh := newEchoBehavior(t, 0)
	a := h.newActor("try-tell-cancelled", beh, 4)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := a.Ref().TryTell(ctx, newTestMsg("cancelled"))
	require.ErrorIs(t, err, context.Canceled)
}

// TestTryTellThroughMapInputRef verifies a wrapped ref stays as
// backpressure-friendly as the ref it wraps: the full-mailbox sentinel
// survives the transform instead of being swallowed or turned into a block.
func TestTryTellThroughMapInputRef(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	a, _ := wedgedActor(t, h)

	wrapped := NewMapInputRef(
		a.Ref(), func(msg *testMsg) *testMsg {
			return msg
		},
	)

	err := wrapped.TryTell(t.Context(), newTestMsg("overflow"))
	require.ErrorIs(t, err, ErrMailboxFull)
}

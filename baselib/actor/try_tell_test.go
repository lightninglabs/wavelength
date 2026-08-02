package actor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
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

// TestTryTellThroughFilterMapInputRef verifies the filtering wrapper reports
// a dropped message as success (there is nothing to retry) while still
// surfacing the target's backpressure for messages it does forward.
func TestTryTellThroughFilterMapInputRef(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	a, _ := wedgedActor(t, h)

	wrapped := NewFilterMapInputRef(
		a.Ref(), func(msg *testMsg) (*testMsg, bool) {
			return msg, msg.data != "dropped"
		},
	)

	// A message the transform drops never reaches the full mailbox.
	require.NoError(t, wrapped.TryTell(t.Context(), newTestMsg("dropped")))

	// One it forwards reports the target's answer unchanged.
	err := wrapped.TryTell(t.Context(), newTestMsg("forwarded"))
	require.ErrorIs(t, err, ErrMailboxFull)
}

// TestTryTellThroughMapRef verifies the ask-capable wrapper forwards the
// non-blocking send, and that a transform failure is reported before the
// target is touched at all.
func TestTryTellThroughMapRef(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	a, _ := wedgedActor(t, h)

	transformErr := errors.New("bad input")
	failing := NewMapRef(
		a.Ref(),
		func(msg *testMsg) (*testMsg, error) {
			return nil, transformErr
		},
		func(r string) string { return r },
	)

	err := failing.TryTell(t.Context(), newTestMsg("unmapped"))
	require.ErrorIs(t, err, transformErr)
	require.NotErrorIs(t, err, ErrMailboxFull)

	forwarding := NewMapRef(
		a.Ref(),
		func(msg *testMsg) (*testMsg, error) { return msg, nil },
		func(r string) string { return r },
	)

	err = forwarding.TryTell(t.Context(), newTestMsg("forwarded"))
	require.ErrorIs(t, err, ErrMailboxFull)
}

// TestTryTellThroughRouter verifies the router reports the selected actor's
// backpressure, and reports its own inability to select one when the service
// key has no registrations. The latter is transient by design, so callers
// must not read it as a permanent failure.
func TestTryTellThroughRouter(t *testing.T) {
	t.Parallel()

	h := newRouterTestHarness(t)
	key := NewServiceKey[*testMsg, string]("try-tell-router")

	router := NewRouter(
		h.receptionist, key, NewRoundRobinStrategy[*testMsg, string](),
		nil,
	)

	// With nothing registered there is no actor to try.
	err := router.TryTell(t.Context(), newTestMsg("unrouted"))
	require.ErrorIs(t, err, ErrNoActorsAvailable)

	// With a wedged actor registered, the router surfaces the mailbox
	// answer rather than its own.
	a, _ := wedgedActor(t, h.actorTestHarness)
	require.NoError(
		t,
		RegisterWithReceptionist(
			h.receptionist, key, a.Ref(),
		),
	)

	err = router.TryTell(t.Context(), newTestMsg("routed"))
	require.ErrorIs(t, err, ErrMailboxFull)
}

// slowEnqueueStore wraps a delivery store and holds every enqueue until it is
// released, modelling a database too busy to answer promptly.
type slowEnqueueStore struct {
	*mockDeliveryStore

	release chan struct{}
}

// newSlowEnqueueStore returns a store whose enqueues park until released.
func newSlowEnqueueStore() *slowEnqueueStore {
	return &slowEnqueueStore{
		mockDeliveryStore: newMockDeliveryStore(),
		release:           make(chan struct{}),
	}
}

// EnqueueMessage waits for the test to release it, for the caller's context
// to expire, whichever comes first.
func (s *slowEnqueueStore) EnqueueMessage(ctx context.Context,
	params EnqueueParams) error {

	select {
	case <-s.release:
		return s.mockDeliveryStore.EnqueueMessage(ctx, params)

	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestDurableTryTellPersists verifies the success path of the durable
// implementation: the message is written to the store, just as Tell writes it.
func TestDurableTryTellPersists(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	cfg := DefaultDurableActorConfig(
		"durable-try-tell",
		newMockBehavior(
			fn.Ok(42),
		),
		store,
		newActorTestCodec(),
	)
	a := NewDurableActor(cfg).UnwrapOrFail(t)

	msg := &actorTestMsg{
		Value:   tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(7)),
		Payload: tlv.NewPrimitiveRecord[tlv.TlvType2]([]byte("try")),
	}
	require.NoError(t, a.Ref().TryTell(t.Context(), msg))

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.messages, 1)
}

// TestDurableTryTellSlowWriteIsNotMailboxFull pins the semantics that make
// the durable implementation the odd one out. Its queue has no capacity, so
// it never reports a full mailbox; a database that cannot answer inside the
// mailbox's internal deadline shows up as a deadline instead. A caller that
// only retries ErrMailboxFull silently discards durable messages exactly when
// the database is struggling, which is when losing them hurts most.
func TestDurableTryTellSlowWriteIsNotMailboxFull(t *testing.T) {
	t.Parallel()

	store := newSlowEnqueueStore()
	defer close(store.release)

	cfg := DefaultDurableActorConfig(
		"durable-slow-write",
		newMockBehavior(
			fn.Ok(42),
		),
		store,
		newActorTestCodec(),
	)
	a := NewDurableActor(cfg).UnwrapOrFail(t)

	msg := &actorTestMsg{
		Value:   tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(9)),
		Payload: tlv.NewPrimitiveRecord[tlv.TlvType2]([]byte("slow")),
	}

	err := a.Ref().TryTell(t.Context(), msg)
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, ErrMailboxFull)

	// The caller's own context is untouched: the deadline that expired
	// belongs to the mailbox, not to them.
	require.NoError(t, t.Context().Err())
}

// TestDurableTryTellTerminatedActor verifies the durable ref still reports
// the terminal error that tells a caller to stop retrying.
func TestDurableTryTellTerminatedActor(t *testing.T) {
	t.Parallel()

	cfg := DefaultDurableActorConfig(
		"durable-terminated",
		newMockBehavior(
			fn.Ok(42),
		),
		newMockDeliveryStore(),
		newActorTestCodec(),
	)
	a := NewDurableActor(cfg).UnwrapOrFail(t)

	a.Start()
	a.Stop()

	msg := &actorTestMsg{
		Value:   tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(11)),
		Payload: tlv.NewPrimitiveRecord[tlv.TlvType2]([]byte("dead")),
	}

	require.Eventually(t, func() bool {
		return errors.Is(
			a.Ref().TryTell(t.Context(), msg),
			ErrActorTerminated,
		)
	}, time.Second, 10*time.Millisecond)
}

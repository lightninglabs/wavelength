package actor

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// The tests in this file change process-wide state (the await-in-turn policy
// and the wait cycle hook), so none of them calls t.Parallel. Go runs a
// package's serial tests to completion before it resumes any parallel one.

const (
	// waitLong is the Await deadline of the ring actors. It is far above
	// the time a detection takes, so a result that arrives quickly cannot
	// be a timeout passing for a detection.
	waitLong = 30 * time.Second

	// waitFast is the bound within which a detection must show up.
	waitFast = 5 * time.Second
)

// ringMsg is the message of the ring actors. A "go" message makes the actor
// ask its successor and await the reply; a "tail" message is answered at once.
type ringMsg struct {
	BaseMessage

	kind string
}

// MessageType returns the message type name.
func (m *ringMsg) MessageType() string {
	return "ringMsg"
}

// ring is a set of actors where each one, on "go", Asks its successor with
// "go" and Awaits the reply inside its turn. The last actor closes the ring by
// sending "tail" to its own successor, which answers without waiting.
type ring struct {
	refs  map[string]ActorRef[*ringMsg, string]
	tails atomic.Int64
}

// startRing starts one actor per id. The successor of ids[i] is ids[i+1], and
// the successor of the last id is ids[0]. When open is true the last actor
// does not wait on its successor, which makes the chain acyclic.
func startRing(t *testing.T, ids []string, open bool) *ring {
	t.Helper()

	r := &ring{refs: make(map[string]ActorRef[*ringMsg, string])}

	for i, id := range ids {
		next := ids[(i+1)%len(ids)]
		last := i == len(ids)-1

		a := NewActor(ActorConfig[*ringMsg, string]{
			ID: id,
			Behavior: NewFunctionBehavior(
				func(ctx context.Context,
					msg *ringMsg) fn.Result[string] {

					if msg.kind == "tail" {
						r.tails.Add(1)

						return fn.Ok("tail")
					}

					if last && open {
						return fn.Ok("end")
					}

					kind := "go"
					if last {
						kind = "tail"
					}

					waitCtx, cancel := context.WithTimeout(
						ctx, waitLong,
					)
					defer cancel()

					fut := r.refs[next].Ask(
						ctx, &ringMsg{
							kind: kind,
						},
					)

					return fut.Await(waitCtx)
				},
			),
			MailboxSize: 4,
		})
		a.Start()
		t.Cleanup(a.Stop)

		r.refs[id] = a.Ref()
	}

	return r
}

// start sends "go" to id from outside any turn and returns the reply.
func (r *ring) start(id string) fn.Result[string] {
	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()

	return r.refs[id].Ask(ctx, &ringMsg{kind: "go"}).Await(ctx)
}

// requireNoEdges asserts that the registry drains.
func requireNoEdges(t *testing.T) {
	t.Helper()

	require.Eventually(t, func() bool {
		return waitGraphLen() == 0
	}, waitFast, time.Millisecond)
}

// waitEdgeTargets returns the targets of the live edges registered for id.
func waitEdgeTargets(id string) []string {
	waitGraph.Lock()
	defer waitGraph.Unlock()

	var targets []string
	for _, e := range waitGraph.waiting[id] {
		if e.live() {
			targets = append(targets, e.target)
		}
	}

	return targets
}

// waitEdgeTarget returns the target of the first live edge registered for id,
// if any.
func waitEdgeTarget(id string) (string, bool) {
	targets := waitEdgeTargets(id)
	if len(targets) == 0 {
		return "", false
	}

	return targets[0], true
}

// requireCyclePath asserts that path is a closed cycle through exactly the
// given participants. Which actor closes the cycle depends on which Ask is
// registered last, and either is legal, so the test fixes neither the start of
// the path nor its direction.
func requireCyclePath(t *testing.T, path []string, ids ...string) {
	t.Helper()

	require.Len(t, path, len(ids)+1)
	require.Equal(t, path[0], path[len(path)-1])
	for _, id := range ids {
		require.Contains(t, path, id)
	}
}

// errPath extracts the cycle path from an ErrWaitCycle error.
func errPath(t *testing.T, err error) []string {
	t.Helper()

	require.ErrorIs(t, err, ErrWaitCycle)

	msg := err.Error()
	idx := strings.LastIndex(msg, ": ")
	require.GreaterOrEqual(t, idx, 0)

	return strings.Split(msg[idx+2:], " -> ")
}

// TestWaitCycleSelfAsk checks that an actor awaiting its own reply fails at
// once rather than waiting out the deadline.
func TestWaitCycleSelfAsk(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnAllow)

	r := startRing(t, []string{"solo"}, false)

	begin := time.Now()
	res := r.start("solo")
	require.ErrorIs(t, res.Err(), ErrWaitCycle)
	require.Contains(t, res.Err().Error(), "solo -> solo")
	require.Less(t, time.Since(begin), waitFast)

	requireNoEdges(t)
}

// TestWaitCycleTwoActors checks that of two actors awaiting each other, one
// fails with the cycle path and the other then makes progress.
func TestWaitCycleTwoActors(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnAllow)

	r := startRing(t, []string{"a", "b"}, false)

	begin := time.Now()
	res := r.start("a")
	require.ErrorIs(t, res.Err(), ErrWaitCycle)
	require.Less(t, time.Since(begin), waitFast)

	requireCyclePath(t, errPath(t, res.Err()), "a", "b")

	// The failed side returned from its turn, so the Ask it had queued
	// for the other actor gets processed.
	require.Eventually(t, func() bool {
		return r.tails.Load() == 1
	}, waitFast, time.Millisecond)

	requireNoEdges(t)
}

// TestWaitCycleThreeActors checks a cycle a -> b -> c -> a.
func TestWaitCycleThreeActors(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnAllow)

	r := startRing(t, []string{"a", "b", "c"}, false)

	begin := time.Now()
	res := r.start("a")
	require.ErrorIs(t, res.Err(), ErrWaitCycle)
	require.Less(t, time.Since(begin), waitFast)
	requireCyclePath(t, errPath(t, res.Err()), "a", "b", "c")

	require.Eventually(t, func() bool {
		return r.tails.Load() == 1
	}, waitFast, time.Millisecond)

	requireNoEdges(t)
}

// TestWaitCycleNoFalsePositive checks that waits which do not form a cycle all
// succeed and leave no edges behind.
func TestWaitCycleNoFalsePositive(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnAllow)

	t.Run("single wait", func(t *testing.T) {
		r := startRing(t, []string{"a", "b"}, true)
		require.Equal(t, "end", r.start("a").UnwrapOr("fail"))
		requireNoEdges(t)
	})

	t.Run("chain", func(t *testing.T) {
		r := startRing(t, []string{"a", "b", "c"}, true)
		require.Equal(t, "end", r.start("a").UnwrapOr("fail"))
		requireNoEdges(t)
	})

	t.Run("concurrent unrelated awaits", func(t *testing.T) {
		r := startRing(t, []string{"a", "b", "c"}, true)

		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			id := []string{"a", "b"}[i%2]

			wg.Add(1)
			go func() {
				defer wg.Done()

				res := r.start(id)
				require.NoError(t, res.Err())
			}()
		}
		wg.Wait()

		requireNoEdges(t)
	})
}

// TestWaitCycleNonSerialTurn checks that a turn of a multi-worker actor does
// not register an edge, since a parked worker does not park the actor.
func TestWaitCycleNonSerialTurn(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnAllow)

	ctx, end := beginTurn(context.Background(), "pool", false)
	defer end()

	// A self wait would be a cycle if it were registered.
	p := newTargetedPromise[int]("pool")

	done := make(chan fn.Result[int], 1)
	go func() {
		done <- p.Future().Await(ctx)
	}()

	require.Never(t, func() bool {
		return waitGraphLen() != 0
	}, 100*time.Millisecond, time.Millisecond)

	p.Complete(fn.Ok(7))

	select {
	case res := <-done:
		require.Equal(t, 7, res.UnwrapOr(0))

	case <-time.After(waitFast):
		t.Fatal("await did not return")
	}
}

// TestWaitCycleEdgeRemovedOnCancel checks that the edge is removed when the
// wait ends because the context was cancelled.
func TestWaitCycleEdgeRemovedOnCancel(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnAllow)

	turnCtx, end := WithTurnForTest(context.Background(), "waiter")
	defer end()

	ctx, cancel := context.WithCancel(turnCtx)
	p := newTargetedPromise[int]("other")

	done := make(chan fn.Result[int], 1)
	go func() {
		done <- p.Future().Await(ctx)
	}()

	require.Eventually(t, func() bool {
		return waitGraphLen() == 1
	}, waitFast, time.Millisecond)

	cancel()

	select {
	case res := <-done:
		require.ErrorIs(t, res.Err(), context.Canceled)

	case <-time.After(waitFast):
		t.Fatal("await did not return")
	}

	requireNoEdges(t)
}

// TestWaitCycleUntargetedFutureNoEdge checks that a future that is not the
// reply to an Ask never creates an edge.
func TestWaitCycleUntargetedFutureNoEdge(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnAllow)

	ctx, end := WithTurnForTest(context.Background(), "driver")
	defer end()

	p := NewPromise[int]()

	done := make(chan fn.Result[int], 1)
	go func() {
		done <- p.Future().Await(ctx)
	}()

	require.Never(t, func() bool {
		return waitGraphLen() != 0
	}, 100*time.Millisecond, time.Millisecond)

	p.Complete(fn.Ok(1))
	require.Equal(t, 1, (<-done).UnwrapOr(0))
}

// staleWaiter is a goroutine that awaits a targeted promise with a context.
type staleWaiter struct {
	p    Promise[int]
	done chan fn.Result[int]
}

// startWaiter parks a goroutine in Await on a promise targeting target, using
// ctx, and returns once the goroutine has registered its edge or, when the
// wait is not tracked, once it is running.
func startWaiter(ctx context.Context, target string) *staleWaiter {
	w := &staleWaiter{
		p:    newTargetedPromise[int](target),
		done: make(chan fn.Result[int], 1),
	}
	go func() {
		w.done <- w.p.Future().Await(ctx)
	}()

	return w
}

// finish completes the promise and waits for the Await to return.
func (w *staleWaiter) finish(t *testing.T) fn.Result[int] {
	t.Helper()

	w.p.Complete(fn.Ok(1))

	select {
	case res := <-w.done:
		return res

	case <-time.After(waitFast):
		t.Fatal("await did not return")
	}

	return fn.Result[int]{}
}

// TestWaitCycleStaleEdgeNoFalsePositive checks that an edge registered by a
// goroutine that outlived its turn does not make the actor look blocked. Actor
// "a" spawns a goroutine that awaits "x" with the turn's context, the turn
// returns, and "x" then awaits "a": "a" is free, so this is no cycle.
func TestWaitCycleStaleEdgeNoFalsePositive(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnAllow)

	ctxA, endA := WithTurnForTest(context.Background(), "a")
	aWaiter := startWaiter(ctxA, "x")

	require.Eventually(t, func() bool {
		_, ok := waitEdgeTarget("a")

		return ok
	}, waitFast, time.Millisecond)

	// The turn ends while the goroutine is still parked, and "a" goes on
	// serving.
	endA()

	ctxX, endX := WithTurnForTest(context.Background(), "x")
	defer endX()

	xWaiter := startWaiter(ctxX, "a")

	// The wait of "x" must park rather than fail, so its edge shows up and
	// no result arrives.
	require.Eventually(t, func() bool {
		_, ok := waitEdgeTarget("x")

		return ok
	}, waitFast, time.Millisecond)
	require.Never(t, func() bool {
		return len(xWaiter.done) != 0
	}, 100*time.Millisecond, time.Millisecond)

	require.NoError(t, xWaiter.finish(t).Err())
	require.NoError(t, aWaiter.finish(t).Err())
	requireNoEdges(t)
}

// TestWaitCycleStaleEdgeReplaced checks that after a stale edge, the next turn
// of the same actor registers its own edge, so a real cycle through it is
// detected.
func TestWaitCycleStaleEdgeReplaced(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnAllow)

	ctxA1, endA1 := WithTurnForTest(context.Background(), "a")
	stale := startWaiter(ctxA1, "z")

	require.Eventually(t, func() bool {
		_, ok := waitEdgeTarget("a")

		return ok
	}, waitFast, time.Millisecond)
	endA1()

	// The next turn of "a" really blocks on "x".
	ctxA2, endA2 := WithTurnForTest(context.Background(), "a")
	defer endA2()

	live := startWaiter(ctxA2, "x")

	require.Eventually(t, func() bool {
		target, ok := waitEdgeTarget("a")

		return ok && target == "x"
	}, waitFast, time.Millisecond)

	// "x" awaiting "a" closes a real cycle.
	ctxX, endX := WithTurnForTest(context.Background(), "x")
	defer endX()

	p := newTargetedPromise[int]("a")
	res := p.Future().Await(ctxX)
	requireCyclePath(t, errPath(t, res.Err()), "a", "x")

	require.NoError(t, live.finish(t).Err())
	require.NoError(t, stale.finish(t).Err())
	requireNoEdges(t)
}

// TestWaitCycleStaleLateRelease checks that a stale wait that returns after a
// newer turn registered its edge does not remove that edge.
func TestWaitCycleStaleLateRelease(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnAllow)

	ctxA1, endA1 := WithTurnForTest(context.Background(), "a")
	stale := startWaiter(ctxA1, "x")

	require.Eventually(t, func() bool {
		_, ok := waitEdgeTarget("a")

		return ok
	}, waitFast, time.Millisecond)
	endA1()

	ctxA2, endA2 := WithTurnForTest(context.Background(), "a")
	defer endA2()

	newer := startWaiter(ctxA2, "y")

	require.Eventually(t, func() bool {
		target, ok := waitEdgeTarget("a")

		return ok && target == "y"
	}, waitFast, time.Millisecond)

	// The stale wait returns and releases late.
	require.NoError(t, stale.finish(t).Err())

	target, ok := waitEdgeTarget("a")
	require.True(t, ok)
	require.Equal(t, "y", target)

	require.NoError(t, newer.finish(t).Err())
	requireNoEdges(t)
}

// TestFutureThenApplyKeepsTarget checks that a transformed future waits on the
// same actor as the original.
func TestFutureThenApplyKeepsTarget(t *testing.T) {
	p := newTargetedPromise[int]("callee")
	mapped := p.Future().ThenApply(
		context.Background(), func(v int) int { return v + 1 },
	)
	require.Equal(t, "callee", futureTarget(mapped))

	p.Complete(fn.Ok(1))
	require.Equal(t, 2, mapped.Await(context.Background()).UnwrapOr(0))
}

// TestAskFutureTargets checks that each Ask path records the ID of the actor
// that answers: directly, through a MapRef, through a MapRef over a router, and
// through a router. A promise made outside an Ask has no target.
func TestAskFutureTargets(t *testing.T) {
	r := startRing(t, []string{"target"}, true)
	ref := r.refs["target"]
	ctx := context.Background()

	fut := ref.Ask(ctx, &ringMsg{kind: "go"})
	require.Equal(t, "target", futureTarget(fut))
	require.NoError(t, fut.Await(ctx).Err())

	mapped := NewMapRef(
		ref, func(m *ringMsg) (*ringMsg, error) {
			return m, nil
		}, func(s string) string {
			return s
		},
	)
	fut = mapped.Ask(ctx, &ringMsg{kind: "go"})
	require.Equal(t, "target", futureTarget(fut))
	require.NoError(t, fut.Await(ctx).Err())

	h := newRouterTestHarness(t)
	key := NewServiceKey[*ringMsg, string]("wait-graph-router")
	router := NewRouter(
		h.receptionist, key, NewRoundRobinStrategy[*ringMsg, string](),
		nil,
	)
	require.NoError(t, RegisterWithReceptionist(h.receptionist, key, ref))

	fut = router.Ask(ctx, &ringMsg{kind: "go"})
	require.Equal(t, "target", futureTarget(fut))
	require.NoError(t, fut.Await(ctx).Err())

	viaMap := NewMapRef(
		ActorRef[*ringMsg, string](router),
		func(m *ringMsg) (*ringMsg, error) {
			return m, nil
		}, func(s string) string {
			return s
		},
	)
	fut = viaMap.Ask(ctx, &ringMsg{kind: "go"})
	require.Equal(t, "target", futureTarget(fut))
	require.NoError(t, fut.Await(ctx).Err())

	require.Empty(t, futureTarget(NewPromise[int]().Future()))
}

// TestWaitCycleHook checks that the hook fires once per detection with the
// cycle path, and that clearing it stops the calls.
func TestWaitCycleHook(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnAllow)

	var (
		mu    sync.Mutex
		calls []WaitCycle
	)
	SetWaitCycleHook(func(c WaitCycle) {
		mu.Lock()
		defer mu.Unlock()

		calls = append(calls, c)
	})
	t.Cleanup(func() {
		SetWaitCycleHook(nil)
	})

	r := startRing(t, []string{"a", "b"}, false)
	require.ErrorIs(t, r.start("a").Err(), ErrWaitCycle)

	mu.Lock()
	require.Len(t, calls, 1)
	requireCyclePath(t, calls[0].Path, "a", "b")
	require.Contains(t, calls[0].CallSite, "wait_graph_test.go")
	mu.Unlock()

	// Let the queued tail drain so the next detection starts clean.
	require.Eventually(t, func() bool {
		return r.tails.Load() == 1
	}, waitFast, time.Millisecond)
	requireNoEdges(t)

	SetWaitCycleHook(nil)

	r2 := startRing(t, []string{"c", "d"}, false)
	require.ErrorIs(t, r2.start("c").Err(), ErrWaitCycle)

	mu.Lock()
	require.Len(t, calls, 1)
	mu.Unlock()
}

// requireParked asserts that the waiter registered an edge for id and has not
// returned, which shows its wait was not refused as a cycle.
func requireParked(t *testing.T, id string, w *staleWaiter) {
	t.Helper()

	require.Eventually(t, func() bool {
		_, ok := waitEdgeTarget(id)

		return ok
	}, waitFast, time.Millisecond)
	require.Never(t, func() bool {
		return len(w.done) != 0
	}, 100*time.Millisecond, time.Millisecond)
}

// requireDeliveredReplyNoCycle holds an edge a -> b for a future whose reply
// has been delivered, as if the waiter had not yet woken to remove it, and
// checks that b awaiting a is not reported as a cycle. The future's reply is
// delivered by the time of the check, but fut itself may still be pending.
func requireDeliveredReplyNoCycle[T any](t *testing.T, fut Future[T],
	deliver func()) {

	t.Helper()

	ctxA, endA := WithTurnForTest(context.Background(), "a")
	defer endA()

	// Register the edge by hand so the waiter cannot release it.
	release, err := beginWait(
		ctxA, futureTarget(fut), futureRootDone(fut),
	)
	require.NoError(t, err)
	defer release()

	deliver()

	ctxB, endB := WithTurnForTest(context.Background(), "b")
	defer endB()

	reverse := startWaiter(ctxB, "a")
	requireParked(t, "b", reverse)

	require.NoError(t, reverse.finish(t).Err())
}

// TestWaitCycleDeliveredReplyNoCycle checks that an edge whose reply was
// delivered no longer counts, although its waiter has not removed it yet.
// Actor "a" holds its reply from "b", and "b" then awaits "a".
func TestWaitCycleDeliveredReplyNoCycle(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnAllow)

	p := newTargetedPromise[int]("b")
	requireDeliveredReplyNoCycle(t, p.Future(), func() {
		p.Complete(fn.Ok(1))
	})
	requireNoEdges(t)
}

// TestWaitCycleDeliveredReplyThenApply checks the same through ThenApply,
// where the derived future is still pending after the root completed.
func TestWaitCycleDeliveredReplyThenApply(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnAllow)

	gate := make(chan struct{})
	started := make(chan struct{})

	p := newTargetedPromise[int]("b")
	derived := p.Future().ThenApply(
		context.Background(), func(v int) int {
			close(started)
			<-gate

			return v
		},
	)

	requireDeliveredReplyNoCycle(t, derived, func() {
		p.Complete(fn.Ok(1))

		// The transform is running, so the derived future is pending.
		<-started
		select {
		case <-derived.(*futureImpl[int]).done:
			t.Fatal("derived future completed early")

		default:
		}
	})

	close(gate)
	require.NoError(t, derived.Await(context.Background()).Err())
	requireNoEdges(t)
}

// TestWaitCycleDeliveredReplyMapRef checks the same through a MapRef, whose
// output transform holds the derived future pending after the callee replied.
func TestWaitCycleDeliveredReplyMapRef(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnAllow)

	r := startRing(t, []string{"b"}, true)

	gate := make(chan struct{})
	entered := make(chan struct{})
	mapped := NewMapRef(
		r.refs["b"], func(m *ringMsg) (*ringMsg, error) {
			return m, nil
		}, func(s string) string {
			close(entered)
			<-gate

			return s
		},
	)

	fut := mapped.Ask(context.Background(), &ringMsg{kind: "go"})
	requireDeliveredReplyNoCycle(t, fut, func() {
		// The output transform only runs once the callee replied.
		<-entered
		select {
		case <-fut.(*futureImpl[string]).done:
			t.Fatal("derived future completed early")

		default:
		}
	})

	close(gate)
	require.NoError(t, fut.Await(context.Background()).Err())
	requireNoEdges(t)
}

// TestWaitCycleSeveralEdgesPerActor checks that every live wait of an actor is
// recorded. A helper goroutine that awaits "x" with the turn's context does not
// hide the wait of the actor's own goroutine on "y": a real cycle through "y"
// is detected, and the helper finishing does not remove the edge to "y".
func TestWaitCycleSeveralEdgesPerActor(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnAllow)

	ctxA, endA := WithTurnForTest(context.Background(), "a")
	defer endA()

	helper := startWaiter(ctxA, "x")
	require.Eventually(t, func() bool {
		return len(waitEdgeTargets("a")) == 1
	}, waitFast, time.Millisecond)

	own := startWaiter(ctxA, "y")
	require.Eventually(t, func() bool {
		return len(waitEdgeTargets("a")) == 2
	}, waitFast, time.Millisecond)
	require.ElementsMatch(t, []string{"x", "y"}, waitEdgeTargets("a"))

	// The helper finishes and removes only its own edge.
	require.NoError(t, helper.finish(t).Err())
	require.Equal(t, []string{"y"}, waitEdgeTargets("a"))

	// "y" awaiting "a" closes a real cycle through the actor's own wait.
	ctxY, endY := WithTurnForTest(context.Background(), "y")
	defer endY()

	res := newTargetedPromise[int]("a").Future().Await(ctxY)
	requireCyclePath(t, errPath(t, res.Err()), "a", "y")

	require.NoError(t, own.finish(t).Err())
	requireNoEdges(t)
}

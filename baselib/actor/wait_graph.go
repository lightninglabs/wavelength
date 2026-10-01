package actor

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
)

// ErrWaitCycle is returned by Future.Await when waiting would complete a cycle
// of actor turns that are each blocked on another actor's reply, including an
// actor awaiting a reply from itself. Nothing in such a cycle can make
// progress, so the Await fails at once instead of parking. The error wraps the
// path, for example "a -> b -> a". It always indicates a code defect: use
// AskThen to receive the reply as a later message.
var ErrWaitCycle = errors.New("await would complete a wait cycle between " +
	"actors")

// The wait-for graph records which serial actors have a turn blocked in
// Future.Await on another actor's reply. An edge exists only while that turn
// is actually blocked, so a cycle in the graph is a real wait cycle at that
// instant: every actor on it is parked and only a reply from the next actor
// can wake it.
//
// Sends contribute no edges. A send made with a turn's context never parks
// (see turn.go), so the only way a turn blocks on another actor is by awaiting
// its reply. If some participant of a cycle has a deadline, the cycle would
// eventually unwind on its own, but every participant would first sit out its
// full deadline. Failing fast is better in both cases.
//
// The detector is blind to waits outside the framework, such as I/O, mutexes
// or raw channels: a cycle that passes through one is not reported. It is also
// blind where the future's target is not the actor that completes it:
//
//   - A promise taken with DetachAskPromise keeps the detaching coordinator as
//     its target, although a different actor completes it. The edge then points
//     at an actor that is not the completer, so the coordinator later awaiting
//     the original caller can be reported as a cycle that does not exist, and
//     a real cycle through the actual completer is invisible.
//   - A wait that passes through a state machine's driver goroutine is
//     invisible. protofsm's StateMachine.Receive awaits an untargeted AskEvent
//     future, and the driver may itself wait on other actors.
//
// A goroutine the behavior spawns with the turn's context can register an edge
// while that turn is active. Go gives no goroutine identity, so such an Await
// is indistinguishable from the actor's own wait: it counts as the actor being
// blocked, and it can be reported as a cycle even though the actor's own
// goroutine is free. The await-in-turn policy already flags those same
// goroutines. They should use AskThen, or an unguarded framework helper,
// instead. Because of this, an actor can have several live edges at once, one
// per wait in flight, and a cycle through any of them is a cycle. An edge is
// tied to the turn that was running, so once the turn returns it no longer
// counts: the actor is free again even though the goroutine is still parked.
//
// An edge also stops counting once the reply it waits on has been delivered,
// even if the waiter has not yet woken to remove it. Otherwise an actor that
// already holds its reply, but has not run yet, would look blocked to an actor
// that then awaits it.
var waitGraph = struct {
	sync.Mutex

	// waiting maps the ID of an actor with turns blocked in Await to the
	// edges describing those waits, one per wait that has not ended. A
	// serial actor's own goroutine runs one turn at a time, but goroutines
	// the turn spawned can add more. An edge that is no longer live is
	// stale and is ignored until its wait removes it.
	waiting map[string][]*waitEdge
}{
	waiting: make(map[string][]*waitEdge),
}

// waitEdge is one registered wait of an actor. Its address identifies the wait
// that registered it, so only that wait can remove it.
type waitEdge struct {
	// t is the turn that registered the edge. A goroutine spawned by the
	// turn can outlive it, so the edge is only live while t is active.
	t *turn

	// target is the ID of the actor whose reply is awaited.
	target string

	// delivered is closed once that reply has been delivered. It is nil
	// when the future records no such channel, in which case the edge ends
	// only with its wait.
	delivered <-chan struct{}
}

// live reports whether the edge still stands for a blocked actor: its turn is
// running and the reply it waits on has not been delivered.
func (e *waitEdge) live() bool {
	if !e.t.active.Load() {
		return false
	}

	select {
	case <-e.delivered:
		return false

	default:
		return true
	}
}

// waitGraphLen returns the number of edges currently registered. It exists so
// tests can assert that every edge is removed again.
func waitGraphLen() int {
	waitGraph.Lock()
	defer waitGraph.Unlock()

	var n int
	for _, edges := range waitGraph.waiting {
		n += len(edges)
	}

	return n
}

// beginWait registers that the turn in ctx is about to block on the reply of
// target, whose reply is delivered once delivered closes, and checks whether
// that closes a cycle. It returns a function that removes the edge, which the
// caller must invoke once the wait ends by any route. When the wait would close
// a cycle, it registers nothing and returns an error wrapping ErrWaitCycle
// instead. A context outside an active turn, a future without a target and a
// non-serial turn all return a no-op release.
//
// It must be called directly from Await, because the call site it reports is
// two frames up.
func beginWait(ctx context.Context, target string,
	delivered <-chan struct{}) (func(), error) {

	noop := func() {}

	if target == "" {
		return noop, nil
	}

	t, ok := activeTurn(ctx)
	if !ok {
		return noop, nil
	}

	// A durable actor with several workers keeps draining its mailbox
	// while one worker is parked, so a blocked turn does not mean a
	// blocked actor and its wait cannot be part of a deadlock.
	if !t.serial {
		return noop, nil
	}

	self := t.actorID

	// Walking the graph and registering the edge happen under one lock
	// hold. Of two actors that close a cycle at the same time, whichever
	// takes the lock second sees the other's edge and fails, while the
	// first one parks. The failing Await returns into its turn, the turn
	// ends, and the actor can then process the Ask the other is waiting
	// on, so the cycle unwinds instead of both sides reporting it or
	// neither doing so.
	waitGraph.Lock()

	// The edge is added only if it closes no cycle, so the live graph
	// stays acyclic.
	path := findCycleLocked(self, target)
	if path == nil {
		edge := &waitEdge{t: t, target: target, delivered: delivered}
		waitGraph.waiting[self] = append(waitGraph.waiting[self], edge)
		waitGraph.Unlock()

		// Only the wait that registered the edge removes it, however
		// late it returns.
		return func() {
			waitGraph.Lock()
			defer waitGraph.Unlock()

			removeEdgeLocked(self, edge)
		}, nil
	}

	waitGraph.Unlock()

	// Skip this function and Await, so the site is the behavior code that
	// called Await.
	site := "unknown"
	if _, file, line, ok := runtime.Caller(2); ok {
		site = fmt.Sprintf("%s:%d", file, line)
	}

	reportWaitCycle(ctx, WaitCycle{Path: path, CallSite: site})

	return noop, fmt.Errorf("%w: %s", ErrWaitCycle,
		strings.Join(path, " -> "))
}

// removeEdgeLocked removes edge from the edges of actor id. The caller must
// hold the graph lock.
func removeEdgeLocked(id string, edge *waitEdge) {
	edges := waitGraph.waiting[id]
	for i, e := range edges {
		if e != edge {
			continue
		}

		edges = append(edges[:i], edges[i+1:]...)
		if len(edges) == 0 {
			delete(waitGraph.waiting, id)
		} else {
			waitGraph.waiting[id] = edges
		}

		return
	}
}

// findCycleLocked reports the path from self back to self that a new edge from
// self to target would close, or nil if no live chain of waits leads from
// target to self. A chain does not continue through an edge that is not live,
// since that actor is not blocked. The live graph is acyclic, because an edge
// that would close a cycle is refused, but the walk still tracks visited actors
// so it ends on its own regardless. The caller must hold the graph lock.
func findCycleLocked(self, target string) []string {
	visited := make(map[string]struct{})

	var walk func(cur string, path []string) []string
	walk = func(cur string, path []string) []string {
		if cur == self {
			return path
		}

		if _, seen := visited[cur]; seen {
			return nil
		}
		visited[cur] = struct{}{}

		for _, e := range waitGraph.waiting[cur] {
			if !e.live() {
				continue
			}

			next := append(path[:len(path):len(path)], e.target)
			if found := walk(e.target, next); found != nil {
				return found
			}
		}

		return nil
	}

	return walk(target, []string{self, target})
}

// WaitCycle describes a detected cycle of actor turns that were each blocked on
// another's reply.
type WaitCycle struct {
	// Path lists the actor IDs around the cycle, starting and ending with
	// the actor whose Await closed it, for example [a b a].
	Path []string

	// CallSite is the file and line of the Await that would have closed
	// the cycle.
	CallSite string
}

// reportWaitCycle logs a detected cycle. A cycle is a code defect that needs a
// human, so it is logged at error level.
func reportWaitCycle(ctx context.Context, c WaitCycle) {
	logger(ctx).ErrorS(ctx, "Await would deadlock: actor wait cycle",
		ErrWaitCycle,
		"path", strings.Join(c.Path, " -> "),
		"call_site", c.CallSite,
	)
}

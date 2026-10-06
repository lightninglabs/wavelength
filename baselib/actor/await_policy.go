package actor

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
)

// ErrAwaitInTurn is returned by Future.Await under AwaitInTurnError when an
// actor's receive turn waits on a future that has not completed yet. A turn
// that waits on another actor's reply parks its whole mailbox, and if that
// actor is in turn waiting on this one, both hang. Use AskThen to receive the
// reply as a later message instead, or DetachAskPromise to complete the
// caller's promise from the reply.
var ErrAwaitInTurn = errors.New("await of an incomplete future inside an " +
	"actor receive turn")

// AwaitInTurnPolicy selects what Future.Await does when an actor's receive
// turn calls it on a future that is not yet complete.
type AwaitInTurnPolicy int32

const (
	// AwaitInTurnWarn logs once per call site at info level, then waits
	// as it always has. It is the default and the zero value, so the
	// policy needs no initialization and a call site can be enumerated
	// before it is migrated.
	AwaitInTurnWarn AwaitInTurnPolicy = iota

	// AwaitInTurnAllow waits without any check.
	AwaitInTurnAllow

	// AwaitInTurnError returns ErrAwaitInTurn without waiting. A future
	// that has already completed still returns its result, since reading
	// it cannot block.
	AwaitInTurnError
)

var (
	// awaitInTurnPolicy holds the process-wide AwaitInTurnPolicy.
	awaitInTurnPolicy atomic.Int32

	// awaitInTurnSites records the call sites already logged under
	// AwaitInTurnWarn, keyed by program counter.
	awaitInTurnSites sync.Map

	// awaitInTurnFlagged counts the distinct call sites logged so far. It
	// exists so tests can assert on the once-per-site behavior without
	// capturing a logger.
	awaitInTurnFlagged atomic.Int64
)

// SetAwaitInTurnPolicy sets the process-wide policy for awaiting an
// incomplete future inside a receive turn. It is safe to call at any time,
// but it is meant to be set once at startup: rolling out enforcement means
// running under AwaitInTurnWarn until the logged call sites are migrated, then
// switching to AwaitInTurnError.
func SetAwaitInTurnPolicy(p AwaitInTurnPolicy) {
	awaitInTurnPolicy.Store(int32(p))
}

// currentAwaitInTurnPolicy returns the policy in force.
func currentAwaitInTurnPolicy() AwaitInTurnPolicy {
	return AwaitInTurnPolicy(awaitInTurnPolicy.Load())
}

// awaitAllowedKey is the context key under which AllowAwaitInTurn records the
// reason a bounded wait is exempt from the await-in-turn policy.
type awaitAllowedKey struct{}

// AllowAwaitInTurn returns a context under which Future.Await skips the
// await-in-turn policy, for the one wait that is safe even though it parks the
// actor's mailbox. It is an escape hatch for a wait the author can show cannot
// deadlock: the callee never sends to, asks, or otherwise waits on the calling
// actor, directly or through anything it calls. The wait should also carry a
// deadline, but nothing here checks that. Prefer AskThen or DetachAskPromise
// everywhere else.
//
// The reason is a sentence for the reader of the call site that says why the
// wait cannot be part of a cycle. An empty reason is ignored and the returned
// context stays subject to the policy, so the exemption cannot be added
// without an explanation.
//
// The exemption covers only the policy. The wait is still tracked by the wait
// cycle detector, so a callee that does wait on the caller is reported as
// ErrWaitCycle instead of hanging. Derive the context immediately before the
// Await rather than at the top of the behavior, so the exemption does not
// cover later waits that were not reviewed.
func AllowAwaitInTurn(ctx context.Context, reason string) context.Context {
	if reason == "" {
		return ctx
	}

	return context.WithValue(ctx, awaitAllowedKey{}, reason)
}

// awaitAllowed reports whether ctx carries an exemption from AllowAwaitInTurn.
func awaitAllowed(ctx context.Context) bool {
	_, ok := ctx.Value(awaitAllowedKey{}).(string)

	return ok
}

// checkAwaitInTurn applies the await-in-turn policy to an Await called with a
// context that may belong to a running turn. It returns nil when the await may
// proceed and ErrAwaitInTurn when it must not. It must be called directly from
// the Await method, because the call site it reports is two frames up.
func checkAwaitInTurn(ctx context.Context) error {
	policy := currentAwaitInTurnPolicy()
	if policy == AwaitInTurnAllow {
		return nil
	}

	t, ok := activeTurn(ctx)
	if !ok || awaitAllowed(ctx) {
		return nil
	}

	if policy == AwaitInTurnError {
		return ErrAwaitInTurn
	}

	// Skip this function and Await itself, so the site is the behavior
	// code that called Await. The program counter is the dedup key: one
	// line of behavior code is logged once however many turns run it.
	pc, file, line, ok := runtime.Caller(2)
	if !ok {
		return nil
	}

	if _, seen := awaitInTurnSites.LoadOrStore(pc, struct{}{}); seen {
		return nil
	}

	awaitInTurnFlagged.Add(1)

	logger(ctx).InfoS(ctx, "Await of an incomplete future inside an actor "+
		"turn can deadlock; use AskThen or DetachAskPromise",
		"actor_id", t.actorID,
		"call_site", fmt.Sprintf("%s:%d", file, line),
	)

	return nil
}

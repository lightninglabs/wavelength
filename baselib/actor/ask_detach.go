package actor

import (
	"context"
)

// DetachedAsk is the behavior-owned handle for completing an Ask after its
// turn returns. A coordinator behavior that only routes a request to another
// actor can detach the caller's promise, wire it into the downstream future's
// OnComplete, and return immediately instead of parking its single goroutine
// on Await -- the downstream actor's result then settles the original caller's
// future directly, keeping the caller's control flow linear.
type DetachedAsk[R any] struct {
	// Promise completes the original caller's future. Completion is
	// first-wins, so racing a late framework error completion against the
	// behavior's continuation is harmless.
	Promise Promise[R]

	// CallerCtx is the original caller's context. Continuations must use
	// it (not the turn context, which is cancelled when the turn returns)
	// so the caller's deadline still bounds the wait.
	CallerCtx context.Context
}

// askDetachBox carries an Ask delivery's promise through the behavior's turn
// context. The box is written and read on the actor goroutine only: the
// behavior detaches during its turn, and the framework inspects the flag
// right after the turn returns.
type askDetachBox struct {
	promise   any
	callerCtx context.Context
	detached  bool
}

// askDetachKey is the context key for the detachable ask promise.
type askDetachKey struct{}

// withDetachableAskPromise injects an Ask delivery's promise into the turn
// context so the behavior can take ownership of completing it.
func withDetachableAskPromise(ctx context.Context, promise any,
	callerCtx context.Context) (context.Context, *askDetachBox) {

	box := &askDetachBox{
		promise:   promise,
		callerCtx: callerCtx,
	}

	return context.WithValue(ctx, askDetachKey{}, box), box
}

// DetachAskPromise hands the current Ask delivery's promise to the behavior.
// It returns false when the turn has no detachable promise: the message was a
// Tell, a redelivered ask whose caller is gone, a DurableAsk (whose response
// travels via the outbox), or the actor runs on a path that does not support
// detaching. After a successful detach, the framework suppresses its
// automatic promise completion for a successful turn; a failed turn is still
// completed with the error by the framework, since the behavior's
// continuation may never have been wired.
func DetachAskPromise[R any](ctx context.Context) (DetachedAsk[R], bool) {
	box, ok := ctx.Value(askDetachKey{}).(*askDetachBox)
	if !ok || box.promise == nil {
		return DetachedAsk[R]{}, false
	}

	promise, ok := box.promise.(Promise[R])
	if !ok {
		return DetachedAsk[R]{}, false
	}

	callerCtx := box.callerCtx
	if callerCtx == nil {
		callerCtx = context.Background()
	}

	box.detached = true

	return DetachedAsk[R]{
		Promise:   promise,
		CallerCtx: callerCtx,
	}, true
}

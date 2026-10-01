package actor

import (
	"context"
	"errors"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// detachedAskKey marks a context as belonging to an AskThen request, whose
// caller's turn ends before the callee runs.
type detachedAskKey struct{}

// markDetachedAsk returns ctx marked as an AskThen request context. The marker
// lets actorRefImpl.Ask keep the caller's transaction out of the envelope it
// queues, while a durable callee, which enqueues synchronously, still joins the
// transaction through ctx itself.
func markDetachedAsk(ctx context.Context) context.Context {
	return context.WithValue(ctx, detachedAskKey{}, true)
}

// isDetachedAsk reports whether ctx was marked by markDetachedAsk.
func isDetachedAsk(ctx context.Context) bool {
	v, _ := ctx.Value(detachedAskKey{}).(bool)

	return v
}

// AskThen sends msg to ref as an Ask bounded by timeout, returns immediately,
// and later delivers wrap(result) to self as an ordinary message. The caller
// handles the reply in a later turn, so its state is only ever touched inside
// turns and its goroutine never waits on another actor. This is the
// pipe-to-self pattern: where Future.Await inside Receive can deadlock when
// the callee is, transitively, waiting on the caller, AskThen leaves the
// caller free to serve the callee in the meantime.
//
// Whatever happens to the Ask, wrap is applied to exactly one result: the
// callee's reply, the error the callee returned, a send failure such as
// ErrActorTerminated, or context.DeadlineExceeded once timeout elapses without
// a reply. The delivery of that message to self is at most once and best
// effort. It is lost if self has stopped, and for a durable self it is also
// lost when the enqueue fails while self lives, for instance on an encoding or
// database error, which is logged at warning level. A behavior that records a
// pending request must therefore give that entry its own expiry, rather than
// rely on the reply or its error always arriving. A timeout abandons the wait,
// not the request; the callee may still process the message and its late reply
// is discarded.
//
// A timeout that is zero or negative is already expired. No message is sent
// and wrap receives context.DeadlineExceeded, delivered like any other result,
// because there is no sensible unbounded default and an Ask that may never
// be answered is the failure this helper exists to avoid.
//
// A typical use takes two turns. In the first, the behavior sends the request
// and records that it is pending; in the second, it handles the wrapped
// result:
//
//	type balanceReply struct {
//		actor.BaseMessage
//		res fn.Result[Balance]
//	}
//
//	func (b *behavior) Receive(ctx context.Context,
//		msg Msg) fn.Result[Resp] {
//
//		switch m := msg.(type) {
//		case *startMsg:
//			// Turn one: ask, and remember we are waiting.
//			b.pending = true
//			actor.AskThen(
//				ctx, b.wallet, &queryBalance{}, b.self,
//				5*time.Second,
//				func(r fn.Result[Balance]) Msg {
//					return &balanceReply{res: r}
//				},
//			)
//
//		case *balanceReply:
//			// Turn two: the reply arrives as a message.
//			b.pending = false
//			b.apply(m.res)
//		}
//
//		return fn.Ok(Resp{})
//	}
//
// The wrap function must be pure. It runs on a helper goroutine, not on the
// actor's, so it may only map the result into a message and must not read or
// write the caller's state. Contrast DetachAskPromise, which is the forwarding
// special case: a coordinator completes its own caller's promise from a
// downstream reply and has no message of its own to handle afterwards.
//
// The Ask is sent with ctx's values, and what the callee sees of the caller's
// database transaction depends on its kind. A durable callee enqueues the
// request synchronously, before AskThen returns, so the enqueue joins the
// caller's transaction; it rebuilds its delivery context from its own mailbox
// and never exposes that transaction later. A channel-mailbox callee never
// sees the caller's transaction at all, because the turn that owns it has
// ended by the time the callee runs. This differs from a plain ref.Ask(ctx,
// msg), whose caller is parked and still holds its transaction open.
//
// A durable caller's turn may also roll back after AskThen returned. For a
// durable callee the Ask row rolls back with it, and the helper still delivers
// a wrapped timeout, so a behavior must treat a reply it has no pending entry
// for as stale and ignore it.
//
// The Ask's lifetime is detached from ctx and bounded only by timeout, so it
// outlives the turn. The reply is delivered to self with a context that
// carries neither the transaction nor ctx's cancellation: the transaction is
// gone once the turn commits, and the Ask context has already expired on a
// timeout, which would fail the delivery at once. If the turn has ended by
// then, the delivery is an external send and may wait on the helper goroutine
// for room in self's mailbox. If self has stopped, the result is dropped at
// debug level, since nothing is left to handle it.
func AskThen[M Message, R any, S Message](ctx context.Context,
	ref ActorRef[M, R], msg M, self TellOnlyRef[S], timeout time.Duration,
	wrap func(fn.Result[R]) S) {

	// The delivery context must not be the Ask context, see above. It is
	// derived from the same base so the logger and other values survive.
	base := context.WithoutCancel(ctx)
	deliverCtx := WithoutTx(base)

	deliver := func(res fn.Result[R]) {
		err := self.Tell(deliverCtx, wrap(res))
		if err == nil {
			return
		}

		// The delivery context cannot be cancelled, so a shutdown-style
		// error means self is gone and nobody is left to tell. Any
		// other error, such as a durable self failing to encode or
		// enqueue the message, loses the reply of a live actor, which
		// then never completes its pending request.
		if errors.Is(err, ErrMailboxClosed) ||
			errors.Is(err, ErrActorTerminated) ||
			isExpectedShutdownErr(err) {

			logger(deliverCtx).DebugS(
				deliverCtx,
				"AskThen result dropped",
				"err", err,
			)

			return
		}

		logger(deliverCtx).WarnS(
			deliverCtx, "AskThen result lost for a live actor", err,
			"self", self.ID(),
		)
	}

	if timeout <= 0 {
		go deliver(fn.Err[R](context.DeadlineExceeded))

		return
	}

	askCtx, cancel := context.WithTimeout(base, timeout)
	askCtx = markDetachedAsk(askCtx)

	ref.Ask(askCtx, msg).OnComplete(askCtx, func(res fn.Result[R]) {
		// The wait is over, so release the timer before a possibly slow
		// delivery.
		cancel()

		deliver(res)
	})
}

package actor

import (
	"context"
	"errors"
)

// Probe checks the receive loop of a local channel-mailbox actor without
// invoking its behavior. Pass the direct reference returned by NewActor or
// RegisterWithSystem; routers, mapped refs, and durable actors are unsupported.
// Admission never waits for mailbox room. Response waiting respects ctx and
// actor shutdown. Concurrent callers share at most one queued probe, even after
// timeout, so a stalled actor cannot accumulate health-check work. A completed
// probe is replaced on the next call rather than reused as stale success.
// The returned counter counts completed turns (including probes), even when
// the probe fails. Supervisors can use changes in this counter for liveness
// without mistaking a busy queue or a late acknowledgement for a stuck loop.
func Probe[M Message, R any](ctx context.Context,
	ref ActorRef[M, R]) (uint64, error) {

	local, ok := ref.(*actorRefImpl[M, R])
	if !ok {
		return 0, errors.New("probe requires a direct channel actor " +
			"reference")
	}
	a := local.actor
	if err := ctx.Err(); err != nil {
		return a.completed.Load(), err
	}
	if a.ctx.Err() != nil {
		return a.completed.Load(), ErrActorTerminated
	}

	// Only admission is serialized. No behavior, blocking send, or external
	// call runs under this lock.
	a.probeMu.Lock()
	done := a.probeDone
	if done != nil {
		select {
		case <-done:
			done = nil

		default:
		}
	}
	if done == nil {
		done = make(chan struct{})
		err := a.mailbox.TrySend(envelope[M, R]{probe: done})
		if err != nil {
			a.probeMu.Unlock()

			return a.completed.Load(), err
		}
		a.probeDone = done
	}
	a.probeMu.Unlock()

	select {
	case <-done:
		if a.ctx.Err() != nil {
			return a.completed.Load(), ErrActorTerminated
		}

		return a.completed.Load(), ctx.Err()

	case <-ctx.Done():
		return a.completed.Load(), ctx.Err()

	case <-a.ctx.Done():
		return a.completed.Load(), ErrActorTerminated
	}
}

package actor

import (
	"context"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// NonParkingTeller is implemented by every reference in this package and is
// how a reference answers the question "deliver this without ever parking me"
// for the target it actually resolves at send time.
//
// The interface exists because the reference, not the caller, knows two
// things the decision depends on: what kind of mailbox the concrete target
// has, and which target a send-time-resolving reference (a Router, or a
// transform wrapping one) picks for this particular message. A Router key
// can hold both a bounded and a durable actor at once; any answer computed
// before selection can disagree with the target the message is handed to,
// and picking the wrong primitive for a durable target is the expensive
// direction (see TellWithoutParking). Making the reference decide and
// deliver in one step is what keeps the answer and the delivery consistent.
type NonParkingTeller[M Message] interface {
	// TellWithoutParkingTo delivers msg to whichever target this
	// reference resolves for it, choosing Tell or TryTell by that
	// target's own mailbox. The result carries whether the delivery
	// escaped the caller's transaction: true means a successful TryTell
	// hand-off into an in-memory mailbox, which is already visible to
	// the receiving actor and cannot be rolled back, so a caller whose
	// transaction may run more than once must not send it a second
	// time. False means nothing escaped: a Tell enqueue joins whatever
	// transaction the caller's context carries and is undone with it, a
	// filtering transform may have dropped the message, and an errored
	// result delivered nothing at all.
	TellWithoutParkingTo(ctx context.Context, msg M) fn.Result[bool]
}

// TellWithoutParking delivers msg to ref with one guarantee the plain Tell
// cannot make: the calling goroutine is never parked waiting for room in the
// target's mailbox. A bounded in-memory target is sent to with TryTell, so a
// full mailbox comes back as ErrMailboxFull for the caller to stash, redrive,
// or fail on. Everything else keeps Tell. The result carries whether the
// delivery escaped the caller's transaction, exactly as documented on
// NonParkingTeller.
//
// The split is what makes this safe to call from a goroutine that must stay
// live, and from inside a database transaction. A durable target must keep
// Tell there: TryTell performs its write on its own background context, which
// drops the caller's transaction and, on a single-writer store such as
// SQLite, cannot even complete while the caller still holds the writer. A
// bounded in-memory target has the opposite problem — its Send is a blocking
// channel send with no bound at all — and TryTell is the only way to keep the
// caller moving.
//
// A reference this package does not recognise keeps the plain Tell. That is
// the conservative direction: Tell is the primitive that carries a durable
// enqueue into the caller's transaction, and guessing the other way would
// silently break that atomicity for a reference implemented elsewhere.
//
// Callers must handle ErrMailboxFull, and its durable analogue
// ErrMailboxSaturated when the target's mailbox carries backlog watermarks:
// both mean the target refused for want of room and expects the caller to
// stash, redrive, or shed. Treating either as a delivery failure and
// dropping the message converts backpressure into silent message loss, which
// for an at-least-once transport is worse than the stall it replaces.
func TellWithoutParking[M Message](ctx context.Context, ref TellOnlyRef[M],
	msg M) fn.Result[bool] {

	if teller, ok := ref.(NonParkingTeller[M]); ok {
		return teller.TellWithoutParkingTo(ctx, msg)
	}

	return fn.NewResult(false, ref.Tell(ctx, msg))
}

// TellWithoutParkingTo sends with TryTell when the actor runs on the
// fixed-capacity channel mailbox, which is what every actor started through
// NewActor gets, and with Tell otherwise. Only the channel mailbox's Send can
// park the caller waiting for the receiver to make room.
func (ref *actorRefImpl[M, R]) TellWithoutParkingTo(ctx context.Context,
	msg M) fn.Result[bool] {

	if _, bounded := ref.actor.mailbox.(*ChannelMailbox[M, R]); bounded {
		return fn.NewResult(true, ref.TryTell(ctx, msg))
	}

	return fn.NewResult(false, ref.Tell(ctx, msg))
}

// TellWithoutParkingTo keeps the plain Tell: a durable mailbox has no
// in-memory capacity, so its enqueue waits on a database write inside the
// caller's transaction instead of on the receiving actor draining its queue.
// The write itself never parks on the consumer; what it can do is refuse
// with ErrMailboxSaturated when the mailbox carries backlog watermarks and
// the backlog is past the hard one, which the caller handles like
// ErrMailboxFull.
func (ref *durableActorRefImpl[M, R]) TellWithoutParkingTo(ctx context.Context,
	msg M) fn.Result[bool] {

	return fn.NewResult(false, ref.Tell(ctx, msg))
}

// TellWithoutParkingTo resolves the router's target the same way Tell does
// and then makes the bounded-or-not decision against that one reference, so
// the primitive always matches the actor the message is handed to.
//
// Selection consumes a routing turn exactly once, as a Tell through the
// router would. A resolution failure is handed back to the router's own Tell
// rather than returned directly, so the dead-letter fallback for an empty key
// keeps working.
func (r *Router[M, R]) TellWithoutParkingTo(ctx context.Context,
	msg M) fn.Result[bool] {

	selected, err := r.getActor()
	if err != nil {
		return fn.NewResult(false, r.Tell(ctx, msg))
	}

	return TellWithoutParking[M](ctx, selected, msg)
}

// TellWithoutParkingTo transforms the message and lets the wrapped reference
// make the delivery decision, so a transform in front of a router does not
// flatten the router's per-target answer. A transform failure is reported
// before the target is touched at all, matching Tell and TryTell.
func (m *MapRef[In, Out, InR, OutR]) TellWithoutParkingTo(ctx context.Context,
	msg In) fn.Result[bool] {

	transformed, err := m.mapInput(msg)
	if err != nil {
		return fn.Errf[bool]("map input: %w", err)
	}

	return TellWithoutParking[Out](ctx, m.targetRef, transformed)
}

// TellWithoutParkingTo transforms the message and lets the wrapped reference
// make the delivery decision, for the same reason as MapRef's.
func (m *MapInputRef[In, Out]) TellWithoutParkingTo(ctx context.Context,
	msg In) fn.Result[bool] {

	return TellWithoutParking[Out](ctx, m.targetRef, m.mapFn(msg))
}

// TellWithoutParkingTo transforms the message and lets the wrapped reference
// make the delivery decision. A dropped message reports false with no error:
// it reached no mailbox, so nothing escaped the caller's transaction and
// there is nothing for a retrying caller to suppress.
func (m *FilterMapInputRef[In, Out]) TellWithoutParkingTo(ctx context.Context,
	msg In) fn.Result[bool] {

	transformed, ok := m.mapFn(msg)
	if !ok {
		return fn.Ok(false)
	}

	return TellWithoutParking[Out](ctx, m.targetRef, transformed)
}

// Compile-time interface checks. Every reference implementation in this
// package answers the non-parking delivery question itself, so
// TellWithoutParking never has to fall back to its conservative default for
// an in-tree target.
var (
	_ NonParkingTeller[Message]    = (*actorRefImpl[Message, any])(nil)
	_ NonParkingTeller[TLVMessage] = (*durableActorRefImpl[TLVMessage,
		any])(nil)
	_ NonParkingTeller[Message] = (*Router[Message, any])(nil)
	_ NonParkingTeller[Message] = (*MapRef[Message, Message, any, any])(nil)
	_ NonParkingTeller[Message] = (*MapInputRef[Message, Message])(nil)
	_ NonParkingTeller[Message] = (*FilterMapInputRef[Message,
		Message])(nil)
)

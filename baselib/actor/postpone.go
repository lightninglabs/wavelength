package actor

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// A nack is the wrong tool for "not now": it burns one of the message's
// finite delivery attempts, so a consumer that is merely waiting on an
// external condition (a capacity slot, a peer draining, an operator catching
// up) walks an innocent message toward the dead-letter table one redelivery
// at a time. Postpone is the attempt-preserving alternative: the behavior
// returns a PostponeError, and the consume path releases the message for
// redelivery after the requested delay WITHOUT counting the attempt, so the
// message can wait out the condition indefinitely.
//
// The flip side is deliberate: a postponed message never climbs toward
// max_attempts, so nothing dead-letters it automatically. A behavior that
// postpones must bound its own horizon (give up with a real error once the
// wait stops making sense), or accept that the message waits forever.

// deliveryEnqueuedAtKey keys the current delivery's enqueue timestamp in the
// processing context.
type deliveryEnqueuedAtKey struct{}

// withDeliveryEnqueuedAt stamps the current delivery's enqueue timestamp onto
// the processing context. The consume path calls this once per delivery,
// before the behavior runs, so every execution path (tx fold, non-tx tail, and
// the Read/Commit Exec handle) exposes the same value. A zero timestamp is not
// stamped, so a store that does not report one leaves DeliveryEnqueuedAt
// reporting absence rather than a bogus epoch age.
func withDeliveryEnqueuedAt(ctx context.Context,
	enqueuedAt time.Time) context.Context {

	if enqueuedAt.IsZero() {
		return ctx
	}

	return context.WithValue(ctx, deliveryEnqueuedAtKey{}, enqueuedAt)
}

// DeliveryEnqueuedAt reports when the message currently being processed was
// first persisted to the mailbox, and whether that timestamp is available at
// all. The value comes from the durable row and survives every redelivery,
// since neither a nack nor a postpone rewrites it.
//
// This is the intended way for a postponing behavior to bound its own horizon.
// Postpone deliberately removes the attempt-based give-up mechanism, so a
// behavior that waits on an external condition needs some other reference to
// decide when waiting has stopped making sense. Deriving that from the row
// rather than from behavior-side state matters whenever the message stream is
// attacker-controlled: a per-message map keyed on anything the sender chooses
// is unbounded by construction, while the row's own age costs nothing to
// consult and cannot be inflated by fabricating new messages.
//
// The second return is false outside a delivery (so a behavior invoked
// directly in a test sees no timestamp) and for a store that does not report
// one. Treat absence as "no horizon information", not as "age zero".
func DeliveryEnqueuedAt(ctx context.Context) (time.Time, bool) {
	enqueuedAt, ok := ctx.Value(deliveryEnqueuedAtKey{}).(time.Time)

	return enqueuedAt, ok
}

// WithDeliveryEnqueuedAtForTest stamps a delivery enqueue timestamp onto ctx
// so a behavior in another package can be unit tested against its postpone
// horizon without standing up a durable actor and a real mailbox row. It is
// the exported form of what the consume path does for every delivery, and it
// exists only for tests.
func WithDeliveryEnqueuedAtForTest(ctx context.Context,
	enqueuedAt time.Time) context.Context {

	return withDeliveryEnqueuedAt(ctx, enqueuedAt)
}

// ErrPostponed is the sentinel matched by errors.Is for postpone requests.
// Behaviors construct one with Postpone; the consume path detects it and
// releases the delivery without burning an attempt.
var ErrPostponed = errors.New("delivery postponed")

// PostponeError asks the consume path to re-enqueue the current message
// after Delay without counting the attempt. It is a control-flow signal, not
// a failure: the consume path logs it at debug level and never routes it
// toward the dead-letter table.
type PostponeError struct {
	// Delay is how long the message stays invisible before it becomes
	// claim-eligible again. A zero or negative value makes it eligible
	// immediately, which on a still-unmet condition is a busy retry loop;
	// callers should pass a real backoff.
	Delay time.Duration
}

// Error implements the error interface.
func (e *PostponeError) Error() string {
	return fmt.Sprintf("delivery postponed for %v", e.Delay)
}

// Unwrap exposes the ErrPostponed sentinel so errors.Is matches.
func (e *PostponeError) Unwrap() error {
	return ErrPostponed
}

// Postpone builds the error a behavior returns to re-enqueue the current
// Tell message after delay without burning a delivery attempt. Only Tell
// deliveries honor it: an Ask has a caller parked on the promise, so a
// postponed Ask would strand that caller for the length of the delay with
// nothing to observe. An Ask behavior that returns a PostponeError gets the
// ordinary error treatment (the promise completes with it) and the caller
// decides whether to re-issue.
func Postpone(delay time.Duration) error {
	return &PostponeError{Delay: delay}
}

// postponeDelay extracts the postpone request from a behavior error, if one
// is present. It matches anywhere in the wrap chain, so a behavior may
// annotate the postpone with context (fmt.Errorf("%w: over cap", ...)).
func postponeDelay(err error) (time.Duration, bool) {
	var postpone *PostponeError
	if errors.As(err, &postpone) {
		return postpone.Delay, true
	}

	return 0, false
}

// postponeMessage releases a delivery for redelivery without burning an
// attempt, picking the fenced or unfenced store operation by whether a lease
// token is present, exactly as ackMessage/nackMessage do. The fenced variant
// decrements attempts to compensate the increment the lease took at claim;
// the by-ID variant leaves attempts untouched because the leaseless peek
// never bumped it. Either way the message's retry budget is exactly what it
// was before this delivery.
func postponeMessage(ctx context.Context, store DeliveryStore, id,
	leaseToken string, retryAfter time.Duration) (int64, error) {

	if leaseToken == "" {
		return store.PostponeMessageByID(ctx, id, retryAfter)
	}

	return store.PostponeMessage(ctx, id, leaseToken, retryAfter)
}

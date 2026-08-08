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

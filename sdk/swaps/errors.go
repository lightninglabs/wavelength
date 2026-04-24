package swaps

import (
	"context"
	"errors"
	"fmt"

	loopfsm "github.com/lightninglabs/loop/fsm"
)

var (
	// ErrSwapExpired is returned when a client-side swap reaches its
	// negotiated deadline before the expected funding, claim, or preimage
	// observation occurs.
	ErrSwapExpired = errors.New("swap expired")

	// ErrSwapRefunded is returned when a pay-side swap timed out after
	// funding and the client recovered the vHTLC funds through the refund
	// path.
	ErrSwapRefunded = errors.New("swap refunded")
)

// errSwapExpired is kept as an internal alias for older tests and helpers.
var errSwapExpired = ErrSwapExpired

// interventionError records an anomalous swap condition that should stop the
// FSM in a terminal NeedsIntervention state rather than collapsing into a
// generic failure.
type interventionError struct {
	reason string
	cause  error
}

// failureError records a classified terminal failure that is safe to stop
// without operator intervention.
type failureError struct {
	reason string
	cause  error
}

// retryableActionError reports that an external action may have succeeded, but
// the SDK could not durably persist follow-up metadata yet.
type retryableActionError struct {
	cause error
}

// Error returns the wrapped retryable action error.
func (e *retryableActionError) Error() string {
	if e == nil || e.cause == nil {
		return "retryable swap action"
	}

	return e.cause.Error()
}

// Unwrap exposes the underlying retryable action error.
func (e *retryableActionError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.cause
}

// newRetryableActionError marks err as non-terminal for the durable FSM.
func newRetryableActionError(err error) error {
	return &retryableActionError{
		cause: err,
	}
}

// Error returns the human-readable intervention reason.
func (e *interventionError) Error() string {
	if e == nil {
		return "swap needs intervention"
	}

	if e.cause == nil {
		return fmt.Sprintf("swap needs intervention: %s", e.reason)
	}

	return fmt.Sprintf("swap needs intervention: %s: %v", e.reason, e.cause)
}

// Unwrap exposes the underlying cause for callers that want the root error.
func (e *interventionError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.cause
}

// newInterventionError constructs one intervention-classified terminal error.
func newInterventionError(reason string, cause error) error {
	return &interventionError{
		reason: reason,
		cause:  cause,
	}
}

// Error returns the human-readable terminal failure reason.
func (e *failureError) Error() string {
	if e == nil {
		return "swap failed"
	}

	if e.cause == nil {
		return fmt.Sprintf("swap failed: %s", e.reason)
	}

	return fmt.Sprintf("swap failed: %s: %v", e.reason, e.cause)
}

// Unwrap exposes the underlying cause for callers that want the root error.
func (e *failureError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.cause
}

// newFailureError constructs a non-intervention terminal failure error.
func newFailureError(reason string, cause error) error {
	return &failureError{
		reason: reason,
		cause:  cause,
	}
}

// interventionReason extracts the durable intervention reason from err when
// the error marks an anomalous operator-visible swap state.
func interventionReason(err error) string {
	var intervention *interventionError
	if !errors.As(err, &intervention) {
		return ""
	}

	return intervention.reason
}

// failureReason extracts the durable non-intervention failure reason from err.
func failureReason(err error) string {
	var failure *failureError
	if !errors.As(err, &failure) {
		return ""
	}

	return failure.reason
}

// isInterruptErr reports whether the caller interrupted the current blocking
// SDK call without implying a durable terminal swap failure.
func isInterruptErr(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// handleFailure persists the terminal swap state associated with err and
// returns the Loop FSM event that keeps the transport FSM aligned with the
// durable business state.
func handleFailure(ctx context.Context, err error, runErr *error,
	currentExpired bool, currentNeedsIntervention bool,
	markExpired func(context.Context) error, expiredEvent loopfsm.EventType,
	markNeedsIntervention func(context.Context, string) error,
	needsInterventionEvent loopfsm.EventType,
	markFailed func(context.Context, string) error,
	failedEvent loopfsm.EventType,
) loopfsm.EventType {

	*runErr = err
	if isInterruptErr(err) {
		return loopfsm.NoOp
	}

	var retryableAction *retryableActionError
	if errors.As(err, &retryableAction) {
		return loopfsm.NoOp
	}

	if errors.Is(err, errSwapExpired) {
		if currentExpired {
			return loopfsm.NoOp
		}
		if transitionErr := markExpired(ctx); transitionErr != nil {
			*runErr = errors.Join(err, transitionErr)

			return loopfsm.NoOp
		}

		return expiredEvent
	}

	if reason := interventionReason(err); reason != "" {
		if currentNeedsIntervention {
			return loopfsm.NoOp
		}
		transitionErr := markNeedsIntervention(ctx, reason)
		if transitionErr != nil {
			*runErr = errors.Join(err, transitionErr)

			return loopfsm.NoOp
		}

		return needsInterventionEvent
	}

	reason := failureReason(err)
	if reason == "" && err != nil {
		reason = err.Error()
	}
	if transitionErr := markFailed(ctx, reason); transitionErr != nil {
		*runErr = errors.Join(err, transitionErr)

		return loopfsm.NoOp
	}

	return failedEvent
}

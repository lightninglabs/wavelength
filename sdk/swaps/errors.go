package swaps

import (
	"context"
	"errors"
	"fmt"
	"strings"

	loopfsm "github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const unregisteredScriptErrText = "script not registered for principal"

var (
	// ErrSwapExpired is returned when a client-side swap reaches its
	// negotiated deadline before the expected funding, claim, or preimage
	// observation occurs.
	ErrSwapExpired = errors.New("swap expired")

	// ErrSwapRefunded is returned when a pay-side swap timed out after
	// funding and the client recovered the vHTLC funds through the refund
	// path.
	ErrSwapRefunded = errors.New("swap refunded")

	// ErrSwapSummaryNotFound is returned when no persisted pay or receive
	// swap summary exists for a requested payment hash.
	ErrSwapSummaryNotFound = errors.New("swap summary not found")
)

// errSwapExpired is kept as an internal alias for older tests and helpers.
var errSwapExpired = ErrSwapExpired

// errReceiveClaimAlreadyIndexed reports that the receive-side claim has
// already been observed in the indexer, so no OOR session id should be
// persisted for the current process.
var errReceiveClaimAlreadyIndexed = errors.New("receive claim already indexed")

// errReceiveVHTLCSpentWithoutPreimage reports that the funded receive vHTLC
// was spent by a path that did not reveal the invoice preimage.
var errReceiveVHTLCSpentWithoutPreimage = errors.New("receive vHTLC spent " +
	"without claim preimage")

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

// isDeadlineExceededErr reports whether err carries a deadline-exceeded
// signal, regardless of whether it was produced locally as
// context.DeadlineExceeded or returned over gRPC as a status error. gRPC's
// status type does not unwrap to context.DeadlineExceeded, so the bare
// errors.Is check on its own would miss the wire-encoded form.
func isDeadlineExceededErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	return status.Code(err) == codes.DeadlineExceeded
}

// isUnregisteredScriptErr reports an authoritative indexer rejection for a
// script that is no longer bound to the caller's current mailbox principal.
func isUnregisteredScriptErr(err error) bool {
	if err == nil || !strings.Contains(
		err.Error(), unregisteredScriptErrText,
	) {
		return false
	}

	switch status.Code(err) {
	case codes.Unauthenticated, codes.PermissionDenied, codes.Internal,
		codes.Unknown:
		return true

	default:
		return false
	}
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

	if isWalletNotReadyErr(err) {
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

// isWalletNotReadyErr reports daemon wallet-readiness preconditions that are
// transient across startup and unlock. These errors must not durably fail a
// swap because a later resume can continue once the daemon wallet is ready.
func isWalletNotReadyErr(err error) bool {
	return waverpc.IsWalletNotReadyError(err)
}

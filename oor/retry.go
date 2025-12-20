package oor

import (
	"errors"
	"fmt"
	"time"
)

// RetryableOutboxError wraps an error and marks it as retryable.
//
// The actor wrapper interprets this to mean it should feed an OutboxErrorEvent
// into the FSM with Retryable=true rather than failing the call.
type RetryableOutboxError struct {
	Err        error
	RetryAfter time.Duration
}

// Error returns the string representation of the wrapped error.
func (e *RetryableOutboxError) Error() string {
	if e == nil {
		return "<nil>"
	}

	if e.Err == nil {
		return "retryable outbox error"
	}

	return fmt.Sprintf("retryable outbox error: %v", e.Err)
}

// Unwrap returns the wrapped error.
func (e *RetryableOutboxError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}

// NewRetryableOutboxError creates a RetryableOutboxError wrapper.
func NewRetryableOutboxError(err error, retryAfter time.Duration) error {
	if err == nil {
		return &RetryableOutboxError{
			Err:        fmt.Errorf("unknown error"),
			RetryAfter: retryAfter,
		}
	}

	return &RetryableOutboxError{
		Err:        err,
		RetryAfter: retryAfter,
	}
}

// NewOutboxErrorEvent converts an outbox execution error into an FSM event.
func NewOutboxErrorEvent(outbox OutboxEvent, err error) *OutboxErrorEvent {
	outboxType := ""
	if outbox != nil {
		outboxType = outbox.outboxType()
	}

	// The retry behavior is encoded in the event rather than by returning a
	// different Go error type from the actor. This keeps the actor boundary
	// simple: every outbox failure maps to a deterministic event, and the
	// FSM decides whether to back off or fail terminally.
	retryAfter, retryable := retryableOutboxParams(err)

	return &OutboxErrorEvent{
		OutboxType:  outboxType,
		Retryable:   retryable,
		RetryAfter:  retryAfter,
		ErrorReason: err.Error(),
	}
}

// retryableOutboxParams returns the retry delay and whether the error is
// retryable.
func retryableOutboxParams(err error) (time.Duration, bool) {
	var retryErr *RetryableOutboxError
	if errors.As(err, &retryErr) {
		return retryErr.RetryAfter, true
	}

	return 0, false
}

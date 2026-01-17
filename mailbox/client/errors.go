package mailboxclient

import (
	"fmt"

	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
)

// StatusError wraps a mailboxpb.Status failure.
type StatusError struct {
	// Op is the mailbox operation that failed (Send, Pull, AckUpTo).
	Op string

	// Status is the status returned by the mailbox edge.
	Status *mailboxpb.Status
}

// Error returns a human-readable description of the error.
func (e *StatusError) Error() string {
	if e == nil || e.Status == nil {
		return "mailbox status error"
	}

	return fmt.Sprintf("%s failed: %s (%s)", e.Op, e.Status.Message,
		e.Status.Code)
}

// statusOK returns true when status indicates success.
func statusOK(status *mailboxpb.Status) bool {
	return status != nil && status.Ok
}

// statusError constructs a StatusError for a failed mailbox response.
func statusError(op string, status *mailboxpb.Status) error {
	return &StatusError{
		Op:     op,
		Status: status,
	}
}

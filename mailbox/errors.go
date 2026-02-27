package mailbox

import "fmt"

// ErrEnvelopeTooLarge is returned when an envelope exceeds the configured size
// limit.
type ErrEnvelopeTooLarge struct {
	// Size is the protobuf-encoded size of the envelope.
	Size int

	// Max is the maximum permitted size of the envelope.
	Max int
}

// Error returns the error message.
func (e *ErrEnvelopeTooLarge) Error() string {
	return fmt.Sprintf("envelope too large: %d > %d", e.Size, e.Max)
}

// ErrMailboxFull is returned when a mailbox exceeds the configured envelope
// count limit.
type ErrMailboxFull struct {
	// Recipient is the mailbox recipient identifier.
	Recipient string

	// Max is the maximum number of envelopes permitted in the mailbox.
	Max int
}

// Error returns the error message.
func (e *ErrMailboxFull) Error() string {
	return fmt.Sprintf("mailbox full: %q max=%d", e.Recipient, e.Max)
}

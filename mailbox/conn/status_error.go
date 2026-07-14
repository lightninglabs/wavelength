package conn

import (
	"errors"
	"fmt"

	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
)

// Permanent mailbox status codes. These classify a non-OK mailbox edge
// response as an expected, non-retryable version-compatibility failure rather
// than a transient transport hiccup or an internal bug. They are the
// machine-readable values carried in mailboxpb.Status.Code.
const (
	// StatusMailboxVersionUnsupported indicates the envelope's mailbox
	// transport framing version is not supported by the responder. The
	// sender must use a compatible endpoint or client.
	StatusMailboxVersionUnsupported = "MAILBOX_VERSION_UNSUPPORTED"

	// StatusArkVersionUnsupported indicates the envelope's Ark protocol
	// version is not currently enabled by the responder. The sender must
	// renegotiate or upgrade.
	StatusArkVersionUnsupported = "ARK_VERSION_UNSUPPORTED"

	// StatusArkVersionMismatch indicates the envelope's Ark protocol
	// version differs from the version already bound to the responder's
	// runtime for this client. The sender must stop the runtime and
	// renegotiate.
	StatusArkVersionMismatch = "ARK_VERSION_MISMATCH"

	// StatusUpgradeRequired indicates the client software must upgrade
	// before continuing. The sender must stop retrying and surface the
	// upgrade information.
	StatusUpgradeRequired = "UPGRADE_REQUIRED"
)

// permanentVersionCodes is the set of status codes that represent permanent,
// non-retryable version-compatibility failures. It is the single source of
// truth for permanent-error classification so unary, durable event,
// heartbeat, pull, and ack paths all agree.
var permanentVersionCodes = map[string]struct{}{
	StatusMailboxVersionUnsupported: {},
	StatusArkVersionUnsupported:     {},
	StatusArkVersionMismatch:        {},
	StatusUpgradeRequired:           {},
}

// StatusError wraps a non-OK mailbox edge Status so callers can classify the
// failure and recover the full structured payload without flattening it into a
// generic error string. It is the shared status type used by every client Send,
// Pull, and AckUpTo path, replacing previously duplicated unexported status
// handling.
type StatusError struct {
	// Op is the mailbox operation that failed (e.g. "Send", "Pull",
	// "AckUpTo").
	Op string

	// Status is the complete status returned by the mailbox edge. It is
	// retained verbatim so callers can read every structured field.
	Status *mailboxpb.Status
}

// NewStatusError builds a StatusError for the given operation and edge status.
// The status is retained as-is; callers should only construct this for a
// non-OK status.
func NewStatusError(op string, status *mailboxpb.Status) *StatusError {
	return &StatusError{
		Op:     op,
		Status: status,
	}
}

// Error returns a human-readable description that preserves the operation,
// message, and code so logs remain diagnosable.
func (e *StatusError) Error() string {
	if e.Status == nil {
		return e.Op + ": nil status"
	}

	return fmt.Sprintf("%s: %s (%s)", e.Op, e.Status.Message, e.Status.Code)
}

// Code returns the machine-readable status code, or the empty string when no
// status is present.
func (e *StatusError) Code() string {
	if e.Status == nil {
		return ""
	}

	return e.Status.Code
}

// IsPermanentVersion reports whether this status is one of the four permanent
// version-compatibility codes. Every other status (transient transport or
// internal failure) is left to the existing retry policy.
func (e *StatusError) IsPermanentVersion() bool {
	if e.Status == nil {
		return false
	}

	_, ok := permanentVersionCodes[e.Status.Code]

	return ok
}

// SupportedMailboxVersions returns the mailbox transport versions the
// responder advertised, if any.
func (e *StatusError) SupportedMailboxVersions() []uint32 {
	if e.Status == nil {
		return nil
	}

	return e.Status.SupportedMailboxVersions
}

// SupportedArkVersions returns the Ark protocol versions the responder
// advertised, if any.
func (e *StatusError) SupportedArkVersions() []uint32 {
	if e.Status == nil {
		return nil
	}

	return e.Status.SupportedArkVersions
}

// IsPermanentVersionError reports whether err is, or wraps, a StatusError
// carrying one of the permanent version-compatibility codes. Durable senders
// use this to stop retrying and to dead-letter the failing message.
func IsPermanentVersionError(err error) bool {
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		return false
	}

	return statusErr.IsPermanentVersion()
}

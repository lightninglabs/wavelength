package oor

const (
	// DefaultMaxCheckpoints caps checkpoint transactions accepted in one
	// incoming transfer.
	DefaultMaxCheckpoints uint32 = 64

	// DefaultMaxVTXOMatches caps matching VTXOs accepted from one indexer
	// lookup during incoming receive.
	DefaultMaxVTXOMatches uint32 = 128

	// DefaultMaxMailboxItems caps length-prefixed mailbox item counts
	// before allocating the decoded slice.
	DefaultMaxMailboxItems uint32 = 10_000

	// DefaultMaxMailboxScriptBytes caps persisted incoming-recipient
	// script hints decoded from the mailbox.
	DefaultMaxMailboxScriptBytes uint32 = 10_000

	// DefaultMaxConcurrentIncomingSessions caps the number of resident,
	// non-terminal incoming receive sessions one daemon admits at once. It
	// bounds the per-session children (goroutines, durable mailboxes, and
	// control-plane rows) an operator can pin by streaming unanswered
	// incoming hints against an owned receive script.
	DefaultMaxConcurrentIncomingSessions uint32 = 1_024
)

// ReceiveLimits groups bounds for the incoming OOR receive path. Zero fields
// use the package defaults. Functions accepting ReceiveLimits normalize their
// inputs before enforcing any cap.
type ReceiveLimits struct {
	// MaxCheckpoints caps checkpoint transactions accepted in one incoming
	// transfer.
	MaxCheckpoints uint32

	// MaxVTXOMatches caps matching VTXOs accepted from one indexer lookup
	// during incoming receive. The durable metadata query also uses this
	// value as its page-size limit.
	MaxVTXOMatches uint32

	// MaxMailboxItems caps length-prefixed mailbox item counts before the
	// decoder allocates the output slice.
	MaxMailboxItems uint32

	// MaxMailboxScriptBytes caps persisted incoming-recipient script hints
	// decoded from the mailbox.
	MaxMailboxScriptBytes uint32

	// MaxConcurrentIncomingSessions caps the number of resident,
	// non-terminal incoming receive children the registry admits at once.
	// Admission past the cap is rejected; the hint is retried by transport,
	// so a transient over-cap rejection is recoverable once earlier
	// sessions terminate and are reaped.
	MaxConcurrentIncomingSessions uint32
}

// DefaultReceiveLimits returns the default OOR incoming receive limits.
func DefaultReceiveLimits() ReceiveLimits {
	limits := ReceiveLimits{
		MaxCheckpoints:        DefaultMaxCheckpoints,
		MaxVTXOMatches:        DefaultMaxVTXOMatches,
		MaxMailboxItems:       DefaultMaxMailboxItems,
		MaxMailboxScriptBytes: DefaultMaxMailboxScriptBytes,
	}
	limits.MaxConcurrentIncomingSessions =
		DefaultMaxConcurrentIncomingSessions

	return limits
}

// normalizeReceiveLimits fills zero-valued fields with package defaults so
// callers can override one limit without restating the whole set.
func normalizeReceiveLimits(limits ReceiveLimits) ReceiveLimits {
	defaults := DefaultReceiveLimits()

	if limits.MaxCheckpoints == 0 {
		limits.MaxCheckpoints = defaults.MaxCheckpoints
	}

	if limits.MaxVTXOMatches == 0 {
		limits.MaxVTXOMatches = defaults.MaxVTXOMatches
	}

	if limits.MaxMailboxItems == 0 {
		limits.MaxMailboxItems = defaults.MaxMailboxItems
	}

	if limits.MaxMailboxScriptBytes == 0 {
		limits.MaxMailboxScriptBytes = defaults.MaxMailboxScriptBytes
	}

	if limits.MaxConcurrentIncomingSessions == 0 {
		limits.MaxConcurrentIncomingSessions =
			defaults.MaxConcurrentIncomingSessions
	}

	return limits
}

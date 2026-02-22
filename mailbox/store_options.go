package mailbox

import "time"

const (
	// defaultPullPollInterval is the SQLStore poll interval used to emulate
	// long-poll semantics when no notification channel exists.
	defaultPullPollInterval = 25 * time.Millisecond
)

// storeConfig defines optional Store behavior that is not part of the public
// Store interface but is useful for production tuning and safety limits.
type storeConfig struct {
	pullPollInterval time.Duration

	maxEnvelopeBytes       int
	maxEnvelopesPerMailbox int
}

// StoreOption is an option that modifies store behavior.
type StoreOption func(*storeConfig)

// defaultStoreConfig returns the default store configuration.
func defaultStoreConfig() storeConfig {
	return storeConfig{
		pullPollInterval: defaultPullPollInterval,
	}
}

// WithPullPollInterval sets the polling interval used by SQL-backed Pull when
// there are no envelopes available.
func WithPullPollInterval(d time.Duration) StoreOption {
	return func(cfg *storeConfig) {
		cfg.pullPollInterval = d
	}
}

// WithMaxEnvelopeBytes sets the maximum protobuf-encoded size of an Envelope.
//
// A value of 0 disables the limit.
func WithMaxEnvelopeBytes(maxBytes int) StoreOption {
	return func(cfg *storeConfig) {
		cfg.maxEnvelopeBytes = maxBytes
	}
}

// WithMaxEnvelopesPerMailbox sets the maximum number of envelopes that can be
// stored for a single mailbox at once.
//
// Since AckUpTo is expected to delete acked envelopes, this effectively caps
// the number of outstanding (unacked) envelopes.
//
// A value of 0 disables the limit.
func WithMaxEnvelopesPerMailbox(maxEnvelopes int) StoreOption {
	return func(cfg *storeConfig) {
		cfg.maxEnvelopesPerMailbox = maxEnvelopes
	}
}

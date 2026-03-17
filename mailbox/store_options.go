package mailbox

import (
	"time"

	"github.com/btcsuite/btclog/v2"
)

const (
	// defaultPullPollInterval is the SQLStore poll interval used to emulate
	// long-poll semantics when no notification channel exists.
	defaultPullPollInterval = 25 * time.Millisecond
)

// StoreConfig defines optional Store behavior that is not part of the
// public Store interface but is useful for production tuning and safety
// limits.
type StoreConfig struct {
	// PullPollInterval is the polling interval used by SQL-backed
	// Pull when no envelopes are available.
	PullPollInterval time.Duration

	// MaxEnvelopeBytes is the maximum protobuf-encoded size of an
	// Envelope. A value of 0 disables the limit.
	MaxEnvelopeBytes int

	// MaxEnvelopesPerMailbox caps the number of outstanding
	// (unacked) envelopes per mailbox. A value of 0 disables.
	MaxEnvelopesPerMailbox int

	// Log is the logger used by the store.
	Log btclog.Logger
}

// storeConfig is a type alias for backward compatibility within the
// mailbox package.
type storeConfig = StoreConfig

// StoreOption is an option that modifies store behavior.
type StoreOption func(*StoreConfig)

// DefaultStoreConfig returns the default store configuration.
func DefaultStoreConfig() StoreConfig {
	return StoreConfig{
		PullPollInterval: defaultPullPollInterval,
		Log:              btclog.Disabled,
	}
}

// defaultStoreConfig is an alias for backward compatibility.
func defaultStoreConfig() StoreConfig {
	return DefaultStoreConfig()
}

// WithPullPollInterval sets the polling interval used by SQL-backed
// Pull when there are no envelopes available.
func WithPullPollInterval(d time.Duration) StoreOption {
	return func(cfg *StoreConfig) {
		cfg.PullPollInterval = d
	}
}

// WithMaxEnvelopeBytes sets the maximum protobuf-encoded size of an
// Envelope.
//
// A value of 0 disables the limit.
func WithMaxEnvelopeBytes(maxBytes int) StoreOption {
	return func(cfg *StoreConfig) {
		cfg.MaxEnvelopeBytes = maxBytes
	}
}

// WithMaxEnvelopesPerMailbox sets the maximum number of envelopes that
// can be stored for a single mailbox at once.
//
// Since AckUpTo is expected to delete acked envelopes, this effectively
// caps the number of outstanding (unacked) envelopes.
//
// A value of 0 disables the limit.
func WithMaxEnvelopesPerMailbox(maxEnvelopes int) StoreOption {
	return func(cfg *StoreConfig) {
		cfg.MaxEnvelopesPerMailbox = maxEnvelopes
	}
}

// WithLogger sets the logger used by the store.
func WithLogger(log btclog.Logger) StoreOption {
	return func(cfg *StoreConfig) {
		if log != nil {
			cfg.Log = log
		}
	}
}

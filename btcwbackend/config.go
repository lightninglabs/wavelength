package btcwbackend

import (
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/walletcore"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// DefaultFeeMinUpdateTimeout is the default minimum interval between
// fee estimation API queries.
const DefaultFeeMinUpdateTimeout = 5 * time.Minute

// DefaultFeeMaxUpdateTimeout is the default maximum interval between
// fee estimation API queries.
const DefaultFeeMaxUpdateTimeout = 20 * time.Minute

// Config holds the configuration for the neutrino-backed wallet.
// It embeds walletcore.Config for shared base fields (Seed,
// ChainParams, RecoveryWindow, DBDir, Log).
type Config struct {
	// Config provides shared base configuration.
	walletcore.Config

	// NeutrinoDataDir is the directory for neutrino's chain data
	// (headers, cfilters). Defaults to DBDir if empty.
	NeutrinoDataDir string

	// ConnectPeers is a list of host:port addresses to connect to
	// exclusively. When set, neutrino will NOT use DNS seeding and
	// will only connect to these peers.
	ConnectPeers []string

	// AddPeers is a list of additional persistent peers. Unlike
	// ConnectPeers, DNS seeding still runs when AddPeers is set.
	AddPeers []string

	// FeeURL is the URL for the fee estimation API endpoint. The
	// endpoint must return JSON in the format expected by
	// chainfee.SparseConfFeeSource. Required for btcwallet mode.
	FeeURL string

	// FeeMinUpdateTimeout is the minimum interval between fee
	// estimation API queries. Defaults to DefaultFeeMinUpdateTimeout.
	FeeMinUpdateTimeout time.Duration

	// FeeMaxUpdateTimeout is the maximum interval between fee
	// estimation API queries. Defaults to DefaultFeeMaxUpdateTimeout.
	FeeMaxUpdateTimeout time.Duration

	// PersistFilters controls whether neutrino writes compact block
	// filters to disk in addition to the in-memory cache. Useful
	// for wallets that perform frequent rescans.
	PersistFilters bool
}

// WithLogger returns a new config with the given logger set.
func (c Config) WithLogger(log btclog.Logger) Config {
	c.Log = fn.Some(log)

	return c
}

// neutrinoDataDir returns the configured neutrino data directory,
// falling back to DBDir if not explicitly set.
func (c Config) neutrinoDataDir() string {
	if c.NeutrinoDataDir != "" {
		return c.NeutrinoDataDir
	}

	return c.DBDir
}

// feeMinTimeout returns the configured minimum fee update timeout,
// falling back to the default.
func (c Config) feeMinTimeout() time.Duration {
	if c.FeeMinUpdateTimeout > 0 {
		return c.FeeMinUpdateTimeout
	}

	return DefaultFeeMinUpdateTimeout
}

// feeMaxTimeout returns the configured maximum fee update timeout,
// falling back to the default.
func (c Config) feeMaxTimeout() time.Duration {
	if c.FeeMaxUpdateTimeout > 0 {
		return c.FeeMaxUpdateTimeout
	}

	return DefaultFeeMaxUpdateTimeout
}

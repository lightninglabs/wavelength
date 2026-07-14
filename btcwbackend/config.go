//go:build !js || !wasm

package btcwbackend

import (
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/chainbackends"
	"github.com/lightninglabs/wavelength/walletcore"
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

	// BlockHeadersSource is a local file path or HTTP(S) URL that
	// neutrino imports block headers from before falling back to P2P
	// sync. It must be set together with FilterHeadersSource.
	BlockHeadersSource string

	// FilterHeadersSource is a local file path or HTTP(S) URL that
	// neutrino imports compact filter headers from before falling back
	// to P2P sync. It must be set together with BlockHeadersSource.
	FilterHeadersSource string

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

	// PackageSubmitter optionally provides direct bitcoind-backed
	// package relay for v3 parent+child packages. Neutrino can broadcast
	// individual transactions over P2P, but it cannot atomically submit a
	// package whose parent is non-relayable on its own.
	PackageSubmitter chainbackends.PackageSubmitter

	// PersistFilters controls whether neutrino writes compact block
	// filters to disk in addition to the in-memory cache. Useful
	// for wallets that perform frequent rescans.
	PersistFilters bool

	// DisableGlobalLoggers skips wiring neutrino and btcwallet package
	// globals to this wallet logger. Leave this false for normal daemon
	// processes; set it for parallel in-process tests that collect logs
	// into per-test artifacts.
	//
	// This only affects the NeutrinoService created by New. Callers using
	// NewWithNeutrino must configure logger wiring when constructing the
	// supplied NeutrinoService.
	DisableGlobalLoggers bool
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

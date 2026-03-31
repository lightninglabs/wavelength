package btcwbackend

import (
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// coinTypeForNet returns the BIP44 coin type for the given network.
// Mainnet uses coin type 0, while all test networks use coin type 1.
func coinTypeForNet(params *chaincfg.Params) uint32 {
	switch params.Net {
	case chaincfg.MainNetParams.Net:
		return 0

	default:
		return 1
	}
}

// DefaultFeeMinUpdateTimeout is the default minimum interval between
// fee estimation API queries.
const DefaultFeeMinUpdateTimeout = 5 * time.Minute

// DefaultFeeMaxUpdateTimeout is the default maximum interval between
// fee estimation API queries.
const DefaultFeeMaxUpdateTimeout = 20 * time.Minute

// Config holds the configuration for the neutrino-backed wallet.
type Config struct {
	// Seed is the 32-byte master seed used for HD key derivation. The
	// caller is responsible for seed generation, encryption at rest,
	// and BIP39 mnemonic handling. The wallet only uses the raw seed
	// bytes.
	Seed [32]byte

	// ChainParams identifies the Bitcoin network (mainnet, testnet,
	// regtest). Used for address encoding and HD derivation paths.
	ChainParams *chaincfg.Params

	// RecoveryWindow specifies the address look-ahead for discovering
	// used addresses during wallet recovery or restart. A value of 0
	// means no recovery is performed. Typical value: 100 for restart
	// scenarios where previously derived keys must be rediscovered.
	RecoveryWindow uint32

	// DBDir is the directory for btcwallet's bbolt database. The
	// caller owns the lifecycle of this directory.
	DBDir string

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
	// chainfee.SparseConfFeeSource. Required on mainnet.
	FeeURL string

	// FeeMinUpdateTimeout is the minimum interval between fee
	// estimation API queries. Defaults to DefaultFeeMinUpdateTimeout.
	FeeMinUpdateTimeout time.Duration

	// FeeMaxUpdateTimeout is the maximum interval between fee
	// estimation API queries. Defaults to DefaultFeeMaxUpdateTimeout.
	FeeMaxUpdateTimeout time.Duration

	// PersistFilters controls whether neutrino writes compact block
	// filters to disk in addition to the in-memory cache. Useful for
	// wallets that perform frequent rescans.
	PersistFilters bool

	// Log is an optional logger for the wallet and all its
	// sub-components. If None, the wallet falls back to
	// btclog.Disabled.
	Log fn.Option[btclog.Logger]
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

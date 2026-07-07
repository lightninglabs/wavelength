package lwwallet

import (
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// NOTE: coinTypeForNet, PublicWalletPassphrase, and
// DefaultBlockCacheSize are now in the walletcore package.

// Config holds the configuration for the lightweight wallet.
type Config struct {
	// Seed is the raw master seed used for HD key derivation when
	// creating a new wallet database. It must be exactly
	// walletcore.SeedLen bytes when set. A nil Seed opens an
	// existing wallet database instead. The caller is responsible
	// for seed generation and mnemonic handling; the wallet never
	// persists the seed outside btcwallet's own encrypted key
	// store.
	Seed []byte

	// WalletPassword is the private passphrase that encrypts the
	// wallet database's key material (btcwallet's PrivatePass). It
	// is required both when creating a new wallet and when opening
	// an existing one; opening fails when it does not match the
	// passphrase the database was created with.
	WalletPassword []byte

	// Birthday is the time the wallet seed was created. When set, btcwallet
	// uses it to bound recovery rescans instead of starting from genesis.
	Birthday time.Time

	// EsploraURL is the base URL of the Esplora/mempool.space REST API
	// (e.g. "https://mempool.space/api" or "http://localhost:3000"). The
	// wallet uses this for all chain data: blocks, transactions, UTXOs,
	// fee estimates, and broadcasting.
	EsploraURL string

	// ChainParams identifies the Bitcoin network (mainnet, testnet,
	// testnet4, regtest). Used for address encoding and HD derivation
	// paths.
	ChainParams *chaincfg.Params

	// PollInterval controls how frequently the chain backend polls the
	// Esplora API for new blocks and registration updates. Shorter
	// intervals improve responsiveness at the cost of more API requests.
	// Typical values: 1s for regtest, 10s for mainnet.
	PollInterval time.Duration

	// RecoveryWindow specifies the address look-ahead for discovering
	// used addresses during wallet recovery or restart. A value of 0
	// means no recovery is performed. Typical value: 100 for restart
	// scenarios where previously derived keys must be rediscovered.
	RecoveryWindow uint32

	// DBDir is the directory for btcwallet's bbolt database. The
	// caller owns the lifecycle of this directory: for tests a temp
	// directory can be created and cleaned up after the wallet stops,
	// while production callers may use a persistent path.
	DBDir string

	// Log is an optional logger for the wallet and all its sub-components
	// (chain service, chain backend, boarding backend, Esplora client). If
	// None, the wallet falls back to extracting a logger from context via
	// build.LoggerFromContext, or uses btclog.Disabled if no logger is
	// found.
	Log fn.Option[btclog.Logger]
}

// WithLogger returns a new config with the given logger set.
func (c Config) WithLogger(log btclog.Logger) Config {
	c.Log = fn.Some(log)

	return c
}

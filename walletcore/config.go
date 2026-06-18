package walletcore

import (
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// DefaultBlockCacheSize is the number of blocks to cache in memory.
// This prevents redundant block fetches during wallet sync.
const DefaultBlockCacheSize uint64 = 20

// WalletPassphrase is the default passphrase for the wallet DB.
// Both lwwallet and btcwbackend use this for btcwallet's
// PrivatePass and PublicPass.
var WalletPassphrase = []byte("lwwallet")

// CoinTypeForNet returns the BIP44 coin type for the given network.
// Mainnet uses coin type 0, while all test networks use coin type 1.
func CoinTypeForNet(params *chaincfg.Params) uint32 {
	switch params.Net {
	case chaincfg.MainNetParams.Net:
		return 0

	default:
		return 1
	}
}

// Config holds the base configuration shared by all wallet backends
// that wrap btcwallet.
type Config struct {
	// Seed is the 32-byte master seed used for HD key derivation.
	// The caller is responsible for seed generation, encryption at
	// rest, and BIP39 mnemonic handling. The wallet only uses the
	// raw seed bytes.
	Seed [32]byte

	// Birthday is the time the wallet seed was created. When set, btcwallet
	// uses it to bound recovery rescans instead of starting from genesis.
	Birthday time.Time

	// ChainParams identifies the Bitcoin network (mainnet, testnet,
	// regtest). Used for address encoding and HD derivation paths.
	ChainParams *chaincfg.Params

	// RecoveryWindow specifies the address look-ahead for
	// discovering used addresses during wallet recovery or restart.
	// A value of 0 means no recovery is performed. Typical value:
	// 100 for restart scenarios where previously derived keys must
	// be rediscovered.
	RecoveryWindow uint32

	// DBDir is the directory for btcwallet's bbolt database. The
	// caller owns the lifecycle of this directory.
	DBDir string

	// Log is an optional logger for the wallet and all its
	// sub-components. If None, the wallet falls back to
	// btclog.Disabled.
	Log fn.Option[btclog.Logger]
}

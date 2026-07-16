package lwwallet

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

const (
	// DefaultEsploraURLMainnet is the public mempool.space
	// Esplora-compatible REST API for mainnet.
	DefaultEsploraURLMainnet = "https://mempool.space/api"

	// DefaultEsploraURLTestnet3 is the Esplora-compatible REST API for
	// testnet3, backed by a Lightning Labs-operated mempool instance.
	DefaultEsploraURLTestnet3 = "https://mempool-testnet3.testnet." +
		"lightningcluster.com/api"

	// DefaultEsploraURLTestnet4 is the Esplora-compatible REST API for
	// testnet4, backed by a Lightning Labs-operated mempool instance.
	DefaultEsploraURLTestnet4 = "https://mempool-testnet4.testnet." +
		"lightningcluster.com/api"

	// DefaultEsploraURLSignet is the Esplora-compatible REST API for
	// signet, backed by a Lightning Labs-operated mempool instance.
	DefaultEsploraURLSignet = "https://mempool-signet.testnet." +
		"lightningcluster.com/api"
)

// DefaultEsploraURL returns the default Esplora-compatible REST API endpoint
// for params, or an error if the network has no default endpoint.
func DefaultEsploraURL(params *chaincfg.Params) (string, error) {
	if params == nil {
		return "", fmt.Errorf("chain params are required")
	}

	switch {
	case params.Net == wire.MainNet:
		return DefaultEsploraURLMainnet, nil

	case params.Net == wire.TestNet3:
		return DefaultEsploraURLTestnet3, nil

	case params.Net == wire.TestNet4:
		return DefaultEsploraURLTestnet4, nil

	case params.Name == chaincfg.SigNetParams.Name:
		return DefaultEsploraURLSignet, nil

	default:
		return "", fmt.Errorf("no default esplora URL for network %q",
			params.Name)
	}
}

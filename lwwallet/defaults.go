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

	// DefaultEsploraURLTestnet3 is the public mempool.space
	// Esplora-compatible REST API for testnet3.
	DefaultEsploraURLTestnet3 = "https://mempool.space/testnet/api"

	// DefaultEsploraURLTestnet4 is the public mempool.space
	// Esplora-compatible REST API for testnet4.
	DefaultEsploraURLTestnet4 = "https://mempool.space/testnet4/api"

	// DefaultEsploraURLSignet is the public mempool.space
	// Esplora-compatible REST API for signet.
	DefaultEsploraURLSignet = "https://mempool.space/signet/api"
)

// DefaultEsploraURL returns the public mempool.space Esplora-compatible REST
// API endpoint for params, or an error if the network has no public
// endpoint.
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

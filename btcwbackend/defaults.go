package btcwbackend

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

const (
	// DefaultFeeURLMainnet is the default chainfee.SparseConfFeeSource
	// endpoint for mainnet.
	DefaultFeeURLMainnet = "https://nodes.lightning.computer/fees/v1/" +
		"btc-fee-estimates.json"

	// DefaultFeeURLTestnet is the default chainfee.SparseConfFeeSource
	// endpoint shared by testnet3, testnet4, and signet.
	DefaultFeeURLTestnet = "https://nodes.lightning.computer/fees/v1/" +
		"btctestnet-fee-estimates.json"
)

// DefaultFeeURL returns the default chainfee.SparseConfFeeSource endpoint
// for params, or an error if the network has no public endpoint.
func DefaultFeeURL(params *chaincfg.Params) (string, error) {
	if params == nil {
		return "", fmt.Errorf("chain params are required")
	}

	switch {
	case params.Net == wire.MainNet:
		return DefaultFeeURLMainnet, nil

	case params.Net == wire.TestNet3, params.Net == wire.TestNet4,
		params.Name == chaincfg.SigNetParams.Name:
		return DefaultFeeURLTestnet, nil

	default:
		return "", fmt.Errorf("no default fee URL for network %q",
			params.Name)
	}
}

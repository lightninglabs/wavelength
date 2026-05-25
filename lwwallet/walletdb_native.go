//go:build !js || !wasm

package lwwallet

import (
	"time"

	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
)

// newWalletLoaderOptions returns the native btcwallet bbolt loader options.
func newWalletLoaderOptions(cfg Config) ([]btcwallet.LoaderOption, error) {
	return []btcwallet.LoaderOption{
		btcwallet.LoaderWithLocalWalletDB(
			cfg.DBDir, false, 60*time.Second,
		),
	}, nil
}

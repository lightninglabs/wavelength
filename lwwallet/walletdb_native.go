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

// walletExists reports whether a btcwallet bbolt database already
// exists in the configured directory. For local databases the loader
// only checks file existence, so this probe does not take the bbolt
// file lock.
func walletExists(cfg Config) (bool, error) {
	opts, err := newWalletLoaderOptions(cfg)
	if err != nil {
		return false, err
	}

	loader, err := btcwallet.NewWalletLoader(
		cfg.ChainParams, cfg.RecoveryWindow, opts...,
	)
	if err != nil {
		return false, err
	}

	return loader.WalletExists()
}

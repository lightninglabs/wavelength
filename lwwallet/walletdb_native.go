//go:build !js || !wasm

package lwwallet

import (
	"time"

	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
)

// newWalletLoaderOptions returns the native btcwallet bbolt loader
// options. The cleanup func is a no-op: the local database is opened
// (and closed on failure) by btcwallet's own loader.
func newWalletLoaderOptions(cfg Config) ([]btcwallet.LoaderOption, func(),
	error) {

	return []btcwallet.LoaderOption{
		btcwallet.LoaderWithLocalWalletDB(
			cfg.DBDir, false, 60*time.Second,
		),
	}, func() {}, nil
}

// walletExists reports whether a btcwallet bbolt database already
// exists in the configured directory. For local databases the loader
// only checks file existence, so this probe does not take the bbolt
// file lock.
func walletExists(cfg Config) (bool, error) {
	opts, _, err := newWalletLoaderOptions(cfg)
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

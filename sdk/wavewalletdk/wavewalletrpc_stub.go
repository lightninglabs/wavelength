//go:build !wavewalletrpc || !swapruntime

package wavewalletdk

import (
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/waved"
)

// configureWalletRPC keeps non-wallet builds compiling.
func configureWalletRPC(_ *waved.Config, _ bool) {
}

// walletRPCAvailable reports that this build omits the wallet RPC subserver.
func walletRPCAvailable() bool {
	return false
}

// entryFromCanonicalActivity rejects builds without the canonical wallet view.
func entryFromCanonicalActivity(_ sqlc.ActivityEntry) (Entry, error) {
	return Entry{}, ErrWalletRPCUnavailable
}

//go:build wavewalletrpc && swapruntime

package wavewalletdk

import (
	"github.com/lightninglabs/wavelength/swapwallet"
	"github.com/lightninglabs/wavelength/waved"
)

// configureWalletRPC registers the wallet RPC subserver when this package is
// built with the wallet runtime tags.
func configureWalletRPC(cfg *waved.Config, enabled bool) {
	if !enabled {
		return
	}
	if cfg.Swap == nil {
		cfg.Swap = &waved.SwapConfig{}
	}
	cfg.Swap.SuppressResume = true
	cfg.RPCServiceRegistrars = append(
		cfg.RPCServiceRegistrars, swapwallet.Register,
	)

	// Map wavewalletrpc sentinel errors to machine-readable status codes so
	// the embedded client's reconstruct interceptor can surface typed
	// failures, matching the standalone daemon's behavior.
	cfg.UnaryServerInterceptors = append(
		cfg.UnaryServerInterceptors, swapwallet.ErrorMappingInterceptor,
	)
}

// walletRPCAvailable reports whether this build can register wavewalletrpc.
func walletRPCAvailable() bool {
	return true
}

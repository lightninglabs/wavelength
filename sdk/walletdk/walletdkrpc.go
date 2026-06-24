//go:build walletdkrpc && swapruntime && !js

package walletdk

import (
	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/lightninglabs/darepo-client/swapwallet"
)

// configureWalletRPC registers the wallet RPC subserver when this package is
// built with the wallet runtime tags.
func configureWalletRPC(cfg *darepod.Config, enabled bool) {
	if !enabled {
		return
	}
	if cfg.Swap == nil {
		cfg.Swap = &darepod.SwapConfig{}
	}
	cfg.Swap.SuppressResume = true
	cfg.RPCServiceRegistrars = append(
		cfg.RPCServiceRegistrars, swapwallet.Register,
	)

	// Map walletdkrpc sentinel errors to machine-readable status codes so
	// the embedded client's reconstruct interceptor can surface typed
	// failures, matching the standalone daemon's behavior.
	cfg.UnaryServerInterceptors = append(
		cfg.UnaryServerInterceptors, swapwallet.ErrorMappingInterceptor,
	)
}

// walletRPCAvailable reports whether this build can register walletdkrpc.
func walletRPCAvailable() bool {
	return true
}

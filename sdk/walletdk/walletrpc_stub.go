//go:build (!walletrpc || !swapruntime) && !js

package walletdk

import "github.com/lightninglabs/darepo-client/darepod"

// configureWalletRPC keeps non-wallet builds compiling.
func configureWalletRPC(_ *darepod.Config, _ bool) {
}

// walletRPCAvailable reports that this build omits the wallet RPC subserver.
func walletRPCAvailable() bool {
	return false
}

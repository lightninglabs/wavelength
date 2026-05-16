//go:build !swapruntime && !js

package walletdk

import "github.com/lightninglabs/darepo-client/darepod"

// configureSwapRuntime keeps default builds compiling.
func configureSwapRuntime(_ *darepod.Config, enabled bool) error {
	return nil
}

// swapRuntimeAvailable reports that default builds omit the swap executor.
func swapRuntimeAvailable() bool {
	return false
}

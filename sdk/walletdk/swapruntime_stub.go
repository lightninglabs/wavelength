//go:build !swapruntime

package walletdk

import "github.com/lightninglabs/darepo-client/darepod"

// configureSwapRuntime keeps default builds compiling. Swap methods return
// ErrSwapRuntimeUnavailable when the executor is absent.
func configureSwapRuntime(_ *darepod.Config, enabled bool) error {
	return nil
}

// swapRuntimeAvailable reports that default builds omit the swap executor.
func swapRuntimeAvailable() bool {
	return false
}

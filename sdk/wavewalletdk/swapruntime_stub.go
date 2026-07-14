//go:build !swapruntime

package wavewalletdk

import "github.com/lightninglabs/wavelength/waved"

// configureSwapRuntime keeps default builds compiling.
func configureSwapRuntime(_ *waved.Config, enabled bool) error {
	return nil
}

// swapRuntimeAvailable reports that default builds omit the swap executor.
func swapRuntimeAvailable() bool {
	return false
}

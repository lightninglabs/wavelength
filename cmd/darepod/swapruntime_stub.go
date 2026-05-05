//go:build !swapruntime

package main

import "github.com/lightninglabs/darepo-client/darepod"

// configureSwapRuntime is the default-build no-op for optional swap execution.
// It keeps the daemon config surface stable while ensuring non-swapruntime
// binaries do not import or register the swap client subserver.
func configureSwapRuntime(*darepod.Config) {
}

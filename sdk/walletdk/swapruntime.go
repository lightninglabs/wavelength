//go:build swapruntime

package walletdk

import (
	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/lightninglabs/darepo-client/swapclientserver"
)

// configureSwapRuntime registers the daemon-owned swap executor when the
// package is built with the swapruntime tag.
func configureSwapRuntime(cfg *darepod.Config, enabled bool) error {
	if !enabled {
		return nil
	}

	cfg.RPCServiceRegistrars = append(
		cfg.RPCServiceRegistrars, swapclientserver.Register,
	)

	return nil
}

// swapRuntimeAvailable reports whether this build can register swap RPCs.
func swapRuntimeAvailable() bool {
	return true
}

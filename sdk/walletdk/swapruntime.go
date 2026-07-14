//go:build swapruntime

package walletdk

import (
	"github.com/lightninglabs/wavelength/swapclientserver"
	"github.com/lightninglabs/wavelength/waved"
)

// configureSwapRuntime registers the daemon-owned swap executor when the
// package is built with the swapruntime tag.
//
// The build-tag split exists so default walletdk builds do not pull in the
// swap executor's dependency graph (sdk/swaps, swap store, swapdk-server
// transport). Hosts that ship Lightning swap support opt in via -tags
// swapruntime; the stub file in swapruntime_stub.go is the negative half of
// the same contract and keeps the public Client method set identical across
// both build flavors.
func configureSwapRuntime(cfg *waved.Config, enabled bool) error {
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

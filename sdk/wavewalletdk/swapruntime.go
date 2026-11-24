//go:build swapruntime

package wavewalletdk

import (
	"fmt"
	"runtime"

	"github.com/lightninglabs/wavelength/swapclientserver"
	"github.com/lightninglabs/wavelength/waved"
)

// configureSwapRuntime registers the daemon-owned swap executor when the
// package is built with the swapruntime tag.
//
// The build-tag split exists so default wavewalletdk builds do not pull in the
// swap executor's dependency graph (sdk/swaps, swap store, swapdk-server
// transport). Hosts that ship Lightning swap support opt in via -tags
// swapruntime; the stub file in swapruntime_stub.go is the negative half of
// the same contract and keeps the public Client method set identical across
// both build flavors.
func configureSwapRuntime(cfg *waved.Config) error {
	if cfg.Swap.ArkChannelsEnabled {
		if cfg.Wallet == nil ||
			cfg.Wallet.Type != waved.WalletTypeLwwallet {
			return fmt.Errorf("ark_channels_enabled requires " +
				"lwwallet")
		}
		if cfg.HostDrivenIngress {
			return fmt.Errorf("ark_channels_enabled is " +
				"incompatible with host_driven_ingress")
		}
		if runtime.GOOS == "js" {
			if cfg.Server == nil ||
				cfg.Server.Transport != waved.RPCTransportREST ||
				cfg.Swap.ServerTransport != waved.RPCTransportREST {
				return fmt.Errorf("ark_channels_enabled on " +
					"WASM requires REST operator and " +
					"swap transports")
			}
			if cfg.Server.TLSCertPath != "" ||
				cfg.Swap.ServerTLSCertPath != "" {
				return fmt.Errorf("browser HTTP cannot pin " +
					"operator or swap TLS certificates")
			}
		}
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

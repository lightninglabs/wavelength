//go:build swapruntime

package main

import (
	"github.com/lightninglabs/wavelength/swapclientserver"
	"github.com/lightninglabs/wavelength/waved"
)

// configureSwapRuntime attaches the optional swap client subserver registrar
// when waved is compiled with the swapruntime tag. The actual service is
// still constructed during daemon startup so it shares the daemon lifecycle and
// gRPC listener.
func configureSwapRuntime(cfg *waved.Config) {
	cfg.RPCServiceRegistrars = append(
		cfg.RPCServiceRegistrars, swapclientserver.Register,
	)
	cfg.RPCGatewayRegistrars = append(
		cfg.RPCGatewayRegistrars, swapclientserver.RegisterGateway,
	)
}

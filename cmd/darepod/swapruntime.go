//go:build swapruntime

package main

import (
	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/lightninglabs/darepo-client/swapclientserver"
)

// configureSwapRuntime attaches the optional swap client subserver registrar
// when darepod is compiled with the swapruntime tag. The actual service is
// still constructed during daemon startup so it shares the daemon lifecycle and
// gRPC listener.
func configureSwapRuntime(cfg *darepod.Config) {
	cfg.RPCServiceRegistrars = append(
		cfg.RPCServiceRegistrars, swapclientserver.Register,
	)
}

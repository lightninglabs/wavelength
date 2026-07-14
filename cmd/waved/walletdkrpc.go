//go:build walletdkrpc && swapruntime

package main

import (
	"github.com/lightninglabs/wavelength/swapwallet"
	"github.com/lightninglabs/wavelength/waved"
)

// configureWalletRPC attaches the optional walletdkrpc subserver registrar
// when waved is compiled with the walletdkrpc build tag. The tag requires
// swapruntime because swapwallet composes the daemon-owned swap subsystem;
// the build constraint above enforces this at compile time.
//
// The registrar runs AFTER configureSwapRuntime so the swap subserver has
// already published its in-Go backend handle on cfg.Swap.Backend by the
// time swapwallet.Register reads it. The same ordering tells the swap
// subserver to skip its own synchronous resume sweep because the wallet
// layer will drive a unified resume that may also touch wallet-managed
// pending tables.
func configureWalletRPC(cfg *waved.Config) {
	if cfg.Swap == nil {
		cfg.Swap = &waved.SwapConfig{}
	}
	cfg.Swap.SuppressResume = true

	cfg.RPCServiceRegistrars = append(
		cfg.RPCServiceRegistrars, swapwallet.Register,
	)
	cfg.RPCGatewayRegistrars = append(
		cfg.RPCGatewayRegistrars, swapwallet.RegisterGateway,
	)

	// Map walletdkrpc sentinel errors to machine-readable status codes so
	// clients can branch on failure cause without string matching.
	cfg.UnaryServerInterceptors = append(
		cfg.UnaryServerInterceptors, swapwallet.ErrorMappingInterceptor,
	)
}

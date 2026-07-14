//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"fmt"
	"sync"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waved"
	"google.golang.org/grpc"
)

// Register installs the wavewalletrpc subserver on the daemon's gRPC server. It
// is wired into cfg.RPCServiceRegistrars by the wavewalletrpc build tag in
// cmd/waved so the daemon only carries the subserver when explicitly
// compiled in.
//
// Register MUST run after swapclientserver.Register has populated
// cfg.Swap.Backend. The wavewalletrpc build tag enforces this through the
// configureWalletRPC wiring in cmd/waved, which appends the swapwallet
// registrar AFTER the swapclientserver registrar.
//
// The function signature matches waved.RPCServiceRegistrar so it can be
// stored alongside other optional subservers. The returned cleanup function
// stops the runtime; it must be invoked during daemon shutdown.
func Register(ctx context.Context, grpcServer *grpc.Server,
	rpcServer *waved.RPCServer, cfg *waved.Config) (func(), error) {

	if cfg == nil || cfg.Swap == nil || cfg.Swap.Backend == nil {

		// Without a wired backend there is nothing to compensate
		// for: the swap subserver hasn't published the handle, so
		// either it never ran (cfg.Swap == nil) or it failed setup
		// itself. The swap subserver's own startup path is
		// responsible for that diagnosis; we only fail closed here.
		return nil, fmt.Errorf("swapwallet: %w (registrar ordering: "+
			"swapclientserver.Register must run before "+
			"swapwallet.Register)", ErrSwapBackendUnavailable)
	}

	// failoverResume compensates for the SuppressResume handshake when
	// this registrar bails out after the swap subserver has already
	// skipped its own resume sweep. Without this, a downstream failure
	// (type assertion mismatch, future wiring errors) would leave the
	// daemon with NO actor driving pending swap workers. Idempotent:
	// the underlying ResumePending is already gated by a per-payment-
	// hash admission map in the swap subserver.
	failoverResume := func() {
		cfg.Swap.Backend.ResumePending(ctx)
	}

	// Pull the gRPC-shaped swap handle from the in-process subserver.
	// *swapClientService satisfies both waved.SwapBackend (which we
	// hold for unified resume) and swapclientrpc.SwapClientServiceServer
	// (which we hold for in-process gRPC dispatch). The type assertion
	// is safe because swapclientserver always publishes the same handle
	// in both slots.
	swapService, ok :=
		cfg.Swap.Backend.(swapclientrpc.SwapClientServiceServer)
	if !ok {
		failoverResume()

		return nil, fmt.Errorf("swapwallet: swap backend does not " +
			"implement swapclientrpc.SwapClientServiceServer")
	}

	var coreRPC RPCServer
	if rpcServer != nil {
		coreRPC = rpcServer
	}

	chainParams, err := chainParamsForWalletNetwork(cfg.Network)
	if err != nil {
		failoverResume()

		return nil, fmt.Errorf("swapwallet: %w", err)
	}

	deps := &Deps{
		SwapBackend:    cfg.Swap.Backend,
		SwapService:    swapService,
		RPCServer:      coreRPC,
		CreditRegistry: cfg.Swap.CreditRegistry,
		ChainParams:    chainParams,
		ActivityStore:  cfg.ActivityStore,
	}
	if rpcServer != nil {
		deps.Log = rpcServer.SubLogger(waved.WalletRPCSubsystem)
	}

	// Apply optional wavewalletrpc-config overrides. The struct is present
	// in all builds but the wallet-layer subserver only reads it when
	// wavewalletrpc is compiled in (this file), so unknown-field drift
	// across builds is impossible.
	if cfg.SwapWallet != nil {
		deps.WalletDeadline = cfg.SwapWallet.Deadline
		deps.DefaultListLimit = cfg.SwapWallet.DefaultListLimit
		deps.MaxListLimit = cfg.SwapWallet.MaxListLimit
		deps.SubscribeBuffer = cfg.SwapWallet.SubscribeBuffer
	}

	runtime := newRuntime(ctx, deps)
	service := newService(deps, runtime)
	inspectionService := newInspectionService(deps, runtime)

	// Wire the prepared-send store into the credit auto-redeem interlock
	// now that the service (and its store) exists, so the sweep subtracts
	// credits a pending credit-backed send has earmarked before redeeming.
	if cfg.Swap.CreditEarmarkSetter != nil {
		cfg.Swap.CreditEarmarkSetter(service.earmarkedCreditSat)
	}

	wavewalletrpc.RegisterWalletServiceServer(grpcServer, service)
	wavewalletrpc.RegisterWalletInspectionServiceServer(
		grpcServer, inspectionService,
	)

	var startOnce sync.Once
	cfg.WalletReadyHooks = append(cfg.WalletReadyHooks, func(
		ctx context.Context) error {

		startOnce.Do(func() {
			// The unified resume sweep must wait until the main
			// daemon wallet and wallet-dependent actors are ready.
			// Otherwise a locked self-managed wallet restart can
			// resume workers early and persist transient unlock
			// errors as terminal failures.
			runtime.resumeAll(ctx)

			// Start background goroutines after resume so
			// subscribers observe a runtime that owns any
			// wallet-local pending rows it just restored.
			runtime.start()
		})

		return nil
	})

	cleanup := func() {
		runtime.stop()
	}

	return cleanup, nil
}

// RegisterGateway installs the optional WalletService handlers on the daemon
// HTTP/JSON gateway.
func RegisterGateway(ctx context.Context, mux *runtime.ServeMux,
	endpoint string, opts []grpc.DialOption, _ *waved.RPCServer,
	_ *waved.Config) error {

	if err := wavewalletrpc.RegisterWalletServiceHandlerFromEndpoint(
		ctx, mux, endpoint, opts,
	); err != nil {
		return err
	}

	return wavewalletrpc.RegisterWalletInspectionServiceHandlerFromEndpoint(
		ctx, mux, endpoint, opts,
	)
}

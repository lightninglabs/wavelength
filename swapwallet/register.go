//go:build walletrpc && swapruntime

package swapwallet

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"google.golang.org/grpc"
)

// Register installs the walletrpc subserver on the daemon's gRPC server. It
// is wired into cfg.RPCServiceRegistrars by the walletrpc build tag in
// cmd/darepod so the daemon only carries the subserver when explicitly
// compiled in.
//
// Register MUST run after swapclientserver.Register has populated
// cfg.Swap.Backend. The walletrpc build tag enforces this through the
// configureWalletRPC wiring in cmd/darepod, which appends the swapwallet
// registrar AFTER the swapclientserver registrar.
//
// The function signature matches darepod.RPCServiceRegistrar so it can be
// stored alongside other optional subservers. The returned cleanup function
// stops the runtime; it must be invoked during daemon shutdown.
func Register(ctx context.Context, grpcServer *grpc.Server,
	rpcServer *darepod.RPCServer, cfg *darepod.Config) (func(), error) {

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
	// *swapClientService satisfies both darepod.SwapBackend (which we
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

	deps := &Deps{
		SwapBackend: cfg.Swap.Backend,
		SwapService: swapService,
		RPCServer:   rpcServer,
		Log:         rpcServer.SubLogger(darepod.WalletRPCSubsystem),
	}

	// Apply optional walletrpc-config overrides. The struct is present
	// in all builds but the wallet-layer subserver only reads it when
	// walletrpc is compiled in (this file), so unknown-field drift
	// across builds is impossible.
	if cfg.SwapWallet != nil {
		deps.WalletDeadline = cfg.SwapWallet.Deadline
		deps.DefaultListLimit = cfg.SwapWallet.DefaultListLimit
		deps.MaxListLimit = cfg.SwapWallet.MaxListLimit
		deps.SubscribeBuffer = cfg.SwapWallet.SubscribeBuffer
	}

	runtime := newRuntime(ctx, deps)
	service := newService(deps, runtime)

	walletrpc.RegisterWalletServiceServer(grpcServer, service)

	// The unified resume sweep MUST run before this Register returns so
	// the gRPC server begins accepting wallet RPCs with every pending
	// entry already driven by a background worker.
	runtime.resumeAll(ctx)

	// Start background goroutines (deadline watcher, future monitor
	// loop). They anchor to the runtime's rootCtx and live until the
	// cleanup function below cancels it.
	runtime.start()

	cleanup := func() {
		runtime.stop()
	}

	return cleanup, nil
}

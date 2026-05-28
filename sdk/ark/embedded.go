package ark

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/darepod"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const (
	// defaultBufConnSize is the size of the in-memory listener buffer used
	// by embedded clients that do not provide their own transport.
	defaultBufConnSize = 1 << 20
)

// EmbeddedConfig configures an in-process daemon runtime managed by the SDK.
type EmbeddedConfig struct {
	// DaemonConfig is the full darepod configuration snapshot to clone
	// and run in-process. The SDK currently hides transport and
	// lifecycle management, not the underlying daemon configuration
	// surface.
	DaemonConfig *darepod.Config

	// BufferSize overrides the bufconn listener size used for in-process
	// gRPC traffic. When zero, the SDK uses a sane default.
	BufferSize int

	// DialOptions overrides the default gRPC dial options used against the
	// injected in-memory listener.
	DialOptions []grpc.DialOption
}

// StartEmbedded starts a darepod runtime in-process and returns an SDK facade
// that communicates with it over an injected in-memory listener.
//
//nolint:contextcheck // embedded daemon lifetime is detached from dial ctx
func StartEmbedded(ctx context.Context, cfg EmbeddedConfig) (*Client, error) {
	if cfg.DaemonConfig == nil {
		return nil, fmt.Errorf("daemon config is required")
	}

	daemonCfg := cloneDaemonConfig(cfg.DaemonConfig)
	bufferSize := cfg.BufferSize
	if bufferSize == 0 {
		bufferSize = defaultBufConnSize
	}

	listener := bufconn.Listen(bufferSize)
	if daemonCfg.RPC == nil {
		daemonCfg.RPC = &darepod.RPCConfig{}
	}
	daemonCfg.RPC.Listener = listener

	// Validate normalizes path fields (e.g. tilde expansion) and fills
	// in subsystem defaults that NewServer assumes are present, so the
	// embedded path matches the standalone daemon's contract.
	if err := daemonCfg.Validate(); err != nil {
		_ = listener.Close()

		return nil, fmt.Errorf("validate embedded daemon config: %w",
			err)
	}

	server, err := darepod.NewServer(daemonCfg)
	if err != nil {
		_ = listener.Close()

		return nil, fmt.Errorf("create embedded daemon: %w", err)
	}

	// The embedded daemon has its own lifetime and should keep running
	// after the startup dial context expires, so use a detached root
	// context here.
	runCtx, cancel := context.WithCancel(context.Background())
	runErrChan := make(chan error, 1)
	runDoneChan := make(chan error, 1)
	waitErrChan := make(chan error, 1)
	go func() {
		runErr := server.RunWithContext(runCtx)
		runErrChan <- runErr
		runDoneChan <- runErr
		waitErrChan <- runErr
		close(waitErrChan)
	}()

	dialOpts := append([]grpc.DialOption(nil), cfg.DialOptions...)
	if len(dialOpts) == 0 {
		dialOpts = append(
			dialOpts,
			grpc.WithTransportCredentials(
				insecure.NewCredentials(),
			),
		)
	}

	dialOpts = append(
		dialOpts, grpc.WithContextDialer(func(dialCtx context.Context,
			_ string) (net.Conn, error) {

			return listener.DialContext(dialCtx)
		}),
	)

	conn, err := grpc.NewClient("passthrough:///bufnet", dialOpts...)
	if err != nil {
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(
			context.Background(), defaultCloseTimeout,
		)

		runErr := waitForRunExit(shutdownCtx, runErrChan)
		shutdownCancel()
		_ = listener.Close()

		if runErr != nil {
			return nil, fmt.Errorf("dial embedded daemon: %w "+
				"(daemon exited: %w)", err, runErr)
		}

		return nil, fmt.Errorf("dial embedded daemon: %w", err)
	}

	if err := waitForReady(ctx, conn, runDoneChan); err != nil {
		closeErr := conn.Close()
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(
			context.Background(), defaultCloseTimeout,
		)

		runErr := waitForRunExit(shutdownCtx, runErrChan)
		shutdownCancel()
		listenerErr := listener.Close()

		return nil, fmt.Errorf("wait for embedded daemon readiness: %w",
			errors.Join(err, closeErr, runErr, listenerErr))
	}

	return &Client{
		daemon: daemonrpc.NewDaemonServiceClient(conn),
		waitCh: waitErrChan,
		closeFn: func(closeCtx context.Context) error {
			closeErr := conn.Close()
			cancel()
			runErr := waitForRunExit(closeCtx, runErrChan)
			listenerErr := listener.Close()

			return errors.Join(closeErr, runErr, listenerErr)
		},
	}, nil
}

// cloneDaemonConfig deep-copies the daemon config so embedded startup can
// inject a listener and run Validate without mutating the caller's config.
// This must be updated if new reference-typed fields are added to
// darepod.Config or to any sub-config reachable from it.
func cloneDaemonConfig(cfg *darepod.Config) *darepod.Config {
	clone := *cfg

	clone.RPCServiceRegistrars = append(
		[]darepod.RPCServiceRegistrar(nil), cfg.RPCServiceRegistrars...,
	)
	clone.RPCGatewayRegistrars = append(
		[]darepod.RPCGatewayRegistrar(nil), cfg.RPCGatewayRegistrars...,
	)
	clone.WalletReadyHooks = append(
		[]darepod.WalletReadyHook(nil), cfg.WalletReadyHooks...,
	)

	if cfg.Lnd != nil {
		lndCfg := *cfg.Lnd
		clone.Lnd = &lndCfg
	}

	if cfg.Server != nil {
		serverCfg := *cfg.Server
		clone.Server = &serverCfg
	}

	if cfg.RPC != nil {
		rpcCfg := *cfg.RPC
		if cfg.RPC.Gateway != nil {
			gw := *cfg.RPC.Gateway
			gw.AllowedOrigins = append(
				[]string(nil),
				cfg.RPC.Gateway.AllowedOrigins...,
			)
			rpcCfg.Gateway = &gw
		}
		clone.RPC = &rpcCfg
	}

	if cfg.Wallet != nil {
		walletCfg := *cfg.Wallet
		walletCfg.BtcwalletPeers = append(
			[]string(nil), cfg.Wallet.BtcwalletPeers...,
		)
		walletCfg.BtcwalletAddPeers = append(
			[]string(nil), cfg.Wallet.BtcwalletAddPeers...,
		)
		clone.Wallet = &walletCfg
	}

	if cfg.Unroll != nil {
		unrollCfg := *cfg.Unroll
		clone.Unroll = &unrollCfg
	}

	if cfg.OOR != nil {
		oorCfg := *cfg.OOR
		if cfg.OOR.Limits != nil {
			limits := *cfg.OOR.Limits
			oorCfg.Limits = &limits
		}
		clone.OOR = &oorCfg
	}

	if cfg.Swap != nil {
		swapCfg := *cfg.Swap
		clone.Swap = &swapCfg
	}

	if cfg.SwapWallet != nil {
		swapWalletCfg := *cfg.SwapWallet
		clone.SwapWallet = &swapWalletCfg
	}

	return &clone
}

// waitForRunExit waits for the embedded daemon run goroutine to exit or for
// the caller's shutdown context to expire.
func waitForRunExit(ctx context.Context, runErrChan <-chan error) error {
	select {
	case runErr := <-runErrChan:
		return runErr

	case <-ctx.Done():
		return fmt.Errorf("wait for embedded daemon shutdown: %w",
			ctx.Err())
	}
}

// waitForReady forces a new client connection to attempt dialing and waits
// until it reaches READY, the embedded daemon exits, or the caller's context
// expires.
func waitForReady(ctx context.Context, conn *grpc.ClientConn,
	runDoneChan <-chan error) error {

	conn.Connect()

	for {
		state := conn.GetState()
		if state == connectivity.Ready {
			return nil
		}

		if state == connectivity.Shutdown {
			return fmt.Errorf("grpc connection shut down before " +
				"readiness")
		}

		waitCtx, waitCancel := context.WithCancel(ctx)
		runExitErr := make(chan error, 1)
		go func() {
			select {
			case runErr := <-runDoneChan:
				if runErr != nil {
					runExitErr <- fmt.Errorf("embedded "+
						"daemon exited before "+
						"readiness: %w", runErr)
				} else {
					runExitErr <- fmt.Errorf("embedded " +
						"daemon exited before " +
						"readiness")
				}

				waitCancel()

			case <-waitCtx.Done():
			}
		}()

		if !conn.WaitForStateChange(waitCtx, state) {
			waitCancel()

			select {
			case err := <-runExitErr:
				return err

			default:
			}

			return fmt.Errorf("wait for grpc readiness: %w",
				ctx.Err())
		}

		waitCancel()
	}
}

package walletdk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightningnetwork/lnd/fn/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const defaultBufConnSize = 1 << 20

// daemonRuntime carries the embedded daemon's terminal signal so the startup,
// shutdown, and Wait paths share one source of truth.
//
// The lifecycle is modeled as a context whose cancellation marks the daemon
// having exited. runErr is written before the context is cancelled so any
// goroutine observing ctx.Done() is then free to read runErr without further
// synchronization (Go memory model: the write happens-before the receive that
// observes the cancellation).
//
// signalExit is sync.Once-guarded so the daemon goroutine, panic recovery,
// and any defensive callers cannot race on the runErr write or accidentally
// trigger ordering issues. context.CancelFunc itself is already idempotent.
//
// ctx here is a lifecycle signal owned by daemonRuntime, not a per-request
// context; storing it lets Wait and waitForReady reuse context.AfterFunc
// without an extra goroutine — hence the containedctx suppression below.
//
//nolint:containedctx
type daemonRuntime struct {
	ctx       context.Context
	cancelFn  context.CancelFunc
	runErr    error
	closeOnce sync.Once
}

// newDaemonRuntime constructs a runtime whose context is cancelled the first
// time signalExit is called.
func newDaemonRuntime() *daemonRuntime {
	ctx, cancel := context.WithCancel(context.Background())

	return &daemonRuntime{
		ctx:      ctx,
		cancelFn: cancel,
	}
}

// signalExit records the daemon's terminal error and cancels the runtime
// context. Safe to call multiple times; only the first call wins.
func (rt *daemonRuntime) signalExit(err error) {
	rt.closeOnce.Do(func() {
		rt.runErr = err
		rt.cancelFn()
	})
}

// Done returns a channel that is closed when the daemon exits. After
// observing the close, callers may read runErr to inspect the terminal error.
func (rt *daemonRuntime) Done() <-chan struct{} {
	return rt.ctx.Done()
}

// DefaultConfig returns a walletdk config with darepod defaults. Swap methods
// are enabled only when the package is built with the swapruntime tag.
func DefaultConfig() Config {
	cfg := darepod.DefaultConfig()

	return Config{
		DataDir:               cfg.DataDir,
		Network:               cfg.Network,
		DebugLevel:            cfg.DebugLevel,
		ServerAddress:         cfg.Server.Host,
		ServerTLSCertPath:     cfg.Server.TLSCertPath,
		ServerInsecure:        fn.Some(cfg.Server.Insecure),
		WalletType:            cfg.Wallet.Type,
		WalletEsploraURL:      cfg.Wallet.EsploraURL,
		WalletPasswordFile:    cfg.Wallet.PasswordFile,
		WalletPollInterval:    cfg.Wallet.PollInterval,
		WalletRecoveryWindow:  cfg.Wallet.RecoveryWindow,
		WalletFeeURL:          cfg.Wallet.FeeURL,
		SwapServerAddress:     cfg.Swap.ServerAddress,
		SwapServerTLSCertPath: cfg.Swap.ServerTLSCertPath,
		SwapServerInsecure:    fn.Some(cfg.Swap.ServerInsecure),
		SwapDatabaseFileName:  cfg.Swap.DatabaseFileName,
		MaxOperatorFeeSat:     cfg.MaxOperatorFeeSat,
	}
}

// Start starts an embedded darepod runtime and returns the wallet facade.
//
//nolint:contextcheck // embedded daemon lifetime is detached from dial ctx
func Start(ctx context.Context, cfg Config) (*Client, error) {
	daemonCfg, err := daemonConfig(cfg)
	if err != nil {
		return nil, err
	}

	swapsEnabled := !cfg.DisableSwaps && swapRuntimeAvailable()
	if err := configureSwapRuntime(daemonCfg, swapsEnabled); err != nil {
		return nil, err
	}

	bufferSize := cfg.BufferSize
	if bufferSize == 0 {
		bufferSize = defaultBufConnSize
	}

	listener := bufconn.Listen(bufferSize)
	if daemonCfg.RPC == nil {
		daemonCfg.RPC = &darepod.RPCConfig{}
	}
	daemonCfg.RPC.Listener = listener

	if err := daemonCfg.Validate(); err != nil {
		_ = listener.Close()

		return nil, fmt.Errorf("invalid daemon config: %w", err)
	}

	server, err := darepod.NewServer(daemonCfg)
	if err != nil {
		_ = listener.Close()

		return nil, fmt.Errorf("create embedded daemon: %w", err)
	}

	// The daemon outlives the caller's dial context: callers pass a ctx
	// that may carry a tight startup deadline, but the daemon's lifetime is
	// tied to runCtx, which is only cancelled by Stop or the failure paths
	// below. That is what the //nolint:contextcheck directive on Start is
	// guarding.
	runCtx, cancel := context.WithCancel(context.Background())

	rt := newDaemonRuntime()
	go func() {
		// Capture any panic from the daemon goroutine into the
		// terminal error so Wait/Stop see a real error rather than
		// blocking forever on an un-cancelled runtime.
		defer func() {
			if r := recover(); r != nil {
				rt.signalExit(
					fmt.Errorf("embedded daemon "+
						"panicked: %v", r),
				)
			}
		}()

		rt.signalExit(server.RunWithContext(runCtx))
	}()

	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(dialCtx context.Context, _ string) (
			net.Conn, error) {

			return listener.DialContext(dialCtx)
		}),
	}

	conn, err := grpc.NewClient("passthrough:///bufnet", dialOpts...)
	if err != nil {
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(
			context.Background(), defaultCloseTimeout,
		)
		runErr := waitForRunExit(shutdownCtx, rt)
		shutdownCancel()
		listenerErr := listener.Close()

		return nil, fmt.Errorf("dial embedded daemon: %w",
			errors.Join(err, runErr, listenerErr))
	}

	if err := waitForReady(ctx, conn, rt); err != nil {
		closeErr := conn.Close()
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(
			context.Background(), defaultCloseTimeout,
		)
		runErr := waitForRunExit(shutdownCtx, rt)
		shutdownCancel()
		listenerErr := listener.Close()

		return nil, fmt.Errorf("wait for embedded daemon readiness: %w",
			errors.Join(err, closeErr, runErr, listenerErr))
	}

	return &Client{
		conn:    conn,
		daemon:  daemonrpc.NewDaemonServiceClient(conn),
		swaps:   swapclientrpc.NewSwapClientServiceClient(conn),
		canSwap: swapsEnabled,
		runtime: rt,
		closeFn: func(closeCtx context.Context) error {
			closeErr := conn.Close()
			cancel()
			runErr := waitForRunExit(closeCtx, rt)
			listenerErr := listener.Close()

			return errors.Join(closeErr, runErr, listenerErr)
		},
	}, nil
}

// daemonConfig builds the daemon config from either a full caller-supplied
// config or the walletdk convenience fields.
func daemonConfig(cfg Config) (*darepod.Config, error) {
	var daemonCfg *darepod.Config
	if cfg.DaemonConfig != nil {
		daemonCfg = cloneDaemonConfig(cfg.DaemonConfig)
	} else {
		daemonCfg = darepod.DefaultConfig()
	}

	applyConfigOverrides(daemonCfg, cfg)

	return daemonCfg, nil
}

// applyConfigOverrides applies only explicitly set convenience fields so a
// caller-provided DaemonConfig keeps ownership of detailed daemon knobs.
//
// The tri-state booleans (AllowMainnet, ServerInsecure, SwapServerInsecure)
// are fn.Option[bool]: fn.None means "no override, defer to DaemonConfig", and
// fn.Some(v) forces that exact value. This lets callers explicitly clear a
// true-by-default flag without having to construct a full DaemonConfig.
func applyConfigOverrides(daemonCfg *darepod.Config, cfg Config) {
	if cfg.DataDir != "" {
		daemonCfg.DataDir = cfg.DataDir
	}
	if cfg.Network != "" {
		daemonCfg.Network = cfg.Network
	}
	if cfg.DebugLevel != "" {
		daemonCfg.DebugLevel = cfg.DebugLevel
	}
	if cfg.LogWriter != nil {
		daemonCfg.LogWriter = cfg.LogWriter
	}
	cfg.AllowMainnet.WhenSome(func(v bool) {
		daemonCfg.AllowMainnet = v
	})
	if cfg.MaxOperatorFeeSat != 0 {
		daemonCfg.MaxOperatorFeeSat = cfg.MaxOperatorFeeSat
	}

	if daemonCfg.Server == nil {
		daemonCfg.Server = &darepod.ServerConfig{}
	}
	if cfg.ServerAddress != "" {
		daemonCfg.Server.Host = cfg.ServerAddress
	}
	if cfg.ServerTLSCertPath != "" {
		daemonCfg.Server.TLSCertPath = cfg.ServerTLSCertPath
	}
	cfg.ServerInsecure.WhenSome(func(v bool) {
		daemonCfg.Server.Insecure = v
	})

	if daemonCfg.Wallet == nil {
		daemonCfg.Wallet = &darepod.WalletConfig{}
	}
	if cfg.WalletType != "" {
		daemonCfg.Wallet.Type = cfg.WalletType
	}
	if cfg.WalletEsploraURL != "" {
		daemonCfg.Wallet.EsploraURL = cfg.WalletEsploraURL
	}
	if cfg.WalletPasswordFile != "" {
		daemonCfg.Wallet.PasswordFile = cfg.WalletPasswordFile
	}
	if cfg.WalletPollInterval != 0 {
		daemonCfg.Wallet.PollInterval = cfg.WalletPollInterval
	}
	if cfg.WalletRecoveryWindow != 0 {
		daemonCfg.Wallet.RecoveryWindow = cfg.WalletRecoveryWindow
	}
	if cfg.WalletFeeURL != "" {
		daemonCfg.Wallet.FeeURL = cfg.WalletFeeURL
	}

	if daemonCfg.Swap == nil {
		daemonCfg.Swap = &darepod.SwapConfig{}
	}
	if cfg.SwapServerAddress != "" {
		daemonCfg.Swap.ServerAddress = cfg.SwapServerAddress
	}
	if cfg.SwapServerTLSCertPath != "" {
		daemonCfg.Swap.ServerTLSCertPath = cfg.SwapServerTLSCertPath
	}
	cfg.SwapServerInsecure.WhenSome(func(v bool) {
		daemonCfg.Swap.ServerInsecure = v
	})
	if cfg.SwapDatabaseFileName != "" {
		daemonCfg.Swap.DatabaseFileName = cfg.SwapDatabaseFileName
	}
}

// cloneDaemonConfig copies reference-typed daemon config fields before walletdk
// injects its private listener and optional service registrars.
//
// Start mutates the daemon config (assigning daemonCfg.RPC.Listener and
// appending to RPCServiceRegistrars). When the caller supplies their own
// DaemonConfig, we must not write those changes back into the caller's
// pointer graph, otherwise a second Start call (or any caller-side inspection
// of the config) would see walletdk's bufconn listener and registrars. Hence
// the per-subgroup shallow copies below; new pointer/slice fields added to
// darepod.Config in the future need a matching clone here.
// TestCloneDaemonConfigCoversReferenceFields in embedded_test.go reflects over
// darepod.Config and fails when a new pointer/slice/map field is added without
// a matching clone.
func cloneDaemonConfig(cfg *darepod.Config) *darepod.Config {
	clone := *cfg

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
	if cfg.Swap != nil {
		swapCfg := *cfg.Swap
		clone.Swap = &swapCfg
	}
	if cfg.OOR != nil {
		oorCfg := *cfg.OOR
		clone.OOR = &oorCfg
	}

	clone.RPCServiceRegistrars = append(
		[]darepod.RPCServiceRegistrar(nil), cfg.RPCServiceRegistrars...,
	)

	return &clone
}

// waitForRunExit bounds shutdown waits when startup or Stop cancels the
// embedded daemon runtime.
func waitForRunExit(ctx context.Context, rt *daemonRuntime) error {
	select {
	case <-rt.Done():
		return rt.runErr

	case <-ctx.Done():
		return fmt.Errorf("wait for embedded daemon shutdown: %w",
			ctx.Err())
	}
}

// waitForReady forces the lazy gRPC client to dial the embedded daemon before
// Start returns.
//
// grpc.NewClient is intentionally lazy: it does not dial until the first RPC.
// Start's contract is that callers can use the returned Client immediately, so
// we explicitly Connect and then poll connectivity state transitions until
// either gRPC reports Ready or the daemon exits early.
//
// The death watch piggybacks on context.AfterFunc: when the daemon runtime
// context is cancelled, watchCtx is also cancelled, which unblocks
// WaitForStateChange. This replaces an earlier per-iteration goroutine that
// could race the loop and silently drop the daemon-death signal.
func waitForReady(ctx context.Context, conn *grpc.ClientConn,
	rt *daemonRuntime) error {

	conn.Connect()

	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()

	//nolint:contextcheck // rt.ctx is the daemon lifecycle signal, not a
	// request context to inherit; using it here is exactly the point.
	stop := context.AfterFunc(rt.ctx, watchCancel)
	defer stop()

	for {
		state := conn.GetState()
		if state == connectivity.Ready {
			return nil
		}
		if state == connectivity.Shutdown {
			return errors.New("grpc connection shut down before " +
				"readiness")
		}

		if !conn.WaitForStateChange(watchCtx, state) {
			select {
			case <-rt.Done():
				if rt.runErr != nil {
					return fmt.Errorf("embedded daemon "+
						"exited before readiness: %w",
						rt.runErr)
				}

				return errors.New("embedded daemon exited " +
					"before readiness")

			default:
			}

			return fmt.Errorf("wait for grpc readiness: %w",
				ctx.Err())
		}
	}
}

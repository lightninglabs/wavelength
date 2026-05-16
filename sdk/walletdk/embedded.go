//go:build !js

package walletdk

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/lightninglabs/darepo-client/darepod"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const defaultBufConnSize = 1 << 20

// DefaultConfig returns a walletdk config with darepod defaults. Wallet payment
// methods are enabled only when built with walletrpc and swapruntime.
func DefaultConfig() Config {
	cfg := darepod.DefaultConfig()

	return Config{
		DataDir:               cfg.DataDir,
		Network:               cfg.Network,
		DebugLevel:            cfg.DebugLevel,
		ServerAddress:         cfg.Server.Host,
		ServerTLSCertPath:     cfg.Server.TLSCertPath,
		ServerInsecure:        cfg.Server.Insecure,
		WalletType:            cfg.Wallet.Type,
		WalletEsploraURL:      cfg.Wallet.EsploraURL,
		WalletPasswordFile:    cfg.Wallet.PasswordFile,
		WalletPollInterval:    cfg.Wallet.PollInterval,
		WalletRecoveryWindow:  cfg.Wallet.RecoveryWindow,
		WalletFeeURL:          cfg.Wallet.FeeURL,
		SwapServerAddress:     cfg.Swap.ServerAddress,
		SwapServerTLSCertPath: cfg.Swap.ServerTLSCertPath,
		SwapServerInsecure:    cfg.Swap.ServerInsecure,
		SwapDatabaseFileName:  cfg.Swap.DatabaseFileName,
		MaxOperatorFeeSat:     cfg.MaxOperatorFeeSat,
	}
}

// Start starts an embedded darepod runtime and returns the wallet facade.
//
//nolint:contextcheck // embedded daemon lifetime is detached from dial ctx
func Start(ctx context.Context, cfg Config) (*Client, error) {
	if err := requireEmbeddedWalletRuntime(); err != nil {
		return nil, err
	}

	daemonCfg, err := daemonConfig(cfg)
	if err != nil {
		return nil, err
	}

	if err := configureSwapRuntime(daemonCfg, true); err != nil {
		return nil, err
	}
	configureWalletRPC(daemonCfg, true)

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
		runErr := waitForRunExit(shutdownCtx, runErrChan)
		shutdownCancel()
		listenerErr := listener.Close()

		return nil, fmt.Errorf("dial embedded daemon: %w",
			errors.Join(err, runErr, listenerErr))
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

	return newClient(conn, true, waitErrChan,
		func(closeCtx context.Context) error {
			closeErr := conn.Close()
			cancel()
			runErr := waitForRunExit(closeCtx, runErrChan)
			listenerErr := listener.Close()

			return errors.Join(closeErr, runErr, listenerErr)
		},
	), nil
}

// requireEmbeddedWalletRuntime makes Start fail before booting a daemon when
// the current build cannot install the wallet RPC runtime. walletdk's embedded
// mode owns swap resume through swapwallet, so both walletrpc and swapruntime
// must be compiled in.
func requireEmbeddedWalletRuntime() error {
	if !swapRuntimeAvailable() || !walletRPCAvailable() {
		return ErrWalletRPCUnavailable
	}

	return nil
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
	if cfg.AllowMainnet {
		daemonCfg.AllowMainnet = true
	}
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
	if cfg.ServerInsecure {
		daemonCfg.Server.Insecure = true
	}

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
	if cfg.SwapServerInsecure {
		daemonCfg.Swap.ServerInsecure = true
	}
	if cfg.SwapDatabaseFileName != "" {
		daemonCfg.Swap.DatabaseFileName = cfg.SwapDatabaseFileName
	}
}

// cloneDaemonConfig copies reference-typed daemon config fields before walletdk
// injects its private listener and optional service registrars.
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
func waitForRunExit(ctx context.Context, runErrChan <-chan error) error {
	select {
	case runErr := <-runErrChan:
		return runErr

	case <-ctx.Done():
		return fmt.Errorf("wait for embedded daemon shutdown: %w",
			ctx.Err())
	}
}

// waitForReady forces the lazy gRPC client to dial the embedded daemon before
// Start returns.
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
		if runDoneChan != nil {
			go func() {
				select {
				case runErr := <-runDoneChan:
					msg := "embedded daemon exited " +
						"before readiness"
					if runErr != nil {
						runExitErr <- fmt.Errorf(
							"%s: %w", msg, runErr)
					} else {
						runExitErr <- errors.New(msg)
					}

					waitCancel()

				case <-waitCtx.Done():
				}
			}()
		}

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

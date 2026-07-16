// Package lndruntime starts lnd in-process for Wavelength integrations.
package lndruntime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/signal"
	"google.golang.org/grpc"
)

const defaultReadyTimeout = 30 * time.Second

// WalletMode controls how Start handles lnd's wallet state after the
// WalletUnlocker RPC becomes reachable.
type WalletMode uint8

const (
	// WalletModeNone leaves wallet initialization and unlocking to the
	// caller.
	WalletModeNone WalletMode = iota

	// WalletModeUnlock unlocks an existing lnd wallet.
	WalletModeUnlock

	// WalletModeCreateOrUnlock creates the wallet if it does not exist,
	// otherwise unlocks the existing wallet.
	WalletModeCreateOrUnlock
)

// Config holds the inputs needed to start an in-process lnd instance.
type Config struct {
	// Args are lnd command-line arguments, excluding argv[0].
	Args []string

	// AuxComponents are injected into lnd's ImplementationCfg. Virtual
	// channels use this to install publish/close/materialization hooks.
	AuxComponents lnd.AuxComponents

	// WalletMode determines whether Start initializes or unlocks lnd's
	// wallet before returning.
	WalletMode WalletMode

	// WalletPassword is used when WalletMode is not WalletModeNone.
	WalletPassword []byte

	// ReadyTimeout bounds waits for lnd's RPC server and wallet state.
	ReadyTimeout time.Duration
}

// Runtime is a running in-process lnd instance.
type Runtime struct {
	cfg         *lnd.Config
	interceptor signal.Interceptor
	listener    *lnd.ListenerWithSignal
	mainErr     chan error
	stopped     atomic.Bool
}

// Start loads lnd configuration, starts lnd.Main with the supplied auxiliary
// components, and waits until the local gRPC listener is usable.
func Start(ctx context.Context, cfg Config) (*Runtime, error) {
	if cfg.WalletMode != WalletModeNone && len(cfg.WalletPassword) == 0 {
		return nil, errors.New("wallet password is required")
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = defaultReadyTimeout
	}

	interceptor := signal.NewInterceptor()

	loadedCfg, err := lnd.LoadConfigWithArgs(interceptor, cfg.Args)
	if err != nil {
		interceptor.RequestShutdown()
		<-interceptor.ShutdownChannel()

		return nil, err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		interceptor.RequestShutdown()
		<-interceptor.ShutdownChannel()

		return nil, err
	}

	rpcReady := make(chan struct{})
	lis := &lnd.ListenerWithSignal{
		Listener: listener,
		Ready:    rpcReady,
	}
	implCfg := loadedCfg.ImplementationConfig(interceptor)
	implCfg.AuxComponents = cfg.AuxComponents

	rt := &Runtime{
		cfg:         loadedCfg,
		interceptor: interceptor,
		listener:    lis,
		mainErr:     make(chan error, 1),
	}
	go func() {
		rt.mainErr <- lnd.Main(
			loadedCfg, lnd.ListenerCfg{
				RPCListeners: []*lnd.ListenerWithSignal{lis},
			}, implCfg, interceptor,
		)
	}()

	if err := rt.waitRPCReady(ctx, cfg.ReadyTimeout, rpcReady); err != nil {
		rt.Shutdown()

		return nil, err
	}
	if err := rt.prepareWallet(ctx, cfg); err != nil {
		rt.Shutdown()

		return nil, err
	}

	return rt, nil
}

// Address returns the local gRPC endpoint for the running lnd instance.
func (r *Runtime) Address() string {
	return r.listener.Addr().String()
}

// Config returns lnd's loaded configuration.
func (r *Runtime) Config() *lnd.Config {
	return r.cfg
}

// TLSPath returns the lnd TLS certificate path.
func (r *Runtime) TLSPath() string {
	return r.cfg.TLSCertPath
}

// MacaroonPath returns the lnd admin macaroon path.
func (r *Runtime) MacaroonPath() string {
	return r.cfg.AdminMacPath
}

// Shutdown asks lnd to stop and waits for lnd.Main to return.
func (r *Runtime) Shutdown() {
	if r == nil || r.stopped.Swap(true) {
		return
	}

	r.interceptor.RequestShutdown()
	<-r.interceptor.ShutdownChannel()
	<-r.mainErr
}

func (r *Runtime) waitRPCReady(ctx context.Context, timeout time.Duration,
	rpcReady <-chan struct{}) error {

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-rpcReady:
		return nil

	case err := <-r.mainErr:
		return fmt.Errorf("lnd exited before RPC was ready: %w", err)

	case <-timer.C:
		return fmt.Errorf("timed out waiting for lnd RPC readiness")

	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) prepareWallet(ctx context.Context, cfg Config) error {
	if cfg.WalletMode == WalletModeNone {
		return nil
	}

	conn, err := r.unlockerConn()
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	stateClient := lnrpc.NewStateClient(conn)
	walletClient := lnrpc.NewWalletUnlockerClient(conn)

	state, err := stateClient.GetState(ctx, &lnrpc.GetStateRequest{})
	if err != nil {
		return err
	}

	switch state.State {
	case lnrpc.WalletState_NON_EXISTING:
		if cfg.WalletMode != WalletModeCreateOrUnlock {
			return errors.New("lnd wallet does not exist")
		}

		seed, err := walletClient.GenSeed(ctx, &lnrpc.GenSeedRequest{})
		if err != nil {
			return err
		}

		_, err = walletClient.InitWallet(ctx, &lnrpc.InitWalletRequest{
			WalletPassword:     cfg.WalletPassword,
			CipherSeedMnemonic: seed.CipherSeedMnemonic,
		})
		if err != nil {
			return err
		}

		return waitWalletActive(ctx, stateClient, cfg.ReadyTimeout)

	case lnrpc.WalletState_LOCKED:
		_, err := walletClient.UnlockWallet(
			ctx, &lnrpc.UnlockWalletRequest{
				WalletPassword: cfg.WalletPassword,
			},
		)
		if err != nil {
			return err
		}

		return waitWalletActive(ctx, stateClient, cfg.ReadyTimeout)

	case lnrpc.WalletState_RPC_ACTIVE,
		lnrpc.WalletState_SERVER_ACTIVE:
		return nil

	case lnrpc.WalletState_UNLOCKED,
		lnrpc.WalletState_WAITING_TO_START:
		return waitWalletActive(ctx, stateClient, cfg.ReadyTimeout)

	default:
		return fmt.Errorf("unexpected lnd wallet state %v", state.State)
	}
}

func waitWalletActive(ctx context.Context, stateClient lnrpc.StateClient,
	timeout time.Duration) error {

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		state, err := stateClient.GetState(
			ctx, &lnrpc.GetStateRequest{},
		)
		if err != nil {
			return err
		}

		switch state.State {
		case lnrpc.WalletState_RPC_ACTIVE,
			lnrpc.WalletState_SERVER_ACTIVE:
			return nil

		case lnrpc.WalletState_UNLOCKED,
			lnrpc.WalletState_WAITING_TO_START,
			lnrpc.WalletState_NON_EXISTING,
			lnrpc.WalletState_LOCKED:

		default:
			return fmt.Errorf("unexpected lnd wallet state %v",
				state.State)
		}

		select {
		case <-ticker.C:
		case <-timer.C:
			return fmt.Errorf("timed out waiting for lnd wallet " +
				"to become active")

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (r *Runtime) unlockerConn() (*grpc.ClientConn, error) {
	opts, err := lnd.AdminAuthOptions(r.cfg, true)
	if err != nil {
		return nil, err
	}

	return grpc.NewClient(r.Address(), opts...)
}

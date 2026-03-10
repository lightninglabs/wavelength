package darepo

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/lightninglabs/darepo-client/chainsource"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightninglabs/darepo/build"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/indexer"
	"github.com/lightninglabs/darepo/mailbox"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/signal"
)

// Server is the main operator daemon.
type Server struct {
	started atomic.Bool

	cfg *Config

	lnd *lndclient.GrpcLndServices

	actorSystem  *actor.ActorSystem
	chainBackend chainsource.ChainBackend

	db *db.Store

	adminRPC *AdminRPCServer

	rpc        *RPCServer
	mailboxMux *mailboxrpc.ServeMux

	log btclog.Logger

	// quit is closed by Shutdown() to trigger a graceful exit from
	// RunWithContext independently of the parent context. This
	// enables programmatic shutdown from subsystems or external
	// callers that do not hold the context's cancel function.
	quit chan struct{}

	// mailboxStore is the in-process mailbox store used by all
	// subsystems for envelope persistence and delivery.
	mailboxStore mailbox.Store

	// clientBridge is the shared per-client connection bridge that
	// multiplexes round, indexer, and other RPC dispatchers across
	// all registered clients.
	clientBridge *clientconn.ClientsConnBridge

	// indexerService is the transport-free indexer business logic.
	indexerService *indexer.Service

	// indexerOperator provides RPC dispatchers and event publication
	// for the indexer service through the shared bridge.
	indexerOperator *indexer.Operator
}

// subLogger extracts a subsystem logger from the config's Loggers map.
// When the map is nil or the key is absent, btclog.Disabled is returned.
func subLogger(loggers SubLoggers, tag string) btclog.Logger {
	if loggers == nil {
		return btclog.Disabled
	}

	l, ok := loggers[tag]
	if !ok {
		return btclog.Disabled
	}

	return l
}

// Main is the true entry point for the daemon. It is called after CLI
// flag parsing, config validation, and signal interception are
// complete.
func Main(cfg *Config, interceptor signal.Interceptor) error {
	srv, err := NewServer(cfg)
	if err != nil {
		return err
	}

	return srv.RunUntilShutdown(interceptor)
}

// NewServer allocates a Server from a validated Config. The server is
// inert until RunUntilShutdown or RunWithContext is called.
func NewServer(cfg *Config) (*Server, error) {
	return &Server{
		cfg:  cfg,
		log:  cfg.Log.UnwrapOr(btclog.Disabled),
		quit: make(chan struct{}),
	}, nil
}

// RunUntilShutdown starts all subsystems and blocks until the
// shutdown interceptor fires or a fatal error occurs. It wraps
// RunWithContext by translating the interceptor signal into a
// context cancellation.
func (s *Server) RunUntilShutdown(
	interceptor signal.Interceptor) error {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel the context when the interceptor fires so blocking
	// calls unblock promptly.
	go func() {
		select {
		case <-interceptor.ShutdownChannel():
			cancel()

		case <-ctx.Done():
		}
	}()

	return s.RunWithContext(ctx)
}

// RunWithContext starts all subsystems and blocks until the given
// context is cancelled. This is the test-friendly entry point:
// tests manage daemon lifecycle via context cancellation instead of
// requiring a signal.Interceptor (which is process-global).
func (s *Server) RunWithContext(ctx context.Context) error {
	// Only allow the server to be started once.
	if !s.started.CompareAndSwap(false, true) {
		return nil
	}

	s.log.InfoS(ctx, "Starting arkd",
		slog.String("version", build.Version()),
		slog.String("commit", build.CommitHash),
		slog.String("network", s.cfg.Network))

	// -------------------------------------------------------
	// 1. Connect to lnd.
	// -------------------------------------------------------
	s.log.InfoS(ctx, "Connecting to lnd",
		slog.String("host", s.cfg.Lnd.Host))

	lndServices, err := s.connectLnd(ctx)
	if err != nil {
		return fmt.Errorf("unable to connect to lnd: %w",
			err)
	}
	s.lnd = lndServices
	defer s.lnd.Close()

	s.log.InfoS(ctx, "Connected to lnd",
		slog.String("alias", s.lnd.NodeAlias),
		slog.String("pubkey",
			s.lnd.NodePubkey.String()))

	// -------------------------------------------------------
	// 2. Initialize actor system.
	// -------------------------------------------------------
	s.actorSystem = actor.NewActorSystem()
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(
			context.Background(), DefaultShutdownTimeout,
		)
		defer shutdownCancel()

		_ = s.actorSystem.Shutdown(shutdownCtx)
	}()

	s.log.InfoS(ctx, "Actor system initialized")

	// -------------------------------------------------------
	// 3. Create and register chain source actor.
	// -------------------------------------------------------
	s.chainBackend = chainbackends.NewLNDBackendFromLndClient(
		chainbackends.LNDBackendFromLndClientConfig{
			LND: &s.lnd.LndServices,
		},
	)

	if err := s.chainBackend.Start(); err != nil {
		return fmt.Errorf("unable to start chain "+
			"backend: %w", err)
	}
	defer func() {
		_ = s.chainBackend.Stop()
	}()

	chainActor := chainsource.NewChainSourceActor(
		chainsource.ChainSourceConfig{
			Backend: s.chainBackend,
			System:  s.actorSystem,
		},
	)
	_ = actor.RegisterWithSystem(
		s.actorSystem, "chain-source",
		chainsource.ChainSourceKey, chainActor,
	)

	s.log.InfoS(ctx, "Chain source actor registered")

	// -------------------------------------------------------
	// 4. Initialize database.
	// -------------------------------------------------------
	dbLog := subLogger(s.cfg.Loggers, dbSubsystem)

	s.db, err = db.NewStoreFromConfig(
		s.cfg.DB, dbLog, clock.NewDefaultClock(),
	)
	if err != nil {
		return fmt.Errorf("unable to open database: %w",
			err)
	}
	defer func() {
		if s.db != nil {
			_ = s.db.Close()
		}
	}()

	backendName := "sqlite"
	if s.cfg.DB.Backend == "postgres" {
		backendName = "postgres"
	}
	s.log.InfoS(ctx, "Database initialized",
		"backend", backendName)

	// -------------------------------------------------------
	// 5. Setup indexer subsystem.
	// -------------------------------------------------------
	if err := s.setupIndexerSubsystem(ctx); err != nil {
		return fmt.Errorf("unable to setup indexer "+
			"subsystem: %w", err)
	}
	defer s.stopIndexerSubsystem(ctx)

	// -------------------------------------------------------
	// 6. Start admin RPC server.
	// -------------------------------------------------------
	adminLog := subLogger(s.cfg.Loggers, adminRPCSubsystem)

	s.adminRPC, err = NewAdminRPCServer(
		s.cfg.AdminRPC, s, adminLog,
	)
	if err != nil {
		return fmt.Errorf("unable to create admin RPC "+
			"server: %w", err)
	}
	if err := s.adminRPC.Start(ctx); err != nil {
		return fmt.Errorf("unable to start admin "+
			"server: %w", err)
	}
	defer func() {
		_ = s.adminRPC.Stop(ctx)
	}()

	// -------------------------------------------------------
	// 7. Start client RPC server.
	// -------------------------------------------------------
	rpcLog := subLogger(s.cfg.Loggers, clientRPCSubsystem)

	s.rpc, err = NewRPCServer(
		s.cfg.RPC, s, rpcLog,
	)
	if err != nil {
		return fmt.Errorf("unable to create client RPC "+
			"server: %w", err)
	}
	if err := s.rpc.Start(ctx); err != nil {
		return fmt.Errorf("unable to start client RPC "+
			"server: %w", err)
	}
	defer func() {
		_ = s.rpc.Stop(ctx)
	}()

	// Register the ArkService for mailbox RPC access. The
	// ServeMux handles incoming KIND_REQUEST envelopes routed
	// by the serverconn ingress loop. The RPCServer implements
	// both the gRPC and mailbox server interfaces, so the same
	// handler serves both transports.
	s.mailboxMux = mailboxrpc.NewServeMux()
	arkrpc.RegisterArkServiceMailboxServer(
		s.mailboxMux, s.rpc,
	)

	s.log.InfoS(ctx, "Daemon ready")

	// -------------------------------------------------------
	// 8. Block until shutdown.
	// -------------------------------------------------------
	select {
	case <-ctx.Done():

	case <-s.quit:
	}

	s.log.InfoS(ctx, "Shutting down arkd")

	return nil
}

// AdminRPCAddr returns the address the admin RPC server is listening on, or
// nil if the server hasn't been started yet.
func (s *Server) AdminRPCAddr() net.Addr {
	if s.adminRPC == nil {
		return nil
	}

	return s.adminRPC.Addr()
}

// RPCAddr returns the address the client RPC server is listening on,
// or nil if the server hasn't been started yet.
func (s *Server) RPCAddr() net.Addr {
	if s.rpc == nil {
		return nil
	}

	return s.rpc.Addr()
}

// Shutdown triggers a graceful exit of RunWithContext independently
// of the parent context. It is safe to call multiple times.
func (s *Server) Shutdown() {
	select {
	case <-s.quit:
		// Already closed.

	default:
		close(s.quit)
	}
}

// connectLnd establishes a connection to the lnd node using the
// lndclient SDK. The call blocks until lnd is fully synced and the
// wallet is unlocked.
func (s *Server) connectLnd(ctx context.Context) (
	*lndclient.GrpcLndServices, error) {

	network, err := networkToLndclient(s.cfg.Network)
	if err != nil {
		return nil, err
	}

	rpcTimeout := s.cfg.Lnd.RPCTimeout
	if rpcTimeout == 0 {
		rpcTimeout = DefaultRPCTimeout
	}

	return lndclient.NewLndServices(&lndclient.LndServicesConfig{
		LndAddress:            s.cfg.Lnd.Host,
		Network:               network,
		CustomMacaroonPath:    s.cfg.Lnd.MacaroonPath,
		TLSPath:               s.cfg.Lnd.TLSPath,
		BlockUntilChainSynced: true,
		BlockUntilUnlocked:    true,
		CallerCtx:             ctx,
		RPCTimeout:            rpcTimeout,
	})
}

// networkToLndclient maps our network string to the lndclient network
// type.
func networkToLndclient(network string) (lndclient.Network, error) {
	switch network {
	case "mainnet":
		return lndclient.NetworkMainnet, nil

	case "testnet":
		return lndclient.NetworkTestnet, nil

	case "regtest":
		return lndclient.NetworkRegtest, nil

	case "simnet":
		return lndclient.NetworkSimnet, nil

	case "signet":
		return lndclient.NetworkSignet, nil

	default:
		return "", fmt.Errorf("unknown network %q", network)
	}
}

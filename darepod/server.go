package darepod

import (
	"context"
	"fmt"
	"net"

	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/indexer"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/signal"
	"google.golang.org/grpc"
)

// Main is the true entry point for the daemon. It is called after CLI flag
// parsing, config validation, and signal interception are complete.
func Main(cfg *Config, interceptor signal.Interceptor) error {
	srv, err := NewServer(cfg)
	if err != nil {
		return err
	}

	return srv.RunUntilShutdown(interceptor)
}

// Server is the top-level daemon orchestrator. It owns the lnd connection,
// the mailbox transport runtime, the indexer client, and the daemon's own
// gRPC server.
type Server struct {
	cfg *Config

	lnd     *lndclient.GrpcLndServices
	runtime *serverconn.Runtime
	indexer *indexer.Client

	grpcServer *grpc.Server
	rpcServer  *RPCServer
}

// NewServer allocates a Server from a validated Config. The server is inert
// until RunUntilShutdown is called.
func NewServer(cfg *Config) (*Server, error) {
	return &Server{
		cfg: cfg,
	}, nil
}

// RunUntilShutdown starts all subsystems and blocks until the shutdown
// interceptor fires or a fatal error occurs.
func (s *Server) RunUntilShutdown(
	interceptor signal.Interceptor) error {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel the context when the interceptor fires so blocking calls
	// (like lndclient chain sync) unblock promptly.
	go func() {
		select {
		case <-interceptor.ShutdownChannel():
			cancel()

		case <-ctx.Done():
		}
	}()

	log.InfoS(ctx, "Starting darepod",
		"version", build.Version(),
		"commit", build.CommitHash,
		"network", s.cfg.Network)

	// -------------------------------------------------------
	// 1. Connect to lnd.
	// -------------------------------------------------------
	log.InfoS(ctx, "Connecting to lnd",
		"host", s.cfg.Lnd.Host)

	lndServices, err := s.connectLnd(ctx)
	if err != nil {
		return fmt.Errorf("unable to connect to lnd: %w", err)
	}
	s.lnd = lndServices
	defer s.lnd.Close()

	log.InfoS(ctx, "Connected to lnd",
		"alias", s.lnd.NodeAlias,
		"pubkey", s.lnd.NodePubkey)

	// -------------------------------------------------------
	// 2. Start the daemon's own gRPC server.
	// -------------------------------------------------------
	s.rpcServer = NewRPCServer(s)

	s.grpcServer = grpc.NewServer()
	RegisterDaemonServiceServer(s.grpcServer, s.rpcServer)

	lis, err := net.Listen("tcp", s.cfg.RPC.ListenAddr)
	if err != nil {
		return fmt.Errorf("unable to listen on %s: %w",
			s.cfg.RPC.ListenAddr, err)
	}

	go func() {
		log.InfoS(ctx, "gRPC server listening",
			"addr", s.cfg.RPC.ListenAddr)

		if err := s.grpcServer.Serve(lis); err != nil {
			log.ErrorS(ctx, "gRPC server error", err)
		}
	}()
	defer s.grpcServer.GracefulStop()

	log.InfoS(ctx, "Daemon ready")

	// -------------------------------------------------------
	// 3. Block until shutdown.
	// -------------------------------------------------------
	<-interceptor.ShutdownChannel()

	log.InfoS(ctx, "Shutting down darepod")

	return nil
}

// connectLnd establishes a connection to the lnd node using the
// lndclient SDK.
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

// networkToLndclient maps our network string to the lndclient network type.
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

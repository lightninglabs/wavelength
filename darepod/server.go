package darepod

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/indexer"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/signal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
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

	actorSystem  *actor.ActorSystem
	chainBackend chainsource.ChainBackend

	serverConn *grpc.ClientConn

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
	// 2. Connect to the ark operator's mailbox edge server.
	// -------------------------------------------------------
	log.InfoS(ctx, "Connecting to ark server",
		"host", s.cfg.Server.Host)

	serverConn, err := s.dialServer(ctx)
	if err != nil {
		return fmt.Errorf("unable to connect to server: %w", err)
	}
	s.serverConn = serverConn
	defer s.serverConn.Close()

	log.InfoS(ctx, "Connected to ark server")

	// -------------------------------------------------------
	// 3. Initialize the actor system.
	// -------------------------------------------------------
	s.actorSystem = actor.NewActorSystem()
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(
			context.Background(), DefaultShutdownTimeout,
		)
		defer shutdownCancel()

		_ = s.actorSystem.Shutdown(shutdownCtx)
	}()

	log.InfoS(ctx, "Actor system initialized")

	// -------------------------------------------------------
	// 4. Create and register the chain source actor.
	// -------------------------------------------------------
	// The chain backend adapts lndclient's chain notifier, fee
	// estimator, and wallet kit into the unified ChainBackend
	// interface used by the chainsource actor.
	s.chainBackend = chainbackends.NewLNDBackendFromLndClient(
		chainbackends.LNDBackendFromLndClientConfig{
			LND: &s.lnd.LndServices,
		},
	)

	if err := s.chainBackend.Start(); err != nil {
		return fmt.Errorf("unable to start chain backend: %w", err)
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
	actor.RegisterWithSystem(
		s.actorSystem, "chain-source",
		chainsource.ChainSourceKey, chainActor,
	)

	log.InfoS(ctx, "Chain source actor registered")

	// -------------------------------------------------------
	// 5. Create the mailbox transport runtime.
	// -------------------------------------------------------
	// The serverconn runtime requires a persistent DeliveryStore
	// for durable message processing. Once the database layer is
	// initialized, we wire it here along with the mailbox edge
	// client and mailbox ID pair.
	//
	// TODO(roasbeef): Wire a persistent DeliveryStore (DB-backed)
	// once the database layer is initialized.
	log.InfoS(ctx, "Mailbox runtime not yet wired (needs "+
		"DeliveryStore)")

	// -------------------------------------------------------
	// 6. Start the daemon's own gRPC server.
	// -------------------------------------------------------
	s.rpcServer = NewRPCServer(s)

	s.grpcServer = grpc.NewServer()
	daemonrpc.RegisterDaemonServiceServer(
		s.grpcServer, s.rpcServer,
	)

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
	// 7. Block until shutdown.
	// -------------------------------------------------------
	<-interceptor.ShutdownChannel()

	log.InfoS(ctx, "Shutting down darepod")

	return nil
}

// connectLnd establishes a connection to the lnd node using the lndclient
// SDK. The call blocks until lnd is fully synced and the wallet is unlocked.
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

// dialServer establishes a gRPC connection to the ark operator's mailbox
// edge server. When TLSCertPath is set, the connection uses a custom cert
// pool anchored to that certificate. When Insecure is set, TLS is disabled
// entirely (for regtest/development only).
func (s *Server) dialServer(ctx context.Context) (
	*grpc.ClientConn, error) {

	var dialOpts []grpc.DialOption

	switch {
	case s.cfg.Server.Insecure:
		dialOpts = append(
			dialOpts,
			grpc.WithTransportCredentials(
				insecure.NewCredentials(),
			),
		)

	case s.cfg.Server.TLSCertPath != "":
		certBytes, err := os.ReadFile(s.cfg.Server.TLSCertPath)
		if err != nil {
			return nil, fmt.Errorf("unable to read server "+
				"TLS cert: %w", err)
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(certBytes) {
			return nil, fmt.Errorf("unable to parse server " +
				"TLS cert")
		}

		creds := credentials.NewTLS(&tls.Config{
			RootCAs: pool,
		})
		dialOpts = append(
			dialOpts,
			grpc.WithTransportCredentials(creds),
		)

	default:
		// Use the system certificate pool when no explicit cert
		// is provided.
		creds := credentials.NewTLS(&tls.Config{})
		dialOpts = append(
			dialOpts,
			grpc.WithTransportCredentials(creds),
		)
	}

	return grpc.NewClient(s.cfg.Server.Host, dialOpts...)
}

// newMailboxEdge creates a MailboxServiceClient from the established server
// connection.
func (s *Server) newMailboxEdge() mailboxpb.MailboxServiceClient {
	return mailboxpb.NewMailboxServiceClient(s.serverConn)
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

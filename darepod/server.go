package darepod

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/db/actordelivery"
	"github.com/lightninglabs/darepo-client/indexer"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/lndbackend"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/rpc/oorpb"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo-client/timeout"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/signal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
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

	db            *db.SqliteStore
	deliveryStore actor.DeliveryStore

	lnd     *lndclient.GrpcLndServices
	runtime *serverconn.Runtime
	ark     *arkrpc.ArkServiceMailboxClient
	indexer *indexer.Client

	actorSystem  *actor.ActorSystem
	chainBackend chainsource.ChainBackend
	oorActor     *oor.OORClientActor

	serverConn *grpc.ClientConn

	grpcServer *grpc.Server
	rpcServer  *RPCServer
	mailboxMux *mailboxrpc.ServeMux
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
//
//nolint:funlen
func (s *Server) RunUntilShutdown(interceptor signal.Interceptor) error {
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

	// Register the shared timeout actor. This provides wall-clock
	// timer scheduling for any subsystem that needs deadlines.
	timeoutRef := actor.RegisterWithSystem(
		s.actorSystem, "timeout",
		actor.NewServiceKey[timeout.Msg, timeout.Resp]("timeout"),
		timeout.NewActor(),
	)

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
	chainSourceRef := actor.RegisterWithSystem(
		s.actorSystem, "chain-source",
		chainsource.ChainSourceKey, chainActor,
	)

	log.InfoS(ctx, "Chain source actor registered")

	// -------------------------------------------------------
	// 5. Open the database and create the delivery store.
	// -------------------------------------------------------
	if err := s.initDatabase(ctx); err != nil {
		return err
	}
	defer func() {
		_ = s.db.Close()
	}()

	// -------------------------------------------------------
	// 6. Start the daemon's own gRPC server and mailbox mux.
	// -------------------------------------------------------
	s.rpcServer = NewRPCServer(s)

	// Register the DaemonService for local gRPC access (CLI, GUI).
	//
	// TODO(roasbeef): Wire RPC.TLSCertPath/TLSKeyPath into
	// grpc.Creds() once the auto-gen TLS material is in place.
	s.grpcServer = grpc.NewServer()
	daemonrpc.RegisterDaemonServiceServer(
		s.grpcServer, s.rpcServer,
	)

	// Register the DaemonService for mailbox RPC access. The
	// ServeMux handles incoming KIND_REQUEST envelopes routed
	// by the serverconn ingress loop. The RPCServer implements
	// both the gRPC and mailbox server interfaces, so the same
	// handler serves both transports.
	s.mailboxMux = mailboxrpc.NewServeMux()
	daemonrpc.RegisterDaemonServiceMailboxServer(
		s.mailboxMux, s.rpcServer,
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

	// -------------------------------------------------------
	// 7. Wire the mailbox transport runtime.
	// -------------------------------------------------------
	// Build the dispatcher map for inbound RPCs. The server
	// sends KIND_REQUEST envelopes (e.g., GetInfo) which the
	// ingress loop routes to the ServeMux dispatcher. The
	// dispatcher calls ServeRPC, serializes the response, and
	// sends it back as a KIND_RESPONSE envelope.
	edge := s.newMailboxEdge()
	dispatchers := s.buildRPCDispatchers(edge)

	connCfg := serverconn.DefaultConnectorConfig()
	connCfg.Edge = edge
	connCfg.LocalMailboxID = s.cfg.Server.LocalMailboxID
	connCfg.RemoteMailboxID = s.cfg.Server.RemoteMailboxID
	connCfg.Store = s.deliveryStore
	connCfg.Dispatchers = dispatchers

	s.runtime, err = serverconn.NewRuntime(connCfg)
	if err != nil {
		return fmt.Errorf("unable to create serverconn "+
			"runtime: %w", err)
	}

	if err := s.runtime.Start(ctx); err != nil {
		return fmt.Errorf("unable to start serverconn "+
			"runtime: %w", err)
	}
	defer s.runtime.Stop()

	log.InfoS(ctx, "Mailbox transport runtime started",
		slog.String("local_mailbox",
			s.cfg.Server.LocalMailboxID),
		slog.String("remote_mailbox",
			s.cfg.Server.RemoteMailboxID))

	// -------------------------------------------------------
	// 8. Create the Ark and indexer RPC clients.
	// -------------------------------------------------------
	s.initRPCClients(ctx)

	// -------------------------------------------------------
	// 9. Register the wallet (boarding) actor.
	// -------------------------------------------------------
	walletRef, err := s.initWalletActor(ctx, chainSourceRef)
	if err != nil {
		return err
	}

	// -------------------------------------------------------
	// 10. Register the round client actor.
	// -------------------------------------------------------
	if err := s.initRoundActor(
		ctx, chainSourceRef, walletRef, timeoutRef,
	); err != nil {
		return err
	}

	// -------------------------------------------------------
	// 11. Register the OOR client actor.
	// -------------------------------------------------------
	if err := s.initOORActor(ctx); err != nil {
		return err
	}
	defer s.oorActor.Stop()

	log.InfoS(ctx, "Daemon ready")

	// -------------------------------------------------------
	// 12. Block until shutdown.
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
			return nil, fmt.Errorf("unable to parse server "+
				"TLS cert at %s",
				s.cfg.Server.TLSCertPath)
		}

		creds := credentials.NewTLS(&tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		})
		dialOpts = append(
			dialOpts,
			grpc.WithTransportCredentials(creds),
		)

	default:
		// Use the system certificate pool when no explicit cert
		// is provided.
		creds := credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS12,
		})
		dialOpts = append(
			dialOpts,
			grpc.WithTransportCredentials(creds),
		)
	}

	return grpc.NewClient(s.cfg.Server.Host, dialOpts...)
}

// newMailboxEdge creates a MailboxServiceClient from the established server
// connection. The runtime uses this to send and pull envelopes through the
// operator's mailbox edge service.
func (s *Server) newMailboxEdge() mailboxpb.MailboxServiceClient {
	return mailboxpb.NewMailboxServiceClient(s.serverConn)
}

// buildRPCDispatchers creates the dispatcher map for inbound envelopes.
// KIND_REQUEST envelopes are bridged to the local ServeMux (e.g.,
// DaemonService.GetInfo). KIND_EVENT envelopes for server-push OOR responses
// are routed to the OOR actor via the EventRouter and service key lookup.
func (s *Server) buildRPCDispatchers(
	edge mailboxpb.MailboxServiceClient,
) map[mailboxrpc.ServiceMethod]serverconn.EnvelopeDispatcher {

	// Create a catch-all dispatcher that routes any inbound
	// KIND_REQUEST to the ServeMux. We register one entry per
	// known service/method pair so the ingress loop's dispatch
	// table matches.
	dispatch := func(
		ctx context.Context, env *mailboxpb.Envelope,
	) error {

		return s.handleInboundRPC(ctx, edge, env)
	}

	// Build event-based dispatch routes for server-push events
	// that target durable actors via service key lookup.
	eventRouter := s.buildEventRoutes()

	// Start with the event router's dispatch map, then layer
	// on the RPC dispatch entries.
	dispatchers := eventRouter.AsDispatcherMap()

	// DaemonService.GetInfo — server queries client status.
	dispatchers[mailboxrpc.ServiceMethod{
		Service: "daemonrpc.DaemonService",
		Method:  "GetInfo",
	}] = dispatch

	// TODO(roasbeef): Add indexer and wallet service methods
	// here once their clients are initialized (e.g.,
	// WalletService.SignVTXO, RoundService.SubmitNonces).

	return dispatchers
}

// buildEventRoutes registers typed event routes for server-push envelopes.
// Each route maps a (service, method) pair to a durable actor via the
// EventRouter, which handles proto deserialization, domain adaptation, and
// durable Tell delivery.
func (s *Server) buildEventRoutes() *serverconn.EventRouter {
	router := serverconn.NewEventRouter(s.actorSystem)

	s.registerOOREventRoutes(router)

	return router
}

// registerOOREventRoutes registers OOR mailbox service event routes with the
// EventRouter. When the server pushes SubmitPackage or FinalizePackage
// response events, the router decodes the oorpb proto, adapts it into a
// DriveEventRequest, and Tell's it to the OOR actor via service key.
func (s *Server) registerOOREventRoutes(router *serverconn.EventRouter) {
	oorKey := oor.NewServiceKey()

	// SubmitPackage: server accepted the submit and returned co-signed
	// checkpoint PSBTs. Adapt into a DriveEventRequest carrying a
	// SubmitAcceptedEvent so the OOR FSM can advance.
	serverconn.AddRoute(router, serverconn.EventRouteConfig[
		oor.OORDurableMsg, oor.ActorResp,
	]{
		Service: oorpb.ServiceName,
		Method:  oorpb.MethodSubmitPackage,
		NewEvent: func() proto.Message {
			return &oorpb.SubmitPackageResponse{}
		},
		Key: oorKey,
		Adapt: func(p proto.Message) (oor.OORDurableMsg, error) {
			resp, ok := p.(*oorpb.SubmitPackageResponse)
			if !ok {
				return nil, fmt.Errorf(
					"expected SubmitPackageResponse, "+
						"got %T", p,
				)
			}

			sessionID, checkpoints, err :=
				oorpb.ParseSubmitPackageResponse(resp)
			if err != nil {
				return nil, fmt.Errorf("parse submit "+
					"response: %w", err)
			}

			return &oor.DriveEventRequest{
				SessionID: oor.SessionID(sessionID),
				Event: &oor.SubmitAcceptedEvent{
					SessionID: oor.SessionID(
						sessionID,
					),
					CoSignedCheckpointPSBTs: checkpoints,
				},
			}, nil
		},
	})

	// FinalizePackage: server accepted the finalize. Adapt into a
	// DriveEventRequest carrying a FinalizeAcceptedEvent so the OOR
	// FSM can advance to the terminal Completed state.
	serverconn.AddRoute(router, serverconn.EventRouteConfig[
		oor.OORDurableMsg, oor.ActorResp,
	]{
		Service: oorpb.ServiceName,
		Method:  oorpb.MethodFinalizePackage,
		NewEvent: func() proto.Message {
			return &oorpb.FinalizePackageResponse{}
		},
		Key: oorKey,
		Adapt: func(p proto.Message) (oor.OORDurableMsg, error) {
			resp, ok := p.(*oorpb.FinalizePackageResponse)
			if !ok {
				return nil, fmt.Errorf(
					"expected FinalizePackageResponse"+
						", got %T", p,
				)
			}

			sessionID, err :=
				oorpb.ParseFinalizePackageResponse(resp)
			if err != nil {
				return nil, fmt.Errorf("parse finalize "+
					"response: %w", err)
			}

			return &oor.DriveEventRequest{
				SessionID: oor.SessionID(sessionID),
				Event:     &oor.FinalizeAcceptedEvent{},
			}, nil
		},
	})

	// TODO(roasbeef): Register an IncomingAck route once the
	// oorpb proto defines an ack RPC. SendIncomingAckRequest is
	// classified as a transport event but currently has no
	// server-push response route.
}

// handleInboundRPC dispatches a single inbound KIND_REQUEST envelope through
// the ServeMux and sends the response back as a KIND_RESPONSE envelope via
// the edge client.
func (s *Server) handleInboundRPC(ctx context.Context,
	edge mailboxpb.MailboxServiceClient,
	env *mailboxpb.Envelope) error {

	if env.Rpc == nil {
		return fmt.Errorf("missing envelope rpc metadata")
	}
	if env.Body == nil {
		return fmt.Errorf("missing envelope body")
	}

	// Dispatch through the mux to the registered handler.
	respMsg, err := s.mailboxMux.ServeRPC(
		ctx, env.Rpc.Service, env.Rpc.Method,
		env.Body.Value,
	)

	var (
		body    *anypb.Any
		headers map[string]string
	)

	if err != nil {
		// Transport the error via grpc_status headers so the
		// caller sees a proper gRPC status error.
		headers = mailboxrpc.EncodeErrorHeaders(err)
		body = &anypb.Any{}
	} else if body, err = anypb.New(respMsg); err != nil {
		headers = mailboxrpc.EncodeErrorHeaders(fmt.Errorf(
			"wrap response in Any: %w", err,
		))
		body = &anypb.Any{}
	}

	responseEnv := &mailboxpb.Envelope{
		ProtocolVersion: env.ProtocolVersion,
		Sender:          s.cfg.Server.LocalMailboxID,
		Recipient:       env.Rpc.ReplyTo,
		Headers:         headers,
		Body:            body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_RESPONSE,
			CorrelationId: env.Rpc.CorrelationId,
			Service:       env.Rpc.Service,
			Method:        env.Rpc.Method,
		},
	}

	_, err = edge.Send(ctx, &mailboxpb.SendRequest{
		Envelope: responseEnv,
	})
	if err != nil {
		return fmt.Errorf("send RPC response: %w", err)
	}

	return nil
}

// initDatabase opens the SQLite database and creates the actor
// delivery store used by the serverconn runtime for at-least-once
// envelope delivery.
func (s *Server) initDatabase(ctx context.Context) error {
	networkDir := s.cfg.NetworkDir()
	if err := os.MkdirAll(networkDir, 0700); err != nil {
		return fmt.Errorf("unable to create data dir: %w", err)
	}

	sqliteCfg := db.DefaultSqliteConfig(networkDir)

	var err error
	s.db, err = db.NewSqliteStore(sqliteCfg, log)
	if err != nil {
		return fmt.Errorf("unable to open database: %w", err)
	}

	s.deliveryStore, err = actordelivery.NewTxAwareDeliveryStoreFromDB(
		s.db.DB, s.db.Backend(), clock.NewDefaultClock(),
		log,
	)
	if err != nil {
		return fmt.Errorf("unable to create delivery "+
			"store: %w", err)
	}

	log.InfoS(ctx, "Database initialized",
		slog.String("path", sqliteCfg.DatabaseFileName))

	return nil
}

// initRPCClients creates the Ark and indexer mailbox RPC clients. Both
// use the runtime's unary facade to issue RPCs to the server through
// the mailbox transport.
func (s *Server) initRPCClients(ctx context.Context) {
	s.ark = arkrpc.NewArkServiceMailboxClient(s.runtime.Unary())

	// TODO(roasbeef): wire SchnorrSigner from lnd backend once
	// indexer proof-of-control is enabled in the daemon.
	s.indexer = indexer.New(
		s.runtime.Unary(), nil,
		s.cfg.Server.RemoteMailboxID,
		s.lnd.NodePubkey.String(),
	)

	log.InfoS(ctx, "RPC clients initialized")
}

// initWalletActor creates, registers, and starts the boarding wallet
// actor. The wallet manages key derivation, address creation, and
// boarding UTXO tracking. It receives block epoch notifications from
// the chain source actor and can forward confirmation events to
// registered notifiers (e.g., the round actor).
func (s *Server) initWalletActor(ctx context.Context,
	chainSourceRef actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]) (actor.ActorRef[wallet.WalletMsg, wallet.WalletResp], error) {

	clk := clock.NewDefaultClock()
	chainParams := s.lnd.ChainParams

	dbStore := db.NewStore(
		s.db.DB, s.db.Queries, s.db.Backend(), log,
	)
	boardingStore := dbStore.NewBoardingStore(chainParams, clk)

	boardingBackend := lndbackend.NewBoardingBackend(
		s.lnd.WalletKit,
	)

	walletActor := wallet.NewArk(
		boardingBackend, boardingStore, chainSourceRef,
		s.actorSystem, log,
	)
	walletKey := actor.NewServiceKey[
		wallet.WalletMsg, wallet.WalletResp,
	]("boarding-wallet")
	walletRef := actor.RegisterWithSystem(
		s.actorSystem, "boarding-wallet",
		walletKey, walletActor,
	)

	if err := walletActor.Start(ctx, walletRef); err != nil {
		var zero actor.ActorRef[
			wallet.WalletMsg, wallet.WalletResp,
		]

		return zero, fmt.Errorf(
			"unable to start wallet actor: %w", err,
		)
	}

	log.InfoS(ctx, "Wallet actor registered and started")

	return walletRef, nil
}

// initRoundActor creates, registers, and starts the round client
// actor. The round actor manages client-side participation in Ark
// rounds: boarding intent submission, MuSig2 nonce exchange, partial
// signing, and forfeit signing. It requires the operator's terms
// (fetched from the server) and references to the chain source and
// wallet actors.
func (s *Server) initRoundActor(ctx context.Context,
	chainSourceRef actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	],
	walletRef actor.ActorRef[
		wallet.WalletMsg, wallet.WalletResp,
	],
	timeoutRef actor.TellOnlyRef[timeout.Msg]) error {

	clientWallet := lndbackend.NewClientWallet(
		s.lnd.Signer, s.lnd.WalletKit,
	)

	clk := clock.NewDefaultClock()
	chainParams := s.lnd.ChainParams

	dbStore := db.NewStore(
		s.db.DB, s.db.Queries, s.db.Backend(), log,
	)
	roundStore := dbStore.NewRoundStore(chainParams, clk)

	// Fetch the operator's terms from the server. These include
	// the operator pubkey, sweep delay, exit delay, dust limit,
	// and other round parameters.
	operatorTerms, err := s.fetchOperatorTerms(ctx)
	if err != nil {
		return fmt.Errorf("unable to fetch operator "+
			"terms: %w", err)
	}

	roundCfg := &round.RoundClientConfig{
		Name:          "round-client",
		Logger:        log,
		Wallet:        clientWallet,
		RoundStore:    roundStore,
		VTXOStore:     roundStore,
		OperatorTerms: operatorTerms,
		ServerConn:    s.runtime.TellRef(),
		ChainSource:   chainSourceRef,
		WalletActor:   walletRef,
		ChainParams:   chainParams,
		ActorSystem:   s.actorSystem,
		TimeoutActor:  timeoutRef,
		ForfeitCollectionTimeout: s.cfg.
			ForfeitCollectionTimeout,
	}

	roundActor, err := round.NewRoundClientActor(
		roundCfg,
	).Unpack()
	if err != nil {
		return fmt.Errorf("unable to create round "+
			"actor: %w", err)
	}

	roundKey := round.NewServiceKey()
	roundRef := actor.RegisterWithSystem(
		s.actorSystem, "round-client",
		roundKey, roundActor,
	)

	// The round actor needs its own SelfRef for receiving
	// asynchronous notifications (e.g., chain confirmations).
	// We set it after registration since it's a circular dep.
	roundCfg.SelfRef = roundRef

	if err := roundActor.Start(ctx); err != nil {
		return fmt.Errorf("unable to start round "+
			"actor: %w", err)
	}

	log.InfoS(ctx, "Round actor registered and started")

	return nil
}

// initOORActor creates and starts the OOR (out-of-round) client actor.
//
// The OOR actor manages outgoing off-chain transfers: it drives the
// client-side FSM that builds Ark packages, signs checkpoints, and
// coordinates with the server via the serverconn transport. Transport
// outbox events (submit, finalize, ack) are routed through the
// ServerConn reference, while local events (signing, persistence) are
// handled by a layered OutboxHandler stack:
//
//   - LocalPersistenceOutboxHandler: marks inputs spent, materializes
//     incoming VTXOs, handles incoming ack.
//   - SigningOutboxHandler (Next delegate): signs Ark and checkpoint
//     PSBTs, schedules retries.
func (s *Server) initOORActor(ctx context.Context) error {
	clk := clock.NewDefaultClock()
	dbStore := db.NewStore(
		s.db.DB, s.db.Queries, s.db.Backend(), log,
	)

	clientWallet := lndbackend.NewClientWallet(
		s.lnd.Signer, s.lnd.WalletKit,
	)

	vtxoStore := dbStore.NewVTXOStore(clk)
	packageStore := dbStore.NewOORArtifactStore(clk)

	// Create the timeout actor for scheduling retry timers. When a
	// retry timer fires, the callback ref transforms the expiry into
	// a DriveEventRequest and Tell's it back to the OOR actor.
	timeoutActor := timeout.NewActor()

	signingHandler := &oor.SigningOutboxHandler{
		Signer:       clientWallet,
		TimeoutActor: timeoutActor,
	}

	outboxHandler := &oor.LocalPersistenceOutboxHandler{
		Next:         signingHandler,
		Store:        vtxoStore,
		PackageStore: packageStore,
	}

	s.oorActor = oor.NewOORClientActor(oor.ClientActorCfg{
		Logger:        log,
		OutboxHandler: outboxHandler,
		ServerConn:    s.runtime.TellRef(),
		PackageStore:  packageStore,
		DeliveryStore: s.deliveryStore,
		ActorSystem:   s.actorSystem,
		ActorID:       oor.OORActorServiceKeyName,
	})

	// Wire the timeout callback ref using the registered service
	// key. The service key resolves the OOR actor via the
	// receptionist, and the MapInputRef transforms
	// *timeout.ExpiredMsg into a DriveEventRequest with
	// RetryDueEvent targeting the correct session.
	oorKey := oor.NewServiceKey()
	signingHandler.CallbackRef = oor.NewRetryCallbackRef(
		oorKey.Ref(s.actorSystem),
	)

	log.InfoS(ctx, "OOR client actor started")

	return nil
}

// fetchOperatorTerms retrieves the operator's terms from the Ark
// server via the ArkService.GetInfo RPC. The terms include the
// operator pubkey, sweep delay, VTXO exit delay, forfeit script, dust
// limit, and fee rate. These are required before the round actor can
// start, as they govern all round signing and validation parameters.
func (s *Server) fetchOperatorTerms(
	ctx context.Context) (*types.OperatorTerms, error) {

	resp, err := s.ark.GetInfo(ctx, &arkrpc.GetInfoRequest{})
	if err != nil {
		return nil, fmt.Errorf("GetInfo RPC: %w", err)
	}

	if len(resp.Pubkey) == 0 {
		return nil, fmt.Errorf("operator pubkey is missing")
	}

	pubKey, err := btcec.ParsePubKey(resp.Pubkey)
	if err != nil {
		return nil, fmt.Errorf("parse operator pubkey: %w", err)
	}

	var sweepKey *btcec.PublicKey
	if len(resp.SweepKey) > 0 {
		sweepKey, err = btcec.ParsePubKey(resp.SweepKey)
		if err != nil {
			return nil, fmt.Errorf(
				"parse sweep key: %w", err,
			)
		}
	}

	return &types.OperatorTerms{
		PubKey:            pubKey,
		BoardingExitDelay: resp.BoardingExitDelay,
		VTXOExitDelay:     resp.VtxoExitDelay,
		ForfeitScript:     resp.ForfeitScript,
		SweepKey:          sweepKey,
		SweepDelay:        resp.SweepDelay,
		DustLimit:         btcutil.Amount(resp.DustLimit),
		MinBoardingAmount: btcutil.Amount(resp.MinBoardingAmount),
		MaxBoardingAmount: btcutil.Amount(resp.MaxBoardingAmount),
		FeeRate:           btcutil.Amount(resp.FeeRate),
		MinOperatorFee:    btcutil.Amount(resp.MinOperatorFee),
		MinConfirmations:  resp.MinConfirmations,
	}, nil
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

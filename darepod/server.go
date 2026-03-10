package darepod

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/db/actordelivery"
	"github.com/lightninglabs/darepo-client/indexer"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/lndbackend"
	"github.com/lightninglabs/darepo-client/lwwallet"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/rpc/oorpb"
	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo-client/timeout"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/lndclient"
	lndbuild "github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/signal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// WalletState represents the lifecycle state of the wallet subsystem.
// In lwwallet mode, the wallet transitions through these states during
// daemon startup: None → Locked → Ready (or None → Ready if the seed
// is provided via environment variable). The underlying type is int32
// so it can be stored in an atomic.Int32 for lock-free concurrent
// access.
type WalletState int32

const (
	// WalletStateNone indicates no wallet has been created yet.
	// The daemon accepts GenSeed and InitWallet RPCs in this state.
	WalletStateNone WalletState = iota

	// WalletStateLocked indicates an encrypted seed file exists but
	// the wallet has not been unlocked. The daemon accepts
	// UnlockWallet RPCs in this state.
	WalletStateLocked

	// WalletStateReady indicates the wallet is initialized and
	// operational. All wallet RPCs (GetBalance, NewAddress, etc.)
	// are available.
	WalletStateReady
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

// Server is the top-level daemon orchestrator. It owns the wallet
// backend (lnd or lwwallet), the mailbox transport runtime, the
// indexer client, and the daemon's own gRPC server.
type Server struct {
	cfg *Config

	logManager *lndbuild.SubLoggerManager

	db            *db.SqliteStore
	deliveryStore actor.DeliveryStore
	vtxoStore     *db.VTXOPersistenceStore

	// lnd holds the lndclient connection when wallet.type is "lnd".
	// It is None in lwwallet mode.
	lnd fn.Option[*lndclient.GrpcLndServices]

	// lwWallet holds the lightweight wallet instance when
	// wallet.type is "lwwallet". It is None in lnd mode.
	lwWallet fn.Option[*lwwallet.Wallet]

	// walletState tracks the lifecycle state of the wallet
	// subsystem. In lnd mode this is always WalletStateReady
	// after successful lnd connection. In lwwallet mode it
	// transitions through None → Locked → Ready. Stored as
	// atomic.Int32 for lock-free concurrent access from gRPC
	// handler goroutines and the startup goroutine. State
	// transitions use CompareAndSwap to prevent TOCTOU races.
	walletState atomic.Int32

	// walletReadyOnce ensures the walletReady channel is closed
	// exactly once, preventing a double-close panic if
	// markWalletReady is called concurrently.
	walletReadyOnce sync.Once

	// walletReady is closed when the wallet subsystem has been
	// fully initialized and is ready to service requests. RPC
	// handlers that require wallet access select on this channel.
	walletReady chan struct{}

	// chainParams identifies the active Bitcoin network. In lnd
	// mode this is populated from the lnd connection; in lwwallet
	// mode it is derived from the config's network string.
	chainParams *chaincfg.Params

	runtime *serverconn.Runtime
	ark     *arkrpc.ArkServiceMailboxClient
	indexer *indexer.Client

	actorSystem  *actor.ActorSystem
	chainBackend chainsource.ChainBackend
	walletRef    fn.Option[actor.ActorRef[
		wallet.WalletMsg, wallet.WalletResp,
	]]
	oorActor *oor.OORClientActor

	serverConn *grpc.ClientConn

	grpcServer *grpc.Server
	rpcServer  *RPCServer
	mailboxMux *mailboxrpc.ServeMux
}

// NewServer allocates a Server from a validated Config. The server is
// inert until RunUntilShutdown is called. The walletReady channel is
// initialized here so RPC handlers can select on it immediately.
func NewServer(cfg *Config) (*Server, error) {
	return &Server{
		cfg:         cfg,
		walletReady: make(chan struct{}),
	}, nil
}

// isWalletReady returns true if the wallet subsystem has been fully
// initialized. This is a non-blocking check.
func (s *Server) isWalletReady() bool {
	select {
	case <-s.walletReady:
		return true
	default:
		return false
	}
}

// markWalletReady atomically stores WalletStateReady and closes the
// walletReady channel, signaling to all waiting RPC handlers that the
// wallet is operational. The channel close is guarded by sync.Once to
// prevent a double-close panic if this method is called concurrently.
func (s *Server) markWalletReady() {
	s.walletState.Store(int32(WalletStateReady))

	s.walletReadyOnce.Do(func() {
		close(s.walletReady)
	})
}

// RunUntilShutdown starts all subsystems and blocks until the shutdown
// interceptor fires or a fatal error occurs. The startup sequence
// branches on the configured wallet type: in lnd mode, the daemon
// connects to an external lnd node and derives all backends from it;
// in lwwallet mode, the daemon starts an in-process wallet and may
// need to wait for wallet creation or unlock via RPC.
//
//nolint:funlen
func (s *Server) RunUntilShutdown(interceptor signal.Interceptor) error {
	// -------------------------------------------------------
	// 0. Initialize the logging backend and subsystem loggers.
	// -------------------------------------------------------
	// Create a log handler writing to stdout. The SubLoggerManager
	// manages per-subsystem loggers and supports runtime level changes.
	logHandler := btclog.NewDefaultHandler(os.Stdout)
	s.logManager = lndbuild.NewSubLoggerManager(logHandler)

	// Register all package-level loggers with the manager. This
	// replaces the default btclog.Disabled loggers so log output
	// is captured from this point forward.
	SetupLoggers(s.logManager, interceptor)

	// Apply the configured debug level. A bare level like "info"
	// sets all subsystems. A comma-separated list like
	// "ROND=debug,OORC=trace,info" applies per-subsystem overrides
	// with the bare value as the default.
	if err := s.applyDebugLevel(); err != nil {
		return fmt.Errorf("invalid debug level: %w", err)
	}

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
		slog.String("version", build.Version()),
		slog.String("commit", build.CommitHash),
		slog.String("network", s.cfg.Network),
		slog.String("wallet_type", s.cfg.Wallet.Type))

	// Derive chain params from the config network string. In lnd
	// mode this is overwritten by the lnd connection's chain
	// params, but we need it early for lwwallet mode.
	chainParams, err := networkToChainParams(s.cfg.Network)
	if err != nil {
		return fmt.Errorf("invalid network: %w", err)
	}
	s.chainParams = chainParams

	// -------------------------------------------------------
	// 1. Initialize wallet backend (lnd or lwwallet).
	// -------------------------------------------------------
	switch s.cfg.Wallet.Type {
	case WalletTypeLnd:
		if err := s.initLndBackend(ctx); err != nil {
			return err
		}
		defer s.lnd.UnsafeFromSome().Close()

	case WalletTypeLwwallet:
		// In lwwallet mode, we attempt auto-unlock here.
		// If no seed is available yet (no env var, no seed
		// file, or no password for unlock), the daemon
		// continues startup with the wallet in a non-ready
		// state and waits for InitWallet or UnlockWallet
		// RPCs.
		s.tryAutoUnlockLwwallet(ctx)

	default:
		return fmt.Errorf("unknown wallet type %q",
			s.cfg.Wallet.Type)
	}

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
	if err := s.initChainBackend(ctx); err != nil {
		return err
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

	// Create the VTXO store for RPC queries (ListVTXOs, GetBalance).
	clk := clock.NewDefaultClock()
	dbStore := db.NewStore(
		s.db.DB, s.db.Queries, s.db.Backend(), log,
	)
	s.vtxoStore = dbStore.NewVTXOStore(clk)

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
			slog.String("addr", s.cfg.RPC.ListenAddr))

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
	// 9-11. Register wallet, round, and OOR actors.
	// -------------------------------------------------------
	// These steps require the wallet to be ready. In lnd mode
	// the wallet is always ready at this point. In lwwallet
	// mode, if the wallet was auto-unlocked (via env var or
	// password file), it is also ready. Otherwise, these steps
	// are deferred until the wallet is unlocked via RPC (see
	// startWalletDependentActors).
	if s.isWalletReady() {
		if err := s.startWalletDependentActors(
			ctx, chainSourceRef, timeoutRef,
		); err != nil {
			return err
		}
	} else {
		// Launch a goroutine that waits for the wallet to
		// become ready (via InitWallet or UnlockWallet RPC)
		// and then starts the wallet-dependent actors.
		go func() {
			select {
			case <-s.walletReady:
			case <-ctx.Done():
				return
			}

			if err := s.startWalletDependentActors(
				ctx, chainSourceRef, timeoutRef,
			); err != nil {
				log.ErrorS(ctx,
					"Failed to start wallet actors",
					err)
			}
		}()

		log.InfoS(ctx, "Wallet not ready, waiting for "+
			"InitWallet or UnlockWallet RPC",
			slog.Int("state", int(
				s.walletState.Load(),
			)))
	}

	log.InfoS(ctx, "Daemon ready")

	// -------------------------------------------------------
	// 12. Block until shutdown.
	// -------------------------------------------------------
	<-interceptor.ShutdownChannel()

	log.InfoS(ctx, "Shutting down darepod")

	return nil
}

// initLndBackend connects to the lnd node and populates the server's
// lnd connection, chain params, and marks the wallet as ready.
func (s *Server) initLndBackend(ctx context.Context) error {
	log.InfoS(ctx, "Connecting to lnd",
		"host", s.cfg.Lnd.Host)

	lndServices, err := s.connectLnd(ctx)
	if err != nil {
		return fmt.Errorf("unable to connect to lnd: %w", err)
	}
	s.lnd = fn.Some(lndServices)

	// Use lnd's chain params as the authoritative source.
	s.chainParams = lndServices.ChainParams

	log.InfoS(ctx, "Connected to lnd",
		"alias", lndServices.NodeAlias,
		"pubkey", lndServices.NodePubkey)

	// In lnd mode the wallet is immediately ready.
	s.markWalletReady()

	return nil
}

// tryAutoUnlockLwwallet attempts to initialize the lwwallet backend
// at startup without user interaction. It checks for a seed in the
// environment variable first, then checks for an encrypted seed file
// on disk with a password from the environment or a password file.
// If neither source provides a complete seed+password pair, the daemon
// starts with the wallet in a non-ready state.
func (s *Server) tryAutoUnlockLwwallet(ctx context.Context) {
	// Check for a raw seed in the environment (dev/CI path).
	seed, err := LoadSeedFromEnv()
	if err != nil {
		log.WarnS(ctx, "Invalid seed in environment variable",
			err)

		return
	}

	if seed != nil {
		log.InfoS(ctx, "Loaded seed from environment variable")

		if err := s.startLwwallet(ctx, *seed); err != nil {
			log.ErrorS(ctx,
				"Failed to start lwwallet from env seed",
				err)

			return
		}

		return
	}

	networkDir, err := s.cfg.NetworkDir()
	if err != nil {
		log.ErrorS(ctx, "Unable to resolve network directory",
			err)

		return
	}

	// Check for an encrypted seed file on disk.
	if !SeedFileExists(networkDir) {
		log.InfoS(ctx, "No wallet seed found, awaiting "+
			"InitWallet RPC")

		s.walletState.Store(int32(WalletStateNone))

		return
	}

	// Encrypted seed exists. Try to find a password for
	// auto-unlock: check env var first, then password file.
	s.walletState.Store(int32(WalletStateLocked))

	password, ok := LoadPasswordFromEnv()
	if !ok && s.cfg.Wallet.PasswordFile != "" {
		var err error
		password, err = LoadPasswordFromFile(
			s.cfg.Wallet.PasswordFile,
		)
		if err != nil {
			log.WarnS(ctx,
				"Failed to read wallet password file",
				err)

			return
		}

		ok = true
	}

	if !ok {
		log.InfoS(ctx, "Encrypted seed found but no password "+
			"available, awaiting UnlockWallet RPC")

		return
	}

	// We have both seed file and password: auto-unlock.
	seedPath := SeedFilePath(networkDir)
	ciphertext, err := LoadEncryptedSeed(seedPath)
	if err != nil {
		log.ErrorS(ctx, "Failed to load encrypted seed", err)

		return
	}

	decryptedSeed, err := DecryptSeed(ciphertext, password)
	if err != nil {
		log.ErrorS(ctx, "Failed to decrypt seed at startup",
			err)

		return
	}

	log.InfoS(ctx, "Auto-unlocking lwwallet from encrypted seed")

	if err := s.startLwwallet(ctx, decryptedSeed); err != nil {
		log.ErrorS(ctx, "Failed to start lwwallet", err)

		return
	}
}

// startLwwallet creates and starts the lightweight wallet from the
// given raw seed. On success it populates s.lwWallet and marks the
// wallet as ready.
func (s *Server) startLwwallet(ctx context.Context,
	seed [rawSeedLen]byte) error {

	networkDir, err := s.cfg.NetworkDir()
	if err != nil {
		return fmt.Errorf("resolve network directory: %w", err)
	}

	pollInterval := s.cfg.Wallet.PollInterval
	if pollInterval == 0 {
		pollInterval = DefaultEsploraPollInterval
	}

	recoveryWindow := s.cfg.Wallet.RecoveryWindow
	if recoveryWindow == 0 {
		recoveryWindow = DefaultRecoveryWindow
	}

	w, err := lwwallet.New(lwwallet.Config{
		Seed:           seed,
		EsploraURL:     s.cfg.Wallet.EsploraURL,
		ChainParams:    s.chainParams,
		PollInterval:   pollInterval,
		RecoveryWindow: recoveryWindow,
		DBDir:          networkDir,
		Log:            fn.Some(log),
	})
	if err != nil {
		return fmt.Errorf("create lwwallet: %w", err)
	}

	if err := w.Start(); err != nil {
		return fmt.Errorf("start lwwallet: %w", err)
	}

	s.lwWallet = fn.Some(w)

	log.InfoS(ctx, "Lightweight wallet started")

	s.markWalletReady()

	return nil
}

// initChainBackend creates and starts the chain backend appropriate
// for the configured wallet type. In lnd mode it uses the lndclient
// chain notifier and fee estimator. In lwwallet mode it uses the
// lwwallet's Esplora-backed chain backend.
func (s *Server) initChainBackend(ctx context.Context) error {
	// alreadyStarted tracks whether the chain backend was
	// obtained from an already-running lwwallet, in which case
	// we must not call Start() again (it is not idempotent and
	// would create duplicate polling loops).
	var alreadyStarted bool

	switch s.cfg.Wallet.Type {
	case WalletTypeLnd:
		lndSvc := s.lnd.UnsafeFromSome()
		s.chainBackend = chainbackends.NewLNDBackendFromLndClient(
			chainbackends.LNDBackendFromLndClientConfig{
				LND: &lndSvc.LndServices,
			},
		)

	case WalletTypeLwwallet:
		// If the lwwallet is already started (auto-unlock
		// succeeded), use its chain backend. Otherwise, we
		// need a standalone Esplora chain backend that can
		// serve the chain source actor before the wallet is
		// ready.
		if s.lwWallet.IsSome() {
			w := s.lwWallet.UnsafeFromSome()
			s.chainBackend = w.ChainBackend()
			alreadyStarted = true
		} else {
			s.chainBackend = lwwallet.NewChainBackend(
				lwwallet.NewEsploraClient(
					s.cfg.Wallet.EsploraURL, log,
				),
				s.cfg.Wallet.PollInterval, log,
			)
		}

	default:
		return fmt.Errorf("unknown wallet type %q",
			s.cfg.Wallet.Type)
	}

	if !alreadyStarted {
		if err := s.chainBackend.Start(); err != nil {
			return fmt.Errorf(
				"unable to start chain backend: %w", err,
			)
		}
	}

	log.InfoS(ctx, "Chain backend started",
		"type", s.cfg.Wallet.Type)

	return nil
}

// startWalletDependentActors initializes and registers the wallet,
// round, and OOR actors. This is called either synchronously during
// startup (when the wallet is immediately ready) or asynchronously
// after an InitWallet/UnlockWallet RPC in lwwallet mode.
func (s *Server) startWalletDependentActors(ctx context.Context,
	chainSourceRef actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	],
	timeoutRef actor.TellOnlyRef[timeout.Msg]) error {

	// -------------------------------------------------------
	// 9. Register the wallet (boarding) actor.
	// -------------------------------------------------------
	walletRef, err := s.initWalletActor(ctx, chainSourceRef)
	if err != nil {
		return err
	}
	s.walletRef = fn.Some(walletRef)

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

	log.InfoS(ctx, "Wallet-dependent actors started")

	return nil
}

// applyDebugLevel parses the DebugLevel config string and applies it to
// the log manager. A bare level like "info" sets all subsystems globally.
// A comma-separated list like "ROND=debug,OORC=trace,info" applies
// per-subsystem overrides on top of the global default. Parsing uses a
// two-pass approach: first the last bare value (without '=') is applied
// as the global default for all subsystems, then per-subsystem overrides
// are applied. This ensures ordering does not matter — "ROND=debug,info"
// and "info,ROND=debug" produce the same result.
func (s *Server) applyDebugLevel() error {
	debugLevel := s.cfg.DebugLevel
	if debugLevel == "" {
		debugLevel = DefaultDebugLevel
	}

	// Check if this is a simple global level (no commas, no '=').
	if !strings.Contains(debugLevel, ",") &&
		!strings.Contains(debugLevel, "=") {

		_, ok := btclog.LevelFromString(debugLevel)
		if !ok {
			return fmt.Errorf("unknown log level %q",
				debugLevel)
		}

		s.logManager.SetLogLevels(debugLevel)

		return nil
	}

	// Two-pass parse of comma-separated subsystem=level pairs.
	// Pass 1 finds the last bare level (global default) and
	// validates all entries. Pass 2 applies per-subsystem
	// overrides on top of the global default, ensuring that
	// "ROND=debug,info" and "info,ROND=debug" behave identically.
	parts := strings.Split(debugLevel, ",")

	type subsystemLevel struct {
		subsystem string
		level     string
	}

	var (
		globalLevel string
		overrides   []subsystemLevel
	)

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		if !strings.Contains(part, "=") {
			// Bare level — candidate for global default.
			_, ok := btclog.LevelFromString(part)
			if !ok {
				return fmt.Errorf("unknown log level %q",
					part)
			}

			globalLevel = part

			continue
		}

		// Subsystem=level pair.
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return fmt.Errorf("malformed debug level %q",
				part)
		}

		subsystem := strings.TrimSpace(kv[0])
		level := strings.TrimSpace(kv[1])

		_, ok := btclog.LevelFromString(level)
		if !ok {
			return fmt.Errorf("unknown log level %q for "+
				"subsystem %q", level, subsystem)
		}

		overrides = append(overrides, subsystemLevel{
			subsystem: subsystem,
			level:     level,
		})
	}

	// Apply global default first so it doesn't clobber
	// per-subsystem overrides.
	if globalLevel != "" {
		s.logManager.SetLogLevels(globalLevel)
	}

	for _, o := range overrides {
		s.logManager.SetLogLevel(o.subsystem, o.level)
	}

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
	s.registerRoundEventRoutes(router)

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

// registerRoundEventRoutes registers round protocol server-push event
// routes with the EventRouter. When the server pushes round lifecycle
// events (batch built, nonces aggregated, etc.), the router decodes
// the roundpb proto, calls FromProto on the domain event type, wraps
// it in a ServerMessageNotification, and Tell's it to the round actor.
func (s *Server) registerRoundEventRoutes(
	router *serverconn.EventRouter) {

	roundKey := round.NewServiceKey()

	// Build tree deserialization options from the daemon config.
	// This caps the maximum node count in VTXO trees received
	// from the server, preventing memory exhaustion.
	var treeOpts []roundpb.TreeFromProtoOption
	if s.cfg.Server.MaxTreeNodes > 0 {
		treeOpts = append(
			treeOpts,
			roundpb.WithMaxTreeNodes(
				s.cfg.Server.MaxTreeNodes,
			),
		)
	}

	// addRoundRoute is a helper that registers a push event route.
	// It creates a fresh domain event via newEvent, deserializes
	// the proto into it via FromProto, then wraps it in a
	// ServerMessageNotification for delivery to the round actor.
	addRoundRoute := func(method string,
		newProto func() proto.Message,
		newEvent func() round.ClientEvent) {

		serverconn.AddRoute(
			router,
			serverconn.EventRouteConfig[
				actormsg.RoundReceivable,
				actormsg.RoundActorResp,
			]{
				Service:  roundpb.ServiceName,
				Method:   method,
				NewEvent: newProto,
				Key:      roundKey,
				Adapt:    roundEventAdapt(method, newEvent),
			},
		)
	}

	// BatchInfo: server built the commitment transaction.
	addRoundRoute(
		roundpb.MethodBatchInfo,
		func() proto.Message {
			return &roundpb.ClientBatchInfo{}
		},
		func() round.ClientEvent {
			return &round.CommitmentTxBuilt{
				TreeOpts: treeOpts,
			}
		},
	)

	// AwaitingInputSigs: server needs boarding input signatures.
	addRoundRoute(
		roundpb.MethodAwaitingInputSigs,
		func() proto.Message {
			return &roundpb.ClientAwaitingInputSigsResp{}
		},
		func() round.ClientEvent {
			return &round.AwaitingBoardingSigs{}
		},
	)

	// AggNonces: server sends aggregated MuSig2 nonces.
	addRoundRoute(
		roundpb.MethodAggNonces,
		func() proto.Message {
			return &roundpb.ClientVTXOAggNonces{}
		},
		func() round.ClientEvent {
			return &round.NoncesAggregated{}
		},
	)

	// AggSigs: server sends final aggregated signatures.
	addRoundRoute(
		roundpb.MethodAggSigs,
		func() proto.Message {
			return &roundpb.ClientVTXOAggSigs{}
		},
		func() round.ClientEvent {
			return &round.OperatorSigned{}
		},
	)

	// RoundFailed: server reports the round has failed.
	addRoundRoute(
		roundpb.MethodRoundFailed,
		func() proto.Message {
			return &roundpb.ClientRoundFailedResp{}
		},
		func() round.ClientEvent {
			return &round.BoardingFailed{}
		},
	)

	// Error: server reports a general error condition.
	addRoundRoute(
		roundpb.MethodError,
		func() proto.Message {
			return &roundpb.ClientErrorResp{}
		},
		func() round.ClientEvent {
			return &round.BoardingFailed{}
		},
	)
}

// roundEventAdapt returns an Adapt closure for a round push event.
// The closure creates a fresh domain event, populates it via FromProto,
// and wraps it in a ServerMessageNotification.
func roundEventAdapt(method string,
	newEvent func() round.ClientEvent) func(
	proto.Message) (actormsg.RoundReceivable, error) {

	return func(
		p proto.Message,
	) (actormsg.RoundReceivable, error) {

		ev := newEvent()

		inbound, ok := ev.(serverconn.InboundServerMessage)
		if !ok {
			return nil, fmt.Errorf(
				"event %T does not implement "+
					"InboundServerMessage", ev,
			)
		}

		if err := inbound.FromProto(p); err != nil {
			return nil, fmt.Errorf(
				"FromProto %s/%s: %w",
				roundpb.ServiceName, method, err,
			)
		}

		return &round.ServerMessageNotification{
			Message: ev,
		}, nil
	}
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
	networkDir, err := s.cfg.NetworkDir()
	if err != nil {
		return fmt.Errorf("resolve network directory: %w", err)
	}

	if err := os.MkdirAll(networkDir, 0700); err != nil {
		return fmt.Errorf("unable to create data dir: %w", err)
	}

	sqliteCfg := db.DefaultSqliteConfig(networkDir)

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

	// Determine the node identity pubkey for indexer registration.
	// In lnd mode this comes from the lnd connection. In lwwallet
	// mode, the identity is derived from the wallet keyring once
	// the wallet is ready.
	var identityPubkey string
	s.lnd.WhenSome(func(lndSvc *lndclient.GrpcLndServices) {
		identityPubkey = lndSvc.NodePubkey.String()
	})
	s.lwWallet.WhenSome(func(w *lwwallet.Wallet) {
		// Derive the node identity key from the wallet
		// keyring. This is safe even if the wallet isn't
		// fully synced, as it only depends on the seed.
		desc, err := w.DeriveKey(ctx, keychain.KeyLocator{
			Family: identityKeyFamily,
			Index:  0,
		})
		if err != nil {
			log.WarnS(ctx,
				"Unable to derive identity key for "+
					"indexer", err)
		} else {
			identityPubkey = fmt.Sprintf(
				"%x",
				desc.PubKey.SerializeCompressed(),
			)
		}
	})

	// TODO(roasbeef): wire SchnorrSigner from lnd backend once
	// indexer proof-of-control is enabled in the daemon.
	s.indexer = indexer.New(
		s.runtime.Unary(), nil,
		s.cfg.Server.RemoteMailboxID,
		identityPubkey,
	)

	log.InfoS(ctx, "RPC clients initialized")
}

// initWalletActor creates, registers, and starts the boarding wallet
// actor. The wallet manages key derivation, address creation, and
// boarding UTXO tracking. It receives block epoch notifications from
// the chain source actor and can forward confirmation events to
// registered notifiers (e.g., the round actor).
//
// The boarding backend is selected based on the wallet type: in lnd
// mode it uses lndbackend.BoardingBackend, in lwwallet mode it uses
// the lwwallet's BoardingBackendAdapter.
func (s *Server) initWalletActor(ctx context.Context,
	chainSourceRef actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]) (actor.ActorRef[wallet.WalletMsg, wallet.WalletResp], error) {

	clk := clock.NewDefaultClock()

	dbStore := db.NewStore(
		s.db.DB, s.db.Queries, s.db.Backend(), log,
	)
	boardingStore := dbStore.NewBoardingStore(s.chainParams, clk)

	// Select the boarding backend based on wallet type.
	var boardingBackend wallet.BoardingBackend
	switch s.cfg.Wallet.Type {
	case WalletTypeLnd:
		lndSvc := s.lnd.UnsafeFromSome()
		boardingBackend = lndbackend.NewBoardingBackend(
			lndSvc.WalletKit,
		)

	case WalletTypeLwwallet:
		w := s.lwWallet.UnsafeFromSome()
		boardingBackend = w.BoardingBackend()
	}

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

	// Select the client wallet (signing) backend based on
	// wallet type. In lnd mode, signing goes through lnd's
	// remote signer. In lwwallet mode, signing is in-process
	// via btcwallet.
	var clientWallet round.ClientWallet
	switch s.cfg.Wallet.Type {
	case WalletTypeLnd:
		lndSvc := s.lnd.UnsafeFromSome()
		clientWallet = lndbackend.NewClientWallet(
			lndSvc.Signer, lndSvc.WalletKit,
		)

	case WalletTypeLwwallet:
		clientWallet = s.lwWallet.UnsafeFromSome()
	}

	clk := clock.NewDefaultClock()

	dbStore := db.NewStore(
		s.db.DB, s.db.Queries, s.db.Backend(), log,
	)
	roundStore := dbStore.NewRoundStore(s.chainParams, clk)

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
		ChainParams:   s.chainParams,
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

	// Select the OOR signer based on wallet type. The
	// SigningOutboxHandler only needs input.Signer for signing
	// checkpoint PSBTs.
	var oorSigner input.Signer
	switch s.cfg.Wallet.Type {
	case WalletTypeLnd:
		lndSvc := s.lnd.UnsafeFromSome()
		oorSigner = lndbackend.NewClientWallet(
			lndSvc.Signer, lndSvc.WalletKit,
		)

	case WalletTypeLwwallet:
		oorSigner = s.lwWallet.UnsafeFromSome()
	}

	vtxoStore := dbStore.NewVTXOStore(clk)
	packageStore := dbStore.NewOORArtifactStore(clk)

	// Create the timeout actor for scheduling retry timers. When a
	// retry timer fires, the callback ref transforms the expiry into
	// a DriveEventRequest and Tell's it back to the OOR actor.
	timeoutActor := timeout.NewActor()

	signingHandler := &oor.SigningOutboxHandler{
		Signer:       oorSigner,
		TimeoutActor: timeoutActor,
	}

	outboxHandler := &oor.LocalPersistenceOutboxHandler{
		Next:         signingHandler,
		Store:        vtxoStore,
		PackageStore: packageStore,
	}

	s.oorActor = oor.NewOORClientActor(oor.ClientActorCfg{
		Log:           fn.Some(log),
		OutboxHandler: outboxHandler,
		ServerConn:    s.runtime.TellRef(),
		PackageStore:  packageStore,
		DeliveryStore: s.deliveryStore,
		ActorSystem:   s.actorSystem,
		ActorID:       oor.OORActorServiceKeyName,
	})

	// Wire the timeout callback ref using the registered service
	// key. The OOR actor self-registers with the actor system
	// during NewOORClientActor (via durable.Start and
	// RegisterWithReceptionist). The service key resolves the
	// OOR actor via the receptionist, and the MapInputRef
	// transforms *timeout.ExpiredMsg into a DriveEventRequest
	// with RetryDueEvent targeting the correct session.
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

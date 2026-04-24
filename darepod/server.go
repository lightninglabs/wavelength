package darepod

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/btcwbackend"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/db/actordelivery"
	"github.com/lightninglabs/darepo-client/indexer"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/lndbackend"
	"github.com/lightninglabs/darepo-client/lwwallet"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo-client/proofkeys"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/rpc/oorpb"
	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo-client/timeout"
	"github.com/lightninglabs/darepo-client/txconfirm"
	"github.com/lightninglabs/darepo-client/unroll"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/darepo-client/walletcore"
	"github.com/lightninglabs/lndclient"
	lndbuild "github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
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

const (
	// arkServiceName is the protobuf service name for ArkService
	// events pushed by the server indexer.
	arkServiceName = "arkrpc.ArkService"

	// indexerProofServerID is the operator identifier currently used by
	// the daemon indexer service when verifying taproot proof messages.
	//
	// This is intentionally decoupled from mailbox transport IDs:
	// remote mailbox IDs can be per-client routing endpoints (for
	// auto-registration), while proof server ID is a logical operator
	// identity shared by all clients.
	indexerProofServerID = "arkd"

	// MethodIncomingOOR is the routing method name for incoming
	// OOR transfer notifications pushed by the server indexer.
	MethodIncomingOOR = "IncomingOOR"

	// MethodIncomingVTXO is the routing method name for incoming
	// VTXO lifecycle events pushed by the server indexer.
	MethodIncomingVTXO = "IncomingVTXO"
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
	clk clock.Clock

	logManager *lndbuild.SubLoggerManager
	loggers    SubLoggers
	log        btclog.Logger

	db            *db.SqliteStore
	deliveryStore actor.DeliveryStore
	vtxoStore     *db.VTXOPersistenceStore
	roundStore    *db.RoundPersistenceStore
	ueStore       *db.UnilateralExitPersistenceStore

	// lnd holds the lndclient connection when wallet.type is "lnd".
	// It is None in lwwallet mode.
	lnd fn.Option[*lndclient.GrpcLndServices]

	// lwWallet holds the lightweight wallet instance when
	// wallet.type is "lwwallet". It is None in lnd mode.
	lwWallet fn.Option[*lwwallet.Wallet]

	// btcwWallet holds the neutrino-backed wallet instance when
	// wallet.type is "btcwallet". It is None in other modes.
	btcwWallet fn.Option[*btcwbackend.Wallet]

	// neutrinoSvc holds the pre-started neutrino chain service
	// when wallet.type is "btcwallet". Started early in daemon
	// startup (before seed is available) so P2P peer connection
	// and header sync can proceed in parallel. Reused by
	// startBtcwallet when the wallet is finally unlocked.
	neutrinoSvc fn.Option[*btcwbackend.NeutrinoService]

	// runCtx is the context that spans the server's entire Run
	// lifecycle. It is set at the start of run() and cancelled
	// when the daemon shuts down. Background goroutines that
	// must outlive RPC handlers but not the daemon should
	// select on runCtx.Done().
	runCtx context.Context //nolint:containedctx

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

	// daemonReady is closed when all startup steps have
	// completed: wallet initialized, mailbox transport
	// connected, and wallet-dependent actors started. Test
	// harnesses should wait on this before issuing RPCs that
	// require the full stack.
	daemonReady     chan struct{}
	daemonReadyOnce sync.Once

	// chainParams identifies the active Bitcoin network. In lnd
	// mode this is populated from the lnd connection; in lwwallet
	// mode it is derived from the config's network string.
	chainParams *chaincfg.Params

	// clientKeyDesc is the client's identity key descriptor,
	// derived during wallet initialization.
	clientKeyDesc keychain.KeyDescriptor

	// localMailboxID caches the pubkey-derived mailbox ID for
	// this client, avoiding repeated hex encoding of the
	// identity key.
	localMailboxID string

	// authSigHex caches the hex-encoded Schnorr auth signature
	// so response envelopes can include it without re-computing.
	authSigHex string

	runtime *serverconn.Runtime
	ark     *arkrpc.ArkServiceMailboxClient
	indexer *indexer.Client

	// proofKeyBackend derives wallet-managed keys and produces proof
	// signers for daemon-owned receive scripts and indexer identity.
	proofKeyBackend proofkeys.Backend

	// operatorTerms caches the operator policy fetched during daemon
	// bootstrap so local RPC callers can inspect the current server
	// terms. It is stored atomically because startup writes race with
	// concurrent GetInfo RPC reads.
	operatorTerms atomic.Pointer[types.OperatorTerms]

	// serverConnected reports whether mailbox ingress is currently
	// running against the Ark operator. It flips true once ingress
	// starts successfully and flips false again during shutdown.
	serverConnected atomic.Bool

	actorSystem  *actor.ActorSystem
	chainBackend chainsource.ChainBackend
	walletRef    fn.Option[actor.ActorRef[
		wallet.WalletMsg, wallet.WalletResp,
	]]
	oorActor *oor.OORClientActor

	// ledgerStore exposes the client-side ledger DB adapter for
	// read-only RPC handlers (GetFeeHistory). Writes go through
	// the ledger actor; this field is for queries only.
	ledgerStore *db.LedgerStoreDB

	// vtxoMgrRef is the VTXO manager actor ref used by the RPC
	// layer to route manual unroll through the VTXO lifecycle.
	vtxoMgrRef fn.Option[actor.ActorRef[
		vtxo.ManagerMsg, vtxo.ManagerResp,
	]]

	// unrollRegistryRef is the actor ref for the unilateral-exit registry.
	// Set during daemon initialization when the unroll subsystem is wired.
	unrollRegistryRef fn.Option[actor.ActorRef[
		unroll.RegistryMsg, unroll.RegistryResp,
	]]
	unrollRegistry *unroll.UnrollRegistryActor

	// lazyChainResolver is the forwarding ref that connects the
	// VTXO manager's critical-expiry path to the unroll manager.
	// Created before the VTXO manager so actors can reference it
	// immediately, then wired to the real target once the unroll
	// subsystem is initialized.
	lazyChainResolver *vtxo.LazyChainResolver

	serverConn *grpc.ClientConn

	rpcAddrMu sync.RWMutex
	rpcAddr   net.Addr

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
		clk:         clock.NewDefaultClock(),
		walletReady: make(chan struct{}),
		daemonReady: make(chan struct{}),
	}, nil
}

func (s *Server) subLogger(tag string) btclog.Logger {
	if s.loggers == nil {
		return btclog.Disabled
	}

	logger, ok := s.loggers[tag]
	if !ok || logger == nil {
		return btclog.Disabled
	}

	return logger
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

// WalletType returns the configured wallet backend type string.
func (s *Server) WalletType() string {
	return s.cfg.Wallet.Type
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

// markDaemonReady closes the daemonReady channel, signaling that all
// startup steps (wallet + mailbox + actors) have completed.
func (s *Server) markDaemonReady() {
	s.daemonReadyOnce.Do(func() {
		close(s.daemonReady)
	})
}

// DaemonReady returns a channel that is closed when the daemon has
// fully started: wallet initialized, mailbox connected, and all
// actors registered. Test harnesses should wait on this before
// issuing RPCs that require the full stack.
func (s *Server) DaemonReady() <-chan struct{} {
	return s.daemonReady
}

// GetStoredVTXO returns the persisted VTXO descriptor for the given
// outpoint. This method exists for test harnesses only; it lets
// server-side integration tests inspect locally stored partial-unroll
// state without reaching into internal daemon fields.
func (s *Server) GetStoredVTXO(ctx context.Context,
	outpoint wire.OutPoint) (*vtxo.Descriptor, error) {

	if s.vtxoStore == nil {
		return nil, fmt.Errorf("client daemon VTXO " +
			"store not initialized")
	}

	return s.vtxoStore.GetVTXO(ctx, outpoint)
}

// RPCAddr returns the bound daemon gRPC listener address once startup has
// progressed far enough to create the listener.
func (s *Server) RPCAddr() net.Addr {
	s.rpcAddrMu.RLock()
	defer s.rpcAddrMu.RUnlock()

	return s.rpcAddr
}

// loadOperatorTerms returns the latest cached operator terms snapshot, if one
// has been fetched during this daemon session.
func (s *Server) loadOperatorTerms() *types.OperatorTerms {
	return s.operatorTerms.Load()
}

// storeOperatorTerms replaces the cached operator terms snapshot. The terms
// are only refreshed during bootstrap today; future reconnect handling should
// update this when server policy changes are observed.
func (s *Server) storeOperatorTerms(terms *types.OperatorTerms) {
	s.operatorTerms.Store(terms)
}

// isServerConnected returns the latest mailbox-ingress connectivity signal
// reported by the daemon runtime.
func (s *Server) isServerConnected() bool {
	return s.serverConnected.Load()
}

// setServerConnected updates the mailbox-ingress connectivity signal used by
// GetInfo.
func (s *Server) setServerConnected(connected bool) {
	s.serverConnected.Store(connected)
}

// openRPCListener returns the daemon RPC listener. Embedders can inject a
// pre-created listener through the config, while the standalone daemon path
// binds a fresh TCP listener on ListenAddr. If both are provided, the injected
// listener takes precedence because the embedder already owns the transport.
func (s *Server) openRPCListener() (net.Listener, error) {
	if s.cfg.RPC.Listener != nil {
		s.rpcAddrMu.Lock()
		s.rpcAddr = s.cfg.RPC.Listener.Addr()
		s.rpcAddrMu.Unlock()

		return s.cfg.RPC.Listener, nil
	}

	lis, err := net.Listen("tcp", s.cfg.RPC.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("unable to listen on %s: %w",
			s.cfg.RPC.ListenAddr, err)
	}

	s.rpcAddrMu.Lock()
	s.rpcAddr = lis.Addr()
	s.rpcAddrMu.Unlock()

	return lis, nil
}

// RunUntilShutdown starts all subsystems and blocks until the shutdown
// interceptor fires or a fatal error occurs. The startup sequence
// branches on the configured wallet type: in lnd mode, the daemon
// connects to an external lnd node and derives all backends from it;
// in lwwallet mode, the daemon starts an in-process wallet and may
// need to wait for wallet creation or unlock via RPC.
func (s *Server) RunUntilShutdown(interceptor signal.Interceptor) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel the context when the interceptor fires so blocking
	// calls (like lndclient chain sync) unblock promptly.
	go func() {
		select {
		case <-interceptor.ShutdownChannel():
			cancel()

		case <-ctx.Done():
		}
	}()

	// Build a shutdown callback from the interceptor for the
	// logging subsystem.
	shutdownFn := func() {
		if !interceptor.Listening() {
			return
		}

		interceptor.RequestShutdown()
	}

	return s.run(ctx, shutdownFn)
}

// RunWithContext starts all subsystems and blocks until the given
// context is cancelled. This is the harness-friendly entry point:
// callers manage daemon lifecycle via context cancellation instead
// of requiring a signal.Interceptor (which is process-global).
// The derived cancel function is passed as shutdownFn so that
// critical log events can trigger graceful shutdown.
func (s *Server) RunWithContext(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	return s.run(ctx, cancel)
}

// run is the shared core startup logic for both RunUntilShutdown
// and RunWithContext. The shutdownFn is wired into the logging
// subsystem so critical log events can trigger daemon shutdown.
//
//nolint:funlen
func (s *Server) run(ctx context.Context,
	shutdownFn func()) error {

	// Store the run context so background goroutines (like the
	// btcwallet sync poller) can outlive individual RPC
	// handlers but still shut down with the daemon.
	s.runCtx = ctx

	// -------------------------------------------------------
	// 0. Initialize the logging backend and subsystem loggers.
	// -------------------------------------------------------
	// Create a log handler writing to stdout. The SubLoggerManager
	// manages per-subsystem loggers and supports runtime level changes.
	logWriter := s.cfg.LogWriter
	if logWriter == nil {
		logWriter = os.Stdout
	}

	logHandler := btclog.NewDefaultHandler(logWriter)
	s.logManager = lndbuild.NewSubLoggerManager(logHandler)

	// Register all subsystem loggers with the manager so later instance
	// wiring can attach explicit loggers without relying on package-
	// level globals.
	s.loggers = SetupLoggersWithShutdownFn(s.logManager, shutdownFn)
	s.log = s.subLogger(Subsystem)

	// Apply the configured debug level. A bare level like "info"
	// sets all subsystems. A comma-separated list like
	// "ROND=debug,OORC=trace,info" applies per-subsystem overrides
	// with the bare value as the default.
	if err := s.applyDebugLevel(); err != nil {
		return fmt.Errorf("invalid debug level: %w", err)
	}

	s.log.InfoS(ctx, "Starting darepod",
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

	case WalletTypeBtcwallet:
		// In btcwallet mode, use the same auto-unlock flow
		// as lwwallet but start a neutrino-backed wallet.
		s.tryAutoUnlockBtcwallet(ctx)

		// Register neutrino cleanup immediately so it fires
		// even if a later startup step (dialServer, actor
		// system init) fails before the main defer block.
		defer func() {
			s.neutrinoSvc.WhenSome(
				func(svc *btcwbackend.NeutrinoService) {
					_ = svc.Stop()
				},
			)
		}()

	default:
		return fmt.Errorf("unknown wallet type %q",
			s.cfg.Wallet.Type)
	}

	// NOTE: Identity key derivation, server connection, and
	// mailbox transport wiring are deferred to
	// connectAndBootstrapMailbox(), which runs either
	// synchronously (LND, wallet always ready) or
	// asynchronously after wallet unlock (lwwallet/btcwallet).
	// The gRPC server must start first so InitWallet/
	// UnlockWallet RPCs can arrive.

	// -------------------------------------------------------
	// 3. Initialize the actor system.
	// -------------------------------------------------------
	s.actorSystem = actor.NewActorSystemWithConfig(actor.SystemConfig{
		MailboxCapacity: actor.DefaultConfig().MailboxCapacity,
		Log:             fn.Some(s.subLogger(actor.Subsystem)),
	})
	// Register cleanup from least dependent to most dependent so that the
	// defer LIFO order tears down components from most dependent to least
	// dependent: actor system -> chain backend -> DB.
	defer func() {
		if s.runtime != nil {
			s.setServerConnected(false)
			s.runtime.Stop()
		}
	}()
	defer func() {
		if s.serverConn != nil {
			_ = s.serverConn.Close()
		}
	}()
	defer func() {
		if s.db != nil {
			_ = s.db.Close()
		}
	}()
	defer func() {
		if s.chainBackend != nil {
			_ = s.chainBackend.Stop()
		}
	}()
	defer func() {
		s.lwWallet.WhenSome(
			func(w *lwwallet.Wallet) {
				w.Stop()
			},
		)
	}()
	defer func() {
		s.btcwWallet.WhenSome(
			func(w *btcwbackend.Wallet) {
				w.Stop()
			},
		)
	}()
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(
			context.Background(), DefaultShutdownTimeout,
		)
		defer shutdownCancel()

		if s.runtime != nil {
			s.setServerConnected(false)
			_ = s.runtime.StopAndWait(shutdownCtx)
		}
	}()
	defer func() {
		if s.unrollRegistry != nil {
			s.unrollRegistry.Stop()
		}
	}()

	s.log.InfoS(ctx, "Actor system initialized")

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

	// For btcwallet mode when the wallet is not yet unlocked,
	// s.chainBackend is nil because neutrino requires the full
	// service to be running. In this case, chain source actor
	// registration is deferred until startBtcwallet populates
	// s.chainBackend. The wallet-dependent actors (which are
	// the only consumers) are also deferred behind walletReady.
	var chainSourceRef actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]
	if s.chainBackend != nil {
		chainSourceRef = s.registerChainSourceActor(ctx)
	}

	// -------------------------------------------------------
	// 5. Open the database and create the delivery store.
	// -------------------------------------------------------
	if err := s.initDatabase(ctx); err != nil {
		return err
	}

	// Create the VTXO store for RPC queries (ListVTXOs, GetBalance).
	dbStore := db.NewStore(
		s.db.DB, s.db.Queries, s.db.Backend(),
		s.subLogger(db.Subsystem),
	)
	s.vtxoStore = dbStore.NewVTXOStore(s.clk)

	// Start the ledger accounting actor. This must happen after
	// the DB and delivery store are ready but does not depend on
	// the wallet being unlocked.
	if err := s.initLedgerActor(ctx); err != nil {
		return err
	}

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
		s.mailboxMux,
		&rpcMailboxAdapter{RPCServer: s.rpcServer},
	)

	lis, err := s.openRPCListener()
	if err != nil {
		return err
	}

	go func() {
		s.log.InfoS(ctx, "gRPC server listening",
			slog.String("addr", lis.Addr().String()))

		if err := s.grpcServer.Serve(lis); err != nil {
			s.log.ErrorS(ctx, "gRPC server error", err)
		}
	}()
	defer func() {
		stopped := make(chan struct{})

		go func() {
			s.grpcServer.GracefulStop()
			close(stopped)
		}()

		select {
		case <-stopped:
		case <-time.After(DefaultShutdownTimeout):
			s.grpcServer.Stop()
			<-stopped
		}
	}()

	// -------------------------------------------------------
	// 7-11. Mailbox transport + wallet-dependent actors.
	// -------------------------------------------------------
	// The mailbox transport requires the identity key (derived
	// from the wallet) and the operator pubkey (fetched via
	// direct gRPC). In LND mode the wallet is always ready, so
	// this runs synchronously. In lwwallet/btcwallet mode the
	// wallet may not be unlocked yet, so everything is deferred
	// to a background goroutine that fires after walletReady.
	if s.isWalletReady() {
		if err := s.connectAndBootstrapMailbox(ctx); err != nil {
			return err
		}

		if err := s.startWalletDependentActors(
			ctx, chainSourceRef, timeoutRef,
		); err != nil {
			return err
		}

		if err := s.startMailboxIngress(ctx); err != nil {
			return err
		}

		s.markDaemonReady()
	} else {
		// Launch a goroutine that waits for the wallet to
		// become ready (via InitWallet or UnlockWallet RPC)
		// and then bootstraps the full mailbox + actor stack.
		// The WaitGroup ensures the goroutine completes (or
		// is cancelled) before RunWithContext returns and
		// defers start tearing down resources.
		var deferredWg sync.WaitGroup
		deferredWg.Add(1)
		go func() {
			defer deferredWg.Done()

			select {
			case <-s.walletReady:
			case <-ctx.Done():
				return
			}

			// For btcwallet mode, the chain source actor
			// was deferred because s.chainBackend was nil
			// at startup. Now that startBtcwallet has run,
			// register it before starting dependent actors.
			if chainSourceRef == nil {
				chainSourceRef = s.registerChainSourceActor(
					ctx,
				)
			}

			if err := s.connectAndBootstrapMailbox(
				ctx,
			); err != nil {
				s.log.ErrorS(ctx,
					"Failed to bootstrap mailbox "+
						"transport", err,
				)

				return
			}

			if err := s.startWalletDependentActors(
				ctx, chainSourceRef, timeoutRef,
			); err != nil {
				s.log.ErrorS(ctx,
					"Failed to start wallet actors",
					err)

				return
			}

			if err := s.startMailboxIngress(ctx); err != nil {
				s.log.ErrorS(ctx,
					"Failed to start mailbox ingress",
					err)

				return
			}

			s.markDaemonReady()
		}()
		defer deferredWg.Wait()

		s.log.InfoS(ctx, "Wallet not ready, waiting for "+
			"InitWallet or UnlockWallet RPC",
			slog.Int("state", int(
				s.walletState.Load(),
			)))
	}

	s.log.InfoS(ctx, "Daemon ready")

	// -------------------------------------------------------
	// 12. Block until shutdown.
	// -------------------------------------------------------
	<-ctx.Done()

	s.log.InfoS(ctx, "Shutting down darepod")

	return nil
}

// initLndBackend connects to the lnd node and populates the server's
// lnd connection, chain params, and marks the wallet as ready.
func (s *Server) initLndBackend(ctx context.Context) error {
	s.log.InfoS(ctx, "Connecting to lnd",
		"host", s.cfg.Lnd.Host)

	lndServices, err := s.connectLnd(ctx)
	if err != nil {
		return fmt.Errorf("unable to connect to lnd: %w", err)
	}
	s.lnd = fn.Some(lndServices)
	s.refreshProofKeyBackend()

	// Use lnd's chain params as the authoritative source.
	s.chainParams = lndServices.ChainParams

	s.log.InfoS(ctx, "Connected to lnd",
		"alias", lndServices.NodeAlias,
		"pubkey", lndServices.NodePubkey)

	// In lnd mode the wallet is immediately ready.
	s.markWalletReady()

	return nil
}

// refreshProofKeyBackend resolves the active wallet backend to the shared
// proof-key capability used by daemon-owned receive scripts and indexer proof
// generation.
func (s *Server) refreshProofKeyBackend() {
	switch s.cfg.Wallet.Type {
	case WalletTypeLnd:
		if s.lnd.IsSome() {
			lndSvc := s.lnd.UnsafeFromSome()
			s.proofKeyBackend = lndbackend.NewProofKeyBackend(
				lndSvc.WalletKit, lndSvc.Signer,
			)

			return
		}

	case WalletTypeLwwallet:
		if s.lwWallet.IsSome() {
			s.proofKeyBackend = s.lwWallet.UnsafeFromSome()

			return
		}

	case WalletTypeBtcwallet:
		if s.btcwWallet.IsSome() {
			s.proofKeyBackend = s.btcwWallet.UnsafeFromSome()

			return
		}
	}

	s.proofKeyBackend = nil
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
		s.log.WarnS(ctx, "Invalid seed in environment variable",
			err)

		return
	}

	if seed != nil {
		s.log.InfoS(ctx, "Loaded seed from environment variable")

		if err := s.startLwwallet(ctx, *seed); err != nil {
			s.log.ErrorS(ctx,
				"Failed to start lwwallet from env seed",
				err)

			return
		}

		return
	}

	networkDir, err := s.cfg.NetworkDir()
	if err != nil {
		s.log.ErrorS(ctx, "Unable to resolve network directory",
			err)

		return
	}

	// Check for an encrypted seed file on disk.
	if !SeedFileExists(networkDir) {
		s.log.InfoS(ctx, "No wallet seed found, awaiting "+
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
			s.log.WarnS(ctx,
				"Failed to read wallet password file",
				err)

			return
		}

		ok = true
	}

	if !ok {
		s.log.InfoS(ctx, "Encrypted seed found but no password "+
			"available, awaiting UnlockWallet RPC")

		return
	}

	// We have both seed file and password: auto-unlock.
	seedPath := SeedFilePath(networkDir)
	ciphertext, err := LoadEncryptedSeed(seedPath)
	if err != nil {
		s.log.ErrorS(ctx, "Failed to load encrypted seed", err)

		return
	}

	decryptedSeed, err := DecryptSeed(ciphertext, password)
	if err != nil {
		s.log.ErrorS(ctx, "Failed to decrypt seed at startup",
			err)

		return
	}

	s.log.InfoS(ctx, "Auto-unlocking lwwallet from encrypted seed")

	if err := s.startLwwallet(ctx, decryptedSeed); err != nil {
		s.log.ErrorS(ctx, "Failed to start lwwallet", err)

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
		Log:            fn.Some(s.subLogger(lwwallet.Subsystem)),
	})
	if err != nil {
		return fmt.Errorf("create lwwallet: %w", err)
	}

	if err := w.Start(); err != nil {
		return fmt.Errorf("start lwwallet: %w", err)
	}

	s.lwWallet = fn.Some(w)
	s.refreshProofKeyBackend()

	// Refresh the RPC clients once the wallet is available so the
	// indexer client picks up the wallet-backed identity key and signer
	// before any deferred wallet-dependent actors start.
	if s.runtime != nil {
		s.initRPCClients(ctx)
	}

	s.log.InfoS(ctx, "Lightweight wallet started")

	s.markWalletReady()

	return nil
}

// tryAutoUnlockBtcwallet attempts to initialize the btcwallet+neutrino
// backend at startup without user interaction. It follows the same
// pattern as tryAutoUnlockLwwallet: check for a seed in the
// environment, then check for an encrypted seed file with a password.
//
// Neutrino is started eagerly before checking for a wallet seed so
// that P2P peer connection and header sync can proceed in parallel
// while the daemon waits for the InitWallet or UnlockWallet RPC.
// The pre-started service is stored in s.neutrinoSvc and reused by
// startBtcwallet when the seed becomes available.
func (s *Server) tryAutoUnlockBtcwallet(ctx context.Context) {
	// Start neutrino early so it can connect to P2P peers and
	// sync headers while we wait for the wallet seed. This
	// dramatically reduces the time startBtcwallet needs when
	// the UnlockWallet RPC finally arrives.
	if err := s.preStartNeutrino(ctx); err != nil {
		s.log.ErrorS(ctx,
			"Failed to pre-start neutrino service", err)

		// Non-fatal: startBtcwallet will create its own
		// neutrino service if s.neutrinoSvc is nil.
	}

	// Check for a raw seed in the environment (dev/CI path).
	seed, err := LoadSeedFromEnv()
	if err != nil {
		s.log.WarnS(ctx,
			"Invalid seed in environment variable", err)

		return
	}

	if seed != nil {
		s.log.InfoS(ctx,
			"Loaded seed from environment variable")

		if err := s.startBtcwallet(ctx, *seed); err != nil {
			s.log.ErrorS(ctx,
				"Failed to start btcwallet from env seed",
				err)

			return
		}

		return
	}

	networkDir, err := s.cfg.NetworkDir()
	if err != nil {
		s.log.ErrorS(ctx,
			"Unable to resolve network directory", err)

		return
	}

	// Check for an encrypted seed file on disk.
	if !SeedFileExists(networkDir) {
		s.log.InfoS(ctx,
			"No wallet seed found, awaiting InitWallet RPC")

		s.walletState.Store(int32(WalletStateNone))

		return
	}

	// Encrypted seed exists. Try to find a password for
	// auto-unlock.
	s.walletState.Store(int32(WalletStateLocked))

	password, ok := LoadPasswordFromEnv()
	if !ok && s.cfg.Wallet.PasswordFile != "" {
		var err error
		password, err = LoadPasswordFromFile(
			s.cfg.Wallet.PasswordFile,
		)
		if err != nil {
			s.log.WarnS(ctx,
				"Failed to read wallet password file",
				err)

			return
		}

		ok = true
	}

	if !ok {
		s.log.InfoS(ctx, "Encrypted seed found but no "+
			"password available, awaiting UnlockWallet "+
			"RPC")

		return
	}

	// We have both seed file and password: auto-unlock.
	seedPath := SeedFilePath(networkDir)
	ciphertext, err := LoadEncryptedSeed(seedPath)
	if err != nil {
		s.log.ErrorS(ctx,
			"Failed to load encrypted seed", err)

		return
	}

	decryptedSeed, err := DecryptSeed(ciphertext, password)
	if err != nil {
		s.log.ErrorS(ctx,
			"Failed to decrypt seed at startup", err)

		return
	}

	s.log.InfoS(ctx,
		"Auto-unlocking btcwallet from encrypted seed")

	if err := s.startBtcwallet(ctx, decryptedSeed); err != nil {
		s.log.ErrorS(ctx,
			"Failed to start btcwallet", err)

		return
	}
}

// preStartNeutrino creates and starts the neutrino chain service
// early so it can begin P2P peer connection and header/filter sync
// while the daemon waits for a wallet seed. The started service is
// stored in s.neutrinoSvc for reuse by startBtcwallet.
func (s *Server) preStartNeutrino(ctx context.Context) error {
	walletLog := s.subLogger(btcwbackend.Subsystem)

	neutrinoDataDir := s.cfg.Wallet.BtcwalletDataDir
	if neutrinoDataDir == "" {
		networkDir, err := s.cfg.NetworkDir()
		if err != nil {
			return fmt.Errorf(
				"resolve network directory: %w", err,
			)
		}

		neutrinoDataDir = networkDir
	}

	svc, err := btcwbackend.NewNeutrinoService(
		neutrinoDataDir, s.chainParams,
		s.cfg.Wallet.BtcwalletPeers,
		s.cfg.Wallet.BtcwalletAddPeers,
		s.cfg.Wallet.PersistFilters, walletLog,
		btcwbackend.WithGlobalDependencyLoggers(
			!s.cfg.Wallet.DisableGlobalLogs,
		),
	)
	if err != nil {
		return fmt.Errorf("create neutrino service: %w", err)
	}

	if err := svc.Start(); err != nil {
		return fmt.Errorf("start neutrino service: %w", err)
	}

	s.neutrinoSvc = fn.Some(svc)
	s.log.InfoS(ctx,
		"Neutrino service pre-started for P2P sync")

	return nil
}

// startBtcwallet creates and starts the neutrino-backed wallet from
// the given raw seed. If a neutrino service was pre-started via
// preStartNeutrino, it is reused; otherwise a new one is created.
// On success it populates s.btcwWallet and marks the wallet as
// ready.
func (s *Server) startBtcwallet(ctx context.Context,
	seed [rawSeedLen]byte) error {

	networkDir, err := s.cfg.NetworkDir()
	if err != nil {
		return fmt.Errorf("resolve network directory: %w", err)
	}

	recoveryWindow := s.cfg.Wallet.RecoveryWindow
	if recoveryWindow == 0 {
		recoveryWindow = DefaultRecoveryWindow
	}

	cfg := btcwbackend.Config{
		Config: walletcore.Config{
			Seed:           seed,
			ChainParams:    s.chainParams,
			RecoveryWindow: recoveryWindow,
			DBDir:          networkDir,
			Log: fn.Some(
				s.subLogger(btcwbackend.Subsystem),
			),
		},
		NeutrinoDataDir: s.cfg.Wallet.BtcwalletDataDir,
		ConnectPeers:    s.cfg.Wallet.BtcwalletPeers,
		AddPeers:        s.cfg.Wallet.BtcwalletAddPeers,
		FeeURL:          s.cfg.Wallet.FeeURL,
		PersistFilters:  s.cfg.Wallet.PersistFilters,
		DisableGlobalLoggers: s.cfg.Wallet.
			DisableGlobalLogs,
	}

	// Reuse the pre-started neutrino service if available.
	// Otherwise, create a new one (fallback for callers that
	// skip preStartNeutrino).
	var w *btcwbackend.Wallet
	if s.neutrinoSvc.IsSome() {
		svc := s.neutrinoSvc.UnsafeFromSome()
		w, err = btcwbackend.NewWithNeutrino(cfg, svc)
	} else {
		w, err = btcwbackend.New(cfg)
	}
	if err != nil {
		return fmt.Errorf("create btcwallet: %w", err)
	}

	if err := w.Start(); err != nil {
		return fmt.Errorf("start btcwallet: %w", err)
	}

	s.btcwWallet = fn.Some(w)
	s.refreshProofKeyBackend()

	// Initialize the chain backend if it was deferred at startup
	// because the wallet was not yet available.
	if s.chainBackend == nil {
		s.chainBackend = w.ChainBackend()

		if err := s.chainBackend.Start(); err != nil {
			return fmt.Errorf(
				"start chain backend: %w", err,
			)
		}
	}

	// Refresh the RPC clients once the wallet is available so the
	// indexer client picks up the wallet-backed identity key and
	// signer before any deferred wallet-dependent actors start.
	if s.runtime != nil {
		s.initRPCClients(ctx)
	}

	s.log.InfoS(ctx, "Neutrino-backed wallet started, "+
		"waiting for initial sync in background")

	// Wait for btcwallet to fully sync with the chain before
	// marking the wallet ready. This includes the recovery scan
	// (when recoveryWindow > 0) which downloads and checks compact
	// block filters for all existing blocks. Without this wait,
	// the daemon would accept requests while btcwallet's chain
	// notification handler is blocked in syncWithChain, causing
	// newly mined blocks to be missed.
	//
	// The goroutine polls until sync completes or the daemon
	// shuts down. We use s.walletReady as the success signal
	// (closed by markWalletReady) and a server-scoped quit
	// channel so the goroutine doesn't outlive the daemon.
	//
	// We must NOT use the caller's ctx here: the RPC context
	// is cancelled when the handler returns, but the sync
	// goroutine must outlive the RPC. Instead, we select on
	// the server's runCtx (the context passed to Run, which
	// lives until daemon shutdown).
	runCtx := s.runCtx
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		logInterval := 30 * time.Second
		lastLog := time.Now()

		for {
			select {
			case <-runCtx.Done():
				return

			case <-ticker.C:
			}

			synced, bestTimestamp, err := w.IsSynced()
			if err != nil {
				continue
			}

			if !synced {
				if time.Since(lastLog) >= logInterval {
					s.log.InfoS(
						context.Background(),
						"Waiting for neutrino "+
							"wallet sync",
					)
					lastLog = time.Now()
				}

				continue
			}

			height, _, hErr := w.ChainBackend().BestBlock(
				context.Background(),
			)
			if hErr != nil || height == 0 {
				continue
			}

			s.log.InfoS(context.Background(),
				"Neutrino initial sync complete",
				slog.Int("height",
					int(height)),
				slog.Int64("best_time",
					bestTimestamp),
			)

			s.markWalletReady()

			return
		}
	}()

	return nil
}

// registerChainSourceActor creates and registers the chain source
// actor with the current s.chainBackend. The caller must ensure
// s.chainBackend is non-nil before calling this.
func (s *Server) registerChainSourceActor(
	ctx context.Context) actor.ActorRef[
	chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
] {

	chainActor := chainsource.NewChainSourceActor(
		chainsource.ChainSourceConfig{
			Backend: s.chainBackend,
			System:  s.actorSystem,
		},
	)

	ref := actor.RegisterWithSystem(
		s.actorSystem, "chain-source",
		chainsource.ChainSourceKey, chainActor,
	)

	s.log.InfoS(ctx, "Chain source actor registered")

	return ref
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
		notifier := chainbackends.NewLndClientChainNotifier(
			chainbackends.LndClientChainNotifierConfig{
				LND: &lndSvc.LndServices,
				Log: fn.Some(
					s.subLogger(
						chainbackends.
							LndClientSubsystem,
					),
				),
			},
		)
		backend := chainbackends.NewLNDBackend(
			notifier,
			chainbackends.NewLndClientFeeEstimator(
				lndSvc.WalletKit,
			),
			chainbackends.NewLndClientTxBroadcaster(
				lndSvc.WalletKit,
			),
		)
		backend.Log = fn.Some(s.subLogger(chainbackends.Subsystem))

		if s.cfg.PackageSubmitter != nil {
			backend.SetPackageSubmitter(s.cfg.PackageSubmitter)
		}

		s.chainBackend = backend

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
					s.cfg.Wallet.EsploraURL,
					s.subLogger(lwwallet.Subsystem),
				),
				s.cfg.Wallet.PollInterval,
				s.subLogger(lwwallet.Subsystem),
			)
		}

	case WalletTypeBtcwallet:
		// If the btcwallet is already started (auto-unlock
		// succeeded), use its chain backend. Otherwise the
		// chain backend will be initialized in
		// startBtcwallet when the wallet is created via
		// InitWallet/UnlockWallet RPC, since neutrino
		// requires the full service to be running.
		if s.btcwWallet.IsSome() {
			w := s.btcwWallet.UnsafeFromSome()
			s.chainBackend = w.ChainBackend()
			alreadyStarted = true
		} else {
			// Defer chain backend start to
			// startBtcwallet. Skip the Start() call
			// below.
			return nil
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

	s.log.InfoS(ctx, "Chain backend started",
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
	// 10. Start the VTXO manager before the round actor so
	//     the manager ref can be passed directly in the round
	//     config, avoiding a post-Start mutation.
	// -------------------------------------------------------
	s.lazyChainResolver = vtxo.NewLazyChainResolver()
	vtxoManagerRef, err := s.initVTXOManager(
		ctx, chainSourceRef, s.lazyChainResolver,
	)
	if err != nil {
		return err
	}
	s.vtxoMgrRef = fn.Some(vtxoManagerRef)

	roundVTXOManager := actor.NewMapInputRef(
		vtxoManagerRef, mapRoundVTXOManagerMsg,
	)

	// -------------------------------------------------------
	// 11. Register the round client actor.
	// -------------------------------------------------------
	_, err = s.initRoundActor(
		ctx, chainSourceRef, walletRef, timeoutRef,
		roundVTXOManager,
	)
	if err != nil {
		return err
	}

	// -------------------------------------------------------
	// 12. Register the unilateral-exit subsystem.
	// -------------------------------------------------------
	if err := s.initUnrollSubsystem(
		ctx, chainSourceRef, vtxoManagerRef,
	); err != nil {
		return err
	}

	// -------------------------------------------------------
	// 13. Register the OOR client actor.
	// -------------------------------------------------------
	if err := s.initOORActor(ctx, vtxoManagerRef); err != nil {
		return err
	}

	s.log.InfoS(ctx, "Wallet-dependent actors started")

	return nil
}

// startMailboxIngress starts mailbox ingress once all actor dispatch targets
// have been registered.
func (s *Server) startMailboxIngress(ctx context.Context) error {
	if err := s.runtime.StartIngress(ctx); err != nil {
		return fmt.Errorf("start serverconn ingress: %w", err)
	}

	s.setServerConnected(true)

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

	// Generate a P-256 client TLS cert carrying the secp256k1
	// identity pubkey in the Subject CN. The server uses this
	// for per-RPC access control on Pull/AckUpTo. Skip cert
	// generation in insecure mode where TLS is disabled.
	var clientCerts []tls.Certificate
	if !s.cfg.Server.Insecure && s.clientKeyDesc.PubKey != nil {
		clientCert, certErr := serverconn.GenerateClientTLSCert(
			s.clientKeyDesc.PubKey,
		)
		if certErr != nil {
			return nil, fmt.Errorf("generate client TLS "+
				"cert: %w", certErr)
		}

		clientCerts = []tls.Certificate{clientCert}
	}

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
			RootCAs:      pool,
			Certificates: clientCerts,
			MinVersion:   tls.VersionTLS12,
		})
		dialOpts = append(
			dialOpts,
			grpc.WithTransportCredentials(creds),
		)

	default:
		// Use the system certificate pool when no explicit cert
		// is provided.
		creds := credentials.NewTLS(&tls.Config{
			Certificates: clientCerts,
			MinVersion:   tls.VersionTLS12,
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
	if s.cfg.MailboxEdgeFactory != nil {
		return s.cfg.MailboxEdgeFactory(s.serverConn)
	}

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
	s.registerIncomingVTXOEventRoute(router)

	return router
}

// registerIncomingVTXOEventRoute registers the IncomingVTXO push event
// route. When the server publishes a VTXO_CREATED event for a round
// leaf matching a registered receive script, this route dispatches it
// to the incoming VTXO handler actor for materialization.
func (s *Server) registerIncomingVTXOEventRoute(
	router *serverconn.EventRouter) {

	vtxoKey := vtxo.IncomingVTXOServiceKey()

	serverconn.AddRoute(router, serverconn.EventRouteConfig[
		vtxo.IncomingVTXOMsg, vtxo.IncomingVTXOResp,
	]{
		Service: arkServiceName,
		Method:  MethodIncomingVTXO,
		NewEvent: func() proto.Message {
			return &arkrpc.IncomingVTXOEvent{}
		},
		Key: vtxoKey,
		Adapt: func(p proto.Message) (
			vtxo.IncomingVTXOMsg, error) {

			evt, ok := p.(*arkrpc.IncomingVTXOEvent)
			if !ok {
				return vtxo.IncomingVTXOMsg{},
					fmt.Errorf(
						"expected "+
							"IncomingVTXOEvent"+
							", got %T", p,
					)
			}

			return vtxo.IncomingVTXOMsg{Event: evt}, nil
		},
	})
}

// registerOOREventRoutes registers OOR mailbox service event routes with the
// EventRouter. When the server pushes SubmitPackage or FinalizePackage
// response events, the router decodes the oorpb proto, adapts it into a
// DriveEventRequest, and Tell's it to the OOR actor via service key.
func (s *Server) registerOOREventRoutes(router *serverconn.EventRouter) { //nolint:funlen,ll
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

	// ListVTXOsByScripts response: server returned authoritative incoming
	// metadata for a durable OOR receive session query.
	serverconn.AddEnvelopeRoute(router, serverconn.EnvelopeRouteConfig[
		oor.OORDurableMsg, oor.ActorResp,
	]{
		Service: "arkrpc.IndexerService",
		Method:  "ListVTXOsByScripts",
		NewEvent: func() proto.Message {
			return &arkrpc.ListVTXOsByScriptsResponse{}
		},
		Key: oorKey,
		Adapt: func(env *mailboxpb.Envelope,
			p proto.Message) (oor.OORDurableMsg, error) {

			if env == nil || env.Rpc == nil {
				return nil, fmt.Errorf("incoming metadata " +
					"response envelope must be provided")
			}

			sessionID, err := oor.ParseIncomingMetadataCorrelationID( //nolint:ll
				env.Rpc.CorrelationId,
			)
			if err != nil {
				return nil, err
			}

			if rpcErr := mailboxrpc.DecodeErrorHeaders(
				env.Headers,
			); rpcErr != nil {
				return &oor.DriveEventRequest{
					SessionID: sessionID,
					Event: &oor.FailEvent{
						Reason: fmt.Sprintf(
							"query incoming metadata: %v", //nolint:ll
							rpcErr,
						),
					},
				}, nil
			}

			resp, ok := p.(*arkrpc.ListVTXOsByScriptsResponse)
			if !ok {
				return nil, fmt.Errorf("expected "+
					"ListVTXOsByScriptsResponse, got %T", p)
			}

			matches, err := oor.IncomingMetadataMatchesFromResponse(
				sessionID, resp,
			)
			if err != nil {
				return nil, err
			}

			return &oor.DriveEventRequest{
				SessionID: sessionID,
				Event: &oor.IncomingMetadataResolvedEvent{
					Matches: matches,
				},
			}, nil
		},
	})

	// ListOORRecipientEventsByScript response: server resolved the
	// lightweight incoming transfer hint into the full Ark package for a
	// durable OOR receive session query.
	serverconn.AddEnvelopeRoute(router, serverconn.EnvelopeRouteConfig[
		oor.OORDurableMsg, oor.ActorResp,
	]{
		Service: "arkrpc.IndexerService",
		Method:  "ListOORRecipientEventsByScript",
		NewEvent: func() proto.Message {
			return &arkrpc.ListOORRecipientEventsByScriptResponse{}
		},
		Key: oorKey,
		Adapt: func(env *mailboxpb.Envelope,
			p proto.Message) (oor.OORDurableMsg, error) {

			if env == nil || env.Rpc == nil {
				return nil, fmt.Errorf("incoming resolve " +
					"response envelope must be provided")
			}

			sessionID, recipientEventID, err :=
				oor.ParseIncomingResolveCorrelationID(
					env.Rpc.CorrelationId,
				)
			if err != nil {
				return nil, err
			}

			if rpcErr := mailboxrpc.DecodeErrorHeaders(
				env.Headers,
			); rpcErr != nil {
				return &oor.DriveEventRequest{
					SessionID: sessionID,
					Event: &oor.FailEvent{
						Reason: fmt.Sprintf(
							"resolve incoming transfer: %v", //nolint:ll
							rpcErr,
						),
					},
				}, nil
			}

			resp, ok := p.(*arkrpc.ListOORRecipientEventsByScriptResponse) //nolint:ll
			if !ok {
				return nil, fmt.Errorf("expected "+
					"ListOORRecipientEventsByScriptResponse, "+ //nolint:ll
					"got %T", p)
			}

			incomingEvent, err := oor.IncomingTransferEventFromResponse( //nolint:ll
				sessionID, recipientEventID, resp,
			)
			if err != nil {
				return nil, err
			}

			return &oor.DriveEventRequest{
				SessionID: sessionID,
				Event:     incomingEvent,
			}, nil
		},
	})

	// IncomingOOR: server notifies the client about an incoming
	// OOR transfer via the indexer's ArkService. Persist only the
	// lightweight notification hint here; the durable OOR actor
	// performs the follow-up indexer query from its own worker
	// context. This avoids deadlocking mailbox ingress on a unary
	// response that must be delivered by the same runtime.
	serverconn.AddRoute(router, serverconn.EventRouteConfig[
		oor.OORDurableMsg, oor.ActorResp,
	]{
		Service: arkServiceName,
		Method:  MethodIncomingOOR,
		NewEvent: func() proto.Message {
			return &arkrpc.IncomingOOREvent{}
		},
		Key: oorKey,
		Adapt: func(p proto.Message) (
			oor.OORDurableMsg, error) {

			evt, ok := p.(*arkrpc.IncomingOOREvent)
			if !ok {
				return nil, fmt.Errorf(
					"expected IncomingOOREvent"+
						", got %T", p,
				)
			}

			return oor.NewResolveIncomingTransferRequest(evt)
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

	// JoinAck: server accepted the client's join request.
	addRoundRoute(
		roundpb.MethodJoinAck,
		func() proto.Message {
			return &roundpb.ClientSuccessResp{}
		},
		func() round.ClientEvent {
			return &round.RoundJoined{}
		},
	)

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

	// Include the auth signature in response headers so the
	// server can verify identity on all envelopes.
	if headers == nil {
		headers = make(map[string]string, 1)
	}
	if s.authSigHex != "" {
		headers[serverconn.AuthHeaderKey] = s.authSigHex
	}

	responseEnv := &mailboxpb.Envelope{
		ProtocolVersion: env.ProtocolVersion,
		Sender:          s.localMailboxID,
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

	s.db, err = db.NewSqliteStore(
		sqliteCfg, s.subLogger(db.Subsystem),
	)
	if err != nil {
		return fmt.Errorf("unable to open database: %w", err)
	}

	s.deliveryStore, err = actordelivery.NewTxAwareDeliveryStoreFromDB(
		s.db.DB, s.db.Backend(), s.clk,
		s.subLogger(actor.Subsystem),
	)
	if err != nil {
		return fmt.Errorf("unable to create delivery "+
			"store: %w", err)
	}

	s.log.InfoS(ctx, "Database initialized",
		slog.String("path", sqliteCfg.DatabaseFileName))

	return nil
}

// initLedgerActor creates and starts the client-side ledger
// accounting actor with both the double-entry ledger store and
// the UTXO audit log store.
func (s *Server) initLedgerActor(ctx context.Context) error {
	dbStore := db.NewStore(
		s.db.DB, s.db.Queries, s.db.Backend(),
		s.subLogger(db.Subsystem),
	)

	ledgerStore := db.NewLedgerStoreDB(dbStore)
	auditStore := db.NewUTXOAuditStoreDB(dbStore)

	// Stash the ledger store so the RPC layer can query it
	// directly for paginated history without going through the
	// ledger actor (which is write-only / fire-and-forget).
	s.ledgerStore = ledgerStore

	ledgerActor := ledger.NewLedgerActor(
		ledger.ActorConfig{
			Log: fn.Some(
				s.subLogger(ledger.Subsystem),
			),
			DeliveryStore:  s.deliveryStore,
			LedgerStore:    ledgerStore,
			UTXOAuditStore: auditStore,
		},
	)

	if err := ledgerActor.Start(ctx); err != nil {
		return fmt.Errorf("start ledger actor: %w", err)
	}

	ledgerKey := ledger.NewServiceKey()
	actor.RegisterWithSystem(
		s.actorSystem, "ledger-accounting",
		ledgerKey, ledgerActor,
	)

	s.log.InfoS(ctx, "Ledger accounting actor started")

	return nil
}

// initRPCClients creates the Ark and indexer mailbox RPC clients. Both
// use the runtime's unary facade to issue RPCs to the server through
// the mailbox transport.
func (s *Server) initRPCClients(ctx context.Context) {
	s.ark = arkrpc.NewArkServiceMailboxClient(s.runtime.Unary())

	dbStore := db.NewStore(
		s.db.DB, s.db.Queries, s.db.Backend(),
		s.subLogger(db.Subsystem),
	)
	packageStore := dbStore.NewOORArtifactStore(s.clk)

	// Determine the node identity pubkey for indexer registration.
	// In lnd mode this comes from the lnd connection. In lwwallet
	// mode, the identity is derived from the wallet keyring once
	// the wallet is ready.
	//
	// The indexer client itself must prove control over multiple owned
	// receive scripts, so it uses a dynamic signer that resolves the
	// correct wallet key from the persisted owned-script map for each
	// pkScript.
	var signer indexer.SchnorrSigner
	signerFactory, err := s.indexerProofSignerFactory()
	if err != nil {
		s.log.WarnS(ctx,
			"Unable to initialize indexer signer factory", err)
	} else {
		identityDesc, identitySigner, err := s.IndexerProofKey(
			ctx, keychain.KeyLocator{
				Family: identityKeyFamily,
				Index:  0,
			},
		)
		if err != nil {
			s.log.WarnS(ctx,
				"Unable to derive identity key for indexer", err)
		} else {
			s.clientKeyDesc = *identityDesc
			signer = NewFallbackSchnorrSigner(
				NewOwnedReceiveScriptSigner(
					packageStore, signerFactory,
				),
				identitySigner,
			)
		}
	}

	// The indexer principal is the client's pubkey-derived
	// mailbox ID, used in proof-of-control signatures.
	principal := serverconn.PubKeyMailboxID(
		s.clientKeyDesc.PubKey,
	)

	s.indexer = indexer.New(
		s.runtime.Unary(), signer,
		indexerProofServerID, principal,
		fn.Some(s.subLogger(indexer.Subsystem)),
	)

	s.log.InfoS(ctx, "RPC clients initialized")
}

// connectAndBootstrapMailbox derives the identity key, connects to the
// ark operator, fetches the operator pubkey, and wires the mailbox
// transport runtime. This is called synchronously for LND (wallet
// always ready) or after walletReady fires for lwwallet/btcwallet.
func (s *Server) connectAndBootstrapMailbox(
	ctx context.Context) error {

	// Derive the identity key from the now-ready wallet.
	if err := s.deriveIdentityKeyEarly(ctx); err != nil {
		return fmt.Errorf("derive identity key: %w", err)
	}

	// Connect to the ark operator's mailbox edge server.
	s.log.InfoS(ctx, "Connecting to ark server",
		"host", s.cfg.Server.Host)

	serverConn, err := s.dialServer(ctx)
	if err != nil {
		return fmt.Errorf("unable to connect to server: %w",
			err)
	}
	s.serverConn = serverConn

	s.log.InfoS(ctx, "Connected to ark server")

	// Fetch the operator's public key via direct gRPC before
	// wiring the mailbox transport.
	operatorPubKey, err := s.fetchOperatorPubKeyDirect(ctx)
	if err != nil {
		return fmt.Errorf("fetch operator pubkey: %w", err)
	}

	s.log.InfoS(ctx, "Fetched operator pubkey via direct gRPC",
		slog.String("operator_mailbox_id",
			serverconn.PubKeyMailboxID(operatorPubKey),
		),
	)

	// Build the mailbox transport runtime.
	edge := s.newMailboxEdge()
	dispatchers := s.buildRPCDispatchers(edge)

	// Derive compound mailbox ID: operator:client.
	s.localMailboxID = serverconn.PubKeyMailboxID(
		s.clientKeyDesc.PubKey,
	)
	operatorMBID := serverconn.PubKeyMailboxID(operatorPubKey)
	remoteMailboxID := serverconn.CompoundMailboxID(
		operatorMBID, s.localMailboxID,
	)

	// Sign the Schnorr auth proving key ownership, bound to
	// the compound recipient mailbox.
	authSig, err := s.signMailboxAuth(ctx, remoteMailboxID)
	if err != nil {
		return fmt.Errorf("sign mailbox auth: %w", err)
	}

	s.authSigHex = hex.EncodeToString(authSig.Serialize())

	connCfg := serverconn.DefaultConnectorConfig()
	connCfg.Edge = edge
	connCfg.LocalMailboxID = s.localMailboxID
	connCfg.RemoteMailboxID = remoteMailboxID
	connCfg.ProtocolVersion = 1
	connCfg.Store = s.deliveryStore
	connCfg.Dispatchers = dispatchers
	connCfg.AuthSignature = authSig
	connCfg.InitAuthHeader()
	connCfg.DurableUnaryBuilder = &serverDurableUnaryBuilder{
		server: s,
	}
	connCfg.Log = fn.Some(s.subLogger(serverconn.Subsystem))

	s.runtime, err = serverconn.NewRuntime(connCfg)
	if err != nil {
		return fmt.Errorf("unable to create serverconn "+
			"runtime: %w", err)
	}

	// Start durable egress immediately so unary sends and actor outbox
	// delivery can begin, but defer ingress until wallet-dependent actors
	// are registered. On restart the remote mailbox may already contain
	// queued server-push envelopes targeting the round or OOR actors.
	s.runtime.StartEgress()

	s.log.InfoS(ctx, "Mailbox transport runtime started",
		slog.String("local_mailbox", s.localMailboxID),
		slog.String("remote_mailbox", remoteMailboxID),
	)

	// Create RPC clients that use the mailbox transport.
	s.initRPCClients(ctx)

	return nil
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

	dbStore := db.NewStore(
		s.db.DB, s.db.Queries, s.db.Backend(),
		s.subLogger(db.Subsystem),
	)
	boardingStore := dbStore.NewBoardingStore(s.chainParams, s.clk)

	// Select the boarding backend based on wallet type.
	var boardingBackend wallet.BoardingBackend
	switch s.cfg.Wallet.Type {
	case WalletTypeLnd:
		lndSvc := s.lnd.UnsafeFromSome()
		backend := lndbackend.NewBoardingBackend(
			lndSvc.WalletKit, lndSvc.ChainKit,
		)
		backend.Log = fn.Some(s.subLogger(lndbackend.Subsystem))
		boardingBackend = backend

	case WalletTypeLwwallet:
		w := s.lwWallet.UnsafeFromSome()
		boardingBackend = w.BoardingBackend()

	case WalletTypeBtcwallet:
		w := s.btcwWallet.UnsafeFromSome()
		boardingBackend = w.BoardingBackend()
	}

	// Adapt the VTXO persistence store to the wallet's VTXOReader
	// interface. The wallet uses this to load VTXO descriptors when
	// building intent packages for round registration.
	vtxoReader := wallet.VTXOReaderFunc(func(ctx context.Context,
		op wire.OutPoint) (*wallet.VTXODescriptor, error) {

		desc, err := s.vtxoStore.GetVTXO(ctx, op)
		if err != nil {
			return nil, err
		}

		return &wallet.VTXODescriptor{
			Outpoint:       desc.Outpoint,
			Amount:         desc.Amount,
			PolicyTemplate: desc.PolicyTemplate,
			PkScript:       desc.PkScript,
			Expiry:         desc.RelativeExpiry,
			ClientKey:      desc.ClientKey,
			OperatorKey:    desc.OperatorKey,
		}, nil
	})

	walletActor := wallet.NewArk(
		boardingBackend, boardingStore, vtxoReader,
		chainSourceRef, s.actorSystem,
		fn.Some(ledger.NewSink(s.actorSystem)),
		s.subLogger(wallet.Subsystem),
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

	s.log.InfoS(ctx, "Wallet actor registered and started")

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
	timeoutRef actor.TellOnlyRef[timeout.Msg],
	vtxoManager actor.TellOnlyRef[round.VTXOManagerMsg],
) (*round.RoundClientActor, error) {

	// Select the client wallet (signing) backend based on
	// wallet type. In lnd mode, signing goes through lnd's
	// remote signer. In lwwallet mode, signing is in-process
	// via btcwallet.
	var clientWallet round.ClientWallet
	switch s.cfg.Wallet.Type {
	case WalletTypeLnd:
		lndSvc := s.lnd.UnsafeFromSome()
		lndWallet := lndbackend.NewClientWallet(
			lndSvc.Signer, lndSvc.WalletKit,
		)
		lndWallet.Log = fn.Some(s.subLogger(lndbackend.Subsystem))
		clientWallet = lndWallet

	case WalletTypeLwwallet:
		clientWallet = s.lwWallet.UnsafeFromSome()

	case WalletTypeBtcwallet:
		clientWallet = s.btcwWallet.UnsafeFromSome()
	}

	dbStore := db.NewStore(
		s.db.DB, s.db.Queries, s.db.Backend(),
		s.subLogger(db.Subsystem),
	)
	roundStore := dbStore.NewRoundStore(s.chainParams, s.clk)
	s.roundStore = roundStore

	// Fetch the operator's terms from the server. These include
	// the operator pubkey, sweep delay, exit delay, dust limit,
	// and other round parameters.
	operatorTerms, err := s.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch operator "+
			"terms: %w", err)
	}

	s.storeOperatorTerms(operatorTerms)

	// Default maximum operator fee the client is willing to pay
	// per round. This is a safety limit to prevent the client
	// from overpaying. Set to 0.01 BTC (1,000,000 sats) which
	// is generous for regtest/testnet usage.
	const defaultMaxOperatorFee = btcutil.Amount(1_000_000)

	// Build the owned-script checker from the OOR artifact store.
	// This allows the round FSM to determine which VTXOs belong
	// to the local wallet by looking up registered receive scripts.
	var scriptChecker round.OwnedScriptChecker
	var scriptRegistrar round.OwnedScriptRegistrar
	if s.db != nil {
		oorStore := db.NewStore(
			s.db.DB, s.db.Queries, s.db.Backend(),
			s.log,
		).NewOORArtifactStore(s.clk)

		scriptChecker = &ownedScriptCheckerAdapter{
			store: oorStore,
		}
		scriptRegistrar = &ownedScriptRegistrarAdapter{
			store:       oorStore,
			operatorKey: operatorTerms.PubKey,
			exitDelay:   operatorTerms.VTXOExitDelay,
		}
	}

	roundCfg := &round.RoundClientConfig{
		Name:                 "round-client",
		Logger:               s.subLogger(round.Subsystem),
		Wallet:               clientWallet,
		RoundStore:           roundStore,
		VTXOStore:            roundStore,
		OperatorTerms:        operatorTerms,
		ServerConn:           s.runtime.TellRef(),
		ChainSource:          chainSourceRef,
		WalletActor:          walletRef,
		ChainParams:          s.chainParams,
		ActorSystem:          s.actorSystem,
		TimeoutActor:         timeoutRef,
		MaxOperatorFee:       defaultMaxOperatorFee,
		VTXOManager:          vtxoManager,
		OwnedScriptChecker:   scriptChecker,
		OwnedScriptRegistrar: scriptRegistrar,
		LedgerSink:           fn.Some(ledger.NewSink(s.actorSystem)),
		ForfeitCollectionTimeout: s.cfg.
			ForfeitCollectionTimeout,
	}

	roundActor, err := round.NewRoundClientActor(
		roundCfg,
	).Unpack()
	if err != nil {
		return nil, fmt.Errorf("unable to create round "+
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
		return nil, fmt.Errorf("unable to start round "+
			"actor: %w", err)
	}

	s.log.InfoS(ctx, "Round actor registered and started")

	return roundActor, nil
}

// initVTXOManager creates, registers, and starts the VTXO manager actor.
// The manager recovers persisted VTXOs on startup and spawns one VTXO actor
// per live descriptor.
func (s *Server) initVTXOManager(ctx context.Context,
	chainSourceRef actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	],
	chainResolver actor.TellOnlyRef[vtxo.ExpiringNotification],
) (actor.ActorRef[vtxo.ManagerMsg, vtxo.ManagerResp], error) {

	var vtxoWallet vtxo.VTXOWallet
	switch s.cfg.Wallet.Type {
	case WalletTypeLnd:
		lndSvc := s.lnd.UnsafeFromSome()
		lndWallet := lndbackend.NewClientWallet(
			lndSvc.Signer, lndSvc.WalletKit,
		)
		lndWallet.Log = fn.Some(s.subLogger(lndbackend.Subsystem))
		vtxoWallet = lndWallet

	case WalletTypeLwwallet:
		vtxoWallet = s.lwWallet.UnsafeFromSome()

	case WalletTypeBtcwallet:
		vtxoWallet = s.btcwWallet.UnsafeFromSome()
	}

	dbStore := db.NewStore(
		s.db.DB, s.db.Queries, s.db.Backend(),
		s.subLogger(db.Subsystem),
	)
	vtxoStore := dbStore.NewVTXOStore(s.clk)

	manager := vtxo.NewManager(&vtxo.ManagerConfig{
		Store:            vtxoStore,
		Wallet:           vtxoWallet,
		ChainSource:      chainSourceRef,
		ActorSystem:      s.actorSystem,
		ChainParams:      s.chainParams,
		Log:              fn.Some(s.subLogger(vtxo.Subsystem)),
		RoundActor:       round.NewServiceKey().Ref(s.actorSystem),
		LedgerSink:       fn.Some(ledger.NewSink(s.actorSystem)),
		ChainResolver:    chainResolver,
		RefreshFeeQuoter: s.autoRefreshFeeQuoter(),
	})

	managerKey := actor.NewServiceKey[vtxo.ManagerMsg, vtxo.ManagerResp](
		"vtxo-manager",
	)
	managerRef := actor.RegisterWithSystem(
		s.actorSystem, "vtxo-manager", managerKey, manager,
	)

	err := manager.Start(ctx, managerRef)
	if err != nil {
		s.actorSystem.StopAndRemoveActor("vtxo-manager")

		var zero actor.ActorRef[vtxo.ManagerMsg, vtxo.ManagerResp]

		return zero, fmt.Errorf("unable to start vtxo manager: %w", err)
	}

	s.log.InfoS(ctx, "VTXO manager registered and started")

	return managerRef, nil
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
func (s *Server) initOORActor(ctx context.Context,
	vtxoManagerRef actor.TellOnlyRef[vtxo.ManagerMsg]) error {

	dbStore := db.NewStore(
		s.db.DB, s.db.Queries, s.db.Backend(),
		s.subLogger(db.Subsystem),
	)

	// Select the OOR signer based on wallet type. The
	// SigningOutboxHandler only needs input.Signer for signing
	// checkpoint PSBTs.
	var oorSigner input.Signer
	switch s.cfg.Wallet.Type {
	case WalletTypeLnd:
		lndSvc := s.lnd.UnsafeFromSome()
		lndWallet := lndbackend.NewClientWallet(
			lndSvc.Signer, lndSvc.WalletKit,
		)
		lndWallet.Log = fn.Some(s.subLogger(lndbackend.Subsystem))
		oorSigner = lndWallet

	case WalletTypeLwwallet:
		oorSigner = s.lwWallet.UnsafeFromSome()

	case WalletTypeBtcwallet:
		oorSigner = s.btcwWallet.UnsafeFromSome()
	}

	vtxoStore := dbStore.NewVTXOStore(s.clk)
	packageStore := dbStore.NewOORArtifactStore(s.clk)

	operatorTerms := s.loadOperatorTerms()
	if operatorTerms == nil {
		return fmt.Errorf("operator terms not initialized")
	}

	if operatorTerms.PubKey == nil {
		return fmt.Errorf("operator terms missing operator pubkey")
	}

	// Create the timeout actor for scheduling retry timers. When a
	// retry timer fires, the callback ref transforms the expiry into
	// a DriveEventRequest and Tell's it back to the OOR actor.
	timeoutActor := timeout.NewActor()

	signingHandler := &oor.SigningOutboxHandler{
		Signer:       oorSigner,
		TimeoutActor: timeoutActor,
	}

	// Wire spend completion through the VTXO manager so each consumed
	// VTXO transitions to SpentState via its own FSM, rather than
	// writing VTXOStatusSpent directly to the store. Use Tell rather
	// than Ask so the OOR durable actor can commit its own delivery
	// transaction before the VTXO actor performs follow-up DB writes.
	mgrKey := actormsg.VTXOManagerServiceKey()
	completeSpend := func(ctx context.Context,
		outpoints []wire.OutPoint) error {

		return mgrKey.Ref(s.actorSystem).Tell(
			ctx, &actormsg.CompleteSpendRequest{
				Outpoints: outpoints,
			},
		)
	}

	outboxHandler := &oor.LocalPersistenceOutboxHandler{
		Next:         signingHandler,
		Store:        vtxoStore,
		PackageStore: packageStore,
		OperatorKey:  operatorTerms.PubKey,
		ExitDelay:    operatorTerms.VTXOExitDelay,
		NotifyIncomingVTXOs: func(ctx context.Context,
			descs []*vtxo.Descriptor) error {

			return vtxoManagerRef.Tell(
				ctx,
				&vtxo.VTXOsMaterializedNotification{
					VTXOs: descs,
				},
			)
		},
		CompleteSpend: completeSpend,
		ResolveIncomingClientKey: func(ctx context.Context,
			recipient oor.ArkRecipientOutput) (
			keychain.KeyDescriptor, error) {

			return ResolveOwnedReceiveScriptKey(
				ctx, packageStore, recipient,
			)
		},
		ResolveIncomingMetadata: func(ctx context.Context,
			sessionID oor.SessionID,
			recipient oor.ArkRecipientOutput,
			ark *psbt.Packet,
			finalCheckpoints []*psbt.Packet) (
			oor.IncomingVTXOMetadata, error) {

			_ = ark
			_ = finalCheckpoints

			return ResolveIncomingMetadataFromIndexer(
				build.ContextWithLogger(
					ctx, s.subLogger(Subsystem),
				),
				s.indexer, sessionID, recipient,
			)
		},
	}

	s.oorActor = oor.NewOORClientActor(oor.ClientActorCfg{
		Log:           fn.Some(s.subLogger(oor.Subsystem)),
		OutboxHandler: outboxHandler,
		ServerConn:    s.runtime.TellRef(),
		PackageStore:  packageStore,
		DeliveryStore: s.deliveryStore,
		ActorSystem:   s.actorSystem,
		ActorID:       oor.OORActorServiceKeyName,
		VTXOManager:   vtxoManagerRef,
		VTXOStore:     vtxoStore,
		LedgerSink:    fn.Some(ledger.NewSink(s.actorSystem)),
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

	s.log.InfoS(ctx, "OOR client actor started")

	// Register the incoming VTXO handler actor. This handles
	// IncomingVTXOEvent push notifications from the indexer and
	// materializes VTXOs for registered receive scripts.
	incomingVTXOStore := dbStore.NewVTXOStore(s.clk)
	incomingHandler := vtxo.NewIncomingVTXOHandler(
		vtxo.IncomingVTXOHandlerConfig{
			Log: fn.Some(s.subLogger(Subsystem)),
			ScriptStore: &ownedScriptLookupAdapter{
				store: packageStore,
			},
			VTXOStore:   incomingVTXOStore,
			VTXOManager: vtxoManagerRef,
		},
	)
	incomingKey := vtxo.IncomingVTXOServiceKey()
	actor.RegisterWithSystem(
		s.actorSystem, "incoming-vtxo-handler",
		incomingKey, incomingHandler,
	)

	s.log.InfoS(ctx, "Incoming VTXO handler started")

	return nil
}

// Compile-time assertions: every round.VTXOManagerMsg implementor must
// also satisfy vtxo.ManagerMsg. This makes the runtime assertion in
// mapRoundVTXOManagerMsg infallible.
var _ vtxo.ManagerMsg = (*round.VTXOCreatedNotification)(nil)
var _ vtxo.ManagerMsg = (*round.VTXOTerminatedMsg)(nil)

// mapRoundVTXOManagerMsg adapts round-owned manager notifications into the
// concrete message type accepted by the VTXO manager actor.
func mapRoundVTXOManagerMsg(msg round.VTXOManagerMsg) vtxo.ManagerMsg {
	// The compile-time assertions above guarantee this succeeds for
	// all concrete types that implement round.VTXOManagerMsg.
	mapped, ok := msg.(vtxo.ManagerMsg)
	if !ok {
		panic(fmt.Sprintf("unexpected VTXO manager msg type: %T", msg))
	}

	return mapped
}

// fetchOperatorTerms retrieves the operator's terms from the Ark
// server via a direct ArkService.GetInfo RPC on the base gRPC
// transport. This must not depend on mailbox ingress during startup:
// a restarted client can already have queued server-push envelopes in
// its mailbox, and those envelopes may target actors that have not yet
// been registered. Using the mailbox transport here can therefore
// deadlock round/OOR bootstrap behind redelivery of those pending
// events.
//
// The terms include the operator pubkey, sweep delay, VTXO exit delay,
// forfeit script, dust limit, and fee rate. These are required before
// the round actor can start, as they govern all round signing and
// validation parameters.
func (s *Server) fetchOperatorTerms(
	ctx context.Context) (*types.OperatorTerms, error) {

	client := arkrpc.NewArkServiceClient(s.serverConn)

	resp, err := client.GetInfo(ctx, &arkrpc.GetInfoRequest{})
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

	terms := &types.OperatorTerms{
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
	}

	return terms, nil
}

// deriveIdentityKeyEarly derives the client's identity key before the
// mailbox transport starts. In LND mode the key is derived from the
// connected LND wallet. In lwwallet/btcwallet mode the key is derived
// from the respective wallet keyring if already unlocked.
func (s *Server) deriveIdentityKeyEarly(ctx context.Context) error {
	var (
		derived bool
		lndErr  error
		lwErr   error
		btcwErr error
	)

	s.lnd.WhenSome(func(lndSvc *lndclient.GrpcLndServices) {
		desc, err := lndSvc.WalletKit.DeriveKey(ctx,
			&keychain.KeyLocator{
				Family: identityKeyFamily,
				Index:  0,
			},
		)
		if err != nil {
			lndErr = fmt.Errorf("lnd: %w", err)

			s.log.WarnS(ctx,
				"Unable to derive identity key from lnd",
				err,
			)

			return
		}

		s.clientKeyDesc = *desc
		derived = true
	})

	if derived {
		return nil
	}

	s.lwWallet.WhenSome(func(w *lwwallet.Wallet) {
		desc, err := w.DeriveKey(ctx, keychain.KeyLocator{
			Family: identityKeyFamily,
			Index:  0,
		})
		if err != nil {
			lwErr = fmt.Errorf("lwwallet: %w", err)

			s.log.WarnS(ctx,
				"Unable to derive identity key from "+
					"lwwallet", err,
			)

			return
		}

		s.clientKeyDesc = *desc
		derived = true
	})

	if derived {
		return nil
	}

	s.btcwWallet.WhenSome(func(w *btcwbackend.Wallet) {
		desc, err := w.KeyRing().DeriveKey(keychain.KeyLocator{
			Family: identityKeyFamily,
			Index:  0,
		})
		if err != nil {
			btcwErr = fmt.Errorf("btcwallet: %w", err)

			s.log.WarnS(ctx,
				"Unable to derive identity key from "+
					"btcwallet", err,
			)

			return
		}

		s.clientKeyDesc = desc
		derived = true
	})

	if !derived {
		errs := []error{lndErr, lwErr, btcwErr}
		var errMsgs []string
		for _, e := range errs {
			if e != nil {
				errMsgs = append(errMsgs, e.Error())
			}
		}

		if len(errMsgs) > 0 {
			return fmt.Errorf(
				"derive identity key: %s",
				strings.Join(errMsgs, "; "),
			)
		}

		return fmt.Errorf("no wallet backend available " +
			"to derive identity key")
	}

	s.log.InfoS(ctx, "Derived client identity key",
		slog.String("mailbox_id", serverconn.PubKeyMailboxID(
			s.clientKeyDesc.PubKey,
		)))

	return nil
}

// fetchOperatorPubKeyDirect fetches the operator's public key via a
// direct gRPC call to ArkService.GetInfo, bypassing the mailbox
// transport. This is needed before the mailbox runtime starts because
// the remote mailbox ID is derived from the operator's pubkey.
func (s *Server) fetchOperatorPubKeyDirect(
	ctx context.Context) (*btcec.PublicKey, error) {

	client := arkrpc.NewArkServiceClient(s.serverConn)

	resp, err := client.GetInfo(ctx, &arkrpc.GetInfoRequest{})
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

	return pubKey, nil
}

// signMailboxAuth computes the Schnorr auth signature for the
// client's identity key bound to the given recipient mailbox ID.
// The signature is included in every outbound envelope header so
// the server can verify the client's identity during registration.
func (s *Server) signMailboxAuth(ctx context.Context,
	recipientMailboxID string) (*schnorr.Signature, error) {

	var (
		sig *schnorr.Signature
		err error
	)

	// In LND mode, use lnd's tagged Schnorr signing RPC. We
	// pass the raw message and tag so LND computes the BIP-340
	// tagged hash internally, avoiding double-hashing.
	s.lnd.WhenSome(func(lndSvc *lndclient.GrpcLndServices) {
		msg := serverconn.MailboxAuthMessage(
			s.clientKeyDesc.PubKey, recipientMailboxID,
		)

		tag := []byte(serverconn.MailboxAuthTagStr)

		var rawSig []byte
		rawSig, err = lndSvc.Signer.SignMessage(
			ctx, msg, s.clientKeyDesc.KeyLocator,
			lndclient.SignSchnorr(nil),
			withSchnorrTag(tag),
		)
		if err != nil {
			err = fmt.Errorf("lnd sign mailbox auth: %w",
				err)

			return
		}

		sig, err = schnorr.ParseSignature(rawSig)
	})

	if sig != nil || err != nil {
		return sig, err
	}

	// In lwwallet mode, use the keyring's Schnorr signing
	// directly — no private key extraction needed.
	s.lwWallet.WhenSome(func(w *lwwallet.Wallet) {
		sig, err = s.signMailboxAuthViaKeyRing(
			w.KeyRing(), recipientMailboxID,
		)
	})

	if sig != nil || err != nil {
		return sig, err
	}

	// In btcwallet mode, use the neutrino-backed keyring's
	// Schnorr signing — same interface, no private key
	// extraction.
	s.btcwWallet.WhenSome(func(w *btcwbackend.Wallet) {
		sig, err = s.signMailboxAuthViaKeyRing(
			w.KeyRing(), recipientMailboxID,
		)
	})

	if sig == nil && err == nil {
		return nil, fmt.Errorf("no wallet backend available " +
			"to sign mailbox auth")
	}

	return sig, err
}

// withSchnorrTag applies a BIP-340 tag to lnd's SignMessage request.
func withSchnorrTag(tag []byte) lndclient.SignMessageOption {
	return func(req *signrpc.SignMessageReq) {
		req.Tag = tag
	}
}

// signMailboxAuthViaKeyRing signs the mailbox auth digest using a
// keyring's SignMessageSchnorr method. This avoids extracting private
// keys — the keyring handles signing internally. The BIP-340 tagged
// hash is computed by the keyring via the tag parameter.
func (s *Server) signMailboxAuthViaKeyRing(
	keyRing keychain.SecretKeyRing,
	recipientMailboxID string) (*schnorr.Signature, error) {

	msg := serverconn.MailboxAuthMessage(
		s.clientKeyDesc.PubKey, recipientMailboxID,
	)
	tag := []byte(serverconn.MailboxAuthTagStr)

	sig, err := keyRing.SignMessageSchnorr(
		s.clientKeyDesc.KeyLocator, msg,
		false, nil, tag,
	)
	if err != nil {
		return nil, fmt.Errorf("keyring sign mailbox auth: %w",
			err)
	}

	return sig, nil
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

// lndUnrollWallet composes the LND-backed signing/key-derivation wallet
// with the boarding backend's ListUnspent to satisfy the
// UnilateralExitWallet interface needed by the package executor.
type lndUnrollWallet struct {
	*lndbackend.ClientWallet
	boardingBackend *lndbackend.BoardingBackend
}

// ListUnspent delegates to the boarding backend's wallet UTXO
// enumeration.
func (w *lndUnrollWallet) ListUnspent(ctx context.Context,
	minConfs, maxConfs int32) ([]*wallet.Utxo, error) {

	return w.boardingBackend.ListUnspent(ctx, minConfs, maxConfs)
}

// NewWalletPkScript returns a fresh wallet-managed taproot pkScript suitable
// for change and sweep outputs.
func (w *lndUnrollWallet) NewWalletPkScript(ctx context.Context) ([]byte,
	error) {

	addr, err := w.boardingBackend.WalletKit().NextAddr(
		ctx, lnwallet.DefaultAccountName,
		walletrpc.AddressType_TAPROOT_PUBKEY, true,
	)
	if err != nil {
		return nil, fmt.Errorf("LND NextAddr: %w", err)
	}

	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, fmt.Errorf("pay to addr script: %w", err)
	}

	return pkScript, nil
}

// FinalizePsbt signs and finalizes a PSBT using LND's WalletKit.
func (w *lndUnrollWallet) FinalizePsbt(ctx context.Context,
	packetBytes []byte) (*wire.MsgTx, error) {

	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(packetBytes), false,
	)
	if err != nil {
		return nil, fmt.Errorf("parse PSBT: %w", err)
	}

	_, finalTx, err := w.boardingBackend.WalletKit().FinalizePsbt(
		ctx, packet, "",
	)
	if err != nil {
		return nil, fmt.Errorf("LND FinalizePsbt: %w", err)
	}

	return finalTx, nil
}

// LeaseOutput forwards the CPFP fee-input lease to the LND boarding backend,
// so txconfirm's cross-subsystem UTXO coordination uses the same WalletKit
// lock that boarding and other subsystems already honor.
func (w *lndUnrollWallet) LeaseOutput(ctx context.Context, id wallet.LockID,
	op wire.OutPoint, expiry time.Duration) (time.Time, error) {

	return w.boardingBackend.LeaseOutput(ctx, id, op, expiry)
}

// ReleaseOutput forwards the unlock to the LND boarding backend.
func (w *lndUnrollWallet) ReleaseOutput(ctx context.Context, id wallet.LockID,
	op wire.OutPoint) error {

	return w.boardingBackend.ReleaseOutput(ctx, id, op)
}

// lwUnrollWallet adapts the lightweight wallet for the
// UnilateralExitWallet interface needed by the package executor.
// It embeds the lwwallet (which already satisfies input.Signer)
// and adds the ListUnspent and FinalizePsbt methods.
type lwUnrollWallet struct {
	*lwwallet.Wallet
}

// ListUnspent returns confirmed wallet UTXOs from btcwallet,
// converting lnwallet.Utxo to wallet.Utxo.
//
// We intentionally restrict CPFP fee selection to the default backing-wallet
// account. The lightweight wallet can also expose imported/custom-scope
// witness outputs that are not suitable fee inputs for this finalize path.
//
// WaitForSync is called first to close the race between the chain source
// actor (which may trigger the next broadcast immediately after a
// confirmation) and btcwallet's asynchronous block processing. Without
// this, change outputs from a prior CPFP may not yet be visible.
func (w *lwUnrollWallet) ListUnspent(ctx context.Context,
	minConfs, maxConfs int32) ([]*wallet.Utxo, error) {

	if err := w.Wallet.WaitForSync(ctx); err != nil {
		return nil, fmt.Errorf("wait for wallet sync: %w", err)
	}

	lnUtxos, err := w.Wallet.BtcWallet.ListUnspentWitness(
		minConfs, maxConfs, lnwallet.DefaultAccountName,
	)
	if err != nil {
		return nil, err
	}

	// btcwallet's default-account filter can miss wallet-managed P2TR
	// outputs in lwwallet mode even though the same UTXOs are visible when
	// enumerating across all accounts. Fall back to the broader query so
	// harness-funded fee inputs remain available for CPFP package relay.
	if len(lnUtxos) == 0 {
		lnUtxos, err = w.Wallet.BtcWallet.ListUnspentWitness(
			minConfs, maxConfs, "",
		)
		if err != nil {
			return nil, err
		}
	}

	result := make([]*wallet.Utxo, 0, len(lnUtxos))
	for _, u := range lnUtxos {
		result = append(result, &wallet.Utxo{
			Outpoint:      u.OutPoint,
			PkScript:      u.PkScript,
			Amount:        u.Value,
			Confirmations: int32(u.Confirmations),
		})
	}

	return result, nil
}

// NewWalletPkScript returns a fresh wallet-managed taproot pkScript suitable
// for change and sweep outputs.
func (w *lwUnrollWallet) NewWalletPkScript(ctx context.Context) ([]byte,
	error) {

	addr, err := w.Wallet.NewAddress(ctx)
	if err != nil {
		return nil, fmt.Errorf("new address: %w", err)
	}

	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, fmt.Errorf("pay to addr script: %w", err)
	}

	return pkScript, nil
}

// FinalizePsbt signs and finalizes a PSBT via btcwallet.
func (w *lwUnrollWallet) FinalizePsbt(_ context.Context,
	packetBytes []byte) (*wire.MsgTx, error) {

	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(packetBytes), false,
	)
	if err != nil {
		return nil, fmt.Errorf("parse PSBT: %w", err)
	}

	if err := w.Wallet.FinalizePsbtDirect(
		packet,
	); err != nil {
		return nil, fmt.Errorf("btcwallet FinalizePsbt: %w", err)
	}

	finalTx, err := psbt.Extract(packet)
	if err != nil {
		return nil, fmt.Errorf("extract finalized tx: %w", err)
	}

	return finalTx, nil
}

// LeaseOutput forwards the CPFP fee-input lease down to btcwallet. The
// darepo-local wallet.LockID is reinterpreted as wtxmgr.LockID (both are
// [32]byte) so the same lock survives restart and release.
func (w *lwUnrollWallet) LeaseOutput(_ context.Context, id wallet.LockID,
	op wire.OutPoint, expiry time.Duration) (time.Time, error) {

	return w.Wallet.BtcWallet.LeaseOutput(wtxmgr.LockID(id), op, expiry)
}

// ReleaseOutput forwards the unlock to btcwallet. The LockID must match the
// one used at lease time.
func (w *lwUnrollWallet) ReleaseOutput(_ context.Context, id wallet.LockID,
	op wire.OutPoint) error {

	return w.Wallet.BtcWallet.ReleaseOutput(wtxmgr.LockID(id), op)
}

// btcwUnrollWallet adapts the neutrino-backed btcwallet for the
// unroll broadcaster and executor wallet interfaces.
type btcwUnrollWallet struct {
	*btcwbackend.Wallet
}

// ListUnspent returns confirmed wallet UTXOs from btcwallet, converting
// lnwallet.Utxo to wallet.Utxo.
//
// We intentionally restrict CPFP fee selection to the default backing-wallet
// account. Imported/custom-scope witness outputs are not valid fee inputs for
// this finalize path.
func (w *btcwUnrollWallet) ListUnspent(_ context.Context,
	minConfs, maxConfs int32) ([]*wallet.Utxo, error) {

	lnUtxos, err := w.Wallet.BtcWallet.ListUnspentWitness(
		minConfs, maxConfs, lnwallet.DefaultAccountName,
	)
	if err != nil {
		return nil, err
	}

	result := make([]*wallet.Utxo, 0, len(lnUtxos))
	for _, u := range lnUtxos {
		result = append(result, &wallet.Utxo{
			Outpoint:      u.OutPoint,
			PkScript:      u.PkScript,
			Amount:        u.Value,
			Confirmations: int32(u.Confirmations),
		})
	}

	return result, nil
}

// NewWalletPkScript returns a fresh wallet-managed taproot pkScript suitable
// for change and sweep outputs.
func (w *btcwUnrollWallet) NewWalletPkScript(ctx context.Context) ([]byte,
	error) {

	addr, err := w.Wallet.NewAddress(ctx)
	if err != nil {
		return nil, fmt.Errorf("new address: %w", err)
	}

	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, fmt.Errorf("pay to addr script: %w", err)
	}

	return pkScript, nil
}

// FinalizePsbt signs and finalizes a PSBT via btcwallet.
func (w *btcwUnrollWallet) FinalizePsbt(_ context.Context,
	packetBytes []byte) (*wire.MsgTx, error) {

	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(packetBytes), false,
	)
	if err != nil {
		return nil, fmt.Errorf("parse PSBT: %w", err)
	}

	if err := w.Wallet.BtcWallet.FinalizePsbt(
		packet, lnwallet.DefaultAccountName,
	); err != nil {
		return nil, fmt.Errorf("btcwallet FinalizePsbt: %w", err)
	}

	finalTx, err := psbt.Extract(packet)
	if err != nil {
		return nil, fmt.Errorf("extract finalized tx: %w", err)
	}

	return finalTx, nil
}

// LeaseOutput forwards the CPFP fee-input lease down to the neutrino-backed
// btcwallet. The darepo-local wallet.LockID is reinterpreted as wtxmgr.LockID.
func (w *btcwUnrollWallet) LeaseOutput(_ context.Context, id wallet.LockID,
	op wire.OutPoint, expiry time.Duration) (time.Time, error) {

	return w.Wallet.BtcWallet.LeaseOutput(wtxmgr.LockID(id), op, expiry)
}

// ReleaseOutput forwards the unlock to btcwallet.
func (w *btcwUnrollWallet) ReleaseOutput(_ context.Context, id wallet.LockID,
	op wire.OutPoint) error {

	return w.Wallet.BtcWallet.ReleaseOutput(wtxmgr.LockID(id), op)
}

// initUnrollSubsystem creates and wires the new unilateral-exit runtime:
// shared tx confirmation, per-target unroll actors behind the registry, and
// the VTXO critical-expiry handoff into that registry.
func (s *Server) initUnrollSubsystem(ctx context.Context,
	chainSourceRef actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	],
	vtxoManagerRef actor.TellOnlyRef[vtxo.ManagerMsg]) error {

	dbStore := db.NewStore(
		s.db.DB, s.db.Queries, s.db.Backend(),
		s.subLogger(db.Subsystem),
	)
	ueStore := dbStore.NewUnilateralExitStore(s.clk)
	s.ueStore = ueStore
	vtxoStore := dbStore.NewVTXOStore(s.clk)

	// Build the wallet adapter shared by txconfirm and unroll
	// sweep signing.
	var unrollWallet interface {
		txconfirm.Wallet
		unroll.SweepWallet
	}

	switch s.cfg.Wallet.Type {
	case WalletTypeLnd:
		lndSvc := s.lnd.UnsafeFromSome()
		clientWallet := lndbackend.NewClientWallet(
			lndSvc.Signer, lndSvc.WalletKit,
		)
		boardingBackend := lndbackend.NewBoardingBackend(
			lndSvc.WalletKit, lndSvc.ChainKit,
		)
		w := &lndUnrollWallet{
			ClientWallet:    clientWallet,
			boardingBackend: boardingBackend,
		}
		unrollWallet = w

	case WalletTypeLwwallet:
		lww := s.lwWallet.UnsafeFromSome()
		w := &lwUnrollWallet{Wallet: lww}
		unrollWallet = w

	case WalletTypeBtcwallet:
		btcw := s.btcwWallet.UnsafeFromSome()
		w := &btcwUnrollWallet{Wallet: btcw}
		unrollWallet = w
	}

	// 1. Create and register the shared tx confirmation actor.
	txConfirm := txconfirm.NewTxBroadcasterActor(txconfirm.Config{
		ChainSource:           chainSourceRef,
		Wallet:                unrollWallet,
		Log:                   fn.Some(s.subLogger("TXCF")),
		MaxFeeRateSatPerVByte: s.unrollMaxFeeRate(),
		FeeBumpIntervalBlocks: s.unrollBumpAfterBlocks(),
	})
	txConfirmKey := actor.NewServiceKey[
		txconfirm.Msg, txconfirm.Resp,
	]("txconfirm")
	txConfirmRef := actor.RegisterWithSystem(
		s.actorSystem, "txconfirm", txConfirmKey, txConfirm,
	)
	txConfirm.SetSelfRef(txConfirmRef)

	// 2. Create and register the unroll registry.
	oorStore := dbStore.NewOORArtifactStore(s.clk)
	proofAssembler := &unroll.LocalProofAssembler{
		VTXOStore:     vtxoStore,
		ArtifactStore: oorStore,
	}

	registry := unroll.NewUnrollRegistryActor(unroll.RegistryConfig{
		Store: &unroll.DBRegistryStore{
			UEStore: ueStore,
		},
		DeliveryStore:              s.deliveryStore,
		ProofAssembler:             proofAssembler,
		VTXOStore:                  vtxoStore,
		TxConfirmRef:               txConfirmRef,
		ChainSource:                chainSourceRef,
		Wallet:                     unrollWallet,
		Log:                        fn.Some(s.subLogger("UNRL")),
		MaxSweepFeeRateSatPerVByte: s.unrollMaxFeeRate(),
	})
	s.unrollRegistry = registry
	s.unrollRegistryRef = fn.Some(registry.Ref())

	// 3. Restore non-terminal jobs from durable state.
	if err := registry.RestoreNonTerminal(ctx); err != nil {
		s.log.WarnS(ctx,
			"Failed to restore unroll jobs", err)
	}

	// 4. Wire the VTXO manager's ChainResolver to the unroll registry so
	// critical expiry triggers automatic unroll.
	chainResolverRef := actor.NewMapInputRef(
		registry.Ref(),
		func(msg vtxo.ExpiringNotification) unroll.RegistryMsg {
			return &unroll.EnsureUnrollRequest{
				Outpoint: msg.VTXO.Outpoint,
				Trigger:  unroll.TriggerCriticalExpiry,
			}
		},
	)

	// Set the real target on the lazy chain resolver created before
	// the VTXO manager. All existing VTXO actors already hold a
	// reference to the lazy resolver; setting the target here wires
	// the critical-expiry path through to the unroll manager.
	if s.lazyChainResolver != nil {
		s.lazyChainResolver.Set(chainResolverRef)
	}

	s.log.InfoS(ctx, "Unroll subsystem initialized")

	return nil
}

// unrollMaxFeeRate returns the configured max fee rate or zero to let
// the executor use its own default.
func (s *Server) unrollMaxFeeRate() int64 {
	if s.cfg.Unroll != nil &&
		s.cfg.Unroll.MaxFeeRateSatPerVByte > 0 {

		return s.cfg.Unroll.MaxFeeRateSatPerVByte
	}

	return 0
}

// unrollBumpAfterBlocks returns the configured fee-bump cadence (in
// blocks) for the shared txconfirm actor used by the unroll subsystem,
// or zero to let txconfirm fall back to DefaultFeeBumpIntervalBlocks.
func (s *Server) unrollBumpAfterBlocks() int32 {
	if s.cfg.Unroll != nil && s.cfg.Unroll.BumpAfterBlocks > 0 {
		return s.cfg.Unroll.BumpAfterBlocks
	}

	return 0
}

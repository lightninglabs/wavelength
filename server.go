package darepo

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/indexer"
	"github.com/lightninglabs/darepo/mailbox"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn"
)

// Server is the main operator daemon.
type Server struct {
	started atomic.Bool

	cfg *Config

	db *db.Store

	adminRPC *AdminRPCServer

	rpc *RPCServer

	log btclog.Logger

	// cancel is used to cancel the contexts passed from the Server to
	// any other subsystems.
	cancel fn.Option[context.CancelFunc]

	// quit is closed when the server is shutting down. And is used to exit
	// out of any calls made from the outside to this server.
	quit chan struct{}

	// wg is used to manage and monitor all goroutines started by the
	// server.
	wg sync.WaitGroup

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

// NewServer creates a new operator server.
func NewServer(ctx context.Context, cfg *Config) (*Server, error) {
	s := &Server{
		cfg:  cfg,
		log:  cfg.Log.UnwrapOr(btclog.Disabled),
		quit: make(chan struct{}),
	}

	s.log.InfoS(ctx, "Constructing Ark operator server")

	// Initialize database using the dedicated DB subsystem logger.
	dbLog := subLogger(cfg.Loggers, dbSubsystem)

	var err error
	s.db, err = db.NewStoreFromConfig(
		cfg.DB, dbLog, clock.NewDefaultClock(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create database: %w", err)
	}

	backendName := "sqlite"
	if cfg.DB.Backend == "postgres" {
		backendName = "postgres"
	}
	s.log.InfoS(ctx, "Database initialized", "backend", backendName)

	// Create admin RPC server with the ARPC subsystem logger.
	adminLog := subLogger(cfg.Loggers, adminRPCSubsystem)
	s.adminRPC, err = NewAdminRPCServer(cfg.AdminRPC, s, adminLog)
	if err != nil {
		return nil, fmt.Errorf("failed to create admin RPC server: %w",
			err)
	}

	// Create client RPC server with the ORPC subsystem logger.
	rpcLog := subLogger(cfg.Loggers, clientRPCSubsystem)
	s.rpc, err = NewRPCServer(cfg.RPC, s, rpcLog)
	if err != nil {
		return nil, fmt.Errorf("failed to create client RPC server: %w",
			err)
	}

	// Initialize the indexer subsystem (service, operator, bridge).
	if err := s.setupIndexerSubsystem(ctx); err != nil {
		return nil, fmt.Errorf(
			"failed to setup indexer subsystem: %w", err,
		)
	}

	return s, nil
}

// RunUntilShutdown runs the server until the provided context is cancelled.
// This is a blocking call that will exit once both the context is cancelled and
// all cleanup tasks have been completed.
func (s *Server) RunUntilShutdown(ctx context.Context) error {
	// Only allow the server to be started once.
	if !s.started.CompareAndSwap(false, true) {
		return nil
	}

	if err := s.start(ctx); err != nil {
		return err
	}

	// Wait for shutdown signal.
	select {
	case <-ctx.Done():
	case <-s.quit:
	}

	// Perform shutdown operations.
	return s.stop(ctx)
}

// start starts all the main server components. This must be non-blocking.
// The context passed in is only used for any synchronous calls made before this
// method returns. Any long-lived goroutines will use a separate context.
func (s *Server) start(ctx context.Context) error {
	_, cancel := context.WithCancel(context.Background())
	s.cancel = fn.Some(cancel)

	// Start the admin RPC server
	if err := s.adminRPC.Start(ctx); err != nil {
		return fmt.Errorf("unable to start admin server: %w", err)
	}

	// Start the client RPC server
	if err := s.rpc.Start(ctx); err != nil {
		return fmt.Errorf("unable to start client RPC server: %w", err)
	}

	s.log.InfoS(ctx, "Server started successfully")

	return nil
}

// stop stops all the main server components and waits for all goroutines
// to exit.
func (s *Server) stop(ctx context.Context) error {
	s.log.InfoS(ctx, "Shutting down server...")

	// Stop the admin RPC server
	if s.adminRPC != nil {
		if err := s.adminRPC.Stop(ctx); err != nil {
			s.log.ErrorS(ctx, "Could not stop admin RPC server", err)
		}
	}

	// Stop the client RPC server
	if s.rpc != nil {
		if err := s.rpc.Stop(ctx); err != nil {
			s.log.ErrorS(ctx, "Could not stop rpc server", err)
		}
	}

	// Stop the indexer subsystem and shared bridge.
	s.stopIndexerSubsystem(ctx)

	// Close database connection
	if s.db != nil {
		if err := s.db.Close(); err != nil {
			s.log.ErrorS(ctx, "Could not close database", err)
		}
	}

	// Cancel any outgoing calls.
	s.cancel.WhenSome(func(fn context.CancelFunc) {
		fn()
	})

	// Cancel any incoming calls.
	close(s.quit)

	// Wait for all goroutines to exit.
	s.wg.Wait()

	s.log.InfoS(ctx, "Shutdown complete")

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

// RPCAddr returns the address the client RPC server is listening on, or nil
// if the server hasn't been started yet.
func (s *Server) RPCAddr() net.Addr {
	if s.rpc == nil {
		return nil
	}

	return s.rpc.Addr()
}

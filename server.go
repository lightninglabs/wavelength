package darepo

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightningnetwork/lnd/fn"
)

// Server is the main operator daemon.
type Server struct {
	started atomic.Bool

	cfg *Config

	db *db.Store

	adminRPC *AdminRPCServer

	rpc *RPCServer

	loggerFactory func(subsystem string) btclog.Logger

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
}

// NewServer creates a new operator server.
func NewServer(ctx context.Context, cfg *Config) (*Server, error) {
	s := &Server{
		cfg:  cfg,
		quit: make(chan struct{}),
	}

	if err := s.setupLogging(); err != nil {
		return nil, fmt.Errorf("failed to setup logging: %w", err)
	}

	s.log.InfoS(ctx, "Constructing Ark operator server")

	// Initialize database
	var err error
	s.db, err = db.NewStoreFromConfig(cfg.DB, s.loggerFactory("STORE"))
	if err != nil {
		return nil, fmt.Errorf("failed to create database: %w", err)
	}

	backendName := "sqlite"
	if cfg.DB.Backend == "postgres" {
		backendName = "postgres"
	}
	s.log.InfoS(ctx, "Database initialized", "backend", backendName)

	// Create admin RPC server
	s.adminRPC, err = NewAdminRPCServer(
		cfg.AdminRPC, s, s.loggerFactory("ARPC"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create admin RPC server: %w",
			err)
	}

	// Create client RPC server
	s.rpc, err = NewRPCServer(cfg.RPC, s, s.loggerFactory("ORPC"))
	if err != nil {
		return nil, fmt.Errorf("failed to create client RPC server: %w",
			err)
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

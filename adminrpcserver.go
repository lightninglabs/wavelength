package darepo

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/lightninglabs/darepo/build"
	"google.golang.org/grpc"
)

// AdminRPCServer is a gRPC server that serves admin/operator commands.
type AdminRPCServer struct {
	// Required by the grpc-gateway/v2 library for forward compatibility.
	adminrpc.UnimplementedOperatorAdminServer

	cfg        *AdminRPCConfig
	grpcServer *grpc.Server
	listener   net.Listener

	server *Server

	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	quit chan struct{}
	wg   sync.WaitGroup

	log btclog.Logger
}

// NewAdminRPCServer creates a new admin RPC server.
func NewAdminRPCServer(cfg *AdminRPCConfig, operator *Server,
	log btclog.Logger) (*AdminRPCServer, error) {

	// Use existing listener if provided, otherwise bind a new TCP
	// listener.
	listener := cfg.Listener
	if listener == nil {
		var err error
		listener, err = net.Listen("tcp", cfg.ListenAddr)
		if err != nil {
			return nil, fmt.Errorf("admin RPC server unable "+
				"to listen on %s: %w",
				cfg.ListenAddr, err)
		}
	}

	s := &AdminRPCServer{
		cfg:        cfg,
		server:     operator,
		log:        log,
		grpcServer: grpc.NewServer(),
		listener:   listener,
		quit:       make(chan struct{}),
	}

	// Register the admin RPC service.
	adminrpc.RegisterOperatorAdminServer(s.grpcServer, s)

	return s, nil
}

// Start starts the admin RPC server.
func (a *AdminRPCServer) Start(ctx context.Context) error {
	if !atomic.CompareAndSwapUint32(&a.started, 0, 1) {
		return nil
	}

	a.log.InfoS(ctx, "Starting Admin RPC server")

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		a.log.InfoS(ctx, "Admin RPC server listening",
			"addr", a.listener.Addr())

		err := a.grpcServer.Serve(a.listener)
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			a.log.ErrorS(ctx, "Admin RPC server exited with error",
				err)
		}
	}()

	return nil
}

// Stop stops the admin RPC server.
func (a *AdminRPCServer) Stop(ctx context.Context) error {
	if !atomic.CompareAndSwapUint32(&a.stopped, 0, 1) {
		return nil
	}

	a.log.InfoS(ctx, "Stopping admin RPC server")

	close(a.quit)
	a.grpcServer.Stop()
	a.wg.Wait()

	return nil
}

// Addr returns the address the admin RPC server is listening on.
func (a *AdminRPCServer) Addr() net.Addr {
	if a.listener == nil {
		return nil
	}

	return a.listener.Addr()
}

// Info returns basic information about the operator server.
func (a *AdminRPCServer) Info(ctx context.Context,
	req *adminrpc.InfoRequest) (*adminrpc.InfoResponse, error) {

	var pubkey string
	if a.server.lnd != nil {
		pubkey = a.server.lnd.NodePubkey.String()
	}

	return &adminrpc.InfoResponse{
		Version: build.Version(),
		Pubkey:  pubkey,
		Network: a.server.cfg.Network,
	}, nil
}

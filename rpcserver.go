package darepo

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/arkrpc"
	"google.golang.org/grpc"
)

// RPCConfig contains configuration for the client-facing RPC server.
type RPCConfig struct {
	// RPCListen is the listen address for the client RPC server.
	//nolint:ll
	RPCListen string `long:"listen" description:"Listen address for the client RPC server"`

	// RPCListener is the listener for the client RPC server. If nil,
	// a new listener will be created.
	RPCListener net.Listener
}

// DefaultRPCConfig returns the default client RPC configuration.
func DefaultRPCConfig() *RPCConfig {
	return &RPCConfig{
		RPCListen: "localhost:7070",
	}
}

// AdminRPCConfig contains configuration for the admin RPC server.
type AdminRPCConfig struct {
	// RPCListen is the listen address for the admin RPC server.
	//nolint:ll
	RPCListen string `long:"listen" description:"Listen address for the admin RPC server"`

	// RPCListener is the listener for the admin RPC server. If nil,
	// a new listener will be created.
	RPCListener net.Listener
}

// DefaultAdminRPCConfig returns the default admin RPC configuration.
func DefaultAdminRPCConfig() *AdminRPCConfig {
	return &AdminRPCConfig{
		RPCListen: "localhost:8081",
	}
}

// RPCServer is a thin gRPC adapter that wraps the logical ark Server
// and serves client content over gRPC.
type RPCServer struct {
	// Required by the grpc-gateway/v2 library for forward compatibility.
	arkrpc.UnimplementedArkServiceServer

	cfg        *RPCConfig
	grpcServer *grpc.Server
	listener   net.Listener

	server *Server

	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	quit chan struct{}
	wg   sync.WaitGroup

	log btclog.Logger
}

// NewRPCServer creates a new client-facing RPC server.
func NewRPCServer(cfg *RPCConfig, operator *Server, logger btclog.Logger,
	serverOpts ...grpc.ServerOption) (*RPCServer, error) {

	// Use existing listener if provided
	listener := cfg.RPCListener
	if listener == nil {
		var err error
		listener, err = net.Listen("tcp", cfg.RPCListen)
		if err != nil {
			return nil, fmt.Errorf("client RPC server unable to "+
				"listen on %s: %w", cfg.RPCListen, err)
		}
	}

	s := &RPCServer{
		cfg:        cfg,
		server:     operator,
		log:        logger,
		grpcServer: grpc.NewServer(serverOpts...),
		listener:   listener,
		quit:       make(chan struct{}),
	}

	// Register the client RPC service.
	arkrpc.RegisterArkServiceServer(s.grpcServer, s)

	return s, nil
}

// Start starts the RPC server.
func (r *RPCServer) Start(ctx context.Context) error {
	if !atomic.CompareAndSwapUint32(&r.started, 0, 1) {
		return nil
	}

	r.log.InfoS(ctx, "Starting Client RPC server")

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()

		r.log.InfoS(ctx, "Client RPC server listening",
			"addr", r.listener.Addr())

		err := r.grpcServer.Serve(r.listener)
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			r.log.ErrorS(ctx, "Client RPC server exited with error",
				err)
		}
	}()

	return nil
}

// Stop stops the RPC server.
func (r *RPCServer) Stop(ctx context.Context) error {
	if !atomic.CompareAndSwapUint32(&r.stopped, 0, 1) {
		return nil
	}

	r.log.InfoS(ctx, "Stopping client RPC server")

	close(r.quit)
	r.grpcServer.Stop()
	r.wg.Wait()

	return nil
}

// Addr returns the address the RPC server is listening on.
func (r *RPCServer) Addr() net.Addr {
	if r.listener == nil {
		return nil
	}

	return r.listener.Addr()
}

// GetInfo returns basic information about the ark server.
func (r *RPCServer) GetInfo(ctx context.Context,
	req *arkrpc.GetInfoRequest) (*arkrpc.GetInfoResponse, error) {

	return &arkrpc.GetInfoResponse{
		Version:     "0.0.1-skeleton",
		Pubkey:      []byte{},
		Network:     r.server.cfg.Network,
		BlockHeight: 0,
	}, nil
}

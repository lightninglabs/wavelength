package darepo

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo/build"
	"google.golang.org/grpc"
)

// RPCConfig contains configuration for the client-facing RPC server.
type RPCConfig struct {
	// ListenAddr is the network address the gRPC server binds to.
	ListenAddr string `mapstructure:"listen"`

	// TLS contains optional TLS certificate paths for the
	// client-facing gRPC server. When nil, the server runs
	// without TLS.
	TLS *TLSConfig `mapstructure:"tls"`

	// Listener is an optional pre-created listener. When non-nil,
	// the daemon serves on this listener instead of binding to
	// ListenAddr. This enables SDK-style embedding and in-memory
	// transports such as bufconn for tests.
	Listener net.Listener
}

// DefaultRPCConfig returns the default client RPC configuration.
func DefaultRPCConfig() *RPCConfig {
	return &RPCConfig{
		ListenAddr: DefaultRPCListen,
	}
}

// AdminRPCConfig contains configuration for the admin RPC server.
type AdminRPCConfig struct {
	// ListenAddr is the network address the admin gRPC server binds
	// to.
	ListenAddr string `mapstructure:"listen"`

	// Listener is an optional pre-created listener. When non-nil,
	// the daemon serves on this listener instead of binding to
	// ListenAddr. This enables SDK-style embedding and in-memory
	// transports such as bufconn for tests.
	Listener net.Listener
}

// DefaultAdminRPCConfig returns the default admin RPC configuration.
func DefaultAdminRPCConfig() *AdminRPCConfig {
	return &AdminRPCConfig{
		ListenAddr: DefaultAdminRPCListen,
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
func NewRPCServer(cfg *RPCConfig, operator *Server,
	log btclog.Logger) (*RPCServer, error) {

	// Use existing listener if provided, otherwise bind a new TCP
	// listener.
	listener := cfg.Listener
	if listener == nil {
		var err error
		listener, err = net.Listen("tcp", cfg.ListenAddr)
		if err != nil {
			return nil, fmt.Errorf("client RPC server unable "+
				"to listen on %s: %w",
				cfg.ListenAddr, err)
		}
	}

	s := &RPCServer{
		cfg:        cfg,
		server:     operator,
		log:        log,
		grpcServer: grpc.NewServer(),
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

	resp := &arkrpc.GetInfoResponse{
		Version: build.Version(),
		Network: r.server.cfg.Network,
	}

	if r.server.lnd != nil {
		resp.Pubkey = r.server.lnd.NodePubkey[:]

		_, height, err := r.server.lnd.ChainKit.GetBestBlock(
			ctx,
		)
		if err != nil {
			r.log.WarnS(ctx, "Unable to get best "+
				"block", err)
		} else {
			resp.BlockHeight = uint32(height)
		}
	}

	return resp, nil
}

package ark

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// InProcessConfig configures an Ark SDK facade over an already-running daemon
// RPC server implementation in the same process.
type InProcessConfig struct {
	// DaemonServer is the in-process daemon RPC implementation that will
	// be registered on a private bufconn-backed gRPC server.
	DaemonServer waverpc.DaemonServiceServer

	// BufferSize overrides the bufconn listener size used for private
	// in-process gRPC traffic. When zero, the SDK uses a sane default.
	BufferSize int

	// DialOptions overrides the default gRPC dial options used against the
	// private in-memory listener.
	DialOptions []grpc.DialOption

	// ServerOptions are passed to the private gRPC server that exposes the
	// supplied daemon RPC implementation over bufconn.
	ServerOptions []grpc.ServerOption
}

// WrapDaemonServer creates an Ark SDK facade around an in-process daemon RPC
// server without dialing the daemon's public network listener.
//
// The returned client owns a private bufconn listener and gRPC server. It does
// not own the supplied daemon runtime, so Close only tears down the private
// transport used by this SDK facade.
func WrapDaemonServer(ctx context.Context,
	cfg InProcessConfig) (*Client, error) {

	if cfg.DaemonServer == nil {
		return nil, fmt.Errorf("daemon server is required")
	}

	bufferSize := cfg.BufferSize
	if bufferSize == 0 {
		bufferSize = defaultBufConnSize
	}

	listener := bufconn.Listen(bufferSize)
	grpcServer := grpc.NewServer(cfg.ServerOptions...)
	waverpc.RegisterDaemonServiceServer(grpcServer, cfg.DaemonServer)

	serveErrChan := make(chan error, 1)
	readyErrChan := make(chan error, 1)
	waitErrChan := make(chan error, 1)
	go func() {
		serveErr := grpcServer.Serve(listener)
		if errors.Is(serveErr, grpc.ErrServerStopped) {
			serveErr = nil
		}

		serveErrChan <- serveErr
		readyErrChan <- serveErr
		waitErrChan <- serveErr
		close(waitErrChan)
	}()

	dialOpts := append([]grpc.DialOption(nil), cfg.DialOptions...)
	if len(dialOpts) == 0 {
		dialOpts = append(
			dialOpts,
			grpc.WithTransportCredentials(
				insecure.NewCredentials(),
			),
		)
	}

	dialOpts = append(
		dialOpts, grpc.WithContextDialer(func(dialCtx context.Context,
			_ string) (net.Conn, error) {

			return listener.DialContext(dialCtx)
		}),
	)

	conn, err := grpc.NewClient("passthrough:///bufnet", dialOpts...)
	if err != nil {
		grpcServer.Stop()
		serveErr := waitForServeExit(ctx, serveErrChan)
		listenerErr := listener.Close()

		return nil, fmt.Errorf("dial in-process daemon: %w",
			errors.Join(err, serveErr, listenerErr))
	}

	if err := waitForReady(ctx, conn, readyErrChan); err != nil {
		closeErr := conn.Close()
		grpcServer.Stop()
		serveErr := waitForServeExit(ctx, serveErrChan)
		listenerErr := listener.Close()

		return nil, fmt.Errorf("wait for in-process daemon "+
			"readiness: %w",
			errors.Join(err, closeErr, serveErr, listenerErr))
	}

	return &Client{
		daemon: waverpc.NewDaemonServiceClient(conn),
		waitCh: waitErrChan,
		closeFn: func(closeCtx context.Context) error {
			closeErr := conn.Close()
			stopErr := gracefulStop(closeCtx, grpcServer)
			serveErr := waitForServeExit(closeCtx, serveErrChan)
			listenerErr := listener.Close()

			return errors.Join(
				closeErr, stopErr, serveErr, listenerErr,
			)
		},
	}, nil
}

// gracefulStop asks the private gRPC server to stop accepting work and waits
// for active RPC handlers to return before the caller's context expires.
func gracefulStop(ctx context.Context, grpcServer *grpc.Server) error {
	stopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
		return nil

	case <-ctx.Done():
		grpcServer.Stop()
		<-stopped

		return fmt.Errorf("gracefully stop in-process daemon "+
			"transport: %w", ctx.Err())
	}
}

// waitForServeExit waits for a private gRPC server goroutine to stop or for
// the caller's shutdown context to expire.
func waitForServeExit(ctx context.Context, serveErrChan <-chan error) error {
	select {
	case serveErr := <-serveErrChan:
		return serveErr

	case <-ctx.Done():
		return fmt.Errorf("wait for in-process daemon transport: %w",
			ctx.Err())
	}
}

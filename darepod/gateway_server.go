package darepod

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/gateway"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultGatewayReadHeaderTimeout = 5 * time.Second
	defaultGatewayReadTimeout       = 30 * time.Second

	// Server-streaming gateway responses intentionally keep writes open
	// beyond a fixed deadline.
	defaultGatewayWriteTimeout = 0

	defaultGatewayIdleTimeout = 60 * time.Second
)

// gatewayServer serves HTTP/JSON requests through generated grpc-gateway
// handlers.
type gatewayServer struct {
	cfg        *GatewayConfig
	endpoint   string
	rpcServer  *RPCServer
	daemonCfg  *Config
	registrars []RPCGatewayRegistrar
	log        btclog.Logger

	listener net.Listener
	httpSrv  *http.Server
	wg       sync.WaitGroup
	cancel   context.CancelFunc
}

// newGatewayServer constructs the daemon HTTP/JSON gateway.
func newGatewayServer(cfg *GatewayConfig, endpoint string, rpcServer *RPCServer,
	daemonCfg *Config, registrars []RPCGatewayRegistrar,
	log btclog.Logger) *gatewayServer {

	return &gatewayServer{
		cfg:        cfg,
		endpoint:   endpoint,
		rpcServer:  rpcServer,
		daemonCfg:  daemonCfg,
		registrars: registrars,
		log:        log,
	}
}

// Start starts the HTTP/JSON gateway.
func (g *gatewayServer) Start(ctx context.Context) error {
	if g == nil || g.cfg == nil || !g.cfg.Enabled {
		return nil
	}

	listener := g.cfg.Listener
	if listener == nil {
		var err error
		listener, err = net.Listen("tcp", g.cfg.ListenAddr)
		if err != nil {
			return fmt.Errorf("daemon gateway listen: %w", err)
		}
	}
	mux := runtime.NewServeMux(gateway.ServeMuxOptions(nil)...)

	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	endpoint := gateway.NormalizeEndpoint(g.endpoint)
	registerCtx, cancelRegister := context.WithCancel(ctx)

	if err := daemonrpc.RegisterDaemonServiceHandlerFromEndpoint(
		registerCtx, mux, endpoint, dialOpts,
	); err != nil {

		cancelRegister()
		_ = listener.Close()

		return fmt.Errorf("register daemon gateway handlers: %w", err)
	}

	for _, registrar := range g.registrars {
		if err := registrar(
			registerCtx, mux, endpoint, dialOpts, g.rpcServer,
			g.daemonCfg,
		); err != nil {

			cancelRegister()
			_ = listener.Close()

			return fmt.Errorf("register optional gateway "+
				"handlers: %w", err)
		}
	}

	g.listener = listener
	g.cancel = cancelRegister
	g.httpSrv = &http.Server{
		Handler: gateway.BrowserHeaders(
			mux, g.cfg.AllowedOrigins,
		),
		ReadTimeout:       defaultGatewayReadTimeout,
		ReadHeaderTimeout: defaultGatewayReadHeaderTimeout,
		WriteTimeout:      defaultGatewayWriteTimeout,
		IdleTimeout:       defaultGatewayIdleTimeout,
	}

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()

		g.log.InfoS(ctx, "HTTP gateway listening",
			"addr", listener.Addr(),
		)

		err := g.httpSrv.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			g.log.ErrorS(ctx, "HTTP gateway error", err)
		}
	}()

	return nil
}

// Stop stops the HTTP/JSON gateway.
func (g *gatewayServer) Stop(_ context.Context) error {
	if g == nil || g.httpSrv == nil {
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(
		context.Background(), DefaultShutdownTimeout,
	)
	defer cancel()

	//nolint:contextcheck // shutdown intentionally detaches from caller ctx
	err := g.httpSrv.Shutdown(shutdownCtx)
	if g.cancel != nil {
		g.cancel()
	}
	g.wg.Wait()

	return err
}

// Addr returns the address the gateway is listening on.
func (g *gatewayServer) Addr() net.Addr {
	if g == nil || g.listener == nil {
		return nil
	}

	return g.listener.Addr()
}

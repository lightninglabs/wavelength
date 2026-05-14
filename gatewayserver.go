package darepo

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightninglabs/darepo-client/gateway"
	"github.com/lightninglabs/darepo-client/serverconn"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultGatewayReadTimeout  = 5 * time.Second
	defaultGatewayWriteTimeout = 0
	defaultGatewayIdleTimeout  = 60 * time.Second
)

// gatewayRegisterFunc registers grpc-gateway handlers on the provided mux.
type gatewayRegisterFunc func(
	ctx context.Context, mux *runtime.ServeMux, endpoint string,
	opts []grpc.DialOption,
) error

// gatewayServer serves HTTP/JSON requests through generated grpc-gateway
// handlers.
type gatewayServer struct {
	cfg      *GatewayConfig
	name     string
	register gatewayRegisterFunc
	dialOpts []grpc.DialOption
	endpoint string
	log      btclog.Logger

	listener net.Listener
	httpSrv  *http.Server
}

// newGatewayServer constructs a gateway server for one gRPC endpoint.
func newGatewayServer(cfg *GatewayConfig, name, endpoint string,
	log btclog.Logger, dialOpts []grpc.DialOption,
	register gatewayRegisterFunc) *gatewayServer {

	return &gatewayServer{
		cfg:      cfg,
		name:     name,
		endpoint: endpoint,
		log:      log,
		dialOpts: dialOpts,
		register: register,
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
			return fmt.Errorf("%s gateway listen: %w", g.name, err)
		}
	}
	g.listener = listener

	mux := runtime.NewServeMux(
		gateway.ServeMuxOptions(gatewayHeaderMatcher)...,
	)

	endpoint := gateway.NormalizeEndpoint(g.endpoint)
	if err := g.register(ctx, mux, endpoint, g.dialOpts); err != nil {
		_ = listener.Close()

		return fmt.Errorf("%s gateway register handlers: %w", g.name,
			err)
	}

	g.httpSrv = &http.Server{
		Handler: gateway.BrowserHeaders(
			mux, g.cfg.AllowedOrigins, serverconn.AuthHeaderKey,
		),
		ReadTimeout:       defaultGatewayReadTimeout,
		ReadHeaderTimeout: defaultGatewayReadTimeout,
		WriteTimeout:      defaultGatewayWriteTimeout,
		IdleTimeout:       defaultGatewayIdleTimeout,
	}

	go func() {
		g.log.InfoS(ctx, g.name+" HTTP gateway listening",
			"addr", listener.Addr(),
		)

		err := g.httpSrv.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			g.log.ErrorS(ctx, g.name+" HTTP gateway exited "+
				"with error", err)
		}
	}()

	return nil
}

// Stop stops the HTTP/JSON gateway.
func (g *gatewayServer) Stop(ctx context.Context) error {
	if g == nil || g.httpSrv == nil {
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	return g.httpSrv.Shutdown(shutdownCtx)
}

// Addr returns the address the gateway is listening on.
func (g *gatewayServer) Addr() net.Addr {
	if g == nil || g.listener == nil {
		return nil
	}

	return g.listener.Addr()
}

// gatewayHeaderMatcher forwards selected HTTP headers into gRPC metadata.
func gatewayHeaderMatcher(key string) (string, bool) {
	if strings.EqualFold(key, serverconn.AuthHeaderKey) {
		return serverconn.AuthHeaderKey, true
	}

	return runtime.DefaultHeaderMatcher(key)
}

// gatewayDialOptions returns gRPC client options used by the local gateway.
func gatewayDialOptions(tlsCfg *TLSConfig) ([]grpc.DialOption, error) {
	if tlsCfg == nil {
		creds := insecure.NewCredentials()

		return []grpc.DialOption{
			grpc.WithTransportCredentials(creds),
		}, nil
	}

	creds, err := credentials.NewClientTLSFromFile(tlsCfg.CertPath, "")
	if err != nil {
		return nil, fmt.Errorf("load gateway TLS cert: %w", err)
	}

	return []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}, nil
}

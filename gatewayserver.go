package darepo

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightninglabs/darepo-client/gateway"
	"github.com/lightninglabs/darepo-client/serverconn"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

const (
	defaultGatewayReadHeaderTimeout = 5 * time.Second
	defaultGatewayReadTimeout       = 30 * time.Second

	// Server-streaming gateway responses intentionally keep writes open
	// beyond a fixed deadline.
	defaultGatewayWriteTimeout = 0

	defaultGatewayIdleTimeout = 60 * time.Second
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
	wg       sync.WaitGroup
	cancel   context.CancelFunc
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
	mux := runtime.NewServeMux(
		gateway.ServeMuxOptions(gatewayHeaderMatcher)...,
	)

	endpoint := gateway.NormalizeEndpoint(g.endpoint)
	registerCtx, cancelRegister := context.WithCancel(ctx)
	if err := g.register(
		registerCtx, mux, endpoint, g.dialOpts,
	); err != nil {

		cancelRegister()
		_ = listener.Close()

		return fmt.Errorf("%s gateway register handlers: %w", g.name,
			err)
	}

	g.listener = listener
	g.cancel = cancelRegister
	g.httpSrv = &http.Server{
		Handler: gateway.BrowserHeaders(
			mux, g.cfg.AllowedOrigins, serverconn.AuthHeaderKey,
		),
		ReadTimeout:       defaultGatewayReadTimeout,
		ReadHeaderTimeout: defaultGatewayReadHeaderTimeout,
		WriteTimeout:      defaultGatewayWriteTimeout,
		IdleTimeout:       defaultGatewayIdleTimeout,
	}

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()

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
func (g *gatewayServer) Stop(_ context.Context) error {
	if g == nil || g.httpSrv == nil {
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(
		context.Background(), 5*time.Second,
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

// gatewayHeaderMatcher forwards selected HTTP headers into gRPC metadata.
func gatewayHeaderMatcher(key string) (string, bool) {
	if strings.EqualFold(key, serverconn.AuthHeaderKey) {
		return serverconn.AuthHeaderKey, true
	}

	return runtime.DefaultHeaderMatcher(key)
}

// gatewayDialOptions returns gRPC client options used by the local gateway.
func gatewayDialOptions(tlsCfg *TLSConfig,
	gatewayAuthToken string) ([]grpc.DialOption, error) {

	var dialOpts []grpc.DialOption
	if tlsCfg == nil {
		creds := insecure.NewCredentials()

		dialOpts = []grpc.DialOption{
			grpc.WithTransportCredentials(creds),
		}
	} else {
		creds, err := credentials.NewClientTLSFromFile(
			tlsCfg.CertPath, "",
		)
		if err != nil {
			return nil, fmt.Errorf("load gateway TLS cert: %w", err)
		}

		dialOpts = []grpc.DialOption{
			grpc.WithTransportCredentials(creds),
		}
	}

	if gatewayAuthToken != "" {
		dialOpts = append(
			dialOpts,
			grpc.WithUnaryInterceptor(
				gatewayAuthUnaryInterceptor(gatewayAuthToken),
			),
		)
	}

	return dialOpts, nil
}

func gatewayAuthUnaryInterceptor(token string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any,
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption) error {

		ctx = metadata.AppendToOutgoingContext(
			ctx, gatewayAuthMetadataKey, token,
		)

		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

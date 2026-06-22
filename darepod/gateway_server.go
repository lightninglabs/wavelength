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

// errHealthUnavailable flags that the daemon health checks were not available
// when the gateway started, so the liveness/readiness probe routes could not be
// mounted.
var errHealthUnavailable = errors.New("daemon health check unavailable")

// gatewayServer serves HTTP/JSON requests through generated grpc-gateway
// handlers.
type gatewayServer struct {
	cfg        *GatewayConfig
	endpoint   string
	rpcServer  *RPCServer
	daemonCfg  *Config
	registrars []RPCGatewayRegistrar
	log        btclog.Logger

	// liveness and readiness answer the /v1/health and /v1/ready probe
	// routes. When nil they default to the daemon's LivenessCheck and
	// ReadinessCheck. Liveness reports process viability (a failure should
	// restart the pod); readiness reports serve-readiness (a failure only
	// drains the pod). Both perform in-process checks only and never touch
	// the chain backend, so an unauthenticated probe cannot amplify load
	// onto it.
	liveness  func(context.Context) error
	readiness func(context.Context) error

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

	// Mount unauthenticated liveness/readiness routes directly on the mux
	// so a k8s probe can detect a wedged-but-listening daemon. Both perform
	// only in-process checks (no backend round-trip), so they keep
	// answering when the chain backend is stuck and cannot be abused to
	// amplify load onto it. Liveness failure restarts the pod; readiness
	// failure only drains it.
	liveness, readiness := g.liveness, g.readiness
	if g.rpcServer != nil && g.rpcServer.server != nil {
		if liveness == nil {
			liveness = g.rpcServer.server.LivenessCheck
		}
		if readiness == nil {
			readiness = g.rpcServer.server.ReadinessCheck
		}
	}

	if liveness == nil || readiness == nil {
		// Make the absence visible: an operator wiring k8s probes to a
		// daemon whose health checks are unavailable would otherwise
		// see only silent 404s and assume the routes work.
		g.log.WarnS(ctx, "Health probe routes not registered",
			errHealthUnavailable,
		)
	} else {
		if err := mux.HandlePath(
			http.MethodGet, "/v1/health", healthHandler(liveness),
		); err != nil {

			cancelRegister()
			_ = listener.Close()

			return fmt.Errorf("register health route: %w", err)
		}
		if err := mux.HandlePath(
			http.MethodGet, "/v1/ready", healthHandler(readiness),
		); err != nil {

			cancelRegister()
			_ = listener.Close()

			return fmt.Errorf("register ready route: %w", err)
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

// healthHandler builds an HTTP handler that answers a liveness or readiness
// probe from the given check: 200 with `{"status":"ok"}` when healthy, 503 with
// the reason otherwise, so a wedged-but-listening daemon fails its probe. The
// check reports only in-process state, so the reason string carries no chain
// backend transport detail (no information disclosure to the unauthenticated
// caller).
func healthHandler(health func(context.Context) error) runtime.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request,
		_ map[string]string) {

		w.Header().Set("Content-Type", "application/json")

		if err := health(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)

			// The reason is a controlled in-process status string
			// (no backend detail), and the response is JSON with
			// %q-escaping, so reflecting it is safe.
			//nolint:gosec // G705: reason controlled, non-HTML.
			_, _ = fmt.Fprintf(
				w, `{"status":"unavailable","reason":%q}`+"\n",
				err.Error(),
			)

			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	}
}

// Addr returns the address the gateway is listening on.
func (g *gatewayServer) Addr() net.Addr {
	if g == nil || g.listener == nil {
		return nil
	}

	return g.listener.Addr()
}

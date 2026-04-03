package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	// DefaultListenAddr is the default address the metrics HTTP
	// server listens on.
	DefaultListenAddr = "0.0.0.0:9090"

	// defaultReadTimeout is the maximum duration for reading the
	// entire request, including the body.
	defaultReadTimeout = 5 * time.Second

	// defaultWriteTimeout is the maximum duration before timing
	// out writes of the response.
	defaultWriteTimeout = 10 * time.Second

	// defaultIdleTimeout is the maximum amount of time to wait
	// for the next request when keep-alives are enabled.
	defaultIdleTimeout = 60 * time.Second
)

// ServerConfig holds configuration for the Prometheus metrics HTTP
// server.
type ServerConfig struct {
	// ListenAddr is the address the metrics server listens on.
	ListenAddr string `mapstructure:"listen"`

	// Log is an optional logger for the metrics server. If not
	// provided, logging is disabled.
	Log fn.Option[btclog.Logger]
}

// DefaultServerConfig returns a ServerConfig with the default listen
// address.
func DefaultServerConfig() *ServerConfig {
	return &ServerConfig{
		ListenAddr: DefaultListenAddr,
	}
}

// ServerOption is a functional option for configuring the metrics
// HTTP server.
type ServerOption func(*serverOptions)

// serverOptions holds optional configuration for the Server.
type serverOptions struct {
	readTimeout  time.Duration
	writeTimeout time.Duration
	idleTimeout  time.Duration
}

// defaultServerOptions returns serverOptions populated with defaults.
func defaultServerOptions() serverOptions {
	return serverOptions{
		readTimeout:  defaultReadTimeout,
		writeTimeout: defaultWriteTimeout,
		idleTimeout:  defaultIdleTimeout,
	}
}

// WithReadTimeout overrides the HTTP server read timeout.
func WithReadTimeout(d time.Duration) ServerOption {
	return func(o *serverOptions) {
		if d > 0 {
			o.readTimeout = d
		}
	}
}

// WithWriteTimeout overrides the HTTP server write timeout.
func WithWriteTimeout(d time.Duration) ServerOption {
	return func(o *serverOptions) {
		if d > 0 {
			o.writeTimeout = d
		}
	}
}

// WithIdleTimeout overrides the HTTP server idle timeout.
func WithIdleTimeout(d time.Duration) ServerOption {
	return func(o *serverOptions) {
		if d > 0 {
			o.idleTimeout = d
		}
	}
}

// Server exposes a /metrics HTTP endpoint for Prometheus scraping.
type Server struct {
	cfg      *ServerConfig
	opts     serverOptions
	log      btclog.Logger
	httpSrv  *http.Server
	listener net.Listener
}

// NewServer creates a new metrics HTTP server. Call Start to begin
// serving.
func NewServer(cfg *ServerConfig, options ...ServerOption) *Server {
	opts := defaultServerOptions()
	for _, o := range options {
		o(&opts)
	}

	return &Server{
		cfg:  cfg,
		opts: opts,
		log:  cfg.Log.UnwrapOr(btclog.Disabled),
	}
}

// Start begins listening and serving the /metrics endpoint. It
// returns once the listener is bound; the HTTP server runs in a
// background goroutine. RegisterAll must be called before Start.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	listener, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("metrics listen: %w", err)
	}
	s.listener = listener

	s.httpSrv = &http.Server{
		Handler:           mux,
		ReadTimeout:       s.opts.readTimeout,
		ReadHeaderTimeout: s.opts.readTimeout,
		WriteTimeout:      s.opts.writeTimeout,
		IdleTimeout:       s.opts.idleTimeout,
	}

	go func() {
		if sErr := s.httpSrv.Serve(listener); sErr != nil &&
			!errors.Is(sErr, http.ErrServerClosed) {

			// Best-effort log; metrics server failure should
			// not crash the daemon.
			s.log.Warn("Metrics server error", sErr)
		}
	}()

	return nil
}

// Addr returns the address the metrics server is listening on. Returns
// nil if the server hasn't started.
func (s *Server) Addr() net.Addr {
	if s.listener == nil {
		return nil
	}

	return s.listener.Addr()
}

// Stop gracefully shuts down the metrics HTTP server.
func (s *Server) Stop(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}

	return s.httpSrv.Shutdown(ctx)
}

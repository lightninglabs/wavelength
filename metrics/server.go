package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	// DefaultListenAddr is a suggested address for the metrics HTTP
	// server when an operator opts in. The server stays disabled until
	// ListenAddr is set, so this constant is documentation only.
	DefaultListenAddr = "127.0.0.1:9092"

	// defaultReadTimeout bounds reading the entire request, including
	// the body.
	defaultReadTimeout = 5 * time.Second

	// defaultWriteTimeout bounds writing the scrape response.
	defaultWriteTimeout = 10 * time.Second

	// defaultIdleTimeout bounds how long an idle keep-alive connection
	// can sit around between scrapes.
	defaultIdleTimeout = 60 * time.Second
)

// ServerConfig holds configuration for the Prometheus metrics HTTP
// server.
//
// SECURITY: the /metrics endpoint exposes operational and balance data
// (VTXO counts and values, request rates). It is opt-in: an empty
// ListenAddr disables it entirely. Operators who enable it should bind
// ListenAddr to a loopback or firewalled address.
type ServerConfig struct {
	// ListenAddr is the address the metrics server binds to. An empty
	// value (the default) disables metrics serving.
	ListenAddr string `mapstructure:"listen"`
}

// DefaultServerConfig returns a disabled ServerConfig. Set ListenAddr
// to enable the /metrics endpoint.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{}
}

// Server exposes a /metrics HTTP endpoint for Prometheus scraping. The
// daemon owns this server's lifecycle; Start and Stop are called
// sequentially from the daemon run loop.
type Server struct {
	cfg ServerConfig
	log btclog.Logger

	// reg is the registry the /metrics handler gathers from. Each
	// daemon instance owns an isolated registry (not the global
	// default) so multiple daemons in one process — and a daemon that
	// is stopped and restarted — never collide on
	// AlreadyRegisteredError. Without isolation, a restarted daemon's
	// scrape-driven collector would silently keep the prior daemon's
	// (possibly closed) store.
	reg prometheus.Gatherer

	listener net.Listener
	httpSrv  *http.Server
	wg       sync.WaitGroup
}

// NewServer creates a new metrics HTTP server scraping the supplied
// registry. Call Start to begin serving. RegisterAll and any
// SystemCollector must be registered on the same registry before Start
// so the first scrape sees a complete metric set.
func NewServer(cfg ServerConfig, log btclog.Logger,
	reg prometheus.Gatherer) *Server {

	return &Server{
		cfg: cfg,
		log: log,
		reg: reg,
	}
}

// Start begins listening and serving /metrics when a listen address is
// configured. An empty listen address disables metrics and Start is a
// no-op. When enabled, Start returns once the listener is bound; the
// HTTP server runs in a background goroutine.
func (s *Server) Start(ctx context.Context) error {
	if s.cfg.ListenAddr == "" {
		s.log.DebugS(ctx, "Metrics server disabled")

		return nil
	}

	mux := http.NewServeMux()
	mux.Handle(
		"/metrics",
		promhttp.HandlerFor(
			s.reg, promhttp.HandlerOpts{},
		),
	)

	listener, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("metrics listen: %w", err)
	}
	s.listener = listener

	s.httpSrv = &http.Server{
		Handler:           mux,
		ReadTimeout:       defaultReadTimeout,
		ReadHeaderTimeout: defaultReadTimeout,
		WriteTimeout:      defaultWriteTimeout,
		IdleTimeout:       defaultIdleTimeout,
	}

	srvCtx := context.WithoutCancel(ctx)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		s.log.InfoS(srvCtx, "Metrics server listening",
			slog.String("addr", listener.Addr().String()),
		)

		// http.ErrServerClosed is the expected signal from a graceful
		// Shutdown, so it is not logged as an error. A mid-run
		// listener failure is an external trigger, so it warrants
		// WarnS, not the error level reserved for internal bugs.
		err := s.httpSrv.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.WarnS(srvCtx, "Metrics server error", err)
		}
	}()

	return nil
}

// Addr returns the address the metrics server is listening on, or nil
// when the server is disabled, not yet started, or already stopped.
func (s *Server) Addr() net.Addr {
	if s.listener == nil {
		return nil
	}

	return s.listener.Addr()
}

// Stop gracefully shuts down the metrics HTTP server using the
// caller-provided (bounded) context. It is a no-op when the server was
// never started.
func (s *Server) Stop(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}

	err := s.httpSrv.Shutdown(ctx)

	// Shutdown closes the listener before draining connections, so
	// Serve has already returned ErrServerClosed by now; wg.Wait only
	// joins the serve goroutine and cannot block beyond the deadline.
	s.wg.Wait()

	// Clear state so Addr reports the server as inactive and Stop is
	// idempotent.
	s.listener = nil
	s.httpSrv = nil

	return err
}

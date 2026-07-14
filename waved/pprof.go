package waved

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
	"sync"
	"time"

	"github.com/btcsuite/btclog/v2"
)

const (
	// defaultPprofReadHeaderTimeout bounds request-line/header reads so a
	// slow client cannot hold the local debug listener open indefinitely.
	defaultPprofReadHeaderTimeout = 5 * time.Second

	// defaultPprofReadTimeout bounds the full request read. It is larger
	// than the header timeout because /debug/pprof/symbol accepts a POST
	// body (a list of program counters) that must be read fully.
	defaultPprofReadTimeout = 30 * time.Second

	// CPU and trace profiles intentionally hold the response open for the
	// caller-requested sampling window, so no fixed write deadline applies.
	defaultPprofWriteTimeout = 0

	// defaultPprofIdleTimeout bounds how long an idle keep-alive connection
	// can sit around between debug requests.
	defaultPprofIdleTimeout = 60 * time.Second
)

// newPprofMux builds a private HTTP mux exposing the standard
// net/http/pprof endpoints. The mux is deliberately constructed here rather
// than reusing http.DefaultServeMux: importing net/http/pprof registers its
// handlers on the default mux as a side effect, and serving that mux would
// leak the sensitive debug surface onto any other server that happens to use
// the default mux. Routing through a private mux keeps pprof confined to its
// own opt-in listener.
func newPprofMux() *http.ServeMux {
	mux := http.NewServeMux()

	// The Index handler also dispatches the named profile sub-paths
	// (/debug/pprof/goroutine, /heap, /block, /mutex, /allocs,
	// /threadcreate), so a single "/debug/pprof/" registration covers
	// them along with the index page itself.
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	return mux
}

// pprofServer serves the standard net/http/pprof endpoints on a dedicated,
// opt-in HTTP listener.
//
// SECURITY: this server exposes sensitive runtime and debug data and is only
// started when PprofConfig.ListenAddr is non-empty. Operators are responsible
// for binding it to a loopback or firewalled address.
//
// The daemon owns this server's lifecycle; Start and Stop are called
// sequentially from the daemon run loop.
type pprofServer struct {
	cfg *PprofConfig
	log btclog.Logger

	listener net.Listener
	httpSrv  *http.Server
	wg       sync.WaitGroup
}

// newPprofServer constructs the daemon pprof debug server.
func newPprofServer(cfg *PprofConfig, log btclog.Logger) *pprofServer {
	return &pprofServer{
		cfg: cfg,
		log: log,
	}
}

// Start starts the pprof debug server when a listen address is configured. An
// empty listen address disables pprof entirely and Start is a no-op. When
// enabled, it also applies the configured block and mutex profiling rates.
func (p *pprofServer) Start(ctx context.Context) error {
	if p.cfg.ListenAddr == "" {
		p.log.DebugS(ctx, "pprof server disabled")

		return nil
	}

	listener, err := net.Listen("tcp", p.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("pprof listen: %w", err)
	}

	// Enable the optional sampling profilers after the listener is bound so
	// a startup failure cannot leave process-global profiling turned on.
	if p.cfg.BlockProfileRate > 0 {
		runtime.SetBlockProfileRate(p.cfg.BlockProfileRate)
	}
	if p.cfg.MutexProfileFraction > 0 {
		runtime.SetMutexProfileFraction(p.cfg.MutexProfileFraction)
	}

	p.listener = listener
	p.httpSrv = &http.Server{
		Handler:           newPprofMux(),
		ReadTimeout:       defaultPprofReadTimeout,
		ReadHeaderTimeout: defaultPprofReadHeaderTimeout,
		// 0 means no deadline, keeping streaming CPU/trace profiles
		// alive for their requested sampling window.
		WriteTimeout: defaultPprofWriteTimeout,
		IdleTimeout:  defaultPprofIdleTimeout,
	}

	srvCtx := context.WithoutCancel(ctx)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()

		p.log.InfoS(srvCtx, "pprof server listening",
			slog.String("addr", listener.Addr().String()),
		)

		// http.ErrServerClosed is the expected signal from a graceful
		// Shutdown, so it is not logged as an error. A mid-run listener
		// failure is an external trigger, so it warrants WarnS, not the
		// error level reserved for internal bugs.
		err := p.httpSrv.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.log.WarnS(srvCtx, "pprof server error", err)
		}
	}()

	return nil
}

// Stop gracefully shuts down the pprof debug server using the caller-provided
// (bounded) context, mirroring the daemon's other listener shutdowns. It is a
// no-op when the server was never started.
func (p *pprofServer) Stop(ctx context.Context) error {
	if p.httpSrv == nil {
		return nil
	}

	// Mirror the single-lifecycle Start conditions: only reset the
	// process-global profiling rates this server enabled.
	if p.cfg.BlockProfileRate > 0 {
		runtime.SetBlockProfileRate(0)
	}
	if p.cfg.MutexProfileFraction > 0 {
		runtime.SetMutexProfileFraction(0)
	}

	err := p.httpSrv.Shutdown(ctx)

	// Shutdown closes the listener before draining connections, so Serve
	// has already returned ErrServerClosed by now; wg.Wait only joins the
	// serve goroutine and cannot block beyond the shutdown deadline.
	p.wg.Wait()

	// Clear the listener so Addr reports the server as no longer active
	// once it has been stopped. Clear the HTTP server as well so Stop is
	// idempotent.
	p.listener = nil
	p.httpSrv = nil

	return err
}

// Addr returns the address the pprof server is listening on, or nil when the
// server is disabled, not yet started, or already stopped.
func (p *pprofServer) Addr() net.Addr {
	if p.listener == nil {
		return nil
	}

	return p.listener.Addr()
}

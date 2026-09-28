package waved

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	// healthInterval controls probe frequency independently of HTTP
	// traffic.
	healthInterval = 5 * time.Second

	// healthTimeout bounds the entire sample, including both actor probes.
	healthTimeout = 2 * time.Second

	// healthFreshness rejects stale success if the sampler stops
	// progressing.
	healthFreshness = healthInterval + healthTimeout

	// healthLivenessGrace bounds each actor's lack of completed turns.
	healthLivenessGrace = 2 * time.Minute
)

// HealthConfig enables a separate HTTP listener with only /readyz and /livez.
// It exposes no wallet data or RPC methods. Bind it to a private network or
// loopback; it intentionally requires no macaroon so supervisors can probe it.
type HealthConfig struct {
	// ListenAddr enables health serving when non-empty. Disabled by
	// default.
	ListenAddr string `mapstructure:"listen"`
}

// actorProbe returns both response status and completed-turn progress.
type actorProbe func(context.Context) (uint64, error)

// actorSample captures one actor's queue response and progress, independently
// of other actors. A timeout does not erase evidence that a busy loop is
// working.
type actorSample struct {
	completed uint64
	err       error
}

// actorHealth holds cached readiness and a separate activity clock per actor.
// The sampler writes; HTTP requests only read and never enqueue work.
type actorHealth struct {
	mu           sync.Mutex
	started      time.Time
	lastSuccess  time.Time
	responsive   bool
	completed    []uint64
	lastActivity []time.Time
}

// begin starts the local-wedge grace period after full daemon startup.
func (h *actorHealth) begin(now time.Time, actors int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.started = now
	h.completed = make([]uint64, actors)
	h.lastActivity = make([]time.Time, actors)
	for i := range h.lastActivity {
		h.lastActivity[i] = now
	}
}

// record withdraws readiness on any failed probe. Each actor independently
// resets its liveness deadline when it responds or finishes another turn. A
// busy manager must not hide a stuck round actor, or vice versa.
func (h *actorHealth) record(now time.Time, samples []actorSample) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.responsive = true
	for i, sample := range samples {
		if sample.err != nil {
			h.responsive = false
		}
		if sample.err == nil || sample.completed != h.completed[i] {
			h.lastActivity[i] = now
		}
		h.completed[i] = sample.completed
	}
	if h.responsive {
		h.lastSuccess = now
	}
}

// status keeps idle and pre-startup daemons live, gates readiness on a fresh
// successful sample, and fails liveness only after sustained local silence.
func (h *actorHealth) status(now time.Time) (bool, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started.IsZero() {
		return false, true
	}
	ready := h.responsive && now.Sub(h.lastSuccess) <= healthFreshness
	live := true
	for _, last := range h.lastActivity {
		if now.Sub(last) >= healthLivenessGrace {
			live = false
		}
	}

	return ready, live
}

// sampleActors asks each core receive loop to acknowledge a no-op. The shared
// deadline bounds admission and response across the full sample. Probes do not
// call LND, the operator, storage, or any actor behavior.
func sampleActors(ctx context.Context, probes []actorProbe) []actorSample {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	samples := make([]actorSample, len(probes))
	// Start both bounded probes together. A busy actor must not consume the
	// whole deadline before an idle peer gets its acknowledgement request.
	var wg sync.WaitGroup
	wg.Add(len(probes))
	for i, probe := range probes {
		go func() {
			defer wg.Done()
			completed, err := probe(ctx)
			samples[i] = actorSample{completed: completed, err: err}
		}()
	}
	wg.Wait()

	return samples
}

// monitor waits for the startup publication barrier before reading actor refs.
// All sampling belongs to the daemon lifetime, never to an HTTP request.
func (h *actorHealth) monitor(ctx context.Context, s *Server) {
	select {
	case <-ctx.Done():
		return

	case <-s.daemonReady:
	}
	h.begin(s.clk.Now(), len(s.actorProbes))
	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()
	for {
		samples := sampleActors(ctx, s.actorProbes)
		h.record(s.clk.Now(), samples)
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
		}
	}
}

// handler serves cached status only. Shutdown always fails both endpoints.
func (h *actorHealth) handler(ctx context.Context,
	now func() time.Time) http.Handler {

	mux := http.NewServeMux()
	for _, path := range []string{"/readyz", "/livez"} {
		mux.HandleFunc(
			"GET "+path,
			func(w http.ResponseWriter, r *http.Request) {
				ready, live := h.status(now())
				ok := ready
				if r.URL.Path == "/livez" {
					ok = live
				}
				w.Header().Set("Cache-Control", "no-store")
				if !ok || ctx.Err() != nil {
					http.Error(
						w, "unavailable",
						http.StatusServiceUnavailable,
					)

					return
				}
				w.WriteHeader(http.StatusOK)
			},
		)
	}

	return mux
}

// startHealthServer binds before dependency startup, so a locked wallet or
// unavailable startup dependency stays unready without causing restart loops.
// The returned cleanup cancels and joins sampling and bounds HTTP shutdown.
func (s *Server) startHealthServer(ctx context.Context) (func(), error) {
	if s.cfg.Health.ListenAddr == "" {
		return func() {}, nil
	}
	listener, err := net.Listen("tcp", s.cfg.Health.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("health listen: %w", err)
	}
	healthCtx, cancel := context.WithCancel(ctx)
	health := &actorHealth{}
	server := &http.Server{
		Handler:           health.handler(healthCtx, s.clk.Now),
		ReadHeaderTimeout: healthTimeout,
		ReadTimeout:       healthTimeout,
		WriteTimeout:      healthTimeout,
		IdleTimeout:       30 * time.Second,
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		health.monitor(healthCtx, s)
	}()
	go func() {
		defer wg.Done()
		if err := server.Serve(listener); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {

			s.log.ErrorS(healthCtx, "Health server failed", err)
		}
	}()

	// Cleanup is process-owned: the daemon context may already be
	// cancelled.
	//nolint:contextcheck
	return func() {
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(
			context.Background(), healthTimeout,
		)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
		wg.Wait()
	}, nil
}

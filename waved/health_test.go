package waved

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestActorHealthPolicy covers startup, idle operation, transient failure,
// sustained wedge, stale sampler output, and recovery without block arrivals.
func TestActorHealthPolicy(t *testing.T) {
	h := &actorHealth{}
	now := time.Unix(100, 0)
	ready, live := h.status(now.Add(time.Hour))
	require.False(t, ready)
	require.True(t, live, "startup delay must not trigger restarts")
	h.begin(now, 1)
	ready, live = h.status(now)
	require.False(t, ready)
	require.True(t, live)

	// No external dependency or block timestamp participates in health.
	for range 100 {
		now = now.Add(healthInterval)
		h.record(now, []actorSample{{}})
		ready, live = h.status(now)
		require.True(t, ready)
		require.True(t, live)
	}
	h.record(now.Add(time.Second), []actorSample{{
		err: errors.New("probe timed out"),
	}})
	ready, live = h.status(now.Add(time.Second))
	require.False(t, ready)
	require.True(t, live)
	ready, live = h.status(now.Add(healthLivenessGrace))
	require.False(t, ready)
	require.False(t, live)

	now = now.Add(healthLivenessGrace)
	h.record(now, []actorSample{{}})
	ready, live = h.status(now)
	require.True(t, ready)
	require.True(t, live)
	ready, live = h.status(now.Add(healthFreshness + time.Nanosecond))
	require.False(t, ready, "stale success must not mask a stopped sampler")
	require.True(t, live)
	_, live = h.status(now.Add(healthLivenessGrace))
	require.False(t, live)
}

// TestHealthMonitorStartupAndShutdown checks the startup publication barrier:
// no actor is probed until full startup completes; cancellation joins the loop.
func TestHealthMonitorStartupAndShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls atomic.Int32
	ready := make(chan struct{})
	s := &Server{daemonReady: ready, clk: clock.NewDefaultClock()}
	h := &actorHealth{}
	done := make(chan struct{})
	go func() {
		h.monitor(ctx, s)
		close(done)
	}()
	handler := h.handler(ctx, s.clk.Now)
	for range 20 {
		assertHealthStatus(
			t, handler, "/readyz", http.StatusServiceUnavailable,
		)
		assertHealthStatus(t, handler, "/livez", http.StatusOK)
	}
	require.Zero(t, calls.Load())

	// These refs are deliberately published after the monitor starts.
	// Closing ready provides the synchronization needed for concurrent HTTP
	// serving.
	s.actorProbes = []actorProbe{
		func(ctx context.Context) (uint64, error) {
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			require.LessOrEqual(
				t, time.Until(deadline), healthTimeout,
			)
			calls.Add(1)

			return 0, nil
		},
	}
	close(ready)
	require.Eventually(t, func() bool {
		ready, _ := h.status(s.clk.Now())

		return ready
	}, time.Second, time.Millisecond)
	assertHealthStatus(t, handler, "/readyz", http.StatusOK)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("health monitor did not shut down")
	}
	assertHealthStatus(t, handler, "/readyz", http.StatusServiceUnavailable)
	assertHealthStatus(t, handler, "/livez", http.StatusServiceUnavailable)
}

// healthTestMessage drives a local actor turn that can simulate a blocked peer
// or an external call. Health probes themselves never invoke this behavior.
type healthTestMessage struct {
	actor.BaseMessage
}

// MessageType identifies the test-only blocking turn.
func (healthTestMessage) MessageType() string { return "health-test" }

// TestHealthActorStallAndRecovery exercises real actor mailboxes through the
// daemon sampler. Either core actor can withdraw readiness; HTTP polling cannot
// enqueue work, and a bounded dependency failure can recover without restart.
func TestHealthActorStallAndRecovery(t *testing.T) {
	for _, stalled := range []int{0, 1} {
		name := []string{"manager", "round"}[stalled]
		t.Run(name, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			var refs []healthTestRef
			var probes []actorProbe
			for range 2 {
				ref := newHealthTestActor(t, entered, release)
				refs = append(refs, ref)
				probe := func(ctx context.Context) (uint64,
					error) {

					return actor.Probe(ctx, ref)
				}
				probes = append(probes, probe)
			}
			h := &actorHealth{}
			now := time.Unix(100, 0)
			h.begin(now, len(probes))
			samples := sampleActors(t.Context(), probes)
			require.NoError(t, samples[0].err)
			require.NoError(t, samples[1].err)
			h.record(now, samples)
			require.NoError(
				t,
				refs[stalled].Tell(
					t.Context(), healthTestMessage{},
				),
			)
			<-entered
			ctx, cancel := context.WithTimeout(
				t.Context(), 30*time.Millisecond,
			)
			defer cancel()
			samples = sampleActors(ctx, probes)
			require.ErrorIs(
				t, samples[stalled].err,
				context.DeadlineExceeded,
			)
			require.NoError(t, samples[1-stalled].err)
			h.record(now, samples)
			handler := h.handler(
				t.Context(),
				func() time.Time { return now },
			)
			for range 20 {
				assertHealthStatus(
					t, handler, "/readyz",
					http.StatusServiceUnavailable,
				)
				assertHealthStatus(
					t, handler, "/livez", http.StatusOK,
				)
			}
			close(release)
			samples = sampleActors(t.Context(), probes)
			require.NoError(t, samples[0].err)
			require.NoError(t, samples[1].err)
			now = now.Add(time.Second)
			h.record(now, samples)
			assertHealthStatus(t, handler, "/readyz", http.StatusOK)
		})
	}
}

// healthTestRef keeps the test actor's message/response pairing concise.
type healthTestRef = actor.ActorRef[healthTestMessage, struct{}]

// newHealthTestActor creates a receive loop that blocks only when sent a
// business message. Cancellation releases the turn and cleanup joins the actor.
func newHealthTestActor(t *testing.T, entered,
	release chan struct{}) healthTestRef {

	t.Helper()
	behavior := actor.NewFunctionBehavior(
		func(ctx context.Context,
			_ healthTestMessage) fn.Result[struct{}] {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}

			return fn.Err[struct{}](
				errors.New("dependency unavailable"),
			)
		},
	)
	var wg sync.WaitGroup
	a := actor.NewActor(actor.ActorConfig[healthTestMessage, struct{}]{
		ID: "core", Wg: &wg, Behavior: behavior,
	})
	a.Start()
	t.Cleanup(func() { a.Stop(); wg.Wait() })

	return a.Ref()
}

// assertHealthStatus checks only public HTTP status; probe endpoints
// deliberately reveal no internal error details or wallet data.
func assertHealthStatus(t *testing.T, handler http.Handler, path string,
	want int) {

	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response, httptest.NewRequest(http.MethodGet, path, nil),
	)
	require.Equal(t, want, response.Code)
}

// TestHealthServerLifecycle proves the opt-in listener can stop while startup
// is still blocked, without waiting for the daemon-ready signal or a client.
func TestHealthServerLifecycle(t *testing.T) {
	s := &Server{cfg: &Config{}}
	stop, err := s.startHealthServer(t.Context())
	require.NoError(t, err)
	stop()

	s.cfg.Health.ListenAddr = "127.0.0.1:0"
	s.clk = clock.NewDefaultClock()
	s.daemonReady = make(chan struct{})
	stop, err = s.startHealthServer(t.Context())
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("listener shutdown waited on incomplete daemon startup")
	}
}

// TestActorHealthBusyPeerIndependence keeps a saturated but progressing daemon
// live, then proves one busy actor cannot hide its peer's sustained wedge.
func TestActorHealthBusyPeerIndependence(t *testing.T) {
	h := &actorHealth{}
	now := time.Unix(100, 0)
	h.begin(now, 2)
	samples := []actorSample{
		{
			err: actor.ErrMailboxFull,
		},
		{
			err: context.DeadlineExceeded,
		},
	}
	for range 100 {
		now = now.Add(healthInterval)
		samples[0].completed++
		samples[1].completed++
		h.record(now, samples)
		ready, live := h.status(now)
		require.False(t, ready)
		require.True(t, live, "backlog must not cause a restart")
	}
	stoppedAt := now
	for now.Sub(stoppedAt) < healthLivenessGrace {
		now = now.Add(healthInterval)
		samples[0].completed++
		h.record(now, samples)
	}
	ready, live := h.status(now)
	require.False(t, ready)
	require.False(t, live, "busy manager must not mask a stuck round actor")
	samples[1].completed++
	h.record(now, samples)
	ready, live = h.status(now)
	require.False(t, ready)
	require.True(t, live, "progress restores liveness before readiness")
}

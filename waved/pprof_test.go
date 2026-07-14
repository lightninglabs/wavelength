package waved

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// TestNewPprofMuxServesStandardEndpoints verifies the private pprof mux serves
// the standard net/http/pprof endpoints, including the named profiles the
// Index handler dispatches, without registering them on the default mux.
func TestNewPprofMuxServesStandardEndpoints(t *testing.T) {
	t.Parallel()

	mux := newPprofMux()

	// Each path should resolve to a pprof handler rather than the
	// net/http 404 handler. The named profiles (goroutine, heap, ...) are
	// dispatched by the Index handler under the "/debug/pprof/" prefix.
	// The streaming profile/trace endpoints are covered by pattern lookup
	// below so this test does not trigger a live capture.
	paths := []string{
		"/debug/pprof/",
		"/debug/pprof/cmdline",
		"/debug/pprof/symbol",
		"/debug/pprof/goroutine",
		"/debug/pprof/heap",
		"/debug/pprof/block",
		"/debug/pprof/mutex",
		"/debug/pprof/allocs",
		"/debug/pprof/threadcreate",
	}

	for _, path := range paths {
		// Slashes delimit subtest hierarchy in `go test -run`, so the
		// raw path can't be the subtest name; use a sanitized key.
		name := strings.ReplaceAll(
			strings.TrimPrefix(path, "/"),
			"/", "_",
		)
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			require.NotEqual(
				t, http.StatusNotFound, rec.Code, "pprof "+
					"endpoint %q should be registered",
				path,
			)
		})
	}
}

// TestNewPprofMuxRejectsUnknownPaths verifies the private pprof mux does not
// serve paths outside the pprof surface, confirming it is a confined mux and
// not the catch-all default mux.
func TestNewPprofMuxRejectsUnknownPaths(t *testing.T) {
	t.Parallel()

	mux := newPprofMux()

	req := httptest.NewRequest(http.MethodGet, "/not-pprof", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestNewPprofMuxRegistersProfileAndTrace asserts the streaming profile and
// trace endpoints have their own explicit routes on the private mux. They are
// checked via the mux's pattern lookup rather than served, so the test does
// not trigger an actual (slow) CPU/trace capture. The assertion requires the
// matched pattern to equal the exact path: a prefix-only match against
// "/debug/pprof/" (i.e. the explicit registration removed) would fail.
func TestNewPprofMuxRegistersProfileAndTrace(t *testing.T) {
	t.Parallel()

	mux := newPprofMux()

	for _, path := range []string{
		"/debug/pprof/profile",
		"/debug/pprof/trace",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		_, pattern := mux.Handler(req)
		require.Equalf(
			t, path, pattern, "endpoint %q must have its own "+
				"explicit route", path,
		)
	}
}

// TestPprofServerStartStop exercises the server lifecycle: Start binds an
// OS-assigned loopback port and serves a pprof endpoint, and Stop releases
// the listener and lets the serving goroutine exit.
func TestPprofServerStartStop(t *testing.T) {
	t.Parallel()

	srv := newPprofServer(
		&PprofConfig{
			ListenAddr: "127.0.0.1:0",
		},
		btclog.Disabled,
	)

	require.NoError(t, srv.Start(context.Background()))

	addr := srv.Addr()
	require.NotNil(t, addr, "server must be listening after Start")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr.String() + "/debug/pprof/")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.NoError(t, srv.Stop(context.Background()))
	require.NoError(t, srv.Stop(context.Background()))

	// After Stop the listener is released, so a fresh request fails.
	resp, err = client.Get("http://" + addr.String() + "/debug/pprof/")
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err, "request after Stop should fail")
}

// TestPprofServerStartDisabledIsNoOp asserts Start is a no-op when the
// listen address is empty (pprof disabled).
func TestPprofServerStartDisabledIsNoOp(t *testing.T) {
	t.Parallel()

	srv := newPprofServer(&PprofConfig{}, btclog.Disabled)

	require.NoError(t, srv.Start(context.Background()))
	require.Nil(t, srv.Addr(), "disabled server must not listen")
	require.NoError(t, srv.Stop(context.Background()))
}

// TestPprofServerAppliesProfilingRates exercises the block/mutex profiling
// rate paths in Start and the reset in Stop. The rates are process-global
// with no public getter, so this guards the wiring rather than the runtime
// values. Not parallel: it mutates process-global runtime state.
func TestPprofServerAppliesProfilingRates(t *testing.T) {
	// Reset the process-global rates even if the test fails before Stop
	// resets them, so other tests are not affected.
	t.Cleanup(func() {
		runtime.SetBlockProfileRate(0)
		runtime.SetMutexProfileFraction(0)
	})

	srv := newPprofServer(
		&PprofConfig{
			ListenAddr:           "127.0.0.1:0",
			BlockProfileRate:     1,
			MutexProfileFraction: 1,
		},
		btclog.Disabled,
	)

	require.NoError(t, srv.Start(context.Background()))
	require.NotNil(t, srv.Addr())
	require.NoError(t, srv.Stop(context.Background()))

	// After Stop the listener is cleared, so Addr reports inactive.
	require.Nil(t, srv.Addr())
	require.NoError(t, srv.Stop(context.Background()))
}

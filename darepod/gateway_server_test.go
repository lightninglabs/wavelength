package darepod

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestGatewayHealthHandler verifies the liveness route returns 200 when the
// health check passes and 503 with the reason when it fails, so a k8s probe can
// restart a wedged-but-listening daemon.
func TestGatewayHealthHandler(t *testing.T) {
	t.Parallel()

	var healthErr error
	h := healthHandler(func(context.Context) error { return healthErr })

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/v1/health", nil), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"ok"`)

	healthErr = errors.New("chain backend unreachable: dial timeout")
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/v1/health", nil), nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "unreachable")
}

// TestServerLivenessReadiness verifies the split health checks: while the
// wallet is still starting (within the startup deadline) liveness is healthy
// but readiness reports not-ready; once the wallet is ready both pass; and a
// fatal escalation latches both unhealthy. Neither check touches the chain
// backend.
func TestServerLivenessReadiness(t *testing.T) {
	t.Parallel()

	s := &Server{startedAt: time.Now()}

	// Still starting: liveness healthy (within the deadline), readiness
	// not.
	require.NoError(t, s.LivenessCheck(context.Background()))
	require.ErrorContains(
		t,
		s.ReadinessCheck(
			context.Background(),
		),
		"not ready",
	)

	// Once the wallet subsystem is ready, readiness passes too.
	s.walletState.Store(int32(WalletStateReady))
	require.NoError(t, s.ReadinessCheck(context.Background()))
	require.NoError(t, s.LivenessCheck(context.Background()))

	// A fatal escalation latches both unhealthy.
	s.signalFatal(errors.New("boom"))
	require.ErrorContains(
		t,
		s.LivenessCheck(
			context.Background(),
		),
		"fatal",
	)
	require.ErrorContains(
		t,
		s.ReadinessCheck(
			context.Background(),
		),
		"fatal",
	)
}

// TestServerLivenessStartupDeadline verifies liveness fails once the daemon has
// sat in startup past the deadline without the wallet becoming ready, so a
// daemon wedged before init is restarted rather than reported live forever.
func TestServerLivenessStartupDeadline(t *testing.T) {
	t.Parallel()

	s := &Server{startedAt: time.Now().Add(-2 * healthStartupDeadline)}
	require.ErrorContains(
		t,
		s.LivenessCheck(
			context.Background(),
		),
		"not ready",
	)
}

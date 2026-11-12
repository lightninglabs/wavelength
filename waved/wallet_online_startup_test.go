package waved

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestHostStartupRunsLocalHooksWithPausedIngress reproduces the mobile boot
// path. No mailbox or wallet actor exists: online work here would fail/panic.
func TestHostStartupRunsLocalHooksWithPausedIngress(t *testing.T) {
	calls := 0
	srv := &Server{cfg: &Config{HostDrivenIngress: true,
		WalletReadyHooks: []WalletReadyHook{
			func(context.Context) error {
				calls++

				return nil
			},
		}},
		daemonReady: make(chan struct{})}
	require.False(t, srv.serverConnected.Load())
	require.NoError(t, srv.finishWalletStartup(t.Context()))
	require.Equal(t, 1, calls)
	require.False(t, srv.onlineStartup.complete)
	require.False(t, srv.serverConnected.Load())
	select {
	case <-srv.daemonReady:
	default:
		t.Fatal("local startup not ready")
	}
}

// TestOnlineStartupGatesReplayAndRetries proves no replay occurs before a
// successful policy read and that repeated foreground calls do not replay
// twice.
func TestOnlineStartupGatesReplayAndRetries(t *testing.T) {
	var phase walletOnlineStartup
	reads, replays := 0, 0
	offline := errors.New("transport unavailable")
	authenticate := func(context.Context) error {
		reads++
		if reads == 1 {
			return offline
		}

		return nil
	}
	replay := func(context.Context) error {
		replays++

		return nil
	}
	require.ErrorIs(
		t,
		phase.run(
			t.Context(), authenticate, replay,
		),
		offline,
	)
	require.Zero(t, replays)
	require.NoError(t, phase.run(t.Context(), authenticate, replay))
	require.NoError(t, phase.run(t.Context(), authenticate, replay))
	require.Equal(t, 2, reads)
	require.Equal(t, 1, replays)
}

// TestOnlineStartupCancellationDoesNotReplay covers a successful read racing
// caller cancellation; a later foreground attempt still initializes normally.
func TestOnlineStartupCancellationDoesNotReplay(t *testing.T) {
	var phase walletOnlineStartup
	ctx, cancel := context.WithCancel(t.Context())
	replays := 0
	authenticate := func(context.Context) error {
		cancel()

		return nil
	}
	replay := func(context.Context) error {
		replays++

		return nil
	}
	require.ErrorIs(
		t, phase.run(ctx, authenticate, replay), context.Canceled,
	)
	require.Zero(t, replays)
	require.NoError(
		t,
		phase.run(
			t.Context(),
			func(context.Context) error { return nil }, replay,
		),
	)
	require.Equal(t, 1, replays)
}

// TestOnlineStartupReplayFailuresRetry reproduces budget expiry inside replay,
// including a callback that returns success despite caller cancellation. A
// later host attempt must replay again and only then complete initialization.
func TestOnlineStartupReplayFailuresRetry(t *testing.T) {
	replayFailure := errors.New("replay unavailable")
	for _, test := range []struct {
		name   string
		cancel bool
		err    error
		want   error
	}{
		{
			"cancel reported",
			true,
			context.Canceled,
			context.Canceled,
		},
		{
			"cancel after success",
			true,
			nil,
			context.Canceled,
		},
		{
			"replay failure",
			false,
			replayFailure,
			replayFailure,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var phase walletOnlineStartup
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reads, replays := 0, 0
			authenticate := func(context.Context) error {
				reads++

				return nil
			}
			replay := func(context.Context) error {
				replays++
				if replays == 1 {
					if test.cancel {
						cancel()
					}

					return test.err
				}

				return nil
			}
			require.ErrorIs(
				t, phase.run(ctx, authenticate, replay),
				test.want,
			)
			require.False(t, phase.complete)
			require.NoError(
				t,
				phase.run(
					t.Context(), authenticate, replay,
				),
			)
			require.True(t, phase.complete)
			require.NoError(
				t,
				phase.run(
					t.Context(), authenticate, replay,
				),
			)
			require.Equal(t, 2, reads)
			require.Equal(t, 2, replays)
		})
	}
}

// TestOrdinaryStartupStillRequiresAuthenticatedPolicy is the negative control:
// non-host daemon hooks cannot run after a failed authenticated policy refresh.
func TestOrdinaryStartupStillRequiresAuthenticatedPolicy(t *testing.T) {
	calls := 0
	srv := &Server{cfg: &Config{WalletReadyHooks: []WalletReadyHook{
		func(context.Context) error {
			calls++

			return nil
		},
	}}, daemonReady: make(chan struct{})}
	require.ErrorContains(
		t,
		srv.finishWalletStartup(
			t.Context(),
		),
		"mailbox ingress not running",
	)
	require.Zero(t, calls)
}

// TestOnlineStartupConcurrentForegroundCalls shares one authenticated phase
// and replay even when two host callers reach startup concurrently.
func TestOnlineStartupConcurrentForegroundCalls(t *testing.T) {
	var phase walletOnlineStartup
	entered, release := make(chan struct{}), make(chan struct{})
	results := make(chan error, 2)
	reads, replays := 0, 0
	authenticate := func(context.Context) error {
		reads++
		close(entered)
		<-release

		return nil
	}
	replay := func(context.Context) error {
		replays++

		return nil
	}
	go func() {
		results <- phase.run(t.Context(), authenticate, replay)
	}()
	<-entered
	go func() {
		results <- phase.run(t.Context(), authenticate, replay)
	}()
	close(release)
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	require.Equal(t, 1, reads)
	require.Equal(t, 1, replays)
}

// TestForegroundBootstrapRetainsModeOwnership proves a pause cannot interrupt
// the authenticated request's ingress before its final access returns.
func TestForegroundBootstrapRetainsModeOwnership(t *testing.T) {
	ingress := &windowTestIngress{}
	controller := newWindowTestController(t, ingress)
	entered, release := make(chan struct{}), make(chan struct{})
	controller.afterForeground = func(context.Context) error {
		close(entered)
		<-release

		return nil
	}
	foreground, paused := make(chan error, 1), make(chan error, 1)
	go func() {
		foreground <- controller.setForeground(t.Context(), true)
	}()
	<-entered
	go func() {
		paused <- controller.setForeground(t.Context(), false)
	}()
	select {
	case err := <-paused:
		t.Fatalf("pause escaped bootstrap ownership: %v", err)

	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-foreground)
	require.NoError(t, <-paused)
	require.False(t, controller.foreground)
	require.ErrorIs(t, ingress.foreground.Err(), context.Canceled)
}

// TestForegroundBootstrapFailureRetainsJoinableIngress keeps mode reporting
// honest when policy fails after intake starts: pause must still join that run.
func TestForegroundBootstrapFailureRetainsJoinableIngress(t *testing.T) {
	ingress := &windowTestIngress{}
	controller := newWindowTestController(t, ingress)
	want := errors.New("policy unavailable")
	controller.afterForeground = func(context.Context) error { return want }
	require.ErrorIs(t, controller.setForeground(t.Context(), true), want)
	require.True(t, controller.foreground)
	require.NoError(t, ingress.foreground.Err())
	require.NoError(t, controller.setForeground(t.Context(), false))
	require.ErrorIs(t, ingress.foreground.Err(), context.Canceled)
}

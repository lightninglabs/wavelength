package waved

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWaitForWalletServicesReady verifies the readiness result is broadcast to
// every waiter and that the first success or pre-ready failure remains final.
func TestWaitForWalletServicesReady(t *testing.T) {
	t.Parallel()

	t.Run("success ignores later error", func(t *testing.T) {
		t.Parallel()

		server := &Server{
			walletServicesReady: make(chan struct{}),
		}
		ctx := t.Context()
		results := make(chan error, 2)
		for range 2 {
			go func() {
				results <- server.WaitForWalletServicesReady(
					ctx,
				)
			}()
		}

		server.markWalletServicesReady(nil)
		server.markWalletServicesReady(errors.New("late hook failed"))

		require.NoError(t, <-results)
		require.NoError(t, <-results)
	})

	t.Run("failure remains final", func(t *testing.T) {
		t.Parallel()

		wantErr := errors.New("mailbox ingress failed")
		server := &Server{
			walletServicesReady: make(chan struct{}),
		}
		server.markWalletServicesReady(wantErr)
		server.markWalletServicesReady(nil)

		require.ErrorIs(
			t,
			server.WaitForWalletServicesReady(
				t.Context(),
			),
			wantErr,
		)
		require.ErrorIs(
			t,
			server.WaitForWalletServicesReady(
				t.Context(),
			),
			wantErr,
		)
	})

	t.Run("context cancelled", func(t *testing.T) {
		t.Parallel()

		server := &Server{
			walletServicesReady: make(chan struct{}),
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		require.ErrorIs(
			t, server.WaitForWalletServicesReady(ctx),
			context.Canceled,
		)
	})

	t.Run("daemon exits before ready", func(t *testing.T) {
		t.Parallel()

		runErr := errors.New("run failed")
		server := &Server{
			walletServicesReady: make(chan struct{}),
		}
		server.markWalletServicesStopped(runErr, nil)

		require.ErrorIs(
			t,
			server.WaitForWalletServicesReady(
				t.Context(),
			),
			runErr,
		)
	})

	t.Run("shutdown before ready", func(t *testing.T) {
		t.Parallel()

		server := &Server{
			walletServicesReady: make(chan struct{}),
		}
		server.markWalletServicesStopped(nil, context.Canceled)

		require.ErrorIs(
			t,
			server.WaitForWalletServicesReady(
				t.Context(),
			),
			context.Canceled,
		)
	})

	t.Run("signal unavailable", func(t *testing.T) {
		t.Parallel()

		var server *Server
		err := server.WaitForWalletServicesReady(t.Context())
		require.ErrorContains(t, err, "readiness signal unavailable")
	})
}

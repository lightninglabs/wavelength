package waved

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRetryRecoveryIndexerRPCRetriesResourceExhausted verifies seed recovery
// backs off and retries when the operator query limiter rejects a scan request.
func TestRetryRecoveryIndexerRPCRetriesResourceExhausted(t *testing.T) {
	t.Parallel()

	var attempts int
	err := retryRecoveryIndexerRPC(t.Context(), func() error {
		attempts++
		if attempts == 1 {
			return status.Error(
				codes.ResourceExhausted, "rate limited",
			)
		}

		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, attempts)
}

// TestRetryRecoveryIndexerRPCStopsOnContextCancel verifies recovery does not
// spin forever if the restore RPC is cancelled during rate-limit backoff.
func TestRetryRecoveryIndexerRPCStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var attempts int
	err := retryRecoveryIndexerRPC(ctx, func() error {
		attempts++

		return status.Error(codes.ResourceExhausted, "rate limited")
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, attempts)
}

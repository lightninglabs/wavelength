package waved

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/serverconn/mailboxpull"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// testOperatorBackoff keeps the retry loop's sleeps negligible in tests.
var testOperatorBackoff = mailboxpull.BackoffConfig{
	BaseDelay: time.Millisecond,
	MaxDelay:  time.Millisecond,
}

// TestRetryWhileOperatorUnreachable pins which failures the startup connect
// retries: an unreachable operator until it answers, nothing that retrying
// cannot fix, and nothing once the daemon shuts down.
func TestRetryWhileOperatorUnreachable(t *testing.T) {
	t.Parallel()

	// The negotiation wraps the transport error, so the code has to be
	// found through the wrapping.
	unreachable := fmt.Errorf("negotiate ark protocol version: %w",
		status.Error(codes.Unavailable, "connection refused"))
	incompatible := errors.New("no compatible ark protocol version")

	tests := []struct {
		name string

		// failures lists the errors of the attempts before the one
		// that succeeds.
		failures []error

		wantErr      error
		wantAttempts int
		wantRetries  int
	}{{
		name:         "operator answers at once",
		wantAttempts: 1,
	}, {
		name: "operator comes back",
		failures: []error{
			unreachable, unreachable,
			status.Error(codes.DeadlineExceeded, "timeout"),
		},
		wantAttempts: 4,
		wantRetries:  3,
	}, {
		name: "incompatible operator",
		failures: []error{
			incompatible,
		},
		wantErr:      incompatible,
		wantAttempts: 1,
	}, {
		name: "incompatible after unreachable",
		failures: []error{
			unreachable,
			incompatible,
		},
		wantErr:      incompatible,
		wantAttempts: 2,
		wantRetries:  1,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var attempts, retries int
			attempt := func(context.Context) (string, error) {
				attempts++
				if attempts <= len(tc.failures) {
					return "", tc.failures[attempts-1]
				}

				return "negotiated", nil
			}
			onRetry := func(attempt int, _ error) {
				retries++
				require.Equal(t, retries, attempt)
			}

			got, err := retryWhileOperatorUnreachable(
				t.Context(), testOperatorBackoff, attempt,
				onRetry,
			)
			require.Equal(t, tc.wantAttempts, attempts)
			require.Equal(t, tc.wantRetries, retries)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				require.Empty(t, got)

				return
			}
			require.NoError(t, err)
			require.Equal(t, "negotiated", got)
		})
	}
}

// TestRetryWhileOperatorUnreachableStopsOnShutdown pins that a daemon shutting
// down while the operator is still unreachable stops retrying and reports the
// shutdown, so the wallet-services signal resolves with it instead of never.
func TestRetryWhileOperatorUnreachableStopsOnShutdown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var attempts int
	attempt := func(context.Context) (string, error) {
		attempts++
		if attempts == 3 {
			cancel()
		}

		return "", status.Error(codes.Unavailable, "connection refused")
	}

	_, err := retryWhileOperatorUnreachable(
		ctx, testOperatorBackoff, attempt,
		func(int, error) {},
	)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "connection refused")
	require.Equal(t, 3, attempts)
}

package ledger

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/stretchr/testify/require"
)

// TestLedgerTellRetryPolicy verifies permanent ledger failures are not
// retried while transient failures retain the durable actor's default policy.
func TestLedgerTellRetryPolicy(t *testing.T) {
	t.Parallel()

	permanentErrors := []error{
		ErrInvalidMessage,
		fmt.Errorf("wrapped: %w", ErrInvalidMessage),
		ErrIdempotencyConflict,
		fmt.Errorf("wrapped: %w", ErrIdempotencyConflict),
	}
	for _, testErr := range permanentErrors {
		retry, delay := ledgerTellRetryPolicy(testErr, 0)
		require.False(t, retry)
		require.Zero(t, delay)
	}

	transientErr := errors.New("storage unavailable")
	wantRetry, wantDelay := actor.DefaultTellRetryPolicy(transientErr, 2)
	gotRetry, gotDelay := ledgerTellRetryPolicy(transientErr, 2)
	require.Equal(t, wantRetry, gotRetry)
	require.Equal(t, wantDelay, gotDelay)
	require.Equal(t, 4*time.Second, gotDelay)
}

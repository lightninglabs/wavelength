package mailboxrpc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fastRetryPolicy returns a policy with the production shape but millisecond
// timings so the retry tests stay quick.
func fastRetryPolicy(attempts int) RetryPolicy {
	return RetryPolicy{
		MaxAttempts: attempts,
		BaseDelay:   time.Millisecond,
		MaxDelay:    5 * time.Millisecond,
	}
}

// TestRetryReusesIdempotencyKey pins the property the whole change exists for:
// every attempt at one logical request carries the same idempotency key, so
// the operator can recognize the retry and dedup it instead of billing it as
// new work.
func TestRetryReusesIdempotencyKey(t *testing.T) {
	t.Parallel()

	shed := status.Error(codes.ResourceExhausted, "rate limited")

	var keys []string
	err := Retry(
		context.Background(), fastRetryPolicy(3),
		func(_ context.Context, opts RPCOptions) error {
			keys = append(keys, opts.IdempotencyKey)

			return shed
		},
	)
	require.ErrorIs(t, err, shed)
	require.Len(t, keys, 3)

	require.NotEmpty(t, keys[0])
	for _, key := range keys[1:] {
		require.Equal(t, keys[0], key)
	}
}

// TestRetryDistinctLogicalRequests pins that the key identifies one logical
// request rather than one request payload: two calls that would build a
// byte-identical request must not collide, or the operator would answer the
// second read from the first read's cached result.
func TestRetryDistinctLogicalRequests(t *testing.T) {
	t.Parallel()

	capture := func() string {
		var key string
		err := Retry(
			context.Background(), fastRetryPolicy(1),
			func(_ context.Context, opts RPCOptions) error {
				key = opts.IdempotencyKey

				return nil
			},
		)
		require.NoError(t, err)

		return key
	}

	seen := make(map[string]struct{})
	for i := 0; i < 64; i++ {
		key := capture()
		require.NotEmpty(t, key)

		_, dup := seen[key]
		require.False(t, dup, "idempotency key %s reused", key)

		seen[key] = struct{}{}
	}
}

// TestRetryBacksOffOnResourceExhausted pins that an explicit shed answer costs
// the client a real wait rather than an immediate re-send. Hammering is what
// turns operator overload into a feedback loop.
func TestRetryBacksOffOnResourceExhausted(t *testing.T) {
	t.Parallel()

	const (
		attempts = 4
		base     = 20 * time.Millisecond
	)

	policy := RetryPolicy{
		MaxAttempts: attempts,
		BaseDelay:   base,
		MaxDelay:    time.Second,
	}

	var calls int
	start := time.Now()
	err := Retry(
		context.Background(), policy,
		func(_ context.Context, _ RPCOptions) error {
			calls++

			return status.Error(
				codes.ResourceExhausted, "rate limited",
			)
		},
	)
	elapsed := time.Since(start)

	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Equal(t, attempts, calls)

	// Three gaps at half the un-jittered exponential minimum
	// (base + 2*base + 4*base) / 2 is the floor the jitter can produce.
	minWait := (base + 2*base + 4*base) / 2
	require.GreaterOrEqual(t, elapsed, minWait)
}

// TestRetryStopsOnNonShedError pins that only an explicit shed is retried. Any
// other failure is either terminal or already the operator's answer, and
// re-sending it just adds load.
func TestRetryStopsOnNonShedError(t *testing.T) {
	t.Parallel()

	codesUnderTest := []codes.Code{
		codes.InvalidArgument,
		codes.NotFound,
		codes.PermissionDenied,
		codes.DeadlineExceeded,
		codes.Unavailable,
	}

	for _, code := range codesUnderTest {
		t.Run(code.String(), func(t *testing.T) {
			t.Parallel()

			var calls int
			err := Retry(
				context.Background(), fastRetryPolicy(5),
				func(_ context.Context, _ RPCOptions) error {
					calls++

					return status.Error(code, "boom")
				},
			)

			require.Equal(t, code, status.Code(err))
			require.Equal(t, 1, calls)
		})
	}
}

// TestRetryStopsOnCancelledContext pins that a caller who has already given up
// never puts another request on the wire. The abandoned work is exactly what
// produces a response with no waiter left to receive it.
func TestRetryStopsOnCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	var calls int
	err := Retry(
		ctx, fastRetryPolicy(5),
		func(_ context.Context, _ RPCOptions) error {
			calls++
			cancel()

			return status.Error(
				codes.ResourceExhausted, "rate limited",
			)
		},
	)

	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Equal(t, 1, calls)
}

// TestRetrySucceedsAfterShed pins that a shed request that later gets through
// returns the successful result, and that the successful attempt carried the
// same key as the shed ones.
func TestRetrySucceedsAfterShed(t *testing.T) {
	t.Parallel()

	var keys []string
	err := Retry(
		context.Background(), fastRetryPolicy(4),
		func(_ context.Context, opts RPCOptions) error {
			keys = append(keys, opts.IdempotencyKey)
			if len(keys) < 3 {
				return status.Error(
					codes.ResourceExhausted, "rate limited",
				)
			}

			return nil
		},
	)
	require.NoError(t, err)
	require.Len(t, keys, 3)
	require.Equal(t, keys[0], keys[2])
}

// TestRetryWithKeyPreservesCallerKey pins that a caller holding a durable key
// keeps it, so a retry that spans a restart is still recognizable to the
// operator.
func TestRetryWithKeyPreservesCallerKey(t *testing.T) {
	t.Parallel()

	const key = "idem-durable-key"

	var keys []string
	err := RetryWithKey(
		context.Background(), fastRetryPolicy(2), key,
		func(_ context.Context, opts RPCOptions) error {
			keys = append(keys, opts.IdempotencyKey)

			return status.Error(
				codes.ResourceExhausted, "rate limited",
			)
		},
	)
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Equal(t, []string{key, key}, keys)
}

// TestIsShedError pins the classification the retry loop keys off, including
// the wrapped case: callers routinely wrap transport errors with context
// before they reach a retry boundary.
func TestIsShedError(t *testing.T) {
	t.Parallel()

	shed := status.Error(codes.ResourceExhausted, "rate limited")

	require.False(t, IsShedError(nil))
	require.True(t, IsShedError(shed))
	require.True(
		t,
		IsShedError(
			fmt.Errorf("list VTXOs by script: %w", shed),
		),
	)
	require.False(t, IsShedError(errors.New("boom")))
	require.False(
		t,
		IsShedError(
			status.Error(codes.DeadlineExceeded, "too slow"),
		),
	)
}

// TestNewIdempotencyKeyIsUnique pins that freshly minted keys do not repeat,
// since a collision would make the operator answer one request with another's
// result.
func TestNewIdempotencyKeyIsUnique(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{})
	for i := 0; i < 1024; i++ {
		key, err := NewIdempotencyKey()
		require.NoError(t, err)
		require.Len(t, key, idempotencyKeyBytes*2)

		_, dup := seen[key]
		require.False(t, dup)

		seen[key] = struct{}{}
	}
}

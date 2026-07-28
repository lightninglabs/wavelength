package waved

import (
	"context"
	"testing"

	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRetryRecoveryIndexerRPCRetriesResourceExhausted verifies seed recovery
// backs off and retries when the operator query limiter rejects a scan request.
func TestRetryRecoveryIndexerRPCRetriesResourceExhausted(t *testing.T) {
	t.Parallel()

	var attempts int
	err := retryRecoveryIndexerRPC(
		t.Context(), func(_ mailboxrpc.RPCOptions) error {
			attempts++
			if attempts == 1 {
				return status.Error(
					codes.ResourceExhausted, "rate limited",
				)
			}

			return nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, 2, attempts)
}

// TestRetryRecoveryIndexerRPCReusesIdempotencyKey verifies every re-issue of
// one recovery scan carries the same idempotency key. Recovery walks the whole
// recovery window and is the heaviest indexer client the daemon has, so a
// fresh key per attempt would make the operator re-run every shed scan instead
// of deduplicating it.
func TestRetryRecoveryIndexerRPCReusesIdempotencyKey(t *testing.T) {
	t.Parallel()

	var keys []string
	err := retryRecoveryIndexerRPC(
		t.Context(), func(opts mailboxrpc.RPCOptions) error {
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

	require.NotEmpty(t, keys[0])
	require.Equal(t, keys[0], keys[1])
	require.Equal(t, keys[0], keys[2])
}

// TestRetryRecoveryIndexerRPCDistinctLogicalScans verifies two separate
// recovery scans do not share a key. Recovery pages through scripts with
// structurally similar queries, and collapsing two of them into one operator
// side operation would let a later page be answered from an earlier page's
// result.
func TestRetryRecoveryIndexerRPCDistinctLogicalScans(t *testing.T) {
	t.Parallel()

	scanKey := func() string {
		var key string
		err := retryRecoveryIndexerRPC(
			t.Context(), func(opts mailboxrpc.RPCOptions) error {
				key = opts.IdempotencyKey

				return nil
			},
		)
		require.NoError(t, err)

		return key
	}

	first, second := scanKey(), scanKey()
	require.NotEmpty(t, first)
	require.NotEqual(t, first, second)
}

// TestRetryRecoveryIndexerRPCStopsOnContextCancel verifies recovery does not
// spin forever if the restore RPC is cancelled during rate-limit backoff. A
// cancelled caller must not put another scan on the wire at all: the operator
// would run it and answer nobody, which is the abandoned in-flight work this
// path exists to avoid.
func TestRetryRecoveryIndexerRPCStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var attempts int
	err := retryRecoveryIndexerRPC(
		ctx,
		func(_ mailboxrpc.RPCOptions) error {
			attempts++

			return status.Error(
				codes.ResourceExhausted, "rate limited",
			)
		},
	)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, attempts)
}

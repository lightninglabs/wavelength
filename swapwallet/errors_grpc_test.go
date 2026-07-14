//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestMapSentinel maps each swapwallet sentinel to its gRPC code and stable
// ErrorInfo reason, and leaves pre-formed statuses and unknown errors alone.
func TestMapSentinel(t *testing.T) {
	t.Parallel()

	for _, m := range sentinelMappings {
		// A wrapped sentinel maps to the right code + reason detail.
		mapped := mapSentinel(fmt.Errorf("context: %w", m.err))
		st, ok := status.FromError(mapped)
		require.True(t, ok, "reason=%s", m.reason)
		require.Equal(t, m.code, st.Code(), "reason=%s", m.reason)
		require.Equal(t, m.reason, errorInfoReason(t, st))
	}

	// A non-sentinel error passes through unchanged.
	other := errors.New("some other failure")
	require.Equal(t, other, mapSentinel(other))

	// An error that already carries a gRPC status is left alone.
	pre := status.Error(codes.NotFound, "missing")
	require.Equal(t, pre, mapSentinel(pre))
}

// TestSentinelMappingsCoverage asserts that every request-rejection sentinel
// the package defines is either mapped to a machine-readable reason or
// explicitly exempt. A new sentinel added to errors.go without a mapping (the
// regression that left the Recv limit errors as opaque codes.Unknown) is caught
// here: add it to sentinelMappings, or to exempt below with a justification.
func TestSentinelMappingsCoverage(t *testing.T) {
	t.Parallel()

	// Every request-rejection sentinel the swapwallet package defines.
	allTaxonomySentinels := []error{
		ErrSwapBackendUnavailable,
		ErrInvalidDestination,
		ErrInvalidSendIntent,
		ErrAmountRequired,
		ErrAmountInvalid,
		ErrUnsupportedKind,
		ErrAmountExceedsVTXOLimit,
		ErrBalanceLimitExceeded,
		ErrWalletNotReady,
	}

	// ErrWalletNotReady is intentionally unmapped: the readiness gate
	// returns waverpc's own structured WALLET_NOT_READY status, so the
	// bare sentinel never reaches the interceptor.
	exempt := map[error]bool{
		ErrWalletNotReady: true,
	}

	// The table itself must be internally consistent: no duplicate
	// sentinel, no duplicate reason, no empty reason.
	mapped := make(map[error]bool, len(sentinelMappings))
	reasons := make(map[string]bool, len(sentinelMappings))
	for _, m := range sentinelMappings {
		require.NotNil(t, m.err, "mapping has a nil sentinel")
		require.NotEmpty(t, m.reason, "mapping has an empty reason")
		require.False(
			t, mapped[m.err], "duplicate sentinel in table: %v",
			m.err,
		)
		require.False(
			t, reasons[m.reason], "duplicate reason in table: %s",
			m.reason,
		)
		mapped[m.err] = true
		reasons[m.reason] = true
	}

	// Every taxonomy sentinel is mapped unless it is explicitly exempt.
	for _, s := range allTaxonomySentinels {
		if exempt[s] {
			require.False(
				t, mapped[s], "exempt sentinel must not be "+
					"mapped: %v", s,
			)

			continue
		}

		require.True(
			t, mapped[s], "taxonomy sentinel is not mapped; add "+
				"it to sentinelMappings or exempt it: %v", s,
		)
	}

	// The table covers exactly the non-exempt taxonomy sentinels: a stray
	// mapping not listed above, or a listed sentinel left unmapped, trips
	// this length check.
	require.Len(
		t, sentinelMappings, len(allTaxonomySentinels)-len(exempt),
	)
}

// TestRecvLimitSentinelsMapped locks the fix that brought the two
// operator-limit Recv rejections into the taxonomy: each must map to
// FailedPrecondition with its stable reason, rather than crossing the wire as
// an opaque codes.Unknown.
func TestRecvLimitSentinelsMapped(t *testing.T) {
	t.Parallel()

	cases := []struct {
		sentinel error
		reason   string
	}{
		{
			ErrAmountExceedsVTXOLimit,
			wavewalletrpc.ReasonAmountExceedsVTXOLimit,
		},
		{
			ErrBalanceLimitExceeded,
			wavewalletrpc.ReasonBalanceLimitExceeded,
		},
	}
	for _, tc := range cases {
		mapped := mapSentinel(fmt.Errorf("context: %w", tc.sentinel))
		st, ok := status.FromError(mapped)
		require.True(t, ok, "reason=%s", tc.reason)
		require.Equal(
			t, codes.FailedPrecondition, st.Code(),
			"reason=%s", tc.reason,
		)
		require.Equal(t, tc.reason, errorInfoReason(t, st))
	}
}

// TestStatusSwapBackendUnavailable confirms the shared pre-formed-status helper
// carries the same code and ErrorInfo the interceptor attaches to a bare
// sentinel, so handlers that must return a status directly (the readiness gate,
// the admin proxies) stay SDK-reconstructable.
func TestStatusSwapBackendUnavailable(t *testing.T) {
	t.Parallel()

	st, ok := status.FromError(statusSwapBackendUnavailable())
	require.True(t, ok)
	require.Equal(t, codes.Unavailable, st.Code())
	require.Equal(
		t, wavewalletrpc.ReasonSwapBackendUnavailable,
		errorInfoReason(t, st),
	)
}

// TestErrorMappingInterceptor confirms the interceptor passes a nil error and
// happy-path response through, and maps a handler sentinel.
func TestErrorMappingInterceptor(t *testing.T) {
	t.Parallel()

	okHandler := func(context.Context, any) (any, error) {
		return "resp", nil
	}
	resp, err := ErrorMappingInterceptor(
		context.Background(), nil, &grpc.UnaryServerInfo{}, okHandler,
	)
	require.NoError(t, err)
	require.Equal(t, "resp", resp)

	failHandler := func(context.Context, any) (any, error) {
		return nil, ErrAmountInvalid
	}
	_, err = ErrorMappingInterceptor(
		context.Background(), nil, &grpc.UnaryServerInfo{}, failHandler,
	)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.InvalidArgument, st.Code())
	require.Equal(
		t, wavewalletrpc.ReasonAmountInvalid, errorInfoReason(t, st),
	)

	// A handler returning a pre-formed status is passed through untouched.
	statusHandler := func(context.Context, any) (any, error) {
		return nil, status.Error(codes.NotFound, "missing")
	}
	_, err = ErrorMappingInterceptor(
		context.Background(), nil, &grpc.UnaryServerInfo{},
		statusHandler,
	)
	st, ok = status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.NotFound, st.Code())
	require.Empty(t, st.Details())
}

// errorInfoReason extracts the wavewalletdk ErrorInfo reason from a status,
// failing the test if no wavewalletdk detail is present.
func errorInfoReason(t *testing.T, st *status.Status) string {
	t.Helper()

	for _, detail := range st.Details() {
		info, ok := detail.(*errdetails.ErrorInfo)
		if !ok {
			continue
		}
		require.Equal(t, wavewalletrpc.FailureDomain, info.GetDomain())

		return info.GetReason()
	}
	t.Fatalf("status carried no wavewalletdk ErrorInfo detail: %v", st)

	return ""
}

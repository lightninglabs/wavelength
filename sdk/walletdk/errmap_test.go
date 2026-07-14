//go:build !js

package walletdk

import (
	"errors"
	"testing"

	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestReconstructSentinel turns each walletdk ErrorInfo reason back into an
// errors.Is-able SDK sentinel while preserving the gRPC status, and leaves
// unknown reasons and non-walletdk errors unchanged. It builds inputs from the
// shared walletdkrpc reason constants, so it doubles as a wire-contract check
// against the daemon-side mapper.
func TestReconstructSentinel(t *testing.T) {
	cases := []struct {
		reason string
		want   error
	}{
		{
			walletdkrpc.ReasonInvalidDestination,
			ErrInvalidDestination,
		},
		{
			walletdkrpc.ReasonInvalidSendIntent,
			ErrInvalidSendIntent,
		},
		{
			walletdkrpc.ReasonAmountRequired,
			ErrAmountRequired,
		},
		{
			walletdkrpc.ReasonAmountInvalid,
			ErrAmountInvalid,
		},
		{
			walletdkrpc.ReasonUnsupportedKind,
			ErrUnsupportedKind,
		},
		{
			walletdkrpc.ReasonSwapBackendUnavailable,
			ErrSwapBackendUnavailable,
		},
		{
			walletdkrpc.ReasonAmountExceedsVTXOLimit,
			ErrAmountExceedsVTXOLimit,
		},
		{
			walletdkrpc.ReasonBalanceLimitExceeded,
			ErrBalanceLimitExceeded,
		},
	}
	for _, tc := range cases {
		in := statusWithReason(t, walletdkrpc.FailureDomain, tc.reason)
		got := reconstructSentinel(in)

		// The result is both errors.Is-able and still a gRPC status.
		require.ErrorIs(t, got, tc.want, "reason=%s", tc.reason)
		require.Equal(
			t, codes.InvalidArgument, status.Code(got),
			"reason=%s", tc.reason,
		)
	}

	// An unknown reason in our domain leaves the original error unchanged.
	unknown := statusWithReason(
		t, walletdkrpc.FailureDomain, "SOMETHING_ELSE",
	)
	require.Equal(t, unknown, reconstructSentinel(unknown))

	// A reason from a different domain is ignored.
	foreign := statusWithReason(t, "other", walletdkrpc.ReasonAmountInvalid)
	require.Equal(t, foreign, reconstructSentinel(foreign))

	// A plain (non-status) error passes through, and nil stays nil.
	plain := errors.New("boom")
	require.Equal(t, plain, reconstructSentinel(plain))
	require.NoError(t, reconstructSentinel(nil))
}

// statusWithReason builds a gRPC status error carrying an ErrorInfo detail with
// the given domain and reason.
func statusWithReason(t *testing.T, domain, reason string) error {
	t.Helper()

	st, err := status.New(codes.InvalidArgument, "rejected").WithDetails(
		&errdetails.ErrorInfo{Reason: reason, Domain: domain},
	)
	require.NoError(t, err)

	return st.Err()
}

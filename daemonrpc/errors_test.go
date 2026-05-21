package daemonrpc

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestWalletNotReadyErrorMatchesStructuredReason verifies callers can match
// daemon wallet lifecycle preconditions without depending on message text.
func TestWalletNotReadyErrorMatchesStructuredReason(t *testing.T) {
	t.Parallel()

	err := WalletNotReadyError("custom readiness wording")

	require.True(t, IsWalletNotReadyError(err))
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "custom readiness wording")
}

// TestIsWalletNotReadyErrorRejectsPlainFailedPrecondition verifies unrelated
// caller errors with the same gRPC code remain terminal to swap callers.
func TestIsWalletNotReadyErrorRejectsPlainFailedPrecondition(t *testing.T) {
	t.Parallel()

	err := status.Error(codes.FailedPrecondition, "operator rejected swap")

	require.False(t, IsWalletNotReadyError(err))
}

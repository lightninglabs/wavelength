package waverpc

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

	err := WalletNotReadyStateError(
		"custom readiness wording", WalletNotReadyStateLocked,
	)

	require.True(t, IsWalletNotReadyError(err))
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "custom readiness wording")

	state, ok := WalletNotReadyState(err)
	require.True(t, ok)
	require.Equal(t, WalletNotReadyStateLocked, state)
}

// TestWalletNotReadyErrorAllowsMissingState verifies older callers can still
// emit the coarse wallet-not-ready reason without state metadata.
func TestWalletNotReadyErrorAllowsMissingState(t *testing.T) {
	t.Parallel()

	err := WalletNotReadyError("custom readiness wording")

	require.True(t, IsWalletNotReadyError(err))

	_, ok := WalletNotReadyState(err)
	require.False(t, ok)
}

// TestIsWalletNotReadyErrorRejectsPlainFailedPrecondition verifies unrelated
// caller errors with the same gRPC code remain terminal to swap callers.
func TestIsWalletNotReadyErrorRejectsPlainFailedPrecondition(t *testing.T) {
	t.Parallel()

	err := status.Error(codes.FailedPrecondition, "operator rejected swap")

	require.False(t, IsWalletNotReadyError(err))
}

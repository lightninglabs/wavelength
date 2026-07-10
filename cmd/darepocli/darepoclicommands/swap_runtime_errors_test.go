package darepoclicommands

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestMapSwapRuntimeRPCError verifies that a gRPC unknown-service response
// for the swap runtime is turned into an actionable rebuild hint.
func TestMapSwapRuntimeRPCError(t *testing.T) {
	t.Parallel()

	err := status.Error(
		codes.Unimplemented,
		"unknown service swapclientrpc.SwapClientService",
	)

	mapped := mapSwapRuntimeRPCError(err)
	require.ErrorContains(
		t, mapped, "daemon was built without swapruntime support",
	)
	require.ErrorContains(t, mapped, `tags="swapruntime"`)
}

// TestMapSwapRuntimeRPCErrorLeavesOtherErrors verifies that errors unrelated
// to a missing swap runtime pass through unchanged so daemon-side validation
// remains visible.
func TestMapSwapRuntimeRPCErrorLeavesOtherErrors(t *testing.T) {
	t.Parallel()

	plain := errors.New("plain failure")
	require.ErrorIs(t, mapSwapRuntimeRPCError(plain), plain)

	daemonErr := status.Error(codes.InvalidArgument, "bad invoice")
	require.Equal(t, daemonErr, mapSwapRuntimeRPCError(daemonErr))
}

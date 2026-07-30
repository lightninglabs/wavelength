//go:build wavewalletrpc && swapruntime

package wavewalletdk

import (
	"context"
	"testing"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/swapwallet"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// reasonContract pins, per machine-readable reason, the daemon-side swapwallet
// sentinel, the SDK-side wavewalletdk sentinel, and the gRPC code that flows
// across the wire. It is the single place the two independently-defined
// sentinel sets are cross-checked against the shared reason constant.
var reasonContract = []struct {
	reason string
	server error
	sdk    error
	code   codes.Code
}{
	{
		wavewalletrpc.ReasonInvalidDestination,
		swapwallet.ErrInvalidDestination, ErrInvalidDestination,
		codes.InvalidArgument,
	},
	{
		wavewalletrpc.ReasonInvalidSendIntent,
		swapwallet.ErrInvalidSendIntent, ErrInvalidSendIntent,
		codes.FailedPrecondition,
	},
	{
		wavewalletrpc.ReasonAmountRequired,
		swapwallet.ErrAmountRequired, ErrAmountRequired,
		codes.InvalidArgument,
	},
	{
		wavewalletrpc.ReasonAmountInvalid,
		swapwallet.ErrAmountInvalid, ErrAmountInvalid,
		codes.InvalidArgument,
	},
	{
		wavewalletrpc.ReasonUnsupportedKind,
		swapwallet.ErrUnsupportedKind, ErrUnsupportedKind,
		codes.InvalidArgument,
	},
	{
		wavewalletrpc.ReasonSwapBackendUnavailable,
		swapwallet.ErrSwapBackendUnavailable, ErrSwapBackendUnavailable,
		codes.Unavailable,
	},
	{
		wavewalletrpc.ReasonAmountExceedsVTXOLimit,
		swapwallet.ErrAmountExceedsVTXOLimit, ErrAmountExceedsVTXOLimit,
		codes.FailedPrecondition,
	},
	{
		wavewalletrpc.ReasonBalanceLimitExceeded,
		swapwallet.ErrBalanceLimitExceeded, ErrBalanceLimitExceeded,
		codes.FailedPrecondition,
	},
	{
		wavewalletrpc.ReasonCreditReceiveUnavailable,
		swapwallet.ErrCreditReceiveUnavailable, ErrCreditReceiveUnavailable,
		codes.Unavailable,
	},
}

// TestSentinelMessageParity guards against drift between the daemon's
// swapwallet sentinels and the SDK's wavewalletdk sentinels. The two sets are
// distinct error values bound only by the shared reason string, and matching is
// by reason (not by message), so a divergence in the human-readable text would
// not break errors.Is and would otherwise go unnoticed. This test is that
// guard, and it also asserts the SDK reason lookup is complete: the contract
// below must name every reason the SDK knows how to reconstruct.
func TestSentinelMessageParity(t *testing.T) {
	t.Parallel()

	for _, c := range reasonContract {
		require.Equal(
			t, c.server.Error(), c.sdk.Error(),
			"message drift between server and SDK for reason %s",
			c.reason,
		)
		require.ErrorIs(
			t, sentinelForReason(c.reason), c.sdk, "SDK reason "+
				"lookup wrong for %s", c.reason,
		)
	}

	// The contract enumerates exactly the reasons the SDK reconstructs; a
	// new reason added to reasonToSentinel without a contract row trips
	// this.
	require.Len(t, reasonContract, len(reasonToSentinel))
}

// TestServerToSDKRoundTrip exercises the full wire contract end to end: a
// daemon handler returns a bare swapwallet sentinel, the server interceptor
// maps it to a gRPC status carrying an ErrorInfo, and the SDK client
// reconstructor turns that status back into the errors.Is-able SDK sentinel
// while preserving the gRPC code. Each half is unit tested in isolation
// elsewhere; this is the only test that runs them together, so a serialization,
// domain, or code-mapping mismatch between the two halves surfaces here.
func TestServerToSDKRoundTrip(t *testing.T) {
	t.Parallel()

	for _, c := range reasonContract {
		// Server side: a handler returns the bare server sentinel, and
		// the mapping interceptor turns it into a status + ErrorInfo.
		_, wire := swapwallet.ErrorMappingInterceptor(
			context.Background(), nil, &grpc.UnaryServerInfo{},
			func(context.Context, any) (any, error) {
				return nil, c.server
			},
		)
		require.Error(t, wire, "reason %s", c.reason)

		// Client side: the reconstructor turns the wire status back
		// into the SDK sentinel, preserving the gRPC code.
		got := reconstructSentinel(wire)
		require.ErrorIs(t, got, c.sdk, "reason %s", c.reason)
		require.Equal(
			t, c.code, status.Code(got),
			"gRPC code not preserved for reason %s", c.reason,
		)
	}
}

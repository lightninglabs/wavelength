//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"errors"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sentinelMapping pairs a swapwallet sentinel with the gRPC status code and the
// stable, machine-readable reason surfaced to clients alongside the
// human-readable message.
type sentinelMapping struct {
	err    error
	code   codes.Code
	reason string
}

// sentinelMappings is the canonical sentinel → (code, reason) table. The reason
// strings are the wavewalletrpc wire contract (clients reconstruct typed
// sentinels from them), so they come from wavewalletrpc rather than being
// duplicated here.
//
// ErrWalletNotReady is intentionally absent: the wallet-readiness gate
// (requireWalletReady) returns waverpc's own structured WALLET_NOT_READY
// status, so the bare sentinel never reaches this interceptor.
var sentinelMappings = []sentinelMapping{
	{
		ErrInvalidDestination, codes.InvalidArgument,
		wavewalletrpc.ReasonInvalidDestination,
	},
	{
		ErrInvalidSendIntent, codes.FailedPrecondition,
		wavewalletrpc.ReasonInvalidSendIntent,
	},
	{
		ErrAmountRequired, codes.InvalidArgument,
		wavewalletrpc.ReasonAmountRequired,
	},
	{
		ErrAmountInvalid, codes.InvalidArgument,
		wavewalletrpc.ReasonAmountInvalid,
	},
	{
		ErrUnsupportedKind, codes.InvalidArgument,
		wavewalletrpc.ReasonUnsupportedKind,
	},
	{
		ErrSwapBackendUnavailable, codes.Unavailable,
		wavewalletrpc.ReasonSwapBackendUnavailable,
	},

	// The two receive-limit rejections are FailedPrecondition rather than
	// InvalidArgument: the amount is well-formed, but the operator's
	// advertised terms (a per-VTXO cap, a total-balance cap) reject it. The
	// same amount can succeed against different terms or a lower balance,
	// so the failure is a function of system state, not a malformed
	// argument.
	{
		ErrAmountExceedsVTXOLimit, codes.FailedPrecondition,
		wavewalletrpc.ReasonAmountExceedsVTXOLimit,
	},
	{
		ErrBalanceLimitExceeded, codes.FailedPrecondition,
		wavewalletrpc.ReasonBalanceLimitExceeded,
	},
}

// statusSwapBackendUnavailable returns the canonical gRPC status for a missing
// swap backend handle, carrying the same machine-readable ErrorInfo the
// interceptor attaches to a bare ErrSwapBackendUnavailable. Handlers that must
// return a pre-formed status directly -- the readiness gate and the admin
// proxies, which run before the swap runtime is live and whose direct return
// value (not just the wire result) is asserted on -- call this so their
// rejection is SDK-reconstructable exactly like an interceptor-mapped sentinel,
// rather than a code-only status the SDK cannot branch on.
func statusSwapBackendUnavailable() error {
	return mapSentinel(ErrSwapBackendUnavailable)
}

// ErrorMappingInterceptor is a unary server interceptor that maps a returned
// swapwallet sentinel into a gRPC status carrying a machine-readable
// google.rpc.ErrorInfo reason, so clients can branch on the failure cause
// without string matching. Errors that already carry a gRPC status, and errors
// that match no sentinel, pass through unchanged.
func ErrorMappingInterceptor(ctx context.Context, req any,
	_ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {

	resp, err := handler(ctx, req)
	if err == nil {
		return resp, nil
	}

	return resp, mapSentinel(err)
}

// mapSentinel translates a bare swapwallet sentinel into a status error with an
// ErrorInfo detail. Errors that already carry a gRPC status (a handler that
// already chose a code) and non-sentinel errors are returned unchanged.
func mapSentinel(err error) error {
	// An error that already carries a gRPC status was deliberately coded by
	// the handler; do not second-guess it. status.FromError reports ok for
	// such errors (and for nil, already excluded by the caller).
	if _, ok := status.FromError(err); ok {
		return err
	}

	for _, m := range sentinelMappings {
		if !errors.Is(err, m.err) {
			continue
		}

		st := status.New(m.code, err.Error())
		detailed, detailErr := st.WithDetails(&errdetails.ErrorInfo{
			Reason: m.reason,
			Domain: wavewalletrpc.FailureDomain,
		})
		if detailErr != nil {
			return st.Err()
		}

		return detailed.Err()
	}

	return err
}

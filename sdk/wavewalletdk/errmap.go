package wavewalletdk

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// reasonToSentinel maps each daemon failure reason to the SDK sentinel callers
// match with errors.Is. The reason strings come from wavewalletrpc so the
// client and the daemon-side mapper share one source of truth.
var reasonToSentinel = map[string]error{
	wavewalletrpc.ReasonInvalidDestination:     ErrInvalidDestination,
	wavewalletrpc.ReasonInvalidSendIntent:      ErrInvalidSendIntent,
	wavewalletrpc.ReasonAmountRequired:         ErrAmountRequired,
	wavewalletrpc.ReasonAmountInvalid:          ErrAmountInvalid,
	wavewalletrpc.ReasonUnsupportedKind:        ErrUnsupportedKind,
	wavewalletrpc.ReasonSwapBackendUnavailable: ErrSwapBackendUnavailable,
	wavewalletrpc.ReasonAmountExceedsVTXOLimit: ErrAmountExceedsVTXOLimit,
	wavewalletrpc.ReasonBalanceLimitExceeded:   ErrBalanceLimitExceeded,
}

// sentinelForReason looks up the SDK sentinel for a daemon failure reason.
// Unknown reasons return nil so the original status error is preserved
// unchanged.
func sentinelForReason(reason string) error {
	return reasonToSentinel[reason]
}

// reconstructSentinel inspects a gRPC status error for a wavewalletdk ErrorInfo
// detail and, when present, wraps the matching SDK sentinel so callers can use
// errors.Is. The original status error is also wrapped, so status.FromError /
// status.Code keep working on the result. Errors that carry no recognizable
// wavewalletdk reason are returned unchanged.
func reconstructSentinel(err error) error {
	if err == nil {
		return nil
	}

	st, ok := status.FromError(err)
	if !ok {
		return err
	}

	for _, detail := range st.Details() {
		info, ok := detail.(*errdetails.ErrorInfo)
		if !ok || info.GetDomain() != wavewalletrpc.FailureDomain {
			continue
		}

		sentinel := sentinelForReason(info.GetReason())
		if sentinel == nil {
			continue
		}

		// Wrap both the sentinel and the original status error so the
		// result is errors.Is-able AND still resolves via
		// status.FromError.
		return fmt.Errorf("%w: %w", sentinel, err)
	}

	return err
}

// errorReconstructInterceptor is a unary client interceptor that rewrites a
// returned wavewalletdk rejection into an errors.Is-able SDK sentinel. It is
// installed on every wavewalletdk client connection so all wallet RPCs surface
// typed failures uniformly.
func errorReconstructInterceptor(ctx context.Context, method string,
	req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker,
	opts ...grpc.CallOption) error {

	return reconstructSentinel(
		invoker(ctx, method, req, reply, cc, opts...),
	)
}

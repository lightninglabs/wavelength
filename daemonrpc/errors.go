package daemonrpc

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	walletNotReadyDomain = "darepo-client/wallet"

	// WalletNotReadyReason identifies gRPC FailedPrecondition errors
	// caused by a daemon wallet lifecycle state that is not yet usable.
	WalletNotReadyReason = "WALLET_NOT_READY"
)

// WalletNotReadyError returns a structured gRPC error for wallet lifecycle
// preconditions. The human message may change, but callers should match the
// stable ErrorInfo reason with IsWalletNotReadyError.
func WalletNotReadyError(msg string) error {
	st := status.New(codes.FailedPrecondition, msg)
	st, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason: WalletNotReadyReason,
		Domain: walletNotReadyDomain,
	})
	if err != nil {
		return status.Error(codes.FailedPrecondition, msg)
	}

	return st.Err()
}

// IsWalletNotReadyError reports whether err is the structured wallet lifecycle
// precondition returned by WalletNotReadyError.
func IsWalletNotReadyError(err error) bool {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition {
		return false
	}

	for _, detail := range st.Details() {
		info, ok := detail.(*errdetails.ErrorInfo)
		if !ok {
			continue
		}

		if info.GetDomain() == walletNotReadyDomain &&
			info.GetReason() == WalletNotReadyReason {
			return true
		}
	}

	return false
}

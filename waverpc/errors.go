package waverpc

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	walletNotReadyDomain = "wavelength/wallet"

	// WalletNotReadyReason identifies gRPC FailedPrecondition errors
	// caused by a daemon wallet lifecycle state that is not yet usable.
	WalletNotReadyReason = "WALLET_NOT_READY"

	// WalletNotReadyStateKey is the ErrorInfo metadata key carrying
	// the specific daemon wallet lifecycle state when the caller needs
	// a more precise user-facing hint.
	WalletNotReadyStateKey = "wallet_state"

	// WalletNotReadyStateNone indicates no wallet has been created.
	WalletNotReadyStateNone = "none"

	// WalletNotReadyStateLocked indicates an encrypted wallet exists
	// but has not been unlocked.
	WalletNotReadyStateLocked = "locked"

	// WalletNotReadyStateSyncing indicates the wallet has been
	// unlocked but is not yet ready to sign.
	WalletNotReadyStateSyncing = "syncing"

	// WalletNotReadyStateUnknown indicates the daemon reported an
	// unrecognized or unspecified wallet lifecycle state.
	WalletNotReadyStateUnknown = "unknown"
)

// WalletNotReadyError returns a structured gRPC error for wallet lifecycle
// preconditions. The human message may change, but callers should match the
// stable ErrorInfo reason with IsWalletNotReadyError.
func WalletNotReadyError(msg string) error {
	return WalletNotReadyStateError(msg, "")
}

// WalletNotReadyStateError returns a structured wallet lifecycle precondition
// and includes the stable wallet_state metadata when state is non-empty.
func WalletNotReadyStateError(msg string, state string) error {
	st := status.New(codes.FailedPrecondition, msg)
	metadata := map[string]string{}
	if state != "" {
		metadata[WalletNotReadyStateKey] = state
	}

	st, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason:   WalletNotReadyReason,
		Domain:   walletNotReadyDomain,
		Metadata: metadata,
	})
	if err != nil {
		return status.Error(codes.FailedPrecondition, msg)
	}

	return st.Err()
}

// IsWalletNotReadyError reports whether err is the structured wallet lifecycle
// precondition returned by WalletNotReadyError.
func IsWalletNotReadyError(err error) bool {
	_, ok := walletNotReadyInfo(err)

	return ok
}

// WalletNotReadyState returns the stable wallet_state metadata carried by a
// structured wallet lifecycle precondition, when present.
func WalletNotReadyState(err error) (string, bool) {
	info, ok := walletNotReadyInfo(err)
	if !ok {
		return "", false
	}

	state, ok := info.GetMetadata()[WalletNotReadyStateKey]

	return state, ok
}

func walletNotReadyInfo(err error) (*errdetails.ErrorInfo, bool) {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition {
		return nil, false
	}

	for _, detail := range st.Details() {
		info, ok := detail.(*errdetails.ErrorInfo)
		if !ok {
			continue
		}

		if info.GetDomain() == walletNotReadyDomain &&
			info.GetReason() == WalletNotReadyReason {
			return info, true
		}
	}

	return nil, false
}

package wavewalletrpc

// This file is hand-written (not generated): it defines the wallet rejection
// taxonomy that the daemon attaches to a failed wallet RPC as a
// google.rpc.ErrorInfo, and that SDK clients reconstruct into typed errors.
// It lives in wavewalletrpc because the reasons are part of the wallet RPC wire
// contract; both the daemon-side mapper and the SDK reconstructor import these
// constants so the two halves cannot drift.

// FailureDomain is the google.rpc.ErrorInfo Domain that tags wavewalletdk
// wallet rejection reasons. Clients confirm this domain before branching on
// Reason so a same-named reason from another subsystem is not misread.
const FailureDomain = "wavewalletdk"

// Stable, machine-readable wallet rejection reasons carried in the ErrorInfo
// detail of a failed wallet RPC. These are a wire contract: existing values
// MUST NOT be renamed (clients match on them); add new values as needed.
const (
	ReasonInvalidDestination     = "INVALID_DESTINATION"
	ReasonInvalidSendIntent      = "INVALID_SEND_INTENT"
	ReasonAmountRequired         = "AMOUNT_REQUIRED"
	ReasonAmountInvalid          = "AMOUNT_INVALID"
	ReasonUnsupportedKind        = "UNSUPPORTED_KIND"
	ReasonSwapBackendUnavailable = "SWAP_BACKEND_UNAVAILABLE"
	ReasonAmountExceedsVTXOLimit = "AMOUNT_EXCEEDS_VTXO_LIMIT"
	ReasonBalanceLimitExceeded   = "BALANCE_LIMIT_EXCEEDED"

	// ReasonInsufficientFunds tags a PrepareSend whose complete quote
	// cannot be funded by the wallet's live VTXOs under the
	// minimum-change rule — the same rejection dispatch would issue at
	// Send time, surfaced at prepare so hosts can disable confirmation.
	// Distinct from AMOUNT_REQUIRED: the amount is well-formed, the
	// wallet simply cannot cover it right now.
	ReasonInsufficientFunds = "INSUFFICIENT_FUNDS"

	// ReasonCreditReceiveUnavailable tags a sub-dust receive that was
	// routed to the operator's credit subsystem but which the swap server
	// did not complete (it timed out or errored). It is distinct from
	// SWAP_BACKEND_UNAVAILABLE so clients can tell "the whole swap backend
	// is down" apart from "this operator does not (currently) serve
	// sub-dust credit receives, try an amount at or above the dust limit".
	ReasonCreditReceiveUnavailable = "CREDIT_RECEIVE_UNAVAILABLE"

	// ReasonReceiveNotFound tags a LookupRecv for a request ID that owns
	// no pending or completed receive and has none being created. Start a
	// new receive under a fresh request ID.
	ReasonReceiveNotFound = "RECEIVE_NOT_FOUND"

	// ReasonReceiveRequestMismatch tags a Recv that reuses a request ID
	// with a different amount or memo.
	ReasonReceiveRequestMismatch = "RECEIVE_REQUEST_MISMATCH"
)

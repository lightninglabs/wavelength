package wavewalletdk

import (
	"errors"
	"fmt"
)

// SubscribeGapError signals that a live SubscribeWallet stream fell behind (the
// server-side send buffer overflowed). No activity is lost: the consumer should
// open a new subscription with SubscribeRequest.Cursor set to Cursor, and the
// replay from it is gap-free because the event log retains everything after it.
type SubscribeGapError struct {
	// Cursor is the resume point: the last event-log position the stream
	// is known to have covered before falling behind.
	Cursor int64

	// Reason is the daemon's human-readable description of the gap.
	Reason string
}

// Error implements the error interface.
func (e *SubscribeGapError) Error() string {
	return fmt.Sprintf("wallet subscription gap at cursor %d: %s: resume "+
		"a new subscription from the cursor", e.Cursor, e.Reason)
}

// ErrWalletRPCUnavailable reports that an embedded wavewalletdk runtime was
// built without the wallet RPC subserver needed by wallet payment methods.
var ErrWalletRPCUnavailable = errors.New("wavewalletdk wallet rpc " +
	"unavailable; rebuild with -tags wavewalletrpc,swapruntime")

// ErrSwapRuntimeUnavailable is retained as a compatibility sentinel for code
// that still checks the old swapruntime-only error.
var ErrSwapRuntimeUnavailable = ErrWalletRPCUnavailable

// ErrMaxQuoteChanged means the final prepare differs from the Max quote.
// Recalculate before reviewing the payment.
var ErrMaxQuoteChanged = errors.New("maximum payment quote changed")

// ErrMaxInsufficientBalance means no quoted amount passed the funding checks
// for the snapshot and the caller's fee and credit limits.
var ErrMaxInsufficientBalance = errors.New("insufficient funds for maximum " +
	"payment")

// The sentinels below mirror the daemon's wavewalletrpc rejection taxonomy. The
// daemon attaches a machine-readable google.rpc.ErrorInfo reason to a failed
// wallet RPC; the SDK reconstructs these errors.Is-able sentinels from that
// reason (see errmap.go) so callers can branch on failure cause without
// matching gRPC status strings.
var (
	// ErrInvalidDestination reports that a send destination was unset or
	// empty: supply exactly one of an invoice or an on-chain address.
	ErrInvalidDestination = errors.New("send destination must set " +
		"invoice or onchain_address")

	// ErrInvalidSendIntent reports that a send ran without a live prepared
	// send intent (missing, expired, or already consumed).
	ErrInvalidSendIntent = errors.New("send intent is missing, expired, " +
		"or already consumed")

	// ErrAmountRequired reports that an amount was required but not
	// supplied (an on-chain send, or an amountless invoice with no caller
	// amount).
	ErrAmountRequired = errors.New("amt_sat is required for the " +
		"requested send")

	// ErrAmountInvalid reports that an amount was out of range.
	ErrAmountInvalid = errors.New("amt_sat is out of range")

	// ErrUnsupportedKind reports that a list filter used an entry kind the
	// daemon does not support.
	ErrUnsupportedKind = errors.New("unsupported entry kind in list filter")

	// ErrSwapBackendUnavailable reports that the daemon's swap backend
	// handle was unavailable (a daemon misconfiguration).
	ErrSwapBackendUnavailable = errors.New("swap backend handle is " +
		"unavailable")

	// ErrAmountExceedsVTXOLimit reports that a receive amount exceeds the
	// operator's advertised maximum VTXO size.
	ErrAmountExceedsVTXOLimit = errors.New("amount exceeds the " +
		"operator's maximum VTXO size")

	// ErrBalanceLimitExceeded reports that an inflow would push the
	// wallet's total balance above the operator's advertised maximum user
	// balance.
	ErrBalanceLimitExceeded = errors.New("amount would exceed the " +
		"maximum allowed wallet balance")

	// ErrInsufficientFunds reports that a prepared send's quoted wallet
	// outflow cannot be funded by the wallet's live VTXOs under the
	// minimum-change rule — the prepare-time form of the dispatch
	// insufficient-funds rejection.
	ErrInsufficientFunds = errors.New("insufficient spendable funds")

	// ErrCreditReceiveUnavailable reports that a sub-dust receive was
	// routed to the operator's credit subsystem but the swap server did not
	// complete the credit request (it timed out or errored). It is distinct
	// from ErrSwapBackendUnavailable so callers can tell "the whole swap
	// backend is down" apart from "this operator does not (currently) serve
	// sub-dust credit receives; retry at or above the dust limit". The
	// message MUST match the daemon-side swapwallet sentinel verbatim
	// (TestSentinelMessageParity enforces this).
	ErrCreditReceiveUnavailable = errors.New("sub-dust receive via the " +
		"operator credit subsystem is unavailable")

	// ErrReceiveNotFound reports that LookupReceive found no pending or
	// completed receive for the request ID, and none being created. Start a
	// new receive under a fresh request ID.
	ErrReceiveNotFound = errors.New("no receive for request id")

	// ErrReceiveRequestMismatch reports that Receive reused a request ID
	// with a different amount or memo.
	ErrReceiveRequestMismatch = errors.New("request id already used with " +
		"different receive parameters")
)

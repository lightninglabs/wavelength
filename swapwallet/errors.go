//go:build wavewalletrpc && swapruntime

package swapwallet

import "errors"

// Sentinel errors returned by the swapwallet subserver. Callers may match
// these with errors.Is for cleanly typed error handling.
var (
	// ErrSwapBackendUnavailable is returned when the underlying swap
	// subserver did not publish its backend handle on cfg.Swap.Backend
	// during Register. The wavewalletrpc subserver cannot function without
	// it, so all wallet RPCs fail with this error until the backend is
	// installed (typically a misconfiguration of registrar ordering).
	ErrSwapBackendUnavailable = errors.New("swap backend handle is " +
		"unavailable")

	// ErrInvalidDestination is returned when a Send request's destination
	// oneof is unset or carries an empty string. The caller must supply
	// exactly one of invoice or onchain_address.
	ErrInvalidDestination = errors.New("send destination must set " +
		"invoice or onchain_address")

	// ErrInvalidSendIntent is returned when Send is invoked without a
	// live prepared send intent.
	ErrInvalidSendIntent = errors.New("send intent is missing, expired, " +
		"or already consumed")

	// ErrAmountRequired is returned when Send is invoked with an onchain
	// destination but no amount, or with an invoice that does not encode
	// an amount and no caller-supplied amount.
	ErrAmountRequired = errors.New("amt_sat is required for the " +
		"requested send")

	// ErrAmountInvalid is returned when an amount is out of range
	// (non-positive or exceeds protocol bounds).
	ErrAmountInvalid = errors.New("amt_sat is out of range")

	// ErrUnsupportedKind is returned when a ListRequest filters on an
	// EntryKind that the subserver does not yet support. v1 supports
	// SEND, RECV, DEPOSIT, and EXIT.
	ErrUnsupportedKind = errors.New("unsupported entry kind in list filter")

	// ErrWalletNotReady is returned when a wallet-side operation is
	// attempted before the daemon's backing wallet is unlocked and ready
	// to sign.
	ErrWalletNotReady = errors.New("wallet is not ready")

	// ErrAmountExceedsVTXOLimit is returned when a Recv amount exceeds
	// the operator's advertised maximum VTXO size. The wrapped message
	// carries the offending and maximum amounts.
	ErrAmountExceedsVTXOLimit = errors.New("amount exceeds the " +
		"operator's maximum VTXO size")

	// ErrBalanceLimitExceeded is returned when an inflow would push the
	// wallet's total balance above the operator's advertised maximum
	// user balance. The wrapped message carries the current balance and
	// the cap.
	ErrBalanceLimitExceeded = errors.New("amount would exceed the " +
		"maximum allowed wallet balance")
)

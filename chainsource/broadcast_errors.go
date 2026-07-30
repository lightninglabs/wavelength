package chainsource

import (
	"errors"
	"strings"

	"github.com/btcsuite/btcwallet/chain"
)

var (
	// ignorableBroadcastSentinels lists sentinel errors that indicate
	// a broadcast failure can be treated as a success (e.g., already known
	// or already confirmed).
	ignorableBroadcastSentinels = []error{
		chain.ErrInsufficientFee,
		chain.ErrSameNonWitnessData,
		chain.ErrTxAlreadyConfirmed,
		chain.ErrTxAlreadyKnown,
	}

	// ignorableBroadcastErrs lists error substrings that are expected
	// to happen when broadcasting the same transaction more than once.
	//
	// We treat these errors as non-fatal because callers may legitimately
	// rebroadcast sweeps (for retry and fee bumping) and some backends
	// report duplicates or already-confirmed transactions as errors.
	//
	// This list is intentionally small and specific. If a backend returns a
	// new string form, prefer adding a concrete sentinel error check
	// (errors.Is) where possible, or add the minimal substring required.
	ignorableBroadcastErrs = []string{
		// Bitcoind.
		"txn-already-in-mempool",
		"already in mempool",
		"already have transaction",

		// Wallet-layer variants.
		"output already spent",
	}

	// ignorableRemoveErrs lists error substrings returned when removing a
	// transaction that the wallet does not (or no longer) knows about. The
	// caller's goal -- the transaction is not in the wallet's rebroadcast
	// set -- is already satisfied in that case, so the removal is treated
	// as a success (wavelength#609).
	ignorableRemoveErrs = []string{
		"not found",
		"unable to locate transaction",
		"no such transaction",
		"already confirmed",
	}
)

// IsIgnorableRemoveError returns true when a transaction-removal error means
// the transaction is already absent from the wallet's rebroadcast set (unknown,
// already removed, or already confirmed), so the removal can be treated as a
// success.
func IsIgnorableRemoveError(err error) bool {
	if err == nil {
		return false
	}

	errStr := strings.ToLower(err.Error())
	for _, substring := range ignorableRemoveErrs {
		if strings.Contains(errStr, substring) {
			return true
		}
	}

	return false
}

// IsIgnorableBroadcastError returns true if the error is expected to happen
// when broadcasting the same transaction multiple times.
func IsIgnorableBroadcastError(err error) bool {
	if err == nil {
		return false
	}

	for _, sentinel := range ignorableBroadcastSentinels {
		if errors.Is(err, sentinel) {
			return true
		}
	}

	errStr := err.Error()
	for _, substring := range ignorableBroadcastErrs {
		if strings.Contains(errStr, substring) {
			return true
		}
	}

	return false
}

// IsIgnorableMempoolRejectReason returns true if the mempool rejection reason
// indicates the transaction is already known (in mempool or chain).
func IsIgnorableMempoolRejectReason(reason string) bool {
	if reason == "" {
		return false
	}

	lowerReason := strings.ToLower(reason)

	ignorableSubstrings := []string{
		strings.ToLower(chain.ErrTxAlreadyKnown.Error()),
		strings.ToLower(chain.ErrTxAlreadyConfirmed.Error()),

		"already in mempool",
		"txn-already-in-mempool",
		"already known",
	}

	for _, substring := range ignorableSubstrings {
		if strings.Contains(lowerReason, substring) {
			return true
		}
	}

	return false
}

package txconfirm

import (
	"errors"
	"strings"

	"github.com/btcsuite/btcwallet/chain"
)

// isAmbiguousSpendError recognizes rejections that cannot distinguish a
// conflicting spend from a rebroadcast of an already-confirmed transaction.
// LND maps missing inputs to "output already spent"; RPC transports may drop
// sentinel identity. Neither form proves publication or terminal failure, so
// callers must retain the signed candidate and its confirmation watch.
func isAmbiguousSpendError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, chain.ErrMissingInputs) ||
		errors.Is(err, chain.ErrMissingInputsOrSpent) {
		return true
	}

	reason := strings.ToLower(err.Error())
	for _, ambiguous := range []string{
		"output already spent",
		"bad-txns-inputs-missingorspent",
		chain.ErrMissingInputsOrSpent.Error(),
		chain.ErrMissingInputs.Error(),
		"missing-inputs",
	} {
		if strings.Contains(reason, ambiguous) {
			return true
		}
	}

	return false
}

// BroadcastFailureClass describes what a failed submission proves. It does
// not prove that a previous submission of the same transaction was absent.
type BroadcastFailureClass uint8

const (
	// BroadcastFailureUnknown includes transport errors and unclassified
	// rejections. A caller must preserve the submitted transaction.
	BroadcastFailureUnknown BroadcastFailureClass = iota

	// BroadcastFailureFee identifies an explicit insufficient-fee policy
	// rejection. A transaction owner may construct a higher-fee
	// replacement, retaining evidence of every candidate that could still
	// confirm.
	BroadcastFailureFee

	// BroadcastFailurePermanent identifies invalid transaction structure or
	// script execution. Changing the fee cannot repair it.
	BroadcastFailurePermanent
)

// ClassifyBroadcastFailure recognizes explicit backend rejection reasons.
// Unknown errors stay ambiguous. The string forms are required for RPC and
// HTTP backends that do not preserve Go error types, and for legacy durable
// failure records. Already-known outcomes must be handled before this helper.
func ClassifyBroadcastFailure(err error) BroadcastFailureClass {
	if err == nil {
		return BroadcastFailureUnknown
	}
	if errors.Is(err, ErrNonTRUCParent) {
		return BroadcastFailurePermanent
	}
	reason := strings.ToLower(err.Error())
	for _, structural := range []string{
		strings.ToLower(ErrNonTRUCParent.Error()),
		"mandatory-script-verify-flag-failed",
		"non-mandatory-script-verify-flag",
		"bad-txns-vout-negative",
		"bad-txns-inputs-duplicate",
		"bad-txns-txouttotal-toolarge",
	} {
		if strings.Contains(reason, structural) {
			return BroadcastFailurePermanent
		}
	}
	for _, fee := range []string{
		"min relay fee not met",
		"mempool min fee not met",
		"insufficient fee",
	} {
		if strings.Contains(reason, fee) {
			return BroadcastFailureFee
		}
	}

	return BroadcastFailureUnknown
}

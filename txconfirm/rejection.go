package txconfirm

import (
	"errors"
	"strings"
)

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

package waveclicommands

import (
	"fmt"
	"strings"
)

// Error substrings used to recognize server-side fee rejections.
// These match the sentinel messages in
// wavelength/rounds/validation.go so the client can surface a concise,
// actionable CLI error instead of a raw gRPC status that leaks
// internal percentages and numeric fields.
//
// Matching is on substring rather than status code because the
// daemon wraps upstream errors through proxyUpstreamError, which
// sanitizes the code and re-wraps the message.
const (
	errMsgOperatorFeeTooLow  = "operator fee is below minimum"
	errMsgVTXOBelowMinViable = "VTXO amount is below minimum viable"
	// errMsgBoardingTooSmall pins the wallet's exact "balance is
	// too small after operator fee" rejection at
	// wallet/wallet.go:1130. We deliberately do NOT match the
	// shorter "boarding balance" substring because the Board RPC
	// also wraps internal failures as
	// "peek boarding balance: ..." (see waved/rpc_server.go),
	// and a broad match would silently rewrite those into the
	// friendly "boarding rejected: balance too small" message
	// even though the failure has nothing to do with boarding
	// economics.
	errMsgBoardingTooSmall    = "too small after operator fee"
	errMsgEstimateFeeNoConfig = "fee calculator not configured"
)

// mapFeeError inspects err for well-known server-side fee
// rejection messages and returns a terse, actionable CLI error
// that a human can read without grepping the server logs.
// Returns nil when err does not match any known pattern so the
// caller can fall through to the original error.
//
// Used by sendInRound and boarding-flow callers that want to
// surface a crisp "too small to refresh" or "fee schedule
// changed, retry" message rather than leaking the raw gRPC
// detail.
func mapFeeError(err error) error {
	if err == nil {
		return nil
	}

	msg := err.Error()

	switch {
	case strings.Contains(msg, errMsgVTXOBelowMinViable):
		return fmt.Errorf("send rejected: the VTXO would be below the "+
			"operator's minimum viable size after fees. Try a "+
			"larger amount or check the current schedule via "+
			"`wavecli fees estimate`. (server: %v)", err)

	case strings.Contains(msg, errMsgOperatorFeeTooLow):
		return fmt.Errorf("send rejected: the operator's fee schedule "+
			"changed between the quote and the submission. Retry "+
			"the command; the client will quote fresh fees. "+
			"(server: %v)", err)

	case strings.Contains(msg, errMsgBoardingTooSmall):
		return fmt.Errorf("boarding rejected: the confirmed boarding "+
			"balance is too small to cover operator fees. Fund "+
			"the boarding address with a larger amount. "+
			"(server: %v)", err)

	case strings.Contains(msg, errMsgEstimateFeeNoConfig):
		return fmt.Errorf("operator has not configured a fee "+
			"calculator; EstimateFee is unavailable. Retry once "+
			"the operator has enabled fees. (server: %v)", err)
	}

	return nil
}

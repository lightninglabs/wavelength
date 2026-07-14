package oor

import (
	"errors"
	"fmt"

	"github.com/lightninglabs/wavelength/rpc/oorpb"
)

// ErrLineageTooLarge is the client-facing typed error returned when the
// operator rejects an OOR submit because the cumulative on-chain
// virtual bytes required to claim the produced VTXO exceeds the
// operator's configured cap. Wallet callers can detect this with
// errors.As to route on the rejection cause without depending on the
// oorpb proto type directly.
//
// The wrapped Reason carries the operator's human-readable explanation
// (e.g. "lineage 36000 vB > cap 25000 vB") and is suitable for surfacing
// directly in UX.
type ErrLineageTooLarge struct {
	Reason string
}

// Error returns a human-readable description of the rejection cause.
func (e *ErrLineageTooLarge) Error() string {
	return fmt.Sprintf("oor lineage too large: %s", e.Reason)
}

// Is reports whether target is also an *ErrLineageTooLarge, supporting
// the standard errors.Is comparison without forcing callers to compare
// reasons.
func (e *ErrLineageTooLarge) Is(target error) bool {
	_, ok := target.(*ErrLineageTooLarge)

	return ok
}

// ErrOutputPolicyViolation is the client-facing typed error returned
// when the operator rejects an OOR submit because one of the Ark
// recipient outputs violates the operator's output policy, e.g. an
// amount above the advertised per-VTXO maximum. Retrying the same
// output shape will fail again, so wallet callers should treat this as
// terminal for the session and restructure the outputs before trying
// again.
//
// The wrapped Reason carries the operator's human-readable explanation
// (e.g. "output 0 of 0.6 BTC exceeds the operator's per-VTXO maximum
// of 0.0003 BTC") and is suitable for surfacing directly in UX.
type ErrOutputPolicyViolation struct {
	Reason string
}

// Error returns a human-readable description of the rejection cause.
func (e *ErrOutputPolicyViolation) Error() string {
	return fmt.Sprintf("oor output policy violation: %s", e.Reason)
}

// Is reports whether target is also an *ErrOutputPolicyViolation,
// supporting the standard errors.Is comparison without forcing callers
// to compare reasons.
func (e *ErrOutputPolicyViolation) Is(target error) bool {
	_, ok := target.(*ErrOutputPolicyViolation)

	return ok
}

// ErrUserBalanceExceeded is the client-facing typed error returned when
// the operator rejects an OOR submit because a recipient mailbox's
// aggregate VTXO balance would exceed the operator's MaxUserBalance cap.
//
// Unlike ErrOutputPolicyViolation, this is NOT terminal for the same
// output shape: the rejection clears once the recipient spends or
// refreshes its balance down, so a custodial sender (e.g. a swap server
// holding the value on the recipient's behalf) should retain the value
// and retry later rather than restructuring the outputs. The wrapped
// Reason carries the operator's human-readable explanation and is
// suitable for surfacing directly in UX.
type ErrUserBalanceExceeded struct {
	Reason string
}

// Error returns a human-readable description of the rejection cause.
func (e *ErrUserBalanceExceeded) Error() string {
	return fmt.Sprintf("oor user balance exceeded: %s", e.Reason)
}

// Is reports whether target is also an *ErrUserBalanceExceeded,
// supporting the standard errors.Is comparison without forcing callers
// to compare reasons.
func (e *ErrUserBalanceExceeded) Is(target error) bool {
	_, ok := target.(*ErrUserBalanceExceeded)

	return ok
}

// ErrInvalidAncestry is the typed error returned by the receive-side
// ancestry cross-check when an operator-supplied IncomingVTXOMetadata
// fails one of the structural invariants required to bind the produced
// VTXO to its claimed commitment lineage. It is a fail-fast validation
// error suitable for failing the receive session rather than retrying
// the submit/finalize round-trip.
//
// The wrapped Reason names the specific invariant that was violated so
// log lines and UX surfaces can distinguish "operator returned an empty
// ancestry" from "fragment 2 carried a duplicate commitment txid"
// without scraping the parent error string.
type ErrInvalidAncestry struct {
	Reason string
}

// Error returns a human-readable description of the validation failure.
func (e *ErrInvalidAncestry) Error() string {
	return fmt.Sprintf("invalid incoming VTXO ancestry: %s", e.Reason)
}

// Is reports whether target is also an *ErrInvalidAncestry, supporting
// the standard errors.Is comparison without forcing callers to compare
// reasons.
func (e *ErrInvalidAncestry) Is(target error) bool {
	_, ok := target.(*ErrInvalidAncestry)

	return ok
}

// ClassifySubmitError converts a generic submit-pipeline error into the
// most specific typed client error available. Currently maps the
// proto-level *oorpb.SubmitRejectedError onto the typed Go errors that
// the wallet caller can route on; pass-through for any error that does
// not match a known typed rejection.
//
// Returns the input error unchanged when no typed mapping applies, so
// callers can chain ClassifySubmitError(err) directly into their error
// return without losing the original wrapping.
func ClassifySubmitError(err error) error {
	if err == nil {
		return nil
	}

	var rejected *oorpb.SubmitRejectedError
	if errors.As(err, &rejected) {
		switch rejected.Code {
		case oorpb.OORRejectCode_OOR_REJECT_LINEAGE_TOO_LARGE:
			return &ErrLineageTooLarge{Reason: rejected.Reason}

		case oorpb.OORRejectCode_OOR_REJECT_OUTPUT_POLICY:
			return &ErrOutputPolicyViolation{
				Reason: rejected.Reason,
			}

		case oorpb.OORRejectCode_OOR_REJECT_USER_BALANCE:
			return &ErrUserBalanceExceeded{
				Reason: rejected.Reason,
			}

		case oorpb.OORRejectCode_OOR_REJECT_UNSPECIFIED:
			// Unspecified rejection codes have no typed
			// mapping; fall through to pass-through.
		}
	}

	return err
}

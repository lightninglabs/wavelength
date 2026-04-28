package oor

import (
	"errors"
	"fmt"

	"github.com/lightninglabs/darepo-client/rpc/oorpb"
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

		case oorpb.OORRejectCode_OOR_REJECT_UNSPECIFIED:
			// Unspecified rejection codes have no typed
			// mapping; fall through to pass-through.
		}
	}

	return err
}

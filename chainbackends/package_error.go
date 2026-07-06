package chainbackends

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/rpcclient"
)

// PackageTxError is one per-tx result error from a `SubmitPackage` response.
// It preserves the original wtxid / txid / reject reason for diagnostics, and
// unwraps to a chain-backend sentinel mapped via `rpcclient.MapRPCErr` so
// callers can `errors.Is` against typed sentinels (e.g.
// `rpcclient.ErrTxAlreadyKnown`, `rpcclient.ErrInsufficientFee`) instead of
// substring-matching the raw bitcoind / btcd reject string.
type PackageTxError struct {
	// Wtxid is the witness txid that bitcoind / btcd echoed for this
	// per-tx package result.
	Wtxid string

	// Txid is the legacy (non-witness) txid associated with this entry.
	Txid chainhash.Hash

	// Reason is the raw reject reason as emitted by the chain backend
	// before normalisation. Kept for diagnostics; do not rely on it for
	// classification — use `errors.Is` against the unwrapped sentinel
	// instead.
	Reason string

	// mapped is the result of `rpcclient.MapRPCErr(errors.New(reason))`.
	// It is `rpcclient.ErrUndefined` (wrapping the raw string) when no
	// known sentinel matches.
	mapped error
}

// NewPackageTxError builds a `PackageTxError` from a per-tx package result.
// The mapped sentinel is computed eagerly via `rpcclient.MapRPCErr` so the
// caller side can rely on `errors.Is` without re-parsing the reason.
func NewPackageTxError(wtxid string, txid chainhash.Hash,
	reason string) *PackageTxError {

	return &PackageTxError{
		Wtxid:  wtxid,
		Txid:   txid,
		Reason: reason,
		mapped: rpcclient.MapRPCErr(errors.New(reason)),
	}
}

// Error implements the `error` interface and preserves the legacy
// "wtxid=<wtxid> txid=<txid>: <reason>" diagnostic shape that joined error
// messages used to carry, so log output and existing string-matching
// fallbacks (e.g. `rejecting replacement` heuristics) keep working until they
// are migrated to typed checks.
func (e *PackageTxError) Error() string {
	return fmt.Sprintf("wtxid=%s txid=%s: %s", e.Wtxid, e.Txid, e.Reason)
}

// Unwrap surfaces the mapped chain sentinel so callers can write
// `errors.Is(err, rpcclient.ErrTxAlreadyKnown)` instead of substring-matching
// the raw reason.
func (e *PackageTxError) Unwrap() error {
	return e.mapped
}

// WalkPackageTxErrors invokes `fn` for every `*PackageTxError` reachable from
// `err` by walking both the `Unwrap() error` and `Unwrap() []error` shapes.
// It is safe to call with `nil`.
//
// Used by callers (e.g. `txconfirm.isParentKnownChildFailed`) that need to
// inspect every per-tx entry in a joined `SubmitPackage` error. `errors.As`
// alone only surfaces the first match, which is insufficient when distinct
// classifications must be observed for the parent and the child.
//
// Implementation note: this walks the error tree directly rather than using
// `errors.As(&pte)`, because `errors.As` short-circuits on the first match
// and would miss sibling per-tx entries — the whole point of the walker.
// The lint disables below are intentional for that reason.
func WalkPackageTxErrors(err error, fn func(*PackageTxError)) {
	for err != nil {
		// Enumerate every entry, not "any match", so the type
		// assertion + type switch are intentional over errors.As.
		//
		//nolint:errorlint
		if pte, ok := err.(*PackageTxError); ok {
			fn(pte)

			return
		}

		//nolint:errorlint
		switch x := err.(type) {
		case interface{ Unwrap() []error }:
			for _, e := range x.Unwrap() {
				WalkPackageTxErrors(e, fn)
			}

			return

		case interface{ Unwrap() error }:
			err = x.Unwrap()

		default:
			return
		}
	}
}

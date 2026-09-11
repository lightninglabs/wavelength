package chainbackends

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/rpcclient"
)

var (
	btcAmountPattern = `([0-9]+(?:\.[0-9]+)?)`

	conflictingFeeRE = regexp.MustCompile(
		`less fees than conflicting txs; ` + btcAmountPattern +
			` < ` + btcAmountPattern,
	)
	additionalFeeRE = regexp.MustCompile(
		`not enough additional fees to relay; ` + btcAmountPattern +
			` < ` + btcAmountPattern,
	)
	conflictingFeeRateRE = regexp.MustCompile(
		`new feerate ` + btcAmountPattern +
			` BTC/kvB <= old feerate ` + btcAmountPattern +
			` BTC/kvB`,
	)
)

// ReplacementFeeConstraints contains fee information that Bitcoin Core
// reports when a replacement child cannot evict a conflicting transaction.
// Nil fields mean that the backend did not report that constraint.
type ReplacementFeeConstraints struct {
	// ConflictingFee is the total fee paid by the transactions that the
	// replacement must evict.
	ConflictingFee *btcutil.Amount

	// AdditionalFeeDeficit is the extra fee the attempted replacement
	// needed to satisfy the node's incremental relay fee.
	AdditionalFeeDeficit *btcutil.Amount

	// ConflictingFeeRateSatPerVByte is the integer part of the highest
	// conflicting feerate. A replacement must pay at least one sat/vByte
	// above this value to be strictly greater.
	ConflictingFeeRateSatPerVByte *int64
}

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

	// replacement contains structured fee constraints parsed from stable
	// Bitcoin Core replacement-policy diagnostics. It remains nil for
	// unrelated or unrecognized rejection reasons.
	replacement *ReplacementFeeConstraints
}

// NewPackageTxError builds a `PackageTxError` from a per-tx package result.
// The mapped sentinel is computed eagerly via `rpcclient.MapRPCErr` so the
// caller side can rely on `errors.Is` without re-parsing the reason.
func NewPackageTxError(wtxid string, txid chainhash.Hash,
	reason string) *PackageTxError {

	return &PackageTxError{
		Wtxid:       wtxid,
		Txid:        txid,
		Reason:      reason,
		mapped:      rpcclient.MapRPCErr(errors.New(reason)),
		replacement: parseReplacementFeeConstraints(reason),
	}
}

// ReplacementConstraints returns the structured replacement-policy fee
// information carried by this per-transaction rejection. The returned value
// is a copy and is nil when the backend did not expose a recognized floor.
func (e *PackageTxError) ReplacementConstraints() *ReplacementFeeConstraints {
	if e == nil || e.replacement == nil {
		return nil
	}

	constraints := *e.replacement
	constraints.ConflictingFee = copyAmount(e.replacement.ConflictingFee)
	constraints.AdditionalFeeDeficit = copyAmount(
		e.replacement.AdditionalFeeDeficit,
	)
	if e.replacement.ConflictingFeeRateSatPerVByte != nil {
		value := *e.replacement.ConflictingFeeRateSatPerVByte
		constraints.ConflictingFeeRateSatPerVByte = &value
	}

	return &constraints
}

// parseReplacementFeeConstraints extracts fee floors from Bitcoin Core's
// replacement-policy diagnostics. Core 28 through 31 use these messages for
// total-fee and incremental-relay failures; Core 28 through 30 also report the
// conflicting feerate before cluster mempool replaced that check.
func parseReplacementFeeConstraints(reason string) *ReplacementFeeConstraints {
	constraints := &ReplacementFeeConstraints{}
	found := false

	if match := conflictingFeeRE.FindStringSubmatch(
		reason,
	); len(match) == 3 {

		if amount, ok := parseBTCAmount(match[2]); ok {
			constraints.ConflictingFee = &amount
			found = true
		}
	}

	if match := additionalFeeRE.FindStringSubmatch(
		reason,
	); len(match) == 3 {

		paid, paidOK := parseBTCAmount(match[1])
		required, requiredOK := parseBTCAmount(match[2])
		if paidOK && requiredOK && required > paid {
			deficit := required - paid
			constraints.AdditionalFeeDeficit = &deficit
			found = true
		}
	}

	if match := conflictingFeeRateRE.FindStringSubmatch(
		reason,
	); len(match) == 3 {

		satsPerKvB, ok := parseBTCAmount(match[2])
		if ok && satsPerKvB > 0 {
			// One BTC/kvB equals 100,000 sat/vByte. Keep the
			// integer floor because the broadcaster adds one
			// sat/vByte when enforcing a strict replacement
			// increase.
			rate := int64(satsPerKvB) / 1000
			constraints.ConflictingFeeRateSatPerVByte = &rate
			found = true
		}
	}

	if !found {
		return nil
	}

	return constraints
}

// parseBTCAmount converts Core's fixed-point BTC diagnostic value to sats.
func parseBTCAmount(value string) (btcutil.Amount, bool) {
	btc, err := strconv.ParseFloat(value, 64)
	if err != nil || btc < 0 {
		return 0, false
	}

	amount, err := btcutil.NewAmount(btc)
	if err != nil {
		return 0, false
	}

	return amount, true
}

// copyAmount copies an optional amount without sharing its pointer.
func copyAmount(amount *btcutil.Amount) *btcutil.Amount {
	if amount == nil {
		return nil
	}

	value := *amount

	return &value
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

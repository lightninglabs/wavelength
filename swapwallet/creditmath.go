//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"fmt"
	"math"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
)

// saturatingAddSat returns a+b, clamped to the maximum uint64 on overflow
// rather than wrapping. Credit figures (caps, applied, top-up) come from the
// swap server and are summed before a credit-routing decision; a wrapped sum
// could silently under-count and mis-route a credit-backed pay, so saturating
// up is the safe direction — a credit cap of "max" simply means "no cap", and
// an over-large cover sum still reads as "covers".
func saturatingAddSat(a, b uint64) uint64 {
	if b > ^uint64(0)-a {
		return ^uint64(0)
	}

	return a + b
}

// creditCoversSat reports whether the applied credits plus the planned top-up
// cover the principal, without overflowing the sum. A pay that covers its full
// principal from credit has no Lightning swap leg, so this decides credit-only
// vs mixed routing; computing it with a wrapping add could flip a credit-only
// pay to mixed and hand terminal authority to the wrong layer.
func creditCoversSat(creditAppliedSat, creditTopupSat,
	principalSat uint64) bool {

	return saturatingAddSat(creditAppliedSat, creditTopupSat) >=
		principalSat
}

// validateCreditPlan verifies that a server-proposed credit plan stays within
// the two independent caller caps. The applied credit plus shortfall is what
// the eventual payment must reserve. The top-up is new Ark value moved into
// server credit and therefore has its own cap.
//
// A top-up may exceed the shortfall because the server rounds it up to the
// minimum Ark output. It may never be smaller than the shortfall, appear
// without a shortfall, or exceed the caller's explicit top-up cap.
func validateCreditPlan(plan *wavewalletrpc.CreditPreview, maxCreditSat,
	maxCreditTopupSat uint64) error {

	if plan == nil {
		return nil
	}

	appliedSat := plan.GetCreditAppliedSat()
	shortfallSat := plan.GetCreditShortfallSat()
	topupSat := plan.GetCreditTopupSat()

	requiredCreditSat, err := creditRequirementSat(
		appliedSat, shortfallSat,
	)
	if err != nil {
		return err
	}
	if requiredCreditSat > maxCreditSat {
		return fmt.Errorf("%w: credit requirement %d exceeds "+
			"max_credit_sat %d", ErrAmountInvalid,
			requiredCreditSat, maxCreditSat)
	}

	return validateCreditTopup(
		shortfallSat, topupSat, maxCreditTopupSat,
	)
}

// creditRequirementSat returns the total credit the eventual payment must
// reserve. A wrapped requirement could pass a smaller caller cap and fund a
// top-up that the later payment is forbidden to use.
func creditRequirementSat(appliedSat, shortfallSat uint64) (uint64, error) {
	requiredCreditSat, ok := checkedAddSat(appliedSat, shortfallSat)
	if !ok {
		return 0, fmt.Errorf("%w: credit requirement overflows uint64",
			ErrAmountInvalid)
	}

	return requiredCreditSat, nil
}

// validateCreditTopup permits only the server's documented upward rounding
// from a real shortfall, within the caller's independent top-up cap.
func validateCreditTopup(shortfallSat, topupSat,
	maxCreditTopupSat uint64) error {

	switch {
	case shortfallSat == 0 && topupSat != 0:
		return fmt.Errorf("%w: credit top-up requires a shortfall",
			ErrAmountInvalid)

	case shortfallSat > 0 && topupSat < shortfallSat:
		return fmt.Errorf("%w: credit top-up %d is below shortfall %d",
			ErrAmountInvalid, topupSat, shortfallSat)

	case topupSat > maxCreditTopupSat:
		return fmt.Errorf("%w: credit top-up %d exceeds "+
			"max_credit_topup_sat %d", ErrAmountInvalid, topupSat,
			maxCreditTopupSat)
	}

	return nil
}

// checkedOutflowSat adds the Ark funding and credit top-up legs and converts
// their sum to the signed wallet RPC amount without wrapping either integer
// representation.
func checkedOutflowSat(arkFundingSat, creditTopupSat uint64) (int64, error) {
	totalSat, ok := checkedAddSat(arkFundingSat, creditTopupSat)
	if !ok || totalSat > math.MaxInt64 {
		return 0, fmt.Errorf("%w: total outflow exceeds int64 range",
			ErrAmountInvalid)
	}

	return int64(totalSat), nil
}

// checkedAddSat returns a+b and reports whether the unsigned addition was
// exact.
func checkedAddSat(a, b uint64) (uint64, bool) {
	if b > math.MaxUint64-a {
		return 0, false
	}

	return a + b, true
}

// ceilMsatToSat converts a millisatoshi amount to satoshis, rounding UP when
// the amount is not a whole number of satoshis. Lightning amounts carry msat
// precision, but credits and Ark outputs are sat-denominated; rounding up
// (never down) guarantees the wallet never under-funds a credit pay or
// under-reports a credit receive by the sub-satoshi remainder.
func ceilMsatToSat(amountMSat uint64) uint64 {
	if amountMSat%1000 != 0 {
		return (amountMSat + 999) / 1000
	}

	return amountMSat / 1000
}

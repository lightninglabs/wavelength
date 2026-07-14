//go:build wavewalletrpc && swapruntime

package swapwallet

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

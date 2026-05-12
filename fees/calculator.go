package fees

import (
	"fmt"
	"math"
	"sync/atomic"

	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// daysPerYear is the number of days in a year used for
	// annualizing the liquidity cost.
	daysPerYear = 365.0

	// minutesPerBlock is the average Bitcoin block time in
	// minutes.
	minutesPerBlock = 10.0

	// minutesPerDay is the number of minutes in a day.
	minutesPerDay = 60.0 * 24.0

	// exitBranchBaseVBytes is the fixed portion of a single
	// branch transaction in the VTXO tree exit path: version,
	// locktime, 1 P2TR keypath input, 1 P2TR output, plus the
	// tapscript control block. Roughly 100 vBytes in practice.
	exitBranchBaseVBytes int64 = 100

	// exitBranchSiblingVBytes is the per-sibling witness cost
	// added to a branch tx when its radix is R > 2. Each branch
	// reveals R-1 sibling hashes as witness data; each 32-byte
	// hash plus its 1-byte varint length is 33 witness bytes =
	// ~8.25 vB (rounded up to 9 for conservative estimation).
	exitBranchSiblingVBytes int64 = 9

	// exitClaimVBytes is the approximate virtual size of the
	// final claim transaction after the CSV delay.
	exitClaimVBytes int64 = 150
)

// branchVBytesForRadix returns the per-branch vByte cost as a
// function of the tree radix R. A branch tx has a fixed overhead
// plus (R-1) sibling hashes revealed as witness data. At R=2 the
// branch carries a single sibling; at larger R the witness grows
// linearly, trading fewer tree levels against a larger per-level
// tx. This produces a U-shaped ExitCost curve in R (see
// docs/fee-model.md "Minimum Viable VTXO").
func branchVBytesForRadix(radix int) int64 {
	if radix < 2 {
		radix = 2
	}

	siblings := int64(radix - 1)

	return exitBranchBaseVBytes + siblings*exitBranchSiblingVBytes
}

// FeeBreakdown contains the itemized result of a fee computation.
type FeeBreakdown struct {
	// LiquidityFeeSat is the time-value-of-money component:
	// A * (delta/365) * effectiveRate.
	LiquidityFeeSat int64

	// OnChainShareSat is the per-participant share of the
	// round's on-chain cost: F_round / B.
	OnChainShareSat int64

	// MarginSat is the fixed operator margin (epsilon).
	MarginSat int64

	// TotalFeeSat is LiquidityFeeSat + OnChainShareSat +
	// MarginSat.
	TotalFeeSat int64

	// EffectiveAnnualRate is the rate used after applying
	// the utilization spread: r + Delta(u).
	EffectiveAnnualRate float64

	// BelowMinViable is true when the fee exceeds the
	// MinViableVTXOPct threshold of the amount.
	BelowMinViable bool
}

// Calculator computes fees using a Schedule and provides
// thread-safe access to the current schedule via atomic pointer.
type Calculator struct {
	schedule atomic.Pointer[Schedule]
}

// NewCalculator creates a Calculator with the given initial
// schedule. The schedule is validated before the calculator is
// returned so that a malformed config fails fast at startup rather
// than silently producing bogus fee quotes.
func NewCalculator(s *Schedule) (*Calculator, error) {
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("invalid initial schedule: %w", err)
	}

	c := &Calculator{}
	c.schedule.Store(s)

	return c, nil
}

// Schedule returns the current fee schedule.
func (c *Calculator) Schedule() *Schedule {
	return c.schedule.Load()
}

// UpdateSchedule atomically replaces the fee schedule. The new
// schedule is validated first; on error the current schedule is
// left in place so a bad hot-reload cannot corrupt the in-memory
// pricing state.
func (c *Calculator) UpdateSchedule(s *Schedule) error {
	if err := s.Validate(); err != nil {
		return fmt.Errorf("invalid schedule: %w", err)
	}

	c.schedule.Store(s)

	return nil
}

// ComputeFee calculates the total fee for a liquidity-requiring
// operation given the remaining VTXO lifetime in fractional days.
//
// Parameters:
//   - amountSat: VTXO amount in satoshis (A).
//   - batchSize: expected number of participants (B).
//   - remainingDays: remaining VTXO lifetime in days (delta).
//   - feeRate: current on-chain fee rate.
//   - utilization: current treasury utilization ratio (0.0-1.0).
func (c *Calculator) ComputeFee(amountSat int64, batchSize int,
	remainingDays float64, feeRate chainfee.SatPerKWeight,
	utilization float64) *FeeBreakdown {

	s := c.schedule.Load()

	effRate := s.EffectiveRate(utilization)

	// Normalize batch size so on-chain share is never zero
	// due to an unset input.
	if batchSize < 1 {
		batchSize = 1
	}

	// Liquidity fee: A * (delta / 365) * r_eff.
	liqFee := float64(amountSat) * (remainingDays / daysPerYear) *
		effRate
	liqFeeSat := int64(math.Ceil(liqFee))

	// On-chain share: F_round / B.
	roundCost := EstimateRoundCost(batchSize, feeRate)
	onChainShare := int64(
		math.Ceil(float64(roundCost) / float64(batchSize)),
	)

	totalFee := liqFeeSat + onChainShare + s.BaseMarginSat

	// Check economic viability.
	belowMinViable := false
	if amountSat > 0 && s.MinViableVTXOPct > 0 {
		maxFee := float64(amountSat) *
			float64(s.MinViableVTXOPct) / 100.0

		belowMinViable = float64(totalFee) > maxFee
	}

	return &FeeBreakdown{
		LiquidityFeeSat:     liqFeeSat,
		OnChainShareSat:     onChainShare,
		MarginSat:           s.BaseMarginSat,
		TotalFeeSat:         totalFee,
		EffectiveAnnualRate: effRate,
		BelowMinViable:      belowMinViable,
	}
}

// ComputeBoardingFee calculates the fee for a boarding input.
// Boarding does not deploy new operator capital (the user brings
// on-chain BTC), so no liquidity fee is charged. The fee is
// only the on-chain share plus the operator margin:
// F_boarding(B; ε) = F_round/B + ε.
func (c *Calculator) ComputeBoardingFee(amountSat int64, batchSize int,
	feeRate chainfee.SatPerKWeight) *FeeBreakdown {

	s := c.schedule.Load()

	if batchSize < 1 {
		batchSize = 1
	}

	// On-chain share: F_round / B.
	roundCost := EstimateRoundCost(batchSize, feeRate)
	onChainShare := int64(
		math.Ceil(float64(roundCost) / float64(batchSize)),
	)

	totalFee := onChainShare + s.BaseMarginSat

	// Check economic viability.
	belowMinViable := false
	if amountSat > 0 && s.MinViableVTXOPct > 0 {
		maxFee := float64(amountSat) *
			float64(s.MinViableVTXOPct) / 100.0

		belowMinViable = float64(totalFee) > maxFee
	}

	return &FeeBreakdown{
		LiquidityFeeSat:     0,
		OnChainShareSat:     onChainShare,
		MarginSat:           s.BaseMarginSat,
		TotalFeeSat:         totalFee,
		EffectiveAnnualRate: s.EffectiveRate(0),
		BelowMinViable:      belowMinViable,
	}
}

// ComputeForfeitFee calculates the fee for a forfeit/refresh
// operation. Delta is computed from the remaining blocks on the
// forfeited VTXO. The δ_min fee floor is applied: the liquidity
// fee is computed using max(δ, δ_min) so that lazy refreshes
// near expiry still pay a minimum liquidity cost.
func (c *Calculator) ComputeForfeitFee(amountSat int64, batchSize int,
	remainingBlocks uint32, feeRate chainfee.SatPerKWeight,
	utilization float64) *FeeBreakdown {

	s := c.schedule.Load()

	// Apply the δ_min fee floor: use the larger of the actual
	// remaining blocks and the configured minimum.
	effectiveBlocks := remainingBlocks
	if s.MinRefreshDeltaBlocks > 0 &&
		effectiveBlocks < s.MinRefreshDeltaBlocks {

		effectiveBlocks = s.MinRefreshDeltaBlocks
	}

	days := BlocksToDays(effectiveBlocks)

	return c.ComputeFee(
		amountSat, batchSize, days, feeRate, utilization,
	)
}

// MinViableAmount returns the minimum VTXO amount (in satoshis)
// where the fee does not exceed MinViableVTXOPct of the amount,
// at the given parameters. Returns 0 if no threshold is
// configured.
//
// The fee is: A*(delta/365)*r_eff + F_round/B + margin.
// Viable when: fee <= A * pct/100.
// Solving: A >= (F_round/B + margin) / (pct/100 - delta/365*r_eff).
func (c *Calculator) MinViableAmount(batchSize int, remainingDays float64,
	feeRate chainfee.SatPerKWeight, utilization float64) int64 {

	s := c.schedule.Load()
	if s.MinViableVTXOPct == 0 {
		return 0
	}

	if batchSize < 1 {
		batchSize = 1
	}

	effRate := s.EffectiveRate(utilization)
	pctFrac := float64(s.MinViableVTXOPct) / 100.0
	liqFrac := (remainingDays / daysPerYear) * effRate

	// If the liquidity fraction alone exceeds the viability
	// threshold, then no finite amount is viable (the fee
	// always exceeds the percentage of value). Return a very
	// large value to signal this.
	denom := pctFrac - liqFrac
	if denom <= 0 {
		return math.MaxInt64
	}

	roundCost := EstimateRoundCost(batchSize, feeRate)
	onChainShare := float64(roundCost) / float64(batchSize)

	fixedCost := onChainShare + float64(s.BaseMarginSat)
	minAmount := fixedCost / denom

	return int64(math.Ceil(minAmount))
}

// ExitCost estimates the on-chain cost in satoshis for a
// unilateral VTXO exit given the batch size, tree radix, and
// current feerate. The exit requires traversing
// ceil(log_radix(batchSize)) branch transactions plus a final
// claim transaction. The radix determines the tree fan-out;
// radix 2 is a binary tree. For batchSize=1 the depth is 0
// (the root is the VTXO itself), so only the claim tx is
// needed.
//
// The branch vByte cost scales with radix R: each branch tx
// reveals R-1 sibling hashes as witness data, so larger radixes
// mean fewer tree levels but heavier per-level txs. This yields
// a U-shaped total-cost curve in R with an optimal radix for
// each (B, feerate) pair (see docs/fee-model.md, "Minimum
// Viable VTXO").
func ExitCost(batchSize int, radix int, feeRate chainfee.SatPerKWeight) int64 {
	if batchSize < 1 {
		batchSize = 1
	}
	if radix < 2 {
		radix = 2
	}

	depth := int64(
		math.Ceil(
			math.Log(float64(batchSize)) /
				math.Log(float64(radix)),
		),
	)

	totalVBytes := depth*branchVBytesForRadix(radix) +
		exitClaimVBytes

	// Convert vBytes to weight units (1 vB = 4 WU) and use
	// FeeForWeight to avoid the precision loss that would result
	// from dividing a sat/kvB rate down to an integer sat/vB
	// rate (low rates like 1.5 sat/vB would truncate to 1).
	return int64(
		feeRate.FeeForWeight(
			lntypes.WeightUnit(totalVBytes * 4),
		),
	)
}

// EstimateRoundCost estimates the total on-chain fee in satoshis
// for a round commitment transaction given the batch size and
// fee rate.
//
// The round tx modeled here has:
//   - 1 operator wallet input (P2TR keypath spend, funded by LND).
//   - B VTXO tree root outputs (P2TR). We amortize a per-
//     participant output across the batch even though the on-
//     chain round commitment only contains a single tree-root
//     output; the participant pays for the share of the tree
//     output they will eventually unroll to.
//   - B/2 connector outputs (P2TR) used by the forfeit covenant.
//   - 1 change output (P2TR).
//
// Weight accounting uses lnd's input.TxWeightEstimator so that
// P2TR input/output sizing (including the BaseTxSize plus varint
// growth as the output count crosses 253) matches lnd's wallet
// layer exactly — any future change to that estimator propagates
// through here automatically.
func EstimateRoundCost(batchSize int, feeRate chainfee.SatPerKWeight) int64 {
	if batchSize < 1 {
		batchSize = 1
	}

	var est input.TxWeightEstimator

	// Default sighash (0x00) omits the sighash byte from the
	// Schnorr signature — the cheaper and more common case.
	est.AddTaprootKeySpendInput(txscript.SigHashDefault)

	// B VTXO tree root outputs + B/2 connectors + 1 change.
	totalP2TROutputs := batchSize + batchSize/2 + 1
	for i := 0; i < totalP2TROutputs; i++ {
		est.AddP2TROutput()
	}

	return int64(feeRate.FeeForWeight(est.Weight()))
}

// BlocksToDays converts a block count to fractional days using
// the 10-minute average block time.
func BlocksToDays(blocks uint32) float64 {
	return float64(blocks) * minutesPerBlock / minutesPerDay
}

// RemainingBlocks computes the remaining blocks until a VTXO
// expires given the round's confirmation height, the CSV delay,
// and the current chain height.
func RemainingBlocks(confirmHeight, csvDelay, currentHeight uint32) uint32 {
	expiry := confirmHeight + csvDelay
	if currentHeight >= expiry {
		return 0
	}

	return expiry - currentHeight
}

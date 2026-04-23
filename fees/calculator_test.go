package fees

import (
	"fmt"
	"math"
	"testing"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// defaultTestSchedule returns a schedule suitable for tests with
// a 5% annual rate, 100 sat margin, and 70% utilization
// threshold.
func defaultTestSchedule() *Schedule {
	return &Schedule{
		AnnualRate:                 0.05,
		BaseMarginSat:              100,
		UtilizationThresholdBPS:    7000,
		UtilizationSpreadDelta0BPS: 100,
		UtilizationSpreadDelta1BPS: 500,
		MinViableVTXOPolicy:        DustPolicyReject,
		MinViableVTXOPct:           50,
	}
}

// TestEffectiveRate verifies the congestion spread calculation.
func TestEffectiveRate(t *testing.T) {
	t.Parallel()

	s := defaultTestSchedule()

	tests := []struct {
		name        string
		utilization float64
		wantRate    float64
	}{
		{
			name:        "below threshold",
			utilization: 0.50,
			wantRate:    0.05,
		},
		{
			name:        "at threshold",
			utilization: 0.70,
			wantRate:    0.05,
		},
		{
			name:        "above threshold",
			utilization: 0.80,
			// r + delta0 + delta1 * (0.80 - 0.70)
			// = 0.05 + 0.01 + 0.05 * 0.10 = 0.065
			wantRate: 0.065,
		},
		{
			name:        "fully utilized",
			utilization: 1.0,
			// r + delta0 + delta1 * (1.0 - 0.70)
			// = 0.05 + 0.01 + 0.05 * 0.30 = 0.075
			wantRate: 0.075,
		},
		{
			name:        "zero utilization",
			utilization: 0.0,
			wantRate:    0.05,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := s.EffectiveRate(tc.utilization)
			require.InDelta(
				t, tc.wantRate, got, 1e-9,
				"effective rate mismatch",
			)
		})
	}
}

// TestEffectiveRateDiscontinuity pins down the intentional step
// Δ₀ at u = u*. The rate at u=u* is the base rate r, and at any
// u strictly above u* it jumps by Δ₀. A future smoothing change
// that collapsed this step would silently shift congestion
// pricing for all refreshes on the u*-boundary.
func TestEffectiveRateDiscontinuity(t *testing.T) {
	t.Parallel()

	s := defaultTestSchedule()

	// Threshold = 7000 bps = 0.70; Δ₀ = 100 bps = 0.01.
	const (
		threshold = 0.70
		delta0    = 0.01
		baseRate  = 0.05
	)

	// At the threshold exactly: no spread applied.
	atThreshold := s.EffectiveRate(threshold)
	require.InDelta(t, baseRate, atThreshold, 1e-12)

	// Infinitesimally above: jumps by Δ₀ (plus a negligible Δ₁
	// contribution from the tiny excess).
	justAbove := s.EffectiveRate(threshold + 1e-12)
	require.Greater(
		t, justAbove-atThreshold, delta0-1e-6,
		"step at u=u* must be at least Δ₀",
	)
	require.InDelta(
		t, baseRate+delta0, justAbove, 1e-6,
		"at u=u*+ε rate = r + Δ₀",
	)
}

// TestEffectiveRateUnitMix verifies the unit-mixing semantics of
// Δ₁: it is bps of rate multiplied by a unit ratio of excess
// utilization. At Δ₁=500 bps and excess=0.10, the Δ₁ contribution
// must be 50 bps, not 5 (as one would get from naively
// multiplying bps × bps) nor 500_000 (as one would get from
// reading Δ₁ as unitless).
func TestEffectiveRateUnitMix(t *testing.T) {
	t.Parallel()

	s := defaultTestSchedule()

	// Δ₀ = 100 bps = 0.01, Δ₁ = 500 bps = 0.05.
	// At u = 0.80 (excess = 0.10):
	//   r_eff = 0.05 + 0.01 + 0.05*0.10 = 0.065.
	got := s.EffectiveRate(0.80)
	require.InDelta(t, 0.065, got, 1e-12)

	// Swap Δ₁ to 0 to isolate Δ₀'s contribution:
	// r_eff = 0.05 + 0.01 + 0*0.10 = 0.06.
	noRamp := *s
	noRamp.UtilizationSpreadDelta1BPS = 0
	require.InDelta(t, 0.06, noRamp.EffectiveRate(0.80), 1e-12)

	// Swap Δ₀ to 0: r_eff = r + Δ₁*(u-u*) is a continuous ramp
	// rooted at the threshold.
	noStep := *s
	noStep.UtilizationSpreadDelta0BPS = 0
	require.InDelta(
		t, 0.05, noStep.EffectiveRate(0.70), 1e-12,
	)
	require.InDelta(
		t, 0.055, noStep.EffectiveRate(0.80), 1e-12,
	)
}

// TestComputeFeeBasic verifies the core fee formula with known
// inputs.
func TestComputeFeeBasic(t *testing.T) {
	t.Parallel()

	calc, err := NewCalculator(defaultTestSchedule())
	require.NoError(t, err)

	// 100,000 sats, batch of 100, 5 days remaining, 10 sat/vB
	// equivalent fee rate, no congestion.
	//
	// Liquidity: 100000 * (5/365) * 0.05 = 68.49 -> 69 sats
	// On-chain: EstimateRoundCost(100, rate) / 100
	// Margin: 100 sats
	feeRate := chainfee.SatPerKVByte(10_000).FeePerKWeight()
	bd := calc.ComputeFee(100_000, 100, 5.0, feeRate, 0.3)

	// Liquidity fee should be ceil(100000 * 5/365 * 0.05).
	expectedLiq := int64(math.Ceil(
		100_000.0 * (5.0 / 365.0) * 0.05,
	))
	require.Equal(t, expectedLiq, bd.LiquidityFeeSat)
	require.Equal(t, int64(100), bd.MarginSat)
	require.Greater(t, bd.OnChainShareSat, int64(0))
	require.Equal(
		t, bd.LiquidityFeeSat+bd.OnChainShareSat+bd.MarginSat,
		bd.TotalFeeSat,
	)
	require.InDelta(t, 0.05, bd.EffectiveAnnualRate, 1e-9)
	require.False(t, bd.BelowMinViable)
}

// TestComputeFeeCongestion verifies that fees increase when
// utilization exceeds the threshold.
func TestComputeFeeCongestion(t *testing.T) {
	t.Parallel()

	calc, err := NewCalculator(defaultTestSchedule())
	require.NoError(t, err)
	feeRate := chainfee.SatPerKVByte(10_000).FeePerKWeight()

	// Below threshold.
	bdLow := calc.ComputeFee(
		1_000_000, 64, 30.0, feeRate, 0.50,
	)

	// Above threshold.
	bdHigh := calc.ComputeFee(
		1_000_000, 64, 30.0, feeRate, 0.90,
	)

	require.Greater(
		t, bdHigh.LiquidityFeeSat, bdLow.LiquidityFeeSat,
		"congestion should increase liquidity fee",
	)
	require.Greater(
		t, bdHigh.EffectiveAnnualRate,
		bdLow.EffectiveAnnualRate,
		"congestion should increase effective rate",
	)
}

// TestComputeFeeDust verifies the BelowMinViable flag.
func TestComputeFeeDust(t *testing.T) {
	t.Parallel()

	calc, err := NewCalculator(defaultTestSchedule())
	require.NoError(t, err)
	feeRate := chainfee.SatPerKVByte(20_000).FeePerKWeight()

	// Tiny amount: 500 sats. The on-chain share + margin
	// alone should exceed 50% of value.
	bd := calc.ComputeFee(500, 64, 30.0, feeRate, 0.3)
	require.True(t, bd.BelowMinViable,
		"500 sats should be below min viable")

	// Large amount: 10M sats. Should be viable.
	bdLarge := calc.ComputeFee(
		10_000_000, 64, 30.0, feeRate, 0.3,
	)
	require.False(t, bdLarge.BelowMinViable,
		"10M sats should be viable")
}

// TestComputeBoardingFee verifies that boarding charges zero
// liquidity fee — only on-chain share and margin per the spec:
// F_boarding(B; ε) = F_round/B + ε.
func TestComputeBoardingFee(t *testing.T) {
	t.Parallel()

	calc, err := NewCalculator(defaultTestSchedule())
	require.NoError(t, err)
	feeRate := chainfee.SatPerKVByte(10_000).FeePerKWeight()

	bd := calc.ComputeBoardingFee(1_000_000, 64, feeRate)

	// Boarding must have zero liquidity fee.
	require.Equal(
		t, int64(0), bd.LiquidityFeeSat,
		"boarding should not charge liquidity fee",
	)

	// Total should be on-chain share + margin only.
	require.Equal(
		t, bd.OnChainShareSat+bd.MarginSat, bd.TotalFeeSat,
		"boarding total should be on-chain + margin",
	)
	require.Equal(t, int64(100), bd.MarginSat)
	require.Greater(t, bd.OnChainShareSat, int64(0))
}

// TestComputeForfeitFee verifies the forfeit fee helper.
func TestComputeForfeitFee(t *testing.T) {
	t.Parallel()

	calc, err := NewCalculator(defaultTestSchedule())
	require.NoError(t, err)
	feeRate := chainfee.SatPerKVByte(10_000).FeePerKWeight()

	// 500 remaining blocks is above the 144-block floor, so
	// the floor should not apply.
	bd := calc.ComputeForfeitFee(
		500_000, 64, 500, feeRate, 0.3,
	)

	expectedDays := BlocksToDays(500)
	bdDirect := calc.ComputeFee(
		500_000, 64, expectedDays, feeRate, 0.3,
	)
	require.Equal(
		t, bdDirect.TotalFeeSat, bd.TotalFeeSat,
		"forfeit fee should match direct computation",
	)
}

// TestComputeForfeitFeeDeltaMinFloor verifies that the δ_min
// fee floor prices lazy refreshes at the minimum delta when the
// actual remaining blocks are below MinRefreshDeltaBlocks.
func TestComputeForfeitFeeDeltaMinFloor(t *testing.T) {
	t.Parallel()

	s := defaultTestSchedule()
	s.MinRefreshDeltaBlocks = 144 // ~1 day floor
	calc, err := NewCalculator(s)
	require.NoError(t, err)
	feeRate := chainfee.SatPerKVByte(10_000).FeePerKWeight()

	// Lazy refresh with only 50 blocks remaining (below the
	// 144-block floor).
	bdLazy := calc.ComputeForfeitFee(
		100_000, 64, 50, feeRate, 0.3,
	)

	// The fee should be computed as if δ = 144 blocks (1 day),
	// not 50 blocks.
	floorDays := BlocksToDays(144)
	bdFloor := calc.ComputeFee(
		100_000, 64, floorDays, feeRate, 0.3,
	)
	require.Equal(
		t, bdFloor.TotalFeeSat, bdLazy.TotalFeeSat,
		"lazy refresh should be priced at δ_min floor",
	)
	require.Equal(
		t, bdFloor.LiquidityFeeSat, bdLazy.LiquidityFeeSat,
		"liquidity fee should use floored delta",
	)

	// Verify the floor actually raises the fee compared to
	// the raw 50-block computation.
	actualDays := BlocksToDays(50)
	bdRaw := calc.ComputeFee(
		100_000, 64, actualDays, feeRate, 0.3,
	)
	require.Greater(
		t, bdLazy.LiquidityFeeSat, bdRaw.LiquidityFeeSat,
		"floored fee should exceed raw low-delta fee",
	)
}

// TestComputeForfeitFeeAboveFloor verifies that when remaining
// blocks exceed the floor, the actual delta is used unchanged.
func TestComputeForfeitFeeAboveFloor(t *testing.T) {
	t.Parallel()

	s := defaultTestSchedule()
	s.MinRefreshDeltaBlocks = 144
	calc, err := NewCalculator(s)
	require.NoError(t, err)
	feeRate := chainfee.SatPerKVByte(10_000).FeePerKWeight()

	// 500 blocks remaining, well above the 144-block floor.
	bd := calc.ComputeForfeitFee(
		100_000, 64, 500, feeRate, 0.3,
	)

	expectedDays := BlocksToDays(500)
	bdDirect := calc.ComputeFee(
		100_000, 64, expectedDays, feeRate, 0.3,
	)
	require.Equal(
		t, bdDirect.LiquidityFeeSat, bd.LiquidityFeeSat,
		"above-floor delta should be used as-is",
	)
}

// TestMinViableAmount verifies the minimum viable VTXO
// computation.
func TestMinViableAmount(t *testing.T) {
	t.Parallel()

	calc, err := NewCalculator(defaultTestSchedule())
	require.NoError(t, err)
	feeRate := chainfee.SatPerKVByte(20_000).FeePerKWeight()

	minAmt := calc.MinViableAmount(64, 30.0, feeRate, 0.3)

	// The minimum amount should be positive.
	require.Greater(t, minAmt, int64(0))

	// Verify: at minAmt, the fee should be at or below 50%
	// of the amount.
	bd := calc.ComputeFee(minAmt, 64, 30.0, feeRate, 0.3)
	maxFee := float64(minAmt) * 0.50
	require.LessOrEqual(
		t, float64(bd.TotalFeeSat), maxFee+1.0,
		"fee at min viable amount should be <= 50%%",
	)

	// At minAmt - 1, it should exceed the threshold
	// (unless minAmt is very small).
	if minAmt > 100 {
		bdBelow := calc.ComputeFee(
			minAmt-100, 64, 30.0, feeRate, 0.3,
		)
		require.True(t, bdBelow.BelowMinViable,
			"below min viable should be flagged")
	}
}

// TestMinViableAmountExtremeRate verifies that when the liquidity
// fraction alone exceeds the viability threshold, MinViableAmount
// returns MaxInt64.
func TestMinViableAmountExtremeRate(t *testing.T) {
	t.Parallel()

	s := defaultTestSchedule()
	s.AnnualRate = 5.0 // 500% annual rate
	calc, err := NewCalculator(s)
	require.NoError(t, err)

	feeRate := chainfee.SatPerKVByte(10_000).FeePerKWeight()

	// With 500% rate and 365 days remaining, the liquidity
	// fraction is 5.0 which exceeds the 50% threshold.
	minAmt := calc.MinViableAmount(64, 365.0, feeRate, 0.0)
	require.Equal(t, int64(math.MaxInt64), minAmt)
}

// TestExitCost verifies the unilateral exit cost estimation.
func TestExitCost(t *testing.T) {
	t.Parallel()

	// 20 sat/vB = 20_000 sat/kvB.
	rate20 := chainfee.SatPerKVByte(20_000).FeePerKWeight()
	rate10 := chainfee.SatPerKVByte(10_000).FeePerKWeight()

	// Expected helper uses the same precision-preserving path as
	// production (weight = vB*4) so the assertion cannot silently
	// regress back to an integer sat/vB truncation.
	expectAt := func(vB int64, fr chainfee.SatPerKWeight) int64 {
		return int64(fr.FeeForWeight(lntypes.WeightUnit(vB * 4)))
	}

	// Batch of 128, binary tree (radix 2):
	// depth = ceil(log2(128)) = 7.
	cost := ExitCost(128, 2, rate20)
	depth := int64(math.Ceil(math.Log2(128)))
	expected := expectAt(
		depth*branchVBytesForRadix(2)+exitClaimVBytes, rate20,
	)
	require.Equal(t, expected, cost)

	// Batch of 1, radix 2: depth = ceil(log2(1)) = 0.
	// Only the claim tx is needed.
	cost1 := ExitCost(1, 2, rate10)
	require.Equal(t, expectAt(exitClaimVBytes, rate10), cost1)

	// Batch of 64, radix 4: depth = ceil(log4(64)) = 3. Branch
	// vBytes now scale with radix via branchVBytesForRadix.
	costR4 := ExitCost(64, 4, rate10)
	depthR4 := int64(math.Ceil(
		math.Log(64) / math.Log(4),
	))
	expectedR4 := expectAt(
		depthR4*branchVBytesForRadix(4)+exitClaimVBytes, rate10,
	)
	require.Equal(t, expectedR4, costR4)
}

// TestExitCostPrecisionLowRate guards the regression where the old
// `int64(feeRate.FeePerKVByte()) / 1000` truncated sub-integer
// sat/vB rates to 0 or 1, under-pricing the exit cost (and thereby
// weakening the security-dust threshold enforced against ExitCost).
//
// At 1500 sat/kvB (1.5 sat/vB), the legacy implementation would
// compute satPerVB=1, so a 1050-vByte exit would cost 1050 sats;
// the correct precision-preserving fee is 1575 sats (1.5×1050).
func TestExitCostPrecisionLowRate(t *testing.T) {
	t.Parallel()

	// 1.5 sat/vB.
	rate := chainfee.SatPerKVByte(1500).FeePerKWeight()

	// depth = ceil(log2(64)) = 6, so vB = 6 * branch + claim.
	cost := ExitCost(64, 2, rate)

	totalVB := int64(math.Ceil(math.Log2(64)))*
		branchVBytesForRadix(2) + exitClaimVBytes
	want := int64(rate.FeeForWeight(lntypes.WeightUnit(totalVB * 4)))
	require.Equal(t, want, cost)

	// The legacy truncation result would be totalVB * 1, which
	// must be strictly less than the correct 1.5×totalVB answer.
	// Guard against regressing to that path.
	legacyTruncated := totalVB * 1
	require.Greater(
		t, cost, legacyTruncated,
		"precision-preserving cost must exceed truncated cost",
	)

	// Fractional rate must round predictably. 1.5 sat/vB × totalVB
	// vBytes = 1.5 × totalVB sats.
	require.InDelta(
		t, float64(totalVB)*1.5, float64(cost), 1.0,
		"exit cost must preserve sub-sat/vB precision",
	)
}

// TestExitCostPrecisionSubSatRate verifies that rates below 1 sat/vB
// do not collapse to zero due to integer division. The legacy
// `int64(feeRate.FeePerKVByte())/1000` would be 0 at 750 sat/kvB,
// which would make the security-dust threshold trivially satisfied
// for any VTXO.
func TestExitCostPrecisionSubSatRate(t *testing.T) {
	t.Parallel()

	// 750 sat/kvB = 0.75 sat/vB.
	rate := chainfee.SatPerKVByte(750).FeePerKWeight()

	cost := ExitCost(16, 2, rate)
	require.Greater(
		t, cost, int64(0),
		"0.75 sat/vB must produce a positive exit cost",
	)

	totalVB := int64(math.Ceil(math.Log2(16)))*
		branchVBytesForRadix(2) + exitClaimVBytes
	want := int64(rate.FeeForWeight(lntypes.WeightUnit(totalVB * 4)))
	require.Equal(t, want, cost)
}

// TestExitCostProperties asserts the invariants that must hold
// across the full (batch size, radix, fee rate) input space:
//
//  1. ExitCost is non-negative.
//  2. ExitCost matches the precision-preserving weight-based fee
//     for the vByte footprint implied by the depth/radix model.
//     This is the strongest regression guard for the P1 truncation
//     bug: any path that re-introduces a sat/vB integer division
//     will break this equality at sub-integer sat/vB rates.
//  3. ExitCost is non-decreasing in fee rate (at fixed B, R).
//  4. ExitCost is non-decreasing in batch size (at fixed R, rate):
//     a larger tree cannot be cheaper to exit.
func TestExitCostProperties(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		// Draw any rate in [1, 500_000] sat/kvB. Includes
		// sub-sat/vB rates that previously truncated.
		rateKvB := rapid.Int64Range(1, 500_000).Draw(rt, "rateKvB")
		rate := chainfee.SatPerKVByte(rateKvB).FeePerKWeight()

		batchSize := rapid.IntRange(1, 10_000).Draw(rt, "batchSize")
		radix := rapid.IntRange(2, 32).Draw(rt, "radix")

		cost := ExitCost(batchSize, radix, rate)

		// (1) Non-negative.
		require.GreaterOrEqual(rt, cost, int64(0))

		// (2) Precision-preserving equality with weight-based
		// fee at the modeled vByte size.
		depth := int64(math.Ceil(
			math.Log(float64(batchSize)) /
				math.Log(float64(radix)),
		))
		totalVB := depth*branchVBytesForRadix(radix) +
			exitClaimVBytes
		wantFee := int64(rate.FeeForWeight(
			lntypes.WeightUnit(totalVB * 4),
		))
		require.Equal(rt, wantFee, cost,
			"cost must match precision-preserving FeeForWeight")

		// (3) Monotone in fee rate (at fixed B, R).
		higherRate := chainfee.SatPerKVByte(
			rateKvB + 1000,
		).FeePerKWeight()
		costHigher := ExitCost(batchSize, radix, higherRate)
		require.GreaterOrEqual(rt, costHigher, cost,
			"ExitCost must be non-decreasing in fee rate")

		// (4) Monotone in batch size (at fixed R, rate).
		if batchSize < 10_000 {
			costLarger := ExitCost(
				batchSize+1, radix, rate,
			)
			require.GreaterOrEqual(rt, costLarger, cost,
				"ExitCost must not shrink with larger B")
		}
	})
}

// TestExitCostRadixUCurve verifies the spec's U-shaped exit cost
// curve: neither very-low nor very-high radix minimizes total
// cost. At a fixed batch size, the optimum sits at an interior
// radix because:
//
//   - Low R → many tree levels × small branch txs.
//   - High R → few levels × large branch txs (R-1 siblings).
//
// We lock in the qualitative shape (middle < extremes) rather
// than the exact optimum, so that retuning the sibling/overhead
// constants does not make this test brittle.
func TestExitCostRadixUCurve(t *testing.T) {
	t.Parallel()

	rate := chainfee.SatPerKVByte(10_000).FeePerKWeight()

	// Large batch so many internal radixes are realistic.
	const batchSize = 1024

	costR2 := ExitCost(batchSize, 2, rate)
	costR4 := ExitCost(batchSize, 4, rate)
	costR8 := ExitCost(batchSize, 8, rate)
	costR64 := ExitCost(batchSize, 64, rate)
	costR1024 := ExitCost(batchSize, batchSize, rate)

	// Interior radix should beat both extremes. R=8 at B=1024
	// sits roughly in the middle of the U.
	require.Less(t, costR8, costR2,
		"R=8 should beat binary at B=1024")
	require.Less(t, costR8, costR1024,
		"R=8 should beat flat-tree at B=1024")

	// R=4 should beat the extremes too (the optimum is likely
	// 4-16 depending on the constants).
	require.Less(t, costR4, costR1024,
		"R=4 should beat flat-tree at B=1024")

	// Sanity: very large radix loses to moderate radix due to
	// sibling-hash witness growth.
	require.Less(t, costR8, costR64,
		"R=8 should beat R=64 at B=1024")
}

// TestExitCostBranchSizeGrowsWithRadix locks in the core radix
// scaling: branch vBytes must strictly grow with R, because each
// additional unit of radix adds one sibling hash to the witness.
func TestExitCostBranchSizeGrowsWithRadix(t *testing.T) {
	t.Parallel()

	require.Less(t,
		branchVBytesForRadix(2), branchVBytesForRadix(3))
	require.Less(t,
		branchVBytesForRadix(3), branchVBytesForRadix(4))
	require.Less(t,
		branchVBytesForRadix(8), branchVBytesForRadix(16))

	// R=2 has exactly 1 sibling; the branch size must equal the
	// base plus one sibling's witness contribution.
	require.Equal(
		t, exitBranchBaseVBytes+exitBranchSiblingVBytes,
		branchVBytesForRadix(2),
	)

	// The radix clamp ensures R < 2 is treated as R=2.
	require.Equal(
		t, branchVBytesForRadix(2),
		branchVBytesForRadix(1),
	)
	require.Equal(
		t, branchVBytesForRadix(2),
		branchVBytesForRadix(0),
	)
}

// TestComputeFeeZeroBatchSize verifies that batchSize=0 is
// normalized to 1, producing a non-zero on-chain share.
func TestComputeFeeZeroBatchSize(t *testing.T) {
	t.Parallel()

	calc, err := NewCalculator(defaultTestSchedule())
	require.NoError(t, err)
	feeRate := chainfee.SatPerKVByte(10_000).FeePerKWeight()

	// batchSize=0 should be treated as batchSize=1.
	bd0 := calc.ComputeFee(100_000, 0, 5.0, feeRate, 0.3)
	bd1 := calc.ComputeFee(100_000, 1, 5.0, feeRate, 0.3)

	require.Equal(
		t, bd1.TotalFeeSat, bd0.TotalFeeSat,
		"batchSize=0 should produce same fee as batchSize=1",
	)
	require.Greater(t, bd0.OnChainShareSat, int64(0),
		"on-chain share must be non-zero for batchSize=0")

	// Same for MinViableAmount.
	min0 := calc.MinViableAmount(0, 30.0, feeRate, 0.3)
	min1 := calc.MinViableAmount(1, 30.0, feeRate, 0.3)
	require.Equal(t, min1, min0,
		"MinViableAmount(0) should equal MinViableAmount(1)")
}

// TestBlocksToDays verifies the block-to-day conversion.
func TestBlocksToDays(t *testing.T) {
	t.Parallel()

	// 144 blocks = 1 day (144 * 10 / 1440 = 1.0).
	require.InDelta(t, 1.0, BlocksToDays(144), 1e-9)

	// 1008 blocks = 7 days.
	require.InDelta(t, 7.0, BlocksToDays(1008), 1e-9)

	// 0 blocks = 0 days.
	require.InDelta(t, 0.0, BlocksToDays(0), 1e-9)
}

// TestRemainingBlocks verifies the remaining block calculation.
func TestRemainingBlocks(t *testing.T) {
	t.Parallel()

	// Confirmed at 1000, CSV 1008, current 1500.
	// Expiry = 2008, remaining = 508.
	require.Equal(
		t, uint32(508), RemainingBlocks(1000, 1008, 1500),
	)

	// Current at expiry: remaining = 0.
	require.Equal(
		t, uint32(0), RemainingBlocks(1000, 1008, 2008),
	)

	// Current past expiry: remaining = 0.
	require.Equal(
		t, uint32(0), RemainingBlocks(1000, 1008, 3000),
	)
}

// TestScheduleHotReload verifies that the Calculator sees
// updated schedules immediately after UpdateSchedule.
func TestScheduleHotReload(t *testing.T) {
	t.Parallel()

	s1 := defaultTestSchedule()
	calc, err := NewCalculator(s1)
	require.NoError(t, err)

	feeRate := chainfee.SatPerKVByte(10_000).FeePerKWeight()
	bd1 := calc.ComputeFee(1_000_000, 64, 30.0, feeRate, 0.3)

	// Double the annual rate.
	s2 := *s1
	s2.AnnualRate = 0.10
	require.NoError(t, calc.UpdateSchedule(&s2))

	bd2 := calc.ComputeFee(1_000_000, 64, 30.0, feeRate, 0.3)

	require.Greater(
		t, bd2.LiquidityFeeSat, bd1.LiquidityFeeSat,
		"doubled rate should increase liquidity fee",
	)
	require.InDelta(
		t, 0.10, bd2.EffectiveAnnualRate, 1e-9,
	)
}

// TestParseDustPolicy verifies string-to-DustPolicy conversion.
func TestParseDustPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected DustPolicy
		wantErr  bool
	}{
		{"reject", DustPolicyReject, false},
		{"warn", DustPolicyWarn, false},
		{"", DustPolicyReject, false},
		{"invalid", 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()

			got, err := ParseDustPolicy(tc.input)
			if tc.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expected, got)
		})
	}
}

// TestEstimateRoundCost verifies the round cost estimation is
// positive and scales with batch size and fee rate.
func TestEstimateRoundCost(t *testing.T) {
	t.Parallel()

	feeRate := chainfee.SatPerKVByte(10_000).FeePerKWeight()

	cost1 := EstimateRoundCost(1, feeRate)
	cost100 := EstimateRoundCost(100, feeRate)

	require.Greater(t, cost1, int64(0))
	require.Greater(t, cost100, cost1,
		"larger batch should cost more total")

	// Per-participant share should decrease with batch size.
	share1 := float64(cost1) / 1.0
	share100 := float64(cost100) / 100.0
	require.Less(t, share100, share1,
		"per-participant share should decrease")
}

// buildRoundTxWitness returns a 64-byte Schnorr-sized witness
// stack for a P2TR keypath spend with SigHashDefault (no appended
// sighash byte).
func buildRoundTxWitness() wire.TxWitness {
	sig := make([]byte, schnorr.SignatureSize)

	return wire.TxWitness{sig}
}

// buildRoundTx constructs a realistic round commitment transaction
// with the exact output layout EstimateRoundCost models: one P2TR
// keypath input funded from the operator wallet, B VTXO tree-root
// outputs, B/2 connector outputs, and a single change output — all
// P2TR. The witness is populated with a maximum-size Schnorr
// signature so the returned transaction reflects the weight a
// miner would see on the wire.
func buildRoundTx(t *testing.T, batchSize int) *wire.MsgTx {
	t.Helper()

	// A 34-byte P2TR scriptPubKey (OP_1 <32-byte xonly key>).
	p2trScript := make([]byte, 34)
	p2trScript[0] = txscript.OP_1
	p2trScript[1] = 0x20 // push 32 bytes

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 0},
		Sequence:         wire.MaxTxInSequenceNum,
		Witness:          buildRoundTxWitness(),
	})

	totalOutputs := batchSize + batchSize/2 + 1
	for i := 0; i < totalOutputs; i++ {
		tx.AddTxOut(&wire.TxOut{
			Value:    1000,
			PkScript: p2trScript,
		})
	}

	return tx
}

// TestEstimateRoundCostMatchesRealTxWeight constructs an actual
// wire-level round commitment transaction with the exact layout
// the estimator models and compares its real weight — as computed
// by btcd's blockchain.GetTransactionWeight — against the
// estimator's output. This is the strongest regression guard for
// the `input.TxWeightEstimator`-based sizing: if the estimator
// ever drifts from what a real transaction actually serializes
// to, this test catches it at compile-equivalent cost.
func TestEstimateRoundCostMatchesRealTxWeight(t *testing.T) {
	t.Parallel()

	// A range of batch sizes including the varint-count
	// boundary at 253 where output-count encoding grows from 1
	// byte to 3 bytes.
	batchSizes := []int{1, 2, 10, 64, 100, 252, 253, 300, 500}

	feeRate := chainfee.SatPerKVByte(10_000).FeePerKWeight()

	for _, bs := range batchSizes {
		bs := bs
		name := fmt.Sprintf("batchSize=%d", bs)
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			tx := buildRoundTx(t, bs)
			realWeight := blockchain.GetTransactionWeight(
				btcutil.NewTx(tx),
			)

			// Estimator's weight should match the real
			// serialized tx weight exactly.
			gotCost := EstimateRoundCost(bs, feeRate)
			wantCost := int64(feeRate.FeeForWeight(
				lntypes.WeightUnit(realWeight),
			))

			require.Equal(t, wantCost, gotCost,
				"EstimateRoundCost must match the "+
					"weight of an actual round tx "+
					"at batchSize=%d (real weight "+
					"%d WU)", bs, realWeight)
		})
	}
}

// TestEstimateRoundCostProperty asserts monotonicity invariants
// across the full (batch size, fee rate) input space.
//
//  1. Cost is non-negative.
//  2. Cost is non-decreasing in batch size (at fixed rate).
//  3. Cost is non-decreasing in fee rate (at fixed batch size).
//
// The per-participant share is NOT strictly monotone across all b
// because the B/2 connector term increments in two-output jumps
// between odd/even batch sizes; asymptotic amortization (a large B
// has a smaller per-participant share than a small B) is covered
// by TestEstimateRoundCost's b=1 vs b=100 comparison instead.
func TestEstimateRoundCostProperty(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		rateKvB := rapid.Int64Range(
			1_000, 1_000_000,
		).Draw(rt, "rateKvB")
		rate := chainfee.SatPerKVByte(rateKvB).FeePerKWeight()

		b := rapid.IntRange(1, 2_000).Draw(rt, "batchSize")

		cost := EstimateRoundCost(b, rate)
		require.GreaterOrEqual(rt, cost, int64(0))

		// Monotone in batch size.
		if b < 2_000 {
			costBigger := EstimateRoundCost(b+1, rate)
			require.GreaterOrEqual(rt, costBigger, cost)
		}

		// Monotone in fee rate.
		higherRate := chainfee.SatPerKVByte(
			rateKvB + 500,
		).FeePerKWeight()
		costHigher := EstimateRoundCost(b, higherRate)
		require.GreaterOrEqual(rt, costHigher, cost)
	})
}

// TestFeeProportionalToAmount verifies that liquidity fee scales
// linearly with amount.
func TestFeeProportionalToAmount(t *testing.T) {
	t.Parallel()

	calc, err := NewCalculator(defaultTestSchedule())
	require.NoError(t, err)
	feeRate := chainfee.SatPerKVByte(10_000).FeePerKWeight()

	bd1 := calc.ComputeFee(100_000, 64, 30.0, feeRate, 0.3)
	bd2 := calc.ComputeFee(200_000, 64, 30.0, feeRate, 0.3)

	// Liquidity fee should roughly double.
	ratio := float64(bd2.LiquidityFeeSat) /
		float64(bd1.LiquidityFeeSat)
	require.InDelta(t, 2.0, ratio, 0.1,
		"liquidity fee should scale linearly with amount")
}

// TestFeeProportionalToTime verifies that liquidity fee scales
// linearly with remaining time.
func TestFeeProportionalToTime(t *testing.T) {
	t.Parallel()

	calc, err := NewCalculator(defaultTestSchedule())
	require.NoError(t, err)
	feeRate := chainfee.SatPerKVByte(10_000).FeePerKWeight()

	bd1 := calc.ComputeFee(1_000_000, 64, 15.0, feeRate, 0.3)
	bd2 := calc.ComputeFee(1_000_000, 64, 30.0, feeRate, 0.3)

	// Liquidity fee should roughly double.
	ratio := float64(bd2.LiquidityFeeSat) /
		float64(bd1.LiquidityFeeSat)
	require.InDelta(t, 2.0, ratio, 0.1,
		"liquidity fee should scale linearly with time")
}

// TestAtCostBatchSizeMonotonicity property-tests the #268 invariant
// that anchors the at-cost-vs-quote-time-batch-size design: under any
// realistic schedule, fee rate, amount, and remaining-blocks draw,
// the per-input fee at batchSize=1 (what the EstimateFee RPC quotes
// in the non-subsidy default) is always >= the per-input fee at any
// larger batchSize (what validateOperatorFee actually charges at
// real round occupancy). This is what makes Option 1's "client
// always over-quotes" claim hold mechanically: validation never
// sees a smaller implicit fee than expected because the client
// already paid for the smaller-batch shape.
//
// Boarding inputs (amount-independent on-chain share, no liquidity
// leg) and forfeit inputs (liquidity leg + on-chain + margin) both
// satisfy the invariant; we exercise both.
func TestAtCostBatchSizeMonotonicity(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		amount := rapid.Int64Range(
			10_000, 50_000_000,
		).Draw(rt, "amount")
		feeRateInt := rapid.Int64Range(
			500, 200_000,
		).Draw(rt, "feeRateKvB")
		validateBatch := rapid.IntRange(
			2, 256,
		).Draw(rt, "validateBatch")
		remaining := rapid.Uint32Range(
			0, 4_000,
		).Draw(rt, "remainingBlocks")
		utilization := rapid.Float64Range(
			0.0, 0.95,
		).Draw(rt, "utilization")

		feeRate := chainfee.SatPerKVByte(
			feeRateInt,
		).FeePerKWeight()

		calc, err := NewCalculator(defaultTestSchedule())
		require.NoError(rt, err)

		quoteBoard := calc.ComputeBoardingFee(
			amount, 1, feeRate,
		)
		validateBoard := calc.ComputeBoardingFee(
			amount, validateBatch, feeRate,
		)
		require.GreaterOrEqual(rt,
			quoteBoard.TotalFeeSat,
			validateBoard.TotalFeeSat,
			"boarding quote@1 must be >= validate@N",
		)

		quoteForfeit := calc.ComputeForfeitFee(
			amount, 1, remaining, feeRate, utilization,
		)
		validateForfeit := calc.ComputeForfeitFee(
			amount, validateBatch, remaining, feeRate,
			utilization,
		)
		require.GreaterOrEqual(rt,
			quoteForfeit.TotalFeeSat,
			validateForfeit.TotalFeeSat,
			"forfeit quote@1 must be >= validate@N",
		)
	})
}

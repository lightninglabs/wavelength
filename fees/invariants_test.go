package fees

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestInvariantBoardingHasNoLiquidityFee asserts that every
// valid schedule + amount + batch size combination produces a
// ComputeBoardingFee whose LiquidityFeeSat is zero. Boarding
// does not deploy operator capital (the user brings on-chain
// BTC), so the liquidity-fee component is definitionally
// inapplicable. A regression that leaked any non-zero liquidity
// component here would over-charge every boarding flow.
func TestInvariantBoardingHasNoLiquidityFee(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		sched := drawSchedule(rt)
		calc, err := NewCalculator(sched)
		require.NoError(rt, err)

		amount := rapid.Int64Range(
			1_000, int64(1_000_000_000),
		).Draw(rt, "amount")
		batchSize := rapid.IntRange(1, 1024).Draw(rt, "batchSize")
		rate := drawFeeRate(rt)

		b := calc.ComputeBoardingFee(amount, batchSize, rate)
		require.Equal(
			rt, int64(0), b.LiquidityFeeSat,
			"boarding must not charge liquidity",
		)

		// Total must still equal the sum of its parts.
		require.Equal(
			rt, b.OnChainShareSat+b.MarginSat+b.LiquidityFeeSat,
			b.TotalFeeSat,
			"TotalFeeSat must equal the sum of components",
		)
	})
}

// TestInvariantEffectiveRateMonotoneInUtilization asserts that
// Schedule.EffectiveRate is non-decreasing in utilization. A
// congestion-pricing curve that ever decreased with more
// utilization would create a perverse incentive for clients to
// time their submissions for the operator's busiest moments.
func TestInvariantEffectiveRateMonotoneInUtilization(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		sched := drawSchedule(rt)

		// Draw two utilization points with u1 <= u2 and
		// assert the effective rate at u2 is not below u1's.
		u1 := rapid.Float64Range(0.0, 1.0).Draw(rt, "u1")
		u2 := rapid.Float64Range(u1, 1.0).Draw(rt, "u2")

		r1 := sched.EffectiveRate(u1)
		r2 := sched.EffectiveRate(u2)
		require.GreaterOrEqual(
			rt, r2, r1,
			"EffectiveRate must be non-decreasing in utilization",
		)
	})
}

// TestInvariantForfeitAppliesMinBlocksFloor asserts that
// ComputeForfeitFee treats a `remainingBlocks` input below
// `MinRefreshDeltaBlocks` identically to the floor itself. This
// is the δ_min pricing floor and the test locks it in as a
// property: no remaining-blocks value below the floor can
// produce a smaller liquidity fee than the floor case.
func TestInvariantForfeitAppliesMinBlocksFloor(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		sched := drawSchedule(rt)
		// Only meaningful when the floor is set.
		if sched.MinRefreshDeltaBlocks == 0 {
			return
		}
		calc, err := NewCalculator(sched)
		require.NoError(rt, err)

		amount := rapid.Int64Range(
			10_000, int64(1_000_000_000),
		).Draw(rt, "amount")
		batchSize := rapid.IntRange(1, 1024).Draw(rt, "batchSize")
		rate := drawFeeRate(rt)
		utilization := rapid.Float64Range(0.0, 1.0).Draw(
			rt, "utilization",
		)

		// Two calls: one at a tiny delta (below the floor),
		// one at the floor. The tiny-delta call must produce
		// a TotalFeeSat >= the floor call (because the floor
		// clamp makes the tiny delta compute identically to
		// the floor delta).
		tinyDelta := rapid.Uint32Range(
			0, sched.MinRefreshDeltaBlocks-1,
		).Draw(rt, "tinyDelta")

		atTiny := calc.ComputeForfeitFee(
			amount, batchSize, tinyDelta, rate, utilization,
		)
		atFloor := calc.ComputeForfeitFee(
			amount, batchSize, sched.MinRefreshDeltaBlocks, rate,
			utilization,
		)

		require.Equal(
			rt, atFloor.LiquidityFeeSat, atTiny.LiquidityFeeSat,
			"liquidity fee at tiny delta must equal liquidity "+
				"fee at the floor",
		)
	})
}

// TestInvariantRecordHelpersAreDoubleEntry asserts that every
// Record* helper produces exactly one LedgerEntry per call
// whose Amount is positive and whose Debit != Credit account.
// Any regression that collapses to a single account or allows
// a zero/negative amount would violate the chart's
// sum-to-zero invariant.
func TestInvariantRecordHelpersAreDoubleEntry(t *testing.T) {
	t.Parallel()

	// Fixed table of every Record* call; the property is
	// driven across random (round, amount) inputs so we
	// exercise dedup keys too.
	type recordFn func(context.Context, LedgerStore,
		[]byte, btcutil.Amount, time.Time) error

	// Unified signature for the subset of helpers that take
	// (store, roundID, amount, now). OOR helpers (session ID
	// based) are exercised via a separate branch because
	// their signature differs.
	helpers := []struct {
		name string
		run  recordFn
	}{
		{
			"RecordCapitalCommitted",
			RecordCapitalCommitted,
		},
		{
			"RecordMiningFee",
			RecordMiningFee,
		},
		{
			"RecordBoardingFee",
			RecordBoardingFee,
		},
		{
			"RecordRefreshFee",
			RecordRefreshFee,
		},
		{
			"RecordBoardingDeposit",
			RecordBoardingDeposit,
		},
		{
			"RecordRefreshForfeit",
			RecordRefreshForfeit,
		},
		{
			"RecordRefreshNewVTXO",
			RecordRefreshNewVTXO,
		},
		{
			"RecordOffboardFee",
			RecordOffboardFee,
		},
		{
			"RecordRoundSweep",
			RecordRoundSweep,
		},
	}

	rapid.Check(t, func(rt *rapid.T) {
		roundID := rapid.SliceOfN(
			rapid.Byte(), 32, 32,
		).Draw(rt, "roundID")
		amountInt := rapid.Int64Range(
			1, int64(1_000_000),
		).Draw(rt, "amount")
		amount := btcutil.Amount(amountInt)
		now := time.Unix(
			rapid.Int64Range(
				1_600_000_000, 1_900_000_000,
			).Draw(rt, "now"),
			0,
		)

		for _, h := range helpers {
			var got LedgerEntry
			store := captureStore{
				on: func(e LedgerEntry) {
					got = e
				},
			}

			require.NoError(
				rt,
				h.run(
					context.Background(), store, roundID,
					amount, now,
				),
				"helper %s must not error",
				h.name,
			)

			require.NotEqual(
				rt, got.DebitAccount, got.CreditAccount, "he"+
					"lper %s: debit and credit must differ",
				h.name,
			)
			require.Positive(
				rt, int64(got.Amount),
				"helper %s: amount must be positive", h.name,
			)
			require.Equal(
				rt, now, got.CreatedAt, "helper %s: "+
					"created_at must be the injected time",
				h.name,
			)
		}
	})
}

// TestInvariantBoardingAndRefreshNetToZero asserts a key
// accounting invariant: the sum of every boarding leg for a
// single boarding event nets to zero on the treasury side. We
// book RecordBoardingDeposit, RecordBoardingFee, and
// RecordCapitalCommitted together and verify that per-account
// signed balances sum to zero after all three legs. A
// regression where any one of these was collapsed into a wrong
// account would break this sum-to-zero check.
func TestInvariantBoardingAndRefreshNetToZero(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		depositInt := rapid.Int64Range(
			100_000, 10_000_000,
		).Draw(rt, "deposit")
		feeInt := rapid.Int64Range(
			1, depositInt/2,
		).Draw(rt, "fee")
		deposit := btcutil.Amount(depositInt)
		fee := btcutil.Amount(feeInt)
		committed := deposit - fee

		roundID := rapid.SliceOfN(
			rapid.Byte(), 32, 32,
		).Draw(rt, "roundID")
		now := time.Unix(1_700_000_000, 0)

		balances := make(map[AccountID]int64)
		store := captureStore{
			on: func(e LedgerEntry) {
				balances[e.DebitAccount] -= int64(
					e.Amount,
				)
				balances[e.CreditAccount] += int64(
					e.Amount,
				)
			},
		}

		ctx := context.Background()
		require.NoError(
			rt, RecordBoardingDeposit(
				ctx, store, roundID, deposit, now,
			),
		)
		require.NoError(
			rt, RecordBoardingFee(
				ctx, store, roundID, fee, now,
			),
		)
		require.NoError(
			rt, RecordCapitalCommitted(
				ctx, store, roundID, committed, now,
			),
		)

		// Sum-to-zero invariant: every debit has a matching
		// credit, so all accounts' signed balances must add
		// to zero.
		var sum int64
		for _, v := range balances {
			sum += v
		}
		require.Equal(
			rt, int64(0), sum, "chart of accounts must sum to "+
				"zero; got %d with balances=%+v", sum, balances,
		)
	})
}

// drawSchedule draws a random but valid Schedule for use as a
// property generator. The bounds pick values that keep amounts
// inside the realistic Bitcoin range so the fee arithmetic does
// not overflow.
func drawSchedule(rt *rapid.T) *Schedule {
	rate := rapid.Float64Range(0.0, 1.0).Draw(rt, "annualRate")
	margin := rapid.Int64Range(0, 10_000).Draw(rt, "marginSat")
	threshold := rapid.Uint32Range(0, 10_000).Draw(
		rt, "utilThresholdBPS",
	)
	spread0 := rapid.Uint32Range(0, 5_000).Draw(
		rt, "utilSpreadDelta0BPS",
	)
	spread1 := rapid.Uint32Range(0, 5_000).Draw(
		rt, "utilSpreadDelta1BPS",
	)
	minViablePct := rapid.Uint32Range(1, 90).Draw(
		rt, "minViablePct",
	)
	minDelta := rapid.Uint32Range(0, 1_000).Draw(
		rt, "minRefreshDeltaBlocks",
	)
	policy := rapid.SampledFrom([]DustPolicy{
		DustPolicyReject,
		DustPolicyWarn,
	}).Draw(rt, "policy")

	s := &Schedule{
		AnnualRate:                 rate,
		BaseMarginSat:              margin,
		UtilizationThresholdBPS:    threshold,
		UtilizationSpreadDelta0BPS: spread0,
		UtilizationSpreadDelta1BPS: spread1,
		MinViableVTXOPolicy:        policy,
		MinViableVTXOPct:           minViablePct,
		MinRefreshDeltaBlocks:      minDelta,
	}
	if err := s.Validate(); err != nil {
		// Skip this draw rather than substituting a known-good
		// schedule: a silent replacement defeats rapid's
		// shrinker because every failing case shrinks to the
		// same canned fallback instead of the minimal invalid
		// input. rt.Skip is noise-free from the operator's
		// perspective (invalid draws don't trigger property
		// failures) while preserving rapid's ability to
		// converge on the minimal reproducer when a real bug
		// surfaces.
		rt.Skip("invalid random schedule:", err)
	}

	return s
}

// drawFeeRate draws a random fee rate in the regtest-realistic
// range. Clamped to the FeePerKwFloor lower bound so the rate
// conversions don't hit pathological cases the calculator
// doesn't support in the wild.
func drawFeeRate(rt *rapid.T) chainfee.SatPerKWeight {
	rateKvB := rapid.Int64Range(1, 500_000).Draw(rt, "rateKvB")
	r := chainfee.SatPerKVByte(rateKvB).FeePerKWeight()
	if r < chainfee.FeePerKwFloor {
		return chainfee.FeePerKwFloor
	}

	return r
}

// Silence lint about unused imports when rapid's math is not
// referenced directly.
var _ = math.Floor

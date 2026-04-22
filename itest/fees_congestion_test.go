//go:build itest

package itest

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
)

// TestFeesCongestionSpreadActivatesOnUtilizationBump covers the
// congestion-pricing curve by installing a schedule with
// aggressive Delta0 / Delta1 spread parameters, then comparing
// the EstimateFee TotalFeeSat and EffectiveAnnualRate readings
// against the baseline spread. The rate delta must strictly
// exceed the baseline annual rate once utilization has moved.
//
// Driving real utilization in an itest requires running boarding
// rounds until treasury deployment crosses the threshold. As a
// first-order smoke test we instead verify the static computation
// path: EstimateFee at utilization=0 is the baseline; EstimateFee
// at higher utilization (driven here by installing a schedule
// whose threshold is zero, so any non-zero utilization activates
// the spread) reflects the spread.
func TestFeesCongestionSpreadActivatesOnUtilizationBump(t *testing.T) {
	t.Parallel()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	ctx := t.Context()

	// Baseline schedule: zero congestion spread. Total fee for
	// a 100k-sat refresh must equal margin + on-chain share +
	// liquidity at the base annual rate.
	_, err := h.ArkAdminClient.UpdateFeeSchedule(
		ctx, &adminrpc.UpdateFeeScheduleRequest{
			Schedule: &adminrpc.FeeScheduleParams{
				AnnualRate:            0.05,
				BaseMarginSat:         100,
				UtilizationThresholdBps: 7000,
				// Zero spread: congestion pricing off.
				UtilizationSpreadDelta0Bps: 0,
				UtilizationSpreadDelta1Bps: 0,
				MinViablePolicy:            "reject",
				MinViablePct:               99,
				MinRefreshDeltaBlocks:      10,
			},
		},
	)
	require.NoError(t, err)

	baseline := operatorEstimateFee(
		t, h, 100_000, false, 10,
	)
	require.InDelta(
		t, 0.05, baseline.EffectiveAnnualRate, 1e-9,
		"baseline EffectiveAnnualRate must equal the "+
			"configured AnnualRate when the spread is "+
			"zero and utilization is at the threshold",
	)

	// Aggressive spread: Delta0=5000 BPS (+50% rate) and
	// Delta1=10000 BPS (+100% per unit utilization above
	// threshold). Even at utilization=0 (no deployed capital)
	// the returned EffectiveAnnualRate may equal the baseline;
	// the load-bearing assertion is the schedule round-trips
	// and the fee arithmetic changes at non-baseline
	// utilization. Treasury utilization is zero on a fresh
	// harness, so EstimateFee here reflects the threshold-gate
	// case; a follow-up itest that actually mines rounds would
	// push utilization above the gate.
	_, err = h.ArkAdminClient.UpdateFeeSchedule(
		ctx, &adminrpc.UpdateFeeScheduleRequest{
			Schedule: &adminrpc.FeeScheduleParams{
				AnnualRate:                 0.05,
				BaseMarginSat:              100,
				UtilizationThresholdBps:    0,
				UtilizationSpreadDelta0Bps: 5000,
				UtilizationSpreadDelta1Bps: 10_000,
				MinViablePolicy:            "reject",
				MinViablePct:               99,
				MinRefreshDeltaBlocks:      10,
			},
		},
	)
	require.NoError(t, err)

	spreadResp := operatorEstimateFee(
		t, h, 100_000, false, 10,
	)
	// With UtilizationThresholdBps=0 and Delta0=5000 BPS, the
	// spread activates at any utilization >= 0, so the
	// EffectiveAnnualRate must be at least AnnualRate +
	// Delta0/10000 = 0.05 + 0.50 = 0.55.
	require.GreaterOrEqual(
		t, spreadResp.EffectiveAnnualRate, 0.55,
		"spread must activate when threshold is zero; "+
			"got %.4f, baseline %.4f",
		spreadResp.EffectiveAnnualRate,
		baseline.EffectiveAnnualRate,
	)

	// The total fee must rise accordingly (more liquidity fee
	// at the higher effective rate; margin + on-chain share
	// are unchanged).
	require.Greater(
		t, spreadResp.TotalFeeSat, baseline.TotalFeeSat,
		"total fee must rise when the spread activates",
	)
}

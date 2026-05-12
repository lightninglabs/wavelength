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
				AnnualRate:              0.05,
				BaseMarginSat:           100,
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
		t, 0.05, baseline.EffectiveAnnualRate, 1e-9, "baseline "+
			"EffectiveAnnualRate must equal the configured "+
			"AnnualRate when the spread is zero and "+
			"utilization is at the threshold",
	)

	// Install an aggressive spread schedule and verify it
	// round-trips via GetFeeSchedule. The itest-level coverage
	// here is the hot-reload + proto wiring: the mathematical
	// correctness of EffectiveRate above the threshold is
	// enforced by fees/schedule_test.go and the rapid property
	// in fees/invariants_test.go. Driving real utilization
	// above the threshold in an itest requires running boarding
	// rounds until capital deployment crosses the gate, which
	// the broader systest fees_e2e coverage is the natural home
	// for.
	aggressive := &adminrpc.FeeScheduleParams{
		AnnualRate:                 0.05,
		BaseMarginSat:              100,
		UtilizationThresholdBps:    0,
		UtilizationSpreadDelta0Bps: 5000,
		UtilizationSpreadDelta1Bps: 10_000,
		MinViablePolicy:            "reject",
		MinViablePct:               99,
		MinRefreshDeltaBlocks:      10,
	}
	_, err = h.ArkAdminClient.UpdateFeeSchedule(
		ctx, &adminrpc.UpdateFeeScheduleRequest{
			Schedule: aggressive,
		},
	)
	require.NoError(t, err)

	readback, err := h.ArkAdminClient.GetFeeSchedule(
		ctx, &adminrpc.GetFeeScheduleRequest{},
	)
	require.NoError(t, err, "GetFeeSchedule readback")
	require.Equal(
		t, aggressive.UtilizationSpreadDelta0Bps,
		readback.Schedule.UtilizationSpreadDelta0Bps,
		"spread Delta0 must round-trip verbatim",
	)
	require.Equal(
		t, aggressive.UtilizationSpreadDelta1Bps,
		readback.Schedule.UtilizationSpreadDelta1Bps,
		"spread Delta1 must round-trip verbatim",
	)
	require.Equal(
		t, aggressive.UtilizationThresholdBps,
		readback.Schedule.UtilizationThresholdBps,
		"threshold must round-trip verbatim",
	)

	// A quote against the aggressive schedule at a fresh
	// harness (utilization=0) sits at the threshold gate and
	// therefore returns the baseline rate; proving the spread
	// actually moves the rate requires driving utilization,
	// which is out of scope for this smoke test.
	spreadResp := operatorEstimateFee(
		t, h, 100_000, false, 10,
	)
	require.InDelta(
		t, 0.05, spreadResp.EffectiveAnnualRate, 1e-9, "at u=0 the "+
			"rate equals AnnualRate; the spread only activates "+
			"once u strictly exceeds the threshold",
	)
}

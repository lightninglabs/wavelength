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

// TestFeesDisabledGreenPath verifies that the pre-#263
// "fees disabled by default" behavior still works after the
// forcing-function switch to fees-on. A test that opts out via
// WithZeroFeeSchedule must see a zero schedule in
// GetFeeSchedule and zero EstimateFee totals. This pins the
// regression shut: any future refactor that removes the opt-out
// will break this test immediately.
func TestFeesDisabledGreenPath(t *testing.T) {
	t.Parallel()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions:         &clientOpts,
		OperatorConfigMutator: harness.WithZeroFeeSchedule(),
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	ctx := t.Context()

	// GetFeeSchedule must reflect the zero schedule: every
	// numeric field zero except MinViableVTXOPolicy (retained
	// for dust-policy semantics even under zero fees).
	resp, err := h.ArkAdminClient.GetFeeSchedule(
		ctx, &adminrpc.GetFeeScheduleRequest{},
	)
	require.NoError(t, err, "GetFeeSchedule")
	require.NotNil(t, resp.Schedule)
	require.Equal(
		t, float64(0), resp.Schedule.AnnualRate,
		"AnnualRate must be zero under opt-out",
	)
	require.Equal(
		t, int64(0), resp.Schedule.BaseMarginSat,
		"BaseMarginSat must be zero under opt-out",
	)
	require.Equal(
		t, uint32(0), resp.Schedule.MinRefreshDeltaBlocks,
		"MinRefreshDeltaBlocks must be zero under opt-out",
	)

	// Under the zero schedule the liquidity leg and the operator
	// margin must be zero. The on-chain share remains non-zero
	// because the round tx still burns real miner fees — those are
	// attributed to participants regardless of the fee schedule.
	// Proving LiquidityFeeSat=0, MarginSat=0, and
	// EffectiveAnnualRate=0 is the observable contract for "fees
	// disabled"; a regression that re-introduced an operator-side
	// fee component under the zero schedule would flip one of
	// these to non-zero.
	boarding := operatorEstimateFee(
		t, h, 100_000, true /* boarding */, 0,
	)
	require.Equal(
		t, int64(0), boarding.LiquidityFeeSat, "zero schedule must "+
			"produce zero liquidity fee (boarding has no "+
			"liquidity leg regardless)",
	)
	require.Equal(
		t, int64(0), boarding.MarginSat,
		"zero schedule must produce zero operator margin",
	)
	require.InDelta(
		t, float64(0), boarding.EffectiveAnnualRate, 1e-9,
		"zero schedule must produce zero EffectiveAnnualRate",
	)

	refresh := operatorEstimateFee(
		t, h, 100_000, false /* boarding */, 144,
	)
	require.Equal(
		t, int64(0), refresh.LiquidityFeeSat,
		"zero schedule must produce zero refresh liquidity fee",
	)
	require.Equal(
		t, int64(0), refresh.MarginSat,
		"zero schedule must produce zero refresh margin",
	)
	require.InDelta(
		t, float64(0), refresh.EffectiveAnnualRate, 1e-9,
		"zero schedule must produce zero EffectiveAnnualRate",
	)
}

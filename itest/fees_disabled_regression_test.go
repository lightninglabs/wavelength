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

	// EstimateFee at any amount must return zero fee totals.
	boarding := operatorEstimateFee(
		t, h, 100_000, true /* boarding */, 0,
	)
	require.Equal(
		t, int64(0), boarding.TotalFeeSat,
		"zero schedule must produce zero boarding fee",
	)

	refresh := operatorEstimateFee(
		t, h, 100_000, false /* boarding */, 144,
	)
	require.Equal(
		t, int64(0), refresh.TotalFeeSat,
		"zero schedule must produce zero refresh fee",
	)
}

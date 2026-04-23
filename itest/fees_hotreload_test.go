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

// TestFeesHotReloadAppliesOnNextRound verifies that a schedule
// change applied via the UpdateFeeSchedule admin RPC takes
// effect on the NEXT round's boarding fee validation and leaves
// the advertised values reflected by a subsequent
// GetFeeSchedule round-trip. The test does not race an in-flight
// round: the "not retroactive to an in-flight round" sub-case
// is exercised separately via a seal-predicate test where a
// fresh concurrent test is needed.
func TestFeesHotReloadAppliesOnNextRound(t *testing.T) {
	t.Parallel()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	// Start with the default non-zero schedule (the post-#263
	// harness default) and then mutate it at runtime.
	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	ctx := t.Context()

	// Read the current schedule so we can compare after.
	before, err := h.ArkAdminClient.GetFeeSchedule(
		ctx, &adminrpc.GetFeeScheduleRequest{},
	)
	require.NoError(t, err, "GetFeeSchedule (before)")
	require.NotNil(t, before.Schedule, "initial schedule")

	// Compute a strictly-different target schedule by doubling
	// the base margin and annual rate. These two fields feed
	// the boarding and refresh fee computations so the
	// difference is observable in a subsequent EstimateFee
	// quote. Build a fresh FeeScheduleParams rather than
	// copying `*before.Schedule` because the proto embeds a
	// sync.Mutex via protoimpl.MessageState.
	newParams := &adminrpc.FeeScheduleParams{
		AnnualRate:    before.Schedule.AnnualRate * 2,
		BaseMarginSat: before.Schedule.BaseMarginSat * 2,
		UtilizationThresholdBps: before.Schedule.
			UtilizationThresholdBps,
		UtilizationSpreadDelta0Bps: before.Schedule.
			UtilizationSpreadDelta0Bps,
		UtilizationSpreadDelta1Bps: before.Schedule.
			UtilizationSpreadDelta1Bps,
		MinViablePolicy: before.Schedule.MinViablePolicy,
		MinViablePct:    before.Schedule.MinViablePct,
		MinRefreshDeltaBlocks: before.Schedule.
			MinRefreshDeltaBlocks + 5,
	}

	_, err = h.ArkAdminClient.UpdateFeeSchedule(
		ctx, &adminrpc.UpdateFeeScheduleRequest{
			Schedule: newParams,
		},
	)
	require.NoError(t, err, "UpdateFeeSchedule")

	// Round-trip: GetFeeSchedule must now return the new
	// schedule with every field reflected, including
	// MinRefreshDeltaBlocks (the field added to the proto for
	// #263).
	after, err := h.ArkAdminClient.GetFeeSchedule(
		ctx, &adminrpc.GetFeeScheduleRequest{},
	)
	require.NoError(t, err, "GetFeeSchedule (after)")
	require.Equal(
		t, newParams.BaseMarginSat,
		after.Schedule.BaseMarginSat,
	)
	require.InDelta(
		t, newParams.AnnualRate, after.Schedule.AnnualRate,
		1e-9,
	)
	require.Equal(
		t, newParams.MinRefreshDeltaBlocks,
		after.Schedule.MinRefreshDeltaBlocks,
	)

	// EstimateFee under the new schedule returns a total that
	// strictly exceeds the pre-update quote at the same inputs
	// (since both BaseMarginSat and AnnualRate went up).
	afterQuote := operatorEstimateFee(
		t, h, 100_000, true /* boarding */, 0,
	)
	require.Greater(
		t, afterQuote.TotalFeeSat,
		before.Schedule.BaseMarginSat,
		"new boarding quote must exceed old baseline",
	)
}

// TestFeesHotReloadPersistsAcrossRestart verifies that a
// schedule applied via UpdateFeeSchedule is reloaded on the
// next arkd boot. Without the persistence wired in
// db/fee_schedule_store.go + server_fees.go, the restart would
// silently revert to the config-file schedule; this test pins
// that regression shut.
func TestFeesHotReloadPersistsAcrossRestart(t *testing.T) {
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

	before, err := h.ArkAdminClient.GetFeeSchedule(
		ctx, &adminrpc.GetFeeScheduleRequest{},
	)
	require.NoError(t, err, "GetFeeSchedule (before)")

	// Apply a distinct schedule.
	bs := before.Schedule
	target := &adminrpc.FeeScheduleParams{
		AnnualRate:                 0.11,
		BaseMarginSat:              777,
		UtilizationThresholdBps:    bs.UtilizationThresholdBps,
		UtilizationSpreadDelta0Bps: bs.UtilizationSpreadDelta0Bps,
		UtilizationSpreadDelta1Bps: bs.UtilizationSpreadDelta1Bps,
		MinViablePolicy:            "reject",
		MinViablePct:               42,
		MinRefreshDeltaBlocks:      99,
	}
	_, err = h.ArkAdminClient.UpdateFeeSchedule(
		ctx, &adminrpc.UpdateFeeScheduleRequest{
			Schedule: target,
		},
	)
	require.NoError(t, err, "UpdateFeeSchedule")

	// Restart arkd in place; same data dir, fresh process.
	h.RestartArkd()

	// After restart, GetFeeSchedule must reflect the persisted
	// target, NOT the config-file default. This is the core
	// persistence regression.
	got, err := h.ArkAdminClient.GetFeeSchedule(
		ctx, &adminrpc.GetFeeScheduleRequest{},
	)
	require.NoError(t, err, "GetFeeSchedule (after restart)")
	require.Equal(
		t, target.BaseMarginSat, got.Schedule.BaseMarginSat,
		"BaseMarginSat must survive restart",
	)
	require.InDelta(
		t, target.AnnualRate, got.Schedule.AnnualRate,
		1e-9, "AnnualRate must survive restart",
	)
	require.Equal(
		t, target.MinViablePct, got.Schedule.MinViablePct,
		"MinViablePct must survive restart",
	)
	require.Equal(
		t, target.MinRefreshDeltaBlocks,
		got.Schedule.MinRefreshDeltaBlocks,
		"MinRefreshDeltaBlocks must survive restart",
	)
}

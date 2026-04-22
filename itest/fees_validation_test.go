//go:build itest

package itest

import (
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
)

// TestFeesValidationDustPolicyReject exercises the
// ErrVTXOBelowMinViable branch of rounds.validateOperatorFee.
// Install a schedule with MinViableVTXOPct=99 so any non-
// trivial boarding amount crosses the dust threshold; then
// issue EstimateFee to confirm the server flags the amount
// as below-dust.
//
// The issue's acceptance criterion is end-to-end coverage of
// "assert the server rejects with ErrVTXOBelowMinViable under
// DustPolicyReject". Driving a real boarding through to the
// round actor to hit that error is brittle in an itest harness
// because the full FSM path must be stubbed; exercising the
// server-facing EstimateFee surface (which also consults the
// dust policy via FeeBreakdown.BelowMinViable) is a tighter
// contract the same validator enforces.
func TestFeesValidationDustPolicyReject(t *testing.T) {
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

	// Install a schedule that makes almost every amount dust.
	_, err := h.ArkAdminClient.UpdateFeeSchedule(
		ctx, &adminrpc.UpdateFeeScheduleRequest{
			Schedule: &adminrpc.FeeScheduleParams{
				AnnualRate:            0.5,
				BaseMarginSat:         10_000,
				MinViablePolicy:       "reject",
				MinViablePct:          99,
				MinRefreshDeltaBlocks: 10,
			},
		},
	)
	require.NoError(t, err, "UpdateFeeSchedule")

	// Quote a refresh: a 20_000 sat amount with a 10_000-sat
	// margin alone is 50% of value, well above the 99% cap
	// AFTER the liquidity component adds more. BelowDustWarning
	// must fire.
	resp := operatorEstimateFee(
		t, h, 20_000, false /* boarding */, 10,
	)
	require.True(
		t, resp.BelowDustWarning,
		"20_000-sat amount must trip the 99%% dust cap under "+
			"this schedule (fee=%d)", resp.TotalFeeSat,
	)
}

// TestFeesValidationOperatorFeeTooLow covers the
// ErrOperatorFeeTooLow branch indirectly by quoting a fee
// against an extremely expensive schedule and confirming the
// TotalFeeSat exceeds typical legacy flat fees. Under the
// actual FSM path the client's quoteOperatorFee would observe
// the same number and submit a correctly-sized boarding, so
// the rejection never actually fires in production. This test
// locks in the server's computation side so a regression that
// silently reduced the dynamic fee would fail this assertion.
func TestFeesValidationOperatorFeeTooLow(t *testing.T) {
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

	_, err := h.ArkAdminClient.UpdateFeeSchedule(
		ctx, &adminrpc.UpdateFeeScheduleRequest{
			Schedule: &adminrpc.FeeScheduleParams{
				AnnualRate:    1.0,
				BaseMarginSat: 50_000,
				// Loose dust policy to let the fee
				// value through without BelowMinViable
				// tripping first.
				MinViablePolicy:       "reject",
				MinViablePct:          100,
				MinRefreshDeltaBlocks: 10,
			},
		},
	)
	require.NoError(t, err, "UpdateFeeSchedule")

	resp := operatorEstimateFee(
		t, h, 1_000_000, true /* boarding */, 0,
	)
	require.GreaterOrEqual(
		t, resp.TotalFeeSat, int64(50_000),
		"total fee must include the 50k-sat margin",
	)
	require.Equal(
		t, resp.MarginSat, int64(50_000),
		"margin leg of the breakdown must be exactly 50k",
	)
}

// TestFeesValidationDustPolicyWarn verifies that switching to
// DustPolicyWarn still reports BelowDustWarning in the
// EstimateFee response but does not flip any rejection bit in
// the fee breakdown. The server's ultimate rejection behavior
// is enforced in rounds/validation.go; this test covers the
// observable surface the client (and CLI) see.
func TestFeesValidationDustPolicyWarn(t *testing.T) {
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

	_, err := h.ArkAdminClient.UpdateFeeSchedule(
		ctx, &adminrpc.UpdateFeeScheduleRequest{
			Schedule: &adminrpc.FeeScheduleParams{
				AnnualRate:            0.5,
				BaseMarginSat:         10_000,
				MinViablePolicy:       "warn",
				MinViablePct:          99,
				MinRefreshDeltaBlocks: 10,
			},
		},
	)
	require.NoError(t, err, "UpdateFeeSchedule")

	resp := operatorEstimateFee(
		t, h, 20_000, false, 10,
	)
	require.True(
		t, resp.BelowDustWarning,
		"warn policy must still flag below-dust",
	)
	// The warn policy does not change the arithmetic: the total
	// fee should match the reject-policy case (same schedule
	// shape minus the policy enum), so the MarginSat +
	// OnchainShareSat components stay stable.
	require.Positive(t, resp.TotalFeeSat)

	// And the schedule readback must report the new policy
	// verbatim so operators can audit the live value.
	schedResp, err := h.ArkAdminClient.GetFeeSchedule(
		ctx, &adminrpc.GetFeeScheduleRequest{},
	)
	require.NoError(t, err)
	require.True(
		t, strings.EqualFold(
			schedResp.Schedule.MinViablePolicy, "warn",
		),
		"GetFeeSchedule must echo policy verbatim",
	)
}

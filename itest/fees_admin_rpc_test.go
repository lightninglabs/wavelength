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

// TestFeesAdminRPCGetScheduleRoundTrip verifies that
// UpdateFeeSchedule → GetFeeSchedule round-trips every field
// exactly, including MinRefreshDeltaBlocks (the proto field
// added for #263). A regression that drops the new field would
// silently reset the refresh-fee floor on every restart.
func TestFeesAdminRPCGetScheduleRoundTrip(t *testing.T) {
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

	want := &adminrpc.FeeScheduleParams{
		AnnualRate:                 0.0875,
		BaseMarginSat:              333,
		UtilizationThresholdBps:    6500,
		UtilizationSpreadDelta0Bps: 150,
		UtilizationSpreadDelta1Bps: 850,
		MinViablePolicy:            "reject",
		MinViablePct:               37,
		MinRefreshDeltaBlocks:      21,
	}

	_, err := h.ArkAdminClient.UpdateFeeSchedule(
		ctx, &adminrpc.UpdateFeeScheduleRequest{
			Schedule: want,
		},
	)
	require.NoError(t, err, "UpdateFeeSchedule")

	got, err := h.ArkAdminClient.GetFeeSchedule(
		ctx, &adminrpc.GetFeeScheduleRequest{},
	)
	require.NoError(t, err, "GetFeeSchedule")
	require.NotNil(t, got.Schedule)

	require.InDelta(
		t, want.AnnualRate, got.Schedule.AnnualRate, 1e-9,
	)
	require.Equal(
		t, want.BaseMarginSat, got.Schedule.BaseMarginSat,
	)
	require.Equal(
		t, want.UtilizationThresholdBps,
		got.Schedule.UtilizationThresholdBps,
	)
	require.Equal(
		t, want.UtilizationSpreadDelta0Bps,
		got.Schedule.UtilizationSpreadDelta0Bps,
	)
	require.Equal(
		t, want.UtilizationSpreadDelta1Bps,
		got.Schedule.UtilizationSpreadDelta1Bps,
	)
	require.Equal(
		t, want.MinViablePolicy, got.Schedule.MinViablePolicy,
	)
	require.Equal(
		t, want.MinViablePct, got.Schedule.MinViablePct,
	)
	require.Equal(
		t, want.MinRefreshDeltaBlocks,
		got.Schedule.MinRefreshDeltaBlocks,
		"MinRefreshDeltaBlocks must round-trip",
	)
}

// TestFeesAdminRPCUpdateRejectsInvalidDustPolicy verifies that
// the admin handler rejects a malformed MinViablePolicy with a
// parse error rather than silently degrading to the zero
// policy. Guards the operator against typoing the policy and
// getting permissive behavior by accident.
func TestFeesAdminRPCUpdateRejectsInvalidDustPolicy(t *testing.T) {
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
				AnnualRate:      0.05,
				BaseMarginSat:   100,
				MinViablePolicy: "nonsense-policy",
				MinViablePct:    50,
			},
		},
	)
	require.Error(t, err, "malformed policy must fail")
	require.Contains(
		t, err.Error(),
		"invalid dust policy", "error must cite the bad policy field",
	)
}

// TestFeesAdminRPCListFeeEventsPaginates verifies that
// ListFeeEvents honors limit/offset arguments and that the
// Total field stays consistent with the number of events in
// play. We don't assert specific events because the harness's
// initial state depends on config details; we assert structural
// invariants instead.
func TestFeesAdminRPCListFeeEventsPaginates(t *testing.T) {
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

	// Fresh harness: ledger may start empty or may have
	// daemon-boot external-funding entries. Either way the
	// invariants below hold.
	pageOne, err := h.ArkAdminClient.ListFeeEvents(
		ctx, &adminrpc.ListFeeEventsRequest{
			Limit:  10,
			Offset: 0,
		},
	)
	require.NoError(t, err, "ListFeeEvents page one")

	// Structural: Total must be consistent with returned
	// events (Total is the total-matching count, not the
	// returned count; but returned <= Total).
	require.LessOrEqual(
		t,
		uint32(
			len(pageOne.Events),
		),
		pageOne.Total, "page must not exceed Total",
	)

	// Request offset beyond Total: must return zero events
	// and the same Total.
	beyond, err := h.ArkAdminClient.ListFeeEvents(
		ctx, &adminrpc.ListFeeEventsRequest{
			Limit:  10,
			Offset: pageOne.Total + 1000,
		},
	)
	require.NoError(t, err, "ListFeeEvents beyond Total")
	require.Empty(
		t, beyond.Events, "offset past Total must return no events",
	)
	require.Equal(
		t, pageOne.Total, beyond.Total,
		"Total must be stable across offset changes",
	)

	// Strictly-increasing entry_id within a page: the ledger
	// is append-only so every returned event's entry_id must
	// exceed the previous one.
	var prevID int64
	for i, ev := range pageOne.Events {
		if i == 0 {
			prevID = ev.EntryId
			continue
		}
		require.Greater(
			t, ev.EntryId, prevID,
			"entry_id must be strictly increasing",
		)
		prevID = ev.EntryId
	}
}

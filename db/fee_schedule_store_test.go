package db

import (
	"testing"
	"time"

	"github.com/lightninglabs/darepo/fees"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// TestFeeScheduleStoreRoundTrip verifies that a schedule
// inserted via FeeScheduleStoreDB is read back verbatim by
// LatestFeeSchedule, including every numeric field and the dust
// policy enum. This is the load-bearing behavior for the
// restart-persistence regression in itests.
func TestFeeScheduleStoreRoundTrip(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	defer store.Close()

	clk := clock.NewTestClock(time.Unix(1_700_000_000, 0))
	s := NewFeeScheduleStoreDB(store, clk)

	ctx := t.Context()

	// Before any insert, LatestFeeSchedule must return
	// (nil, false, nil) so the caller can fall through to
	// config.
	sched, found, err := s.LatestFeeSchedule(ctx)
	require.NoError(t, err)
	require.False(t, found)
	require.Nil(t, sched)

	want := &fees.Schedule{
		AnnualRate:                 0.07,
		BaseMarginSat:              250,
		UtilizationThresholdBPS:    6500,
		UtilizationSpreadDelta0BPS: 150,
		UtilizationSpreadDelta1BPS: 800,
		MinViableVTXOPolicy:        fees.DustPolicyReject,
		MinViableVTXOPct:           40,
		MinRefreshDeltaBlocks:      72,
	}
	require.NoError(t, s.InsertFeeSchedule(ctx, want))

	got, found, err := s.LatestFeeSchedule(ctx)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, want.AnnualRate, got.AnnualRate)
	require.Equal(t, want.BaseMarginSat, got.BaseMarginSat)
	require.Equal(
		t, want.UtilizationThresholdBPS,
		got.UtilizationThresholdBPS,
	)
	require.Equal(
		t, want.UtilizationSpreadDelta0BPS,
		got.UtilizationSpreadDelta0BPS,
	)
	require.Equal(
		t, want.UtilizationSpreadDelta1BPS,
		got.UtilizationSpreadDelta1BPS,
	)
	require.Equal(
		t, want.MinViableVTXOPolicy, got.MinViableVTXOPolicy,
	)
	require.Equal(t, want.MinViableVTXOPct, got.MinViableVTXOPct)
	require.Equal(
		t, want.MinRefreshDeltaBlocks,
		got.MinRefreshDeltaBlocks,
	)
}

// TestFeeScheduleStoreLatestWins verifies that when multiple
// schedules have been inserted, LatestFeeSchedule returns the
// most recently inserted one. This is the behavior the restart
// regression test depends on.
func TestFeeScheduleStoreLatestWins(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	defer store.Close()

	clk := clock.NewTestClock(time.Unix(1_700_000_000, 0))
	s := NewFeeScheduleStoreDB(store, clk)

	ctx := t.Context()

	older := &fees.Schedule{
		AnnualRate:          0.03,
		BaseMarginSat:       50,
		MinViableVTXOPolicy: fees.DustPolicyReject,
	}
	require.NoError(t, s.InsertFeeSchedule(ctx, older))

	// Advance the clock so the new row has a strictly greater
	// created_at than the older one. This is the ordering key
	// for ListFeeScheduleHistory.
	clk.SetTime(clk.Now().Add(time.Minute))

	newer := &fees.Schedule{
		AnnualRate:          0.09,
		BaseMarginSat:       500,
		MinViableVTXOPolicy: fees.DustPolicyReject,
	}
	require.NoError(t, s.InsertFeeSchedule(ctx, newer))

	got, found, err := s.LatestFeeSchedule(ctx)
	require.NoError(t, err)
	require.True(t, found)
	require.InDelta(t, newer.AnnualRate, got.AnnualRate, 1e-9)
	require.Equal(t, newer.BaseMarginSat, got.BaseMarginSat)
}

// TestFeeScheduleStoreSameSecondTiebreaker verifies that when two
// schedules are inserted at the same created_at second, the row
// with the higher primary key (inserted last) still wins. This
// pins the `ORDER BY created_at DESC, id DESC` contract so a
// regression that dropped the `id DESC` tiebreaker would surface
// as a non-deterministic latest-row selection here.
func TestFeeScheduleStoreSameSecondTiebreaker(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	defer store.Close()

	// The clock is pinned to a single second and never advanced,
	// so both inserts share the same created_at value and the
	// tiebreaker is the only thing that decides the winner.
	clk := clock.NewTestClock(time.Unix(1_700_000_000, 0))
	s := NewFeeScheduleStoreDB(store, clk)

	ctx := t.Context()

	older := &fees.Schedule{
		AnnualRate:          0.03,
		BaseMarginSat:       50,
		MinViableVTXOPolicy: fees.DustPolicyReject,
	}
	require.NoError(t, s.InsertFeeSchedule(ctx, older))

	newer := &fees.Schedule{
		AnnualRate:          0.09,
		BaseMarginSat:       500,
		MinViableVTXOPolicy: fees.DustPolicyReject,
	}
	require.NoError(t, s.InsertFeeSchedule(ctx, newer))

	got, found, err := s.LatestFeeSchedule(ctx)
	require.NoError(t, err)
	require.True(t, found)
	require.InDelta(t, newer.AnnualRate, got.AnnualRate, 1e-9)
	require.Equal(t, newer.BaseMarginSat, got.BaseMarginSat)
}

// TestFeeScheduleStoreRejectsNil verifies that InsertFeeSchedule
// rejects a nil schedule rather than panicking or inserting a
// zero row.
func TestFeeScheduleStoreRejectsNil(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	defer store.Close()

	s := NewFeeScheduleStoreDB(store, clock.NewDefaultClock())
	require.Error(t, s.InsertFeeSchedule(t.Context(), nil))
}

// TestFeeScheduleStoreValidatesInput verifies that a schedule
// failing Schedule.Validate is rejected at insert time rather
// than being silently persisted as a poisonous row that would
// fail validation on the next boot.
func TestFeeScheduleStoreValidatesInput(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	defer store.Close()

	s := NewFeeScheduleStoreDB(store, clock.NewDefaultClock())

	// Negative AnnualRate violates Schedule.Validate.
	bad := &fees.Schedule{
		AnnualRate:          -0.01,
		MinViableVTXOPolicy: fees.DustPolicyReject,
	}
	require.Error(t, s.InsertFeeSchedule(t.Context(), bad))
}

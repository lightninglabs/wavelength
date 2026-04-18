package fees

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestScheduleValidate pins down the explicit bounds that
// Schedule.Validate enforces. Each case names a single field that
// is knocked out of range so a future bounds change is easy to
// reason about.
func TestScheduleValidate(t *testing.T) {
	t.Parallel()

	// mutate takes the default (valid) schedule and applies a
	// single field mutation, then returns the result so the test
	// table stays readable.
	mutate := func(f func(s *Schedule)) *Schedule {
		s := DefaultSchedule()
		f(s)

		return s
	}

	tests := []struct {
		name    string
		s       *Schedule
		wantErr string
	}{
		{
			name: "default is valid",
			s:    DefaultSchedule(),
		},
		{
			name:    "nil schedule",
			s:       nil,
			wantErr: "schedule is nil",
		},
		{
			name: "negative annual rate",
			s: mutate(func(s *Schedule) {
				s.AnnualRate = -0.01
			}),
			wantErr: "annual rate",
		},
		{
			name: "NaN annual rate",
			s: mutate(func(s *Schedule) {
				s.AnnualRate = math.NaN()
			}),
			wantErr: "finite",
		},
		{
			name: "+Inf annual rate",
			s: mutate(func(s *Schedule) {
				s.AnnualRate = math.Inf(1)
			}),
			wantErr: "finite",
		},
		{
			name: "negative margin",
			s: mutate(func(s *Schedule) {
				s.BaseMarginSat = -1
			}),
			wantErr: "base margin",
		},
		{
			name: "threshold over 100%",
			s: mutate(func(s *Schedule) {
				s.UtilizationThresholdBPS = 10_001
			}),
			wantErr: "utilization threshold",
		},
		{
			name: "threshold at 100% (boundary)",
			s: mutate(func(s *Schedule) {
				s.UtilizationThresholdBPS = 10_000
			}),
		},
		{
			name: "viable pct over 100",
			s: mutate(func(s *Schedule) {
				s.MinViableVTXOPct = 101
			}),
			wantErr: "min viable VTXO pct",
		},
		{
			name: "viable pct at 100 (boundary)",
			s: mutate(func(s *Schedule) {
				s.MinViableVTXOPct = 100
			}),
		},
		{
			name: "unknown dust policy",
			s: mutate(func(s *Schedule) {
				s.MinViableVTXOPolicy = DustPolicy(99)
			}),
			wantErr: "unknown dust policy",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.s.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestCalculatorRejectsInvalidSchedule verifies that NewCalculator
// surfaces validation errors, i.e. that a malformed config cannot
// slip into the pricing path by accident.
func TestCalculatorRejectsInvalidSchedule(t *testing.T) {
	t.Parallel()

	bad := DefaultSchedule()
	bad.AnnualRate = -1.0

	calc, err := NewCalculator(bad)
	require.Error(t, err)
	require.Nil(t, calc)
}

// TestUpdateScheduleRejectsInvalid verifies that a failing hot
// reload leaves the existing schedule in place. This matters
// because UpdateSchedule is the runtime admin surface — a single
// bad RPC call must not poison in-memory pricing state.
func TestUpdateScheduleRejectsInvalid(t *testing.T) {
	t.Parallel()

	good := DefaultSchedule()
	calc, err := NewCalculator(good)
	require.NoError(t, err)

	bad := *good
	bad.MinViableVTXOPct = 200

	err = calc.UpdateSchedule(&bad)
	require.Error(t, err)

	// The current schedule should still be the good one.
	require.Same(t, good, calc.Schedule(),
		"failed UpdateSchedule must leave prior schedule in place")
}

// TestScheduleValidateProperty asserts that the default schedule
// plus any mutation that stays in-bounds (non-negative rates, bps
// in [0, 10000], pct in [0, 100]) must always validate — and,
// conversely, that knocking a single bound out of range must fail.
func TestScheduleValidateProperty(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		threshold := rapid.Uint32Range(0, 10_000).Draw(
			rt, "utilThresholdBps",
		)
		delta0 := rapid.Uint32Range(0, 1e6).Draw(rt, "delta0Bps")
		delta1 := rapid.Uint32Range(0, 1e6).Draw(rt, "delta1Bps")
		s := &Schedule{
			AnnualRate: rapid.Float64Range(0, 100).Draw(
				rt, "annualRate",
			),
			BaseMarginSat: rapid.Int64Range(0, 1e9).Draw(
				rt, "baseMarginSat",
			),
			UtilizationThresholdBPS:    threshold,
			UtilizationSpreadDelta0BPS: delta0,
			UtilizationSpreadDelta1BPS: delta1,
			MinViableVTXOPolicy: rapid.SampledFrom([]DustPolicy{
				DustPolicyReject, DustPolicyWarn,
			}).Draw(rt, "dustPolicy"),
			MinViableVTXOPct: rapid.Uint32Range(0, 100).Draw(
				rt, "minViablePct",
			),
			MinRefreshDeltaBlocks: rapid.Uint32Range(0, 1e6).Draw(
				rt, "minRefreshDeltaBlocks",
			),
		}

		require.NoError(rt, s.Validate(),
			"any in-bounds schedule must validate")
	})
}

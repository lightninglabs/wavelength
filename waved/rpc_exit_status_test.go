package waved

import (
	"testing"

	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightninglabs/wavelength/unrollplan"
	"github.com/lightninglabs/wavelength/waverpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestSpentSoFarSat verifies the estimated fee-committed-so-far projection
// across the exit lifecycle: coarse/terminal states with no live progress, and
// live materialization where the CPFP total is prorated over the proof
// transactions already broadcast (confirmed plus in-flight) with the sweep fee
// folded in once the sweep is built.
func TestSpentSoFarSat(t *testing.T) {
	t.Parallel()

	// A fee breakdown with a round CPFP total so the proration math is easy
	// to read: 1550 over 5 proof txs is 310 per child.
	fees := func() *waverpc.UnrollFees {
		return &waverpc.UnrollFees{
			CpfpFeeSat:   1550,
			SweepFeeSat:  400,
			TotalCostSat: 1950,
		}
	}

	progress := func(confirmed, inFlight, total int) *unroll.ExitProgress {
		return &unroll.ExitProgress{
			ConfirmedTxs: confirmed,
			InFlightTxs:  inFlight,
			TotalTxs:     total,
		}
	}

	tests := []struct {
		name       string
		progress   *unroll.ExitProgress
		sweepBuilt bool
		status     waverpc.UnrollJobStatus
		want       int64
	}{
		{
			name:     "no progress completed spent whole total",
			progress: nil,
			status: waverpc.
				UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED,
			want: 1950,
		},
		{
			name:     "materializing no progress spent nothing",
			progress: nil,
			status: waverpc.
				UnrollJobStatus_UNROLL_JOB_STATUS_MATERIALIZING,
			want: 0,
		},
		{
			name:     "prorate cpfp over broadcast txs",
			progress: progress(2, 1, 5),
			status: waverpc.
				UnrollJobStatus_UNROLL_JOB_STATUS_MATERIALIZING,
			// 310 per child * (2 confirmed + 1 in-flight).
			want: 930,
		},
		{
			name:       "sweep fee folded in once built",
			progress:   progress(2, 1, 5),
			sweepBuilt: true,
			status: waverpc.
				UnrollJobStatus_UNROLL_JOB_STATUS_SWEEPING,
			// 930 CPFP + 400 sweep.
			want: 1330,
		},
		{
			name:     "broadcast count clamps to total",
			progress: progress(5, 3, 5),
			status: waverpc.
				UnrollJobStatus_UNROLL_JOB_STATUS_MATERIALIZING,
			// Clamped to 5/5, so the full CPFP total.
			want: 1550,
		},
		{
			name:     "zero total txs yields zero cpfp",
			progress: progress(0, 0, 0),
			status: waverpc.
				UnrollJobStatus_UNROLL_JOB_STATUS_MATERIALIZING,
			want: 0,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := spentSoFarSat(
				fees(), tc.progress, tc.sweepBuilt, tc.status,
			)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestUnrollPhaseDetail verifies the human phase line folds live progress into
// the materializing and CSV-pending descriptions and degrades to a coarse line
// when no live progress is available.
func TestUnrollPhaseDetail(t *testing.T) {
	t.Parallel()

	materializing := &unroll.ExitProgress{
		ConfirmedTxs: 2,
		InFlightTxs:  1,
		TotalTxs:     5,
		CurrentLayer: 2,
		TotalLayers:  5,
	}
	csvPending := &unroll.ExitProgress{
		TargetConfirmed:   true,
		AllProofConfirmed: true,
		CSV: fn.Some(unrollplan.CSVInfo{
			MaturityHeight:  900,
			BlocksRemaining: 42,
		}),
	}

	tests := []struct {
		name     string
		status   waverpc.UnrollJobStatus
		progress *unroll.ExitProgress
		contains []string
	}{
		{
			name: "materializing folds in layer and tx counts",
			status: waverpc.
				UnrollJobStatus_UNROLL_JOB_STATUS_MATERIALIZING,
			progress: materializing,
			// CurrentLayer is 0-based; the line renders it 1-based.
			contains: []string{
				"layer 3 of 5",
				"2/5 txs confirmed",
			},
		},
		{
			name: "materializing without progress is coarse",
			status: waverpc.
				UnrollJobStatus_UNROLL_JOB_STATUS_MATERIALIZING,
			progress: nil,
			contains: []string{
				"materializing recovery transactions",
			},
		},
		{
			name: "csv pending folds in the countdown",
			status: waverpc.
				UnrollJobStatus_UNROLL_JOB_STATUS_CSV_PENDING,
			progress: csvPending,
			contains: []string{
				"42 blocks remaining",
				"height 900",
			},
		},
		{
			name: "completed is terminal line",
			status: waverpc.
				UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED,
			progress: nil,
			contains: []string{
				"exit complete",
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := unrollPhaseDetail(tc.status, tc.progress)
			for _, want := range tc.contains {
				require.Contains(t, got, want)
			}
		})
	}
}

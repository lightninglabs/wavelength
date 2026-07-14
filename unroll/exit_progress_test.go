package unroll

import (
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestFrontierLayer verifies that the frontier layer is the shallowest layer
// holding an unconfirmed transaction, and collapses to the tree depth once
// nothing is left to materialize.
func TestFrontierLayer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		snapshot    *unrollplan.Snapshot
		totalLayers int
		want        int
	}{
		{
			name:        "all confirmed collapses to depth",
			snapshot:    &unrollplan.Snapshot{},
			totalLayers: 4,
			want:        4,
		},
		{
			name: "shallowest blocked wins over deeper ready",
			snapshot: &unrollplan.Snapshot{
				Ready: []unrollplan.TxFrontier{
					{
						Layer: 3,
					},
				},
				Blocked: []unrollplan.BlockedTx{
					{TxFrontier: unrollplan.TxFrontier{
						Layer: 1,
					}},
				},
			},
			totalLayers: 4,
			want:        1,
		},
		{
			name: "in-flight sets the frontier",
			snapshot: &unrollplan.Snapshot{
				InFlight: []unrollplan.TxFrontier{
					{
						Layer: 2,
					},
				},
			},
			totalLayers: 4,
			want:        2,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := frontierLayer(tc.snapshot, tc.totalLayers)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestBestCaseBlocksRemaining verifies the optimistic block countdown across
// the exit lifecycle: materialization (full CSV wait), CSV pending (live
// remaining), sweep broadcast, and completion.
func TestBestCaseBlocksRemaining(t *testing.T) {
	t.Parallel()

	const csvDelay = 144

	// Alias the long enum constant so the deeply-nested table entry below
	// stays within the 80-column limit.
	const broadcasted = unrollplan.SweepStatusBroadcasted

	tests := []struct {
		name           string
		snapshot       *unrollplan.Snapshot
		currentLayer   int
		totalLayers    int
		lockTimeBlocks int32
		want           int32
	}{
		{
			name: "confirmed sweep is done",
			snapshot: &unrollplan.Snapshot{
				Done: true,
			},
			want: 0,
		},
		{
			name: "broadcast sweep is one confirmation away",
			snapshot: &unrollplan.Snapshot{
				Sweep: unrollplan.SweepState{
					Status: broadcasted,
				},
			},
			want: 1,
		},
		{
			name: "materializing uses remaining layers plus CSV",
			snapshot: &unrollplan.Snapshot{
				Sweep: unrollplan.SweepState{
					Status: unrollplan.SweepStatusPending,
				},
			},
			currentLayer: 1,
			totalLayers:  4,
			// (4-1) layers + 144 CSV + 1 sweep.
			want: 3 + csvDelay + 1,
		},
		{
			name: "csv pending uses live blocks remaining",
			snapshot: &unrollplan.Snapshot{
				Sweep: unrollplan.SweepState{
					Status: unrollplan.SweepStatusPending,
				},
				CSV: fn.Some(unrollplan.CSVInfo{
					BlocksRemaining: 10,
				}),
			},
			currentLayer: 4,
			totalLayers:  4,
			// 0 layers + 10 CSV + 1 sweep.
			want: 0 + 10 + 1,
		},
		{
			name: "target confirmed drops straggler layers",
			snapshot: &unrollplan.Snapshot{
				TargetConfirmed: true,
				Sweep: unrollplan.SweepState{
					Status: unrollplan.SweepStatusPending,
				},
				CSV: fn.Some(unrollplan.CSVInfo{
					BlocksRemaining: 10,
				}),
			},
			// A straggler sibling keeps the frontier below the
			// target layer, but the sweep is gated only on CSV, so
			// the materialization term must be dropped: 0 + 10 + 1.
			currentLayer: 2,
			totalLayers:  4,
			want:         0 + 10 + 1,
		},
		{
			name: "policy locktime dominates the csv wait",
			snapshot: &unrollplan.Snapshot{
				TargetConfirmed: true,
				Sweep: unrollplan.SweepState{
					Status: unrollplan.SweepStatusPending,
				},
				CSV: fn.Some(unrollplan.CSVInfo{
					BlocksRemaining: 10,
				}),
			},
			currentLayer: 4,
			totalLayers:  4,
			// The absolute locktime (30 blocks out) gates the sweep
			// past the CSV maturity (10), so 0 + max(10, 30) + 1.
			lockTimeBlocks: 30,
			want:           0 + 30 + 1,
		},
		{
			name: "csv wait dominates a nearer locktime",
			snapshot: &unrollplan.Snapshot{
				TargetConfirmed: true,
				Sweep: unrollplan.SweepState{
					Status: unrollplan.SweepStatusPending,
				},
				CSV: fn.Some(unrollplan.CSVInfo{
					BlocksRemaining: 20,
				}),
			},
			currentLayer:   4,
			totalLayers:    4,
			lockTimeBlocks: 5,
			// 0 + max(20, 5) + 1.
			want: 0 + 20 + 1,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := bestCaseBlocksRemaining(
				tc.snapshot, csvDelay, tc.currentLayer,
				tc.totalLayers, tc.lockTimeBlocks,
			)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestActualSweepFeeSat verifies the built-sweep fee derivation: the target
// input value the sweep spends minus the swept output value, with defensive
// guards for missing or malformed inputs.
func TestActualSweepFeeSat(t *testing.T) {
	t.Parallel()

	const inputValue int64 = 100_000

	sweepTx := func(outValue int64) *wire.MsgTx {
		tx := wire.NewMsgTx(3)
		tx.AddTxOut(&wire.TxOut{Value: outValue})

		return tx
	}

	t.Run("nil tx yields no fee", func(t *testing.T) {
		t.Parallel()

		_, ok := actualSweepFeeSat(nil, inputValue)
		require.False(t, ok)
	})

	t.Run("positive fee is input minus output", func(t *testing.T) {
		t.Parallel()

		fee, ok := actualSweepFeeSat(sweepTx(99_700), inputValue)
		require.True(t, ok)
		require.Equal(t, int64(300), fee)
	})

	t.Run("non-positive fee is rejected", func(t *testing.T) {
		t.Parallel()

		_, ok := actualSweepFeeSat(sweepTx(100_000), inputValue)
		require.False(t, ok)
	})
}

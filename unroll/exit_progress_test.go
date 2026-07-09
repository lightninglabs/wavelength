package unroll

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/unrollplan"
	"github.com/lightninglabs/darepo-client/vtxo"
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
		name         string
		snapshot     *unrollplan.Snapshot
		currentLayer int
		totalLayers  int
		want         int32
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
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := bestCaseBlocksRemaining(
				tc.snapshot, csvDelay, tc.currentLayer,
				tc.totalLayers,
			)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestActualSweepFeeSat verifies the built-sweep fee derivation: target value
// minus swept output value, with defensive guards for missing or malformed
// inputs.
func TestActualSweepFeeSat(t *testing.T) {
	t.Parallel()

	desc := &vtxo.Descriptor{Amount: btcutil.Amount(100_000)}

	sweepTx := func(outValue int64) *wire.MsgTx {
		tx := wire.NewMsgTx(3)
		tx.AddTxOut(&wire.TxOut{Value: outValue})

		return tx
	}

	t.Run("nil tx yields no fee", func(t *testing.T) {
		t.Parallel()

		_, ok := actualSweepFeeSat(nil, desc)
		require.False(t, ok)
	})

	t.Run("nil desc yields no fee", func(t *testing.T) {
		t.Parallel()

		_, ok := actualSweepFeeSat(sweepTx(99_000), nil)
		require.False(t, ok)
	})

	t.Run("positive fee is target minus output", func(t *testing.T) {
		t.Parallel()

		fee, ok := actualSweepFeeSat(sweepTx(99_700), desc)
		require.True(t, ok)
		require.Equal(t, int64(300), fee)
	})

	t.Run("non-positive fee is rejected", func(t *testing.T) {
		t.Parallel()

		_, ok := actualSweepFeeSat(sweepTx(100_000), desc)
		require.False(t, ok)
	})
}

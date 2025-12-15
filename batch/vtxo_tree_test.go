package batch

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/stretchr/testify/require"
)

// TestVTXOBatches tests that VTXOBatches correctly splits VTXOs into batches
// of the specified maximum size.
func TestVTXOBatches(t *testing.T) {
	t.Parallel()

	// makeVTXOs creates a slice of n dummy VTXODescriptors.
	makeVTXOs := func(n int) []tree.VTXODescriptor {
		vtxos := make([]tree.VTXODescriptor, n)
		for i := range vtxos {
			vtxos[i] = tree.VTXODescriptor{
				Amount: btcutil.Amount(i + 1),
			}
		}

		return vtxos
	}

	tests := []struct {
		name        string
		numVTXOs    int
		maxPerBatch uint32
		wantBatches []int
	}{
		{
			name:        "empty slice",
			numVTXOs:    0,
			maxPerBatch: 5,
			wantBatches: nil,
		},
		{
			name:        "smaller than batch size",
			numVTXOs:    3,
			maxPerBatch: 5,
			wantBatches: []int{3},
		},
		{
			name:        "exact batch size",
			numVTXOs:    5,
			maxPerBatch: 5,
			wantBatches: []int{5},
		},
		{
			name:        "multiple full batches",
			numVTXOs:    10,
			maxPerBatch: 5,
			wantBatches: []int{5, 5},
		},
		{
			name:        "multiple batches with remainder",
			numVTXOs:    12,
			maxPerBatch: 5,
			wantBatches: []int{5, 5, 2},
		},
		{
			name:        "single element batches",
			numVTXOs:    3,
			maxPerBatch: 1,
			wantBatches: []int{1, 1, 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			vtxos := makeVTXOs(tc.numVTXOs)

			// Collect batch sizes and indices.
			var (
				gotBatches []int
				gotIndices []int
			)

			batches, err := VTXOBatches(vtxos, tc.maxPerBatch)
			require.NoError(t, err)

			for idx, batch := range batches {
				gotIndices = append(gotIndices, idx)
				gotBatches = append(gotBatches, len(batch))
			}

			// Verify batch sizes match expectations.
			require.Equal(t, tc.wantBatches, gotBatches)

			// Verify indices are sequential starting from 0.
			for i, idx := range gotIndices {
				require.Equal(t, i, idx)
			}
		})
	}
}

package batch

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/internal/testutils"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// makeVTXODescriptors creates n VTXO descriptors with unique client keys. Each
// descriptor has an amount of baseAmount * (i+1) where i is the index.
func makeVTXODescriptors(t *testing.T, n int, baseAmount btcutil.Amount,
	operatorPub *btcec.PublicKey) []tree.VTXODescriptor {

	t.Helper()

	descs, _ := makeVTXODescriptorsWithKeys(t, n, baseAmount, operatorPub)

	return descs
}

// makeVTXODescriptorsWithKeys creates n VTXO descriptors with unique client
// keys, returning both the descriptors and the client keys. This is useful for
// tests that need to reference the client keys after creating the descriptors.
func makeVTXODescriptorsWithKeys(t *testing.T, n int, baseAmount btcutil.Amount,
	opKey *btcec.PublicKey) ([]tree.VTXODescriptor, []*btcec.PublicKey) {

	t.Helper()

	descriptors := make([]tree.VTXODescriptor, 0, n)
	clientKeys := make([]*btcec.PublicKey, 0, n)

	for i := 0; i < n; i++ {
		clientKey, _ := testutils.CreateKey(int32(i))
		desc, err := tree.NewVTXODescriptor(
			baseAmount*btcutil.Amount(i+1), clientKey,
			opKey, 144,
		)
		require.NoError(t, err)

		descriptors = append(descriptors, *desc)
		clientKeys = append(clientKeys, clientKey)
	}

	return descriptors, clientKeys
}

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

// TestBuildTreeContext tests the BuildTreeContext function.
func TestBuildTreeContext(t *testing.T) {
	t.Parallel()

	operatorPub, _ := testutils.CreateKey(1)
	sweepPub, _ := testutils.CreateKey(2)

	terms := &Terms{
		OperatorKey:     keychain.KeyDescriptor{PubKey: operatorPub},
		SweepKey:        keychain.KeyDescriptor{PubKey: sweepPub},
		SweepDelay:      288,
		MaxVTXOsPerTree: 4,
	}

	t.Run("single VTXO creates one output", func(t *testing.T) {
		descriptors := makeVTXODescriptors(t, 1, 10000, operatorPub)

		ctx, err := BuildTreeContext(terms, descriptors)
		require.NoError(t, err)

		outputs := ctx.Outputs()
		require.Len(t, outputs, 1)
		require.NotNil(t, outputs[0])
		require.Greater(t, outputs[0].Value, int64(0))
	})

	t.Run("multiple VTXOs under limit creates one output",
		func(t *testing.T) {
			// Create 3 VTXOs (under MaxVTXOsPerTree of 4).
			descs := makeVTXODescriptors(t, 3, 10000, operatorPub)

			ctx, err := BuildTreeContext(terms, descs)
			require.NoError(t, err)
			require.Len(t, ctx.Outputs(), 1)
		},
	)

	t.Run("VTXOs over limit split into multiple outputs",
		func(t *testing.T) {
			// Create 10 VTXOs (splits into 3 batches: 4, 4, 2).
			descs := makeVTXODescriptors(t, 10, 5000, operatorPub)

			ctx, err := BuildTreeContext(terms, descs)
			require.NoError(t, err)

			outputs := ctx.Outputs()
			require.Len(t, outputs, 3)

			// Each output should have non-zero value.
			for _, output := range outputs {
				require.Greater(t, output.Value, int64(0))
			}
		},
	)
}

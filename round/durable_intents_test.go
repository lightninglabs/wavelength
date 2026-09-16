package round

import (
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/require"
)

// TestDurableIntentsPreservesPositions binds quote amounts to the same leave
// positions after restart, including absent requests and absent outputs.
func TestDurableIntentsPreservesPositions(t *testing.T) {
	point := wire.OutPoint{Index: 17}
	original := Intents{
		Service: &types.ServiceRequest{
			OperationID: [32]byte{
				1,
			}, Mode: types.ServiceImmediate,
			ExpiresAtUnix: 1700000000,
			FeeLimitSat:   51,
			AllowFallback: true,
		},
		Boarding: []BoardingIntent{
			{},
		},
		VTXOs: []types.VTXORequest{
			{
				Amount:      -1,
				FixedAmount: true,
			},
		},
		Leaves: []*types.LeaveRequest{
			nil, {
				IsChange: true,
			},
			{
				Output: wire.NewTxOut(71, []byte{0x51}),
			},
		},
		Forfeits: []types.ForfeitRequest{
			{
				Amount: -9,
			}, {
				VTXOOutpoint: &point,
				Amount:       81,
			},
		},
		QuotedLeaveAmounts: []int64{
			0,
			-1,
			69,
		},
	}
	raw, err := encodeDurableIntents(original)
	require.NoError(t, err)
	restored, err := decodeDurableIntents(raw, nil)
	require.NoError(t, err)
	require.Equal(t, original.Service, restored.Service)
	require.Equal(t, original.Leaves, restored.Leaves)
	require.Equal(t, original.Forfeits, restored.Forfeits)
	require.Equal(
		t, original.QuotedLeaveAmounts, restored.QuotedLeaveAmounts,
	)
	require.Len(t, restored.Boarding, 1)
	require.Len(t, restored.VTXOs, 1)
	require.Equal(t, original.VTXOs[0].Amount, restored.VTXOs[0].Amount)
	require.True(t, restored.VTXOs[0].FixedAmount)
	again, err := encodeDurableIntents(restored)
	require.NoError(t, err)
	require.Equal(t, raw, again)
	require.EqualValues(t, 69, restored.LeaveAmount(2))
}

// TestDurableIntentListTruncation rejects partial entries instead of silently
// restoring a prefix that would shift the operation's positional accounting.
func TestDurableIntentListTruncation(t *testing.T) {
	raw, err := encodeDurableList([]int64{1, 2}, encodeDurableAmount)
	require.NoError(t, err)
	_, err = decodeDurableList(raw[:len(raw)-1], decodeDurableAmount)
	require.Error(t, err)
}

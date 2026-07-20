package vtxo

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/round"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestManagerTerminalVTXOObserver verifies manager cleanup notifies the
// terminal observer after dropping the child actor reference.
func TestManagerTerminalVTXOObserver(t *testing.T) {
	outpoint := wire.OutPoint{
		Hash: [32]byte{
			1,
		},
		Index: 2,
	}

	var observed []wire.OutPoint
	manager := NewManager(&ManagerConfig{
		TerminalVTXOObserver: func(_ context.Context,
			outpoint wire.OutPoint) error {

			observed = append(observed, outpoint)

			return nil
		},
	})

	resp := manager.Receive(t.Context(), &round.VTXOTerminatedMsg{
		Outpoint:   outpoint,
		FinalState: "spent",
		Reason:     "test",
	})
	require.True(t, resp.IsOk())
	require.Len(t, observed, 1)
	require.Equal(t, outpoint, observed[0])
}

// TestManagerRedemptionBlockObserver verifies the manager-owned subscription
// can trigger reconciliation even when the active VTXO actor map is empty.
func TestManagerRedemptionBlockObserver(t *testing.T) {
	t.Parallel()

	var observed []int32
	manager := NewManager(&ManagerConfig{
		RedemptionBlockObserver: func(_ context.Context,
			height int32) error {

			observed = append(observed, height)

			return nil
		},
	})

	resp := manager.Receive(t.Context(), &redemptionBlockEpochMsg{
		Height: 901_234,
	})
	require.True(t, resp.IsOk())
	require.Equal(t, []int32{901_234}, observed)
}

// TestManagerReconcileRecoversLegacyExpiredFailure verifies startup expiry
// reconciliation upgrades legacy terminal rows before scanning active actors
// and wakes the redemption coordinator for the recovered source.
func TestManagerReconcileRecoversLegacyExpiredFailure(t *testing.T) {
	t.Parallel()

	const bestHeight int32 = 901_234
	outpoint := wire.OutPoint{Hash: [32]byte{3}, Index: 4}
	store := &MockVTXOStore{}
	store.On(
		"RecoverLegacyExpiredVTXOs", mock.Anything, bestHeight,
	).Return([]wire.OutPoint{outpoint}, nil).Once()

	var observed []wire.OutPoint
	manager := NewManager(&ManagerConfig{
		Store: store,
		ExpiredVTXOObserver: func(_ context.Context,
			outpoint wire.OutPoint) error {

			observed = append(observed, outpoint)

			return nil
		},
	})

	result := manager.Receive(t.Context(), &ReconcileExpiryRequest{
		Height: bestHeight,
	})
	respAny, err := result.Unpack()
	require.NoError(t, err)
	resp, ok := respAny.(*ReconcileExpiryResponse)
	require.True(t, ok)
	require.Zero(t, resp.Checked)
	require.Equal(t, 1, resp.Expired)
	require.Equal(t, 1, resp.LegacyRecovered)
	require.Equal(t, []wire.OutPoint{outpoint}, observed)
	store.AssertExpectations(t)
}

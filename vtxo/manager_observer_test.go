package vtxo

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/round"
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

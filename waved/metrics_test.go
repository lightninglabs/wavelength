package waved

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/metrics"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// TestSystemStatsAdapterVTXOStats covers the scrape-time VTXO inventory rows:
// they follow the explicit status list order, omit empty statuses, and drop
// statuses the dashboards do not report (expired).
func TestSystemStatsAdapterVTXOStats(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	vtxoStore := newListVTXOsStore(t)
	adapter := &systemStatsAdapter{srv: &Server{vtxoStore: vtxoStore}}

	// An empty store reports no rows.
	rows, err := adapter.GetVTXOStatsByStatus(ctx)
	require.NoError(t, err)
	require.Empty(t, rows)

	seed := []struct {
		hashByte byte
		amount   btcutil.Amount
		status   vtxo.VTXOStatus
	}{
		{
			0x01,
			10_000,
			vtxo.VTXOStatusLive,
		},
		{
			0x02,
			20_000,
			vtxo.VTXOStatusLive,
		},
		{
			0x03,
			30_000,
			vtxo.VTXOStatusForfeited,
		},
		{
			0x04,
			40_000,
			vtxo.VTXOStatusSpent,
		},
		{
			0x05,
			50_000,
			vtxo.VTXOStatusExpired,
		},
	}
	for _, s := range seed {
		desc := newRefreshEstimateVTXO(t, s.hashByte, s.amount, 900)
		require.NoError(t, vtxoStore.SaveVTXO(ctx, desc))
		require.NoError(
			t, vtxoStore.UpdateVTXOStatus(
				ctx, desc.Outpoint, s.status,
			),
		)
	}

	rows, err = adapter.GetVTXOStatsByStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, []metrics.VTXOStatRow{
		{
			Status:     vtxo.VTXOStatusLive.String(),
			Count:      2,
			TotalValue: 30_000,
		},
		{
			Status:     vtxo.VTXOStatusForfeited.String(),
			Count:      1,
			TotalValue: 30_000,
		},
		{
			Status:     vtxo.VTXOStatusSpent.String(),
			Count:      1,
			TotalValue: 40_000,
		},
	}, rows)
}

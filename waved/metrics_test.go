package waved

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/metrics"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// TestMetricsAddrLifecycle verifies that callers can observe an ephemeral
// listener safely during startup and shutdown without a stale bound address.
func TestMetricsAddrLifecycle(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"enabled", "disabled", "listen failure"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			cfg := DefaultConfig()
			cfg.Metrics.ListenAddr = "127.0.0.1:0"
			switch mode {
			case "disabled":
				cfg.Metrics.ListenAddr = ""

			case "listen failure":
				listener, err := net.Listen(
					"tcp", "127.0.0.1:0",
				)
				require.NoError(t, err)
				t.Cleanup(func() {
					require.NoError(t, listener.Close())
				})
				cfg.Metrics.ListenAddr = listener.
					Addr().
					String()
			}

			server := &Server{
				cfg:         cfg,
				actorSystem: actor.NewActorSystem(),
			}
			t.Cleanup(func() {
				// Cleanup owns shutdown after the test context
				// ends.
				ctx, cancel := context.WithTimeout(
					context.Background(), 5*time.Second,
				)
				defer cancel()
				require.NoError(
					t, server.stopMetricsServer(ctx),
				)
				require.NoError(
					t, server.actorSystem.Shutdown(ctx),
				)
			})
			require.Nil(t, server.MetricsAddr())

			// Only the run loop mutates listener state. External
			// address readers must remain safe while that loop
			// starts and stops.
			stopReader := make(chan struct{})
			readerDone := make(chan struct{})
			go func() {
				defer close(readerDone)
				for {
					select {
					case <-stopReader:
						return

					default:
						_ = server.MetricsAddr()
					}
				}
			}()
			t.Cleanup(func() {
				close(stopReader)
				<-readerDone
			})

			err := server.startMetricsServer(t.Context())
			if mode == "listen failure" {
				require.ErrorContains(t, err, "metrics listen")
				require.Nil(t, server.MetricsAddr())

				return
			}
			require.NoError(t, err)
			if mode == "disabled" {
				require.Nil(t, server.MetricsAddr())
			} else {
				addr := server.MetricsAddr()
				require.Equal(t, server.metricsSrv.Addr(), addr)
				tcpAddr, ok := addr.(*net.TCPAddr)
				require.True(t, ok)
				require.NotZero(t, tcpAddr.Port)
			}

			require.NoError(
				t,
				server.stopMetricsServer(
					t.Context(),
				),
			)
			require.Nil(t, server.MetricsAddr())
		})
	}
}

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

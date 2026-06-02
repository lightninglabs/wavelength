package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// mockVTXOQuerier is a test double for VTXOStatsQuerier returning a
// fixed result or error.
type mockVTXOQuerier struct {
	rows []VTXOStatRow
	err  error
}

// GetVTXOStatsByStatus implements VTXOStatsQuerier.
func (m *mockVTXOQuerier) GetVTXOStatsByStatus(_ context.Context) (
	[]VTXOStatRow, error) {

	return m.rows, m.err
}

// TestSystemCollectorCollect verifies the scrape-driven collector emits
// the expected count, value, and spendable-balance samples for a range
// of VTXO inventories, and emits nothing on a query failure.
func TestSystemCollectorCollect(t *testing.T) {
	t.Parallel()

	const spendableHeader = `
# HELP darepod_spendable_balance_satoshis Total value in satoshis ` +
		`of spendable (live) VTXOs.
# TYPE darepod_spendable_balance_satoshis gauge
`

	tests := []struct {
		name string
		rows []VTXOStatRow
		err  error

		// wantSpendable is the expected spendable balance sample
		// line, appended to the count/value exposition.
		wantSpendable string

		// wantExpfmt is the expected text-format exposition for the
		// count and value gauges. Empty means no count/value samples.
		wantExpfmt string
	}{
		{
			name: "mixed statuses",
			rows: []VTXOStatRow{
				{
					Status:     "live",
					Count:      3,
					TotalValue: 30000,
				},
				{
					Status:     "spent",
					Count:      2,
					TotalValue: 20000,
				},
				{
					Status:     "unilateral_exit",
					Count:      1,
					TotalValue: 5000,
				},
			},
			wantSpendable: "darepod_spendable_balance_satoshis " +
				"30000\n",
			wantExpfmt: `
# HELP darepod_vtxos Number of VTXOs by status.
# TYPE darepod_vtxos gauge
darepod_vtxos{status="live"} 3
darepod_vtxos{status="spent"} 2
darepod_vtxos{status="unilateral_exit"} 1
# HELP darepod_vtxos_value_satoshis Total VTXO value by status in satoshis.
# TYPE darepod_vtxos_value_satoshis gauge
darepod_vtxos_value_satoshis{status="live"} 30000
darepod_vtxos_value_satoshis{status="spent"} 20000
darepod_vtxos_value_satoshis{status="unilateral_exit"} 5000
`,
		},
		{
			name:          "empty inventory",
			rows:          []VTXOStatRow{},
			wantSpendable: "darepod_spendable_balance_satoshis 0\n",
		},
		{
			name: "only non-live statuses have zero spendable",
			rows: []VTXOStatRow{
				{
					Status:     "spent",
					Count:      4,
					TotalValue: 9000,
				},
				{
					Status:     "forfeited",
					Count:      2,
					TotalValue: 1000,
				},
			},
			wantSpendable: "darepod_spendable_balance_satoshis 0\n",
			wantExpfmt: `
# HELP darepod_vtxos Number of VTXOs by status.
# TYPE darepod_vtxos gauge
darepod_vtxos{status="forfeited"} 2
darepod_vtxos{status="spent"} 4
# HELP darepod_vtxos_value_satoshis Total VTXO value by status in satoshis.
# TYPE darepod_vtxos_value_satoshis gauge
darepod_vtxos_value_satoshis{status="forfeited"} 1000
darepod_vtxos_value_satoshis{status="spent"} 9000
`,
		},
		{
			name: "query error emits nothing",
			err:  errors.New("db down"),
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			q := &mockVTXOQuerier{rows: tc.rows, err: tc.err}
			c := NewSystemCollector(
				q, fn.None[btclog.Logger](),
			)

			// On a query failure the collector must emit no
			// samples at all (not even the spendable gauge).
			if tc.err != nil {
				require.Zero(t, testutil.CollectAndCount(c))

				return
			}

			// Verify the count and value gauges plus the spendable
			// balance gauge against the expected exposition.
			expfmt := tc.wantExpfmt + spendableHeader +
				tc.wantSpendable
			err := testutil.CollectAndCompare(
				c, strings.NewReader(expfmt),
				"darepod_vtxos", "darepod_vtxos_value_satoshis",
				"darepod_spendable_balance_satoshis",
			)
			require.NoError(t, err)
		})
	}
}

// TestRegisterAllIdempotent verifies RegisterAll tolerates duplicate
// registration on the same registry without panicking, matching the
// multi-daemon test-process invariant.
func TestRegisterAllIdempotent(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	require.NotPanics(t, func() {
		RegisterAll(reg)
		RegisterAll(reg)
	})
}

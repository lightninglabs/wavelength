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

// mockVTXOQuerier is a test double for SystemStatsQuerier. It returns a
// fixed VTXO result or error and reports the other data sources as
// unavailable so the VTXO-focused tests see only the VTXO gauges.
type mockVTXOQuerier struct {
	rows []VTXOStatRow
	err  error
}

// GetVTXOStatsByStatus implements VTXOStatsQuerier.
func (m *mockVTXOQuerier) GetVTXOStatsByStatus(_ context.Context) (
	[]VTXOStatRow, error) {

	return m.rows, m.err
}

// GetWalletBalance implements SystemStatsQuerier; unavailable in this
// VTXO-focused double.
func (m *mockVTXOQuerier) GetWalletBalance(_ context.Context) (WalletBalance,
	error) {

	return WalletBalance{}, errors.New("not supported")
}

// GetBlockHeight implements SystemStatsQuerier; unavailable here.
func (m *mockVTXOQuerier) GetBlockHeight(_ context.Context) (int64, error) {
	return 0, errors.New("not supported")
}

// GetOORSessionStatsByState implements SystemStatsQuerier; empty here.
func (m *mockVTXOQuerier) GetOORSessionStatsByState(_ context.Context) (
	map[string]int64, error) {

	return nil, nil
}

// GetRoundStatsByStatus implements SystemStatsQuerier; empty here.
func (m *mockVTXOQuerier) GetRoundStatsByStatus(_ context.Context) (
	map[string]int64, error) {

	return nil, nil
}

// TestSystemCollectorCollect verifies the scrape-driven collector emits
// the expected count, value, and spendable-balance samples for a range
// of VTXO inventories, and emits nothing on a query failure.
func TestSystemCollectorCollect(t *testing.T) {
	t.Parallel()

	const spendableHeader = `
# HELP waved_spendable_balance_satoshis Total value in satoshis ` +
		`of spendable (live) VTXOs.
# TYPE waved_spendable_balance_satoshis gauge
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
			wantSpendable: "waved_spendable_balance_satoshis " +
				"30000\n",
			wantExpfmt: `
# HELP waved_vtxos Number of VTXOs by status.
# TYPE waved_vtxos gauge
waved_vtxos{status="live"} 3
waved_vtxos{status="spent"} 2
waved_vtxos{status="unilateral_exit"} 1
# HELP waved_vtxos_value_satoshis Total VTXO value by status in satoshis.
# TYPE waved_vtxos_value_satoshis gauge
waved_vtxos_value_satoshis{status="live"} 30000
waved_vtxos_value_satoshis{status="spent"} 20000
waved_vtxos_value_satoshis{status="unilateral_exit"} 5000
`,
		},
		{
			name:          "empty inventory",
			rows:          []VTXOStatRow{},
			wantSpendable: "waved_spendable_balance_satoshis 0\n",
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
			wantSpendable: "waved_spendable_balance_satoshis 0\n",
			wantExpfmt: `
# HELP waved_vtxos Number of VTXOs by status.
# TYPE waved_vtxos gauge
waved_vtxos{status="forfeited"} 2
waved_vtxos{status="spent"} 4
# HELP waved_vtxos_value_satoshis Total VTXO value by status in satoshis.
# TYPE waved_vtxos_value_satoshis gauge
waved_vtxos_value_satoshis{status="forfeited"} 1000
waved_vtxos_value_satoshis{status="spent"} 9000
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
				"waved_vtxos", "waved_vtxos_value_satoshis",
				"waved_spendable_balance_satoshis",
			)
			require.NoError(t, err)
		})
	}
}

// fullMockQuerier is a SystemStatsQuerier double that returns fixed
// values for every data source, used to exercise the extended scrape
// gauges (wallet balance, block height, OOR sessions, rounds).
type fullMockQuerier struct {
	balance    WalletBalance
	height     int64
	oor        map[string]int64
	rounds     map[string]int64
	balanceErr error
	heightErr  error
}

func (m *fullMockQuerier) GetVTXOStatsByStatus(_ context.Context) (
	[]VTXOStatRow, error) {

	return nil, nil
}

func (m *fullMockQuerier) GetWalletBalance(_ context.Context) (WalletBalance,
	error) {

	return m.balance, m.balanceErr
}

func (m *fullMockQuerier) GetBlockHeight(_ context.Context) (int64, error) {
	return m.height, m.heightErr
}

func (m *fullMockQuerier) GetOORSessionStatsByState(_ context.Context) (
	map[string]int64, error) {

	return m.oor, nil
}

func (m *fullMockQuerier) GetRoundStatsByStatus(_ context.Context) (
	map[string]int64, error) {

	return m.rounds, nil
}

// TestSystemCollectorExtendedGauges verifies the wallet-balance,
// block-height, OOR-sessions, and rounds-by-status gauges are emitted
// from the querier, and that a wallet/height query error suppresses only
// its own gauges (not the others).
func TestSystemCollectorExtendedGauges(t *testing.T) {
	t.Parallel()

	t.Run("all present", func(t *testing.T) {
		t.Parallel()

		q := &fullMockQuerier{
			balance: WalletBalance{
				ConfirmedSat:   120000,
				UnconfirmedSat: 3000,
			},
			height: 850000,
			oor: map[string]int64{
				"pending": 2,
			},
			rounds: map[string]int64{
				"joined": 1,
			},
		}
		c := NewSystemCollector(q, fn.None[btclog.Logger]())

		const want = `
# HELP waved_wallet_confirmed_satoshis Confirmed on-chain wallet ` +
			`balance in satoshis.
# TYPE waved_wallet_confirmed_satoshis gauge
waved_wallet_confirmed_satoshis 120000
# HELP waved_wallet_unconfirmed_satoshis Unconfirmed on-chain wallet ` +
			`balance in satoshis.
# TYPE waved_wallet_unconfirmed_satoshis gauge
waved_wallet_unconfirmed_satoshis 3000
# HELP waved_block_height Best block height seen by the client's ` +
			`chain backend.
# TYPE waved_block_height gauge
waved_block_height 850000
# HELP waved_oor_sessions_by_state Number of currently-tracked OOR ` +
			`sessions by state.
# TYPE waved_oor_sessions_by_state gauge
waved_oor_sessions_by_state{state="pending"} 2
# HELP waved_rounds_by_status Number of currently-live rounds by ` +
			`status.
# TYPE waved_rounds_by_status gauge
waved_rounds_by_status{status="joined"} 1
`
		err := testutil.CollectAndCompare(
			c, strings.NewReader(want),
			"waved_wallet_confirmed_satoshis",
			"waved_wallet_unconfirmed_satoshis",
			"waved_block_height", "waved_oor_sessions_by_state",
			"waved_rounds_by_status",
		)
		require.NoError(t, err)
	})

	t.Run("balance and height errors skip only their gauges", func(
		t *testing.T) {

		t.Parallel()

		q := &fullMockQuerier{
			balanceErr: errors.New("wallet not ready"),
			heightErr:  errors.New("no chain backend"),
			oor: map[string]int64{
				"failed": 1,
			},
			rounds: map[string]int64{
				"confirmed": 3,
			},
		}
		c := NewSystemCollector(q, fn.None[btclog.Logger]())

		// The wallet and height gauges must be absent; the OOR and
		// round gauges must still be present.
		require.Zero(
			t, testutil.CollectAndCount(
				c, "waved_wallet_confirmed_satoshis",
				"waved_block_height",
			),
		)
		require.Equal(
			t, 2, testutil.CollectAndCount(
				c, "waved_oor_sessions_by_state",
				"waved_rounds_by_status",
			),
		)
	})
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

package metrics

import (
	"context"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// defaultCollectTimeout is the maximum duration for the VTXO store
	// queries issued during a single Prometheus scrape. It bounds a
	// scrape so a slow database cannot hold the HTTP handler open.
	defaultCollectTimeout = 5 * time.Second
)

// VTXOStatRow holds a single row of the VTXO inventory grouped by
// status. It mirrors the shape the server's collector consumes so the
// client and server dashboards can share query structure.
type VTXOStatRow struct {
	// Status is the VTXO status string (e.g. "live", "spending",
	// "spent", "forfeited", "unilateral_exit", "failed").
	Status string

	// Count is the number of VTXOs with this status.
	Count int64

	// TotalValue is the sum of VTXO values in satoshis for this
	// status.
	TotalValue int64
}

// VTXOStatsQuerier is the narrow interface for reading VTXO inventory.
// Implementations query the client's VTXO store; the interface keeps the
// metrics package free of any database dependency and makes the
// collector trivially testable with a mock.
type VTXOStatsQuerier interface {
	// GetVTXOStatsByStatus returns VTXO counts and total values
	// grouped by status.
	GetVTXOStatsByStatus(ctx context.Context) ([]VTXOStatRow, error)
}

// WalletBalance holds the client's on-chain wallet balance, split into
// confirmed and unconfirmed satoshis. This is the on-chain side of the
// client's funds (boarding deposits in flight, change, swept outputs) —
// distinct from the off-chain VTXO inventory the other gauges report.
type WalletBalance struct {
	// ConfirmedSat is the confirmed on-chain balance in satoshis.
	ConfirmedSat int64

	// UnconfirmedSat is the unconfirmed on-chain balance in satoshis.
	UnconfirmedSat int64
}

// SystemStatsQuerier is the full scrape-time data source the
// SystemCollector reads. It mirrors the lumosd server's querier so client
// and server dashboards share query structure. Each method is queried
// independently on every scrape; a method returning an error only
// suppresses its own samples for that scrape, never the whole endpoint.
// The "by state/status" gauges report LIVE, in-flight counts (the
// cumulative history lives in the _total counters), so they stay cheap
// and bounded at scrape time.
type SystemStatsQuerier interface {
	VTXOStatsQuerier

	// GetWalletBalance returns the client's on-chain wallet balance.
	// Implementations should return an error (so the scrape skips the
	// gauges) when the wallet is not yet ready.
	GetWalletBalance(ctx context.Context) (WalletBalance, error)

	// GetBlockHeight returns the best block height the client's chain
	// backend has seen. Returns an error when no chain backend is
	// available yet.
	GetBlockHeight(ctx context.Context) (int64, error)

	// GetOORSessionStatsByState returns a count of currently-tracked
	// OOR sessions grouped by state label.
	GetOORSessionStatsByState(ctx context.Context) (map[string]int64, error)

	// GetRoundStatsByStatus returns a count of currently-live rounds
	// grouped by status label.
	GetRoundStatsByStatus(ctx context.Context) (map[string]int64, error)
}

// Metric descriptors for the scrape-driven gauges.
var (
	vtxoCountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "vtxos"),
		"Number of VTXOs by status.", []string{"status"}, nil,
	)
	vtxoValueDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "vtxos_value_satoshis"),
		"Total VTXO value by status in satoshis.", []string{"status"},
		nil,
	)
	spendableBalanceDesc = prometheus.NewDesc(
		prometheus.BuildFQName(
			namespace, "", "spendable_balance_satoshis",
		),
		"Total value in satoshis of spendable (live) VTXOs.",
		nil,
		nil,
	)
	walletConfirmedDesc = prometheus.NewDesc(
		prometheus.BuildFQName(
			namespace, "", "wallet_confirmed_satoshis",
		),
		"Confirmed on-chain wallet balance in satoshis.",
		nil,
		nil,
	)
	walletUnconfirmedDesc = prometheus.NewDesc(
		prometheus.BuildFQName(
			namespace, "", "wallet_unconfirmed_satoshis",
		),
		"Unconfirmed on-chain wallet balance in satoshis.",
		nil,
		nil,
	)
	blockHeightDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "block_height"),
		"Best block height seen by the client's chain backend.", nil,
		nil,
	)
	oorSessionsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "oor_sessions_by_state"),
		"Number of currently-tracked OOR sessions by state.",
		[]string{"state"}, nil,
	)
	roundsByStatusDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "rounds_by_status"),
		"Number of currently-live rounds by status.",
		[]string{"status"}, nil,
	)
)

// liveStatus is the VTXO status label whose value is summed into the
// spendable balance gauge. It matches vtxo.VTXOStatusLive.String().
const liveStatus = "live"

// SystemCollector implements prometheus.Collector by querying the VTXO
// store on each Prometheus scrape. Querying at scrape time keeps the
// gauges fresh without a background ticker, matching the server's
// SystemCollector pattern.
type SystemCollector struct {
	querier SystemStatsQuerier
	log     btclog.Logger
}

// NewSystemCollector creates a collector that queries client system
// state on each Prometheus scrape.
func NewSystemCollector(querier SystemStatsQuerier,
	log fn.Option[btclog.Logger]) *SystemCollector {

	return &SystemCollector{
		querier: querier,
		log:     log.UnwrapOr(btclog.Disabled),
	}
}

// Describe sends the metric descriptors to the channel. It is called
// once during registration.
func (c *SystemCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- vtxoCountDesc
	ch <- vtxoValueDesc
	ch <- spendableBalanceDesc
	ch <- walletConfirmedDesc
	ch <- walletUnconfirmedDesc
	ch <- blockHeightDesc
	ch <- oorSessionsDesc
	ch <- roundsByStatusDesc
}

// Collect queries the client's live system state and emits the scrape
// gauges. It is called on each Prometheus scrape. Each metric group is
// queried independently: a failure in one group is logged and produces
// no samples for that group, rather than failing the whole endpoint or
// suppressing the other groups.
func (c *SystemCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(
		context.Background(), defaultCollectTimeout,
	)
	defer cancel()

	c.collectVTXOStats(ctx, ch)
	c.collectWalletBalance(ctx, ch)
	c.collectBlockHeight(ctx, ch)
	c.collectOORSessions(ctx, ch)
	c.collectRounds(ctx, ch)
}

// collectVTXOStats emits the VTXO inventory gauges (count and value by
// status) plus the derived spendable-balance gauge.
func (c *SystemCollector) collectVTXOStats(ctx context.Context,
	ch chan<- prometheus.Metric) {

	rows, err := c.querier.GetVTXOStatsByStatus(ctx)
	if err != nil {
		c.log.Warnf("VTXO stats query failed during scrape: %v", err)

		return
	}

	var spendable int64
	for _, row := range rows {
		ch <- prometheus.MustNewConstMetric(
			vtxoCountDesc, prometheus.GaugeValue,
			float64(row.Count), row.Status,
		)
		ch <- prometheus.MustNewConstMetric(
			vtxoValueDesc, prometheus.GaugeValue,
			float64(row.TotalValue), row.Status,
		)

		if row.Status == liveStatus {
			spendable += row.TotalValue
		}
	}

	ch <- prometheus.MustNewConstMetric(
		spendableBalanceDesc, prometheus.GaugeValue, float64(spendable),
	)
}

// collectWalletBalance emits the on-chain wallet balance gauges. The
// querier returns an error (skipping the gauges) when the wallet is not
// yet ready, so a locked or still-syncing daemon carries no stale
// sample.
func (c *SystemCollector) collectWalletBalance(ctx context.Context,
	ch chan<- prometheus.Metric) {

	bal, err := c.querier.GetWalletBalance(ctx)
	if err != nil {
		c.log.Debugf("Wallet balance query skipped during scrape: %v",
			err)

		return
	}

	ch <- prometheus.MustNewConstMetric(
		walletConfirmedDesc, prometheus.GaugeValue,
		float64(bal.ConfirmedSat),
	)
	ch <- prometheus.MustNewConstMetric(
		walletUnconfirmedDesc, prometheus.GaugeValue,
		float64(bal.UnconfirmedSat),
	)
}

// collectBlockHeight emits the chain-tip height gauge. The querier
// returns an error (skipping the gauge) when no chain backend is wired
// yet.
func (c *SystemCollector) collectBlockHeight(ctx context.Context,
	ch chan<- prometheus.Metric) {

	height, err := c.querier.GetBlockHeight(ctx)
	if err != nil {
		c.log.Debugf("Block height query skipped during scrape: %v",
			err)

		return
	}

	ch <- prometheus.MustNewConstMetric(
		blockHeightDesc, prometheus.GaugeValue, float64(height),
	)
}

// collectOORSessions emits the live OOR-sessions-by-state gauge.
func (c *SystemCollector) collectOORSessions(ctx context.Context,
	ch chan<- prometheus.Metric) {

	byState, err := c.querier.GetOORSessionStatsByState(ctx)
	if err != nil {
		c.log.Debugf("OOR session stats skipped during scrape: %v", err)

		return
	}

	for state, count := range byState {
		ch <- prometheus.MustNewConstMetric(
			oorSessionsDesc, prometheus.GaugeValue, float64(count),
			state,
		)
	}
}

// collectRounds emits the live rounds-by-status gauge.
func (c *SystemCollector) collectRounds(ctx context.Context,
	ch chan<- prometheus.Metric) {

	byStatus, err := c.querier.GetRoundStatsByStatus(ctx)
	if err != nil {
		c.log.Debugf("Round stats skipped during scrape: %v", err)

		return
	}

	for status, count := range byStatus {
		ch <- prometheus.MustNewConstMetric(
			roundsByStatusDesc, prometheus.GaugeValue,
			float64(count), status,
		)
	}
}

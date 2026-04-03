package metrics

import (
	"context"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// DefaultCollectInterval is the default interval between VTXO
	// gauge refreshes.
	DefaultCollectInterval = 30 * time.Second

	// defaultCollectTimeout is the maximum duration for DB and
	// wallet queries during a Prometheus scrape.
	defaultCollectTimeout = 5 * time.Second
)

// VTXOStatRow holds a single row from the VTXO stats query grouped
// by status. The field types mirror db/sqlc.GetVTXOStatsByStatusRow
// so the collector can be wired without importing the sqlc package.
type VTXOStatRow struct {
	// Status is the VTXO status string (e.g. "pending", "live",
	// "forfeited").
	Status string

	// Count is the number of VTXOs with this status.
	Count int64

	// TotalValue is the sum of VTXO values in satoshis for this
	// status.
	TotalValue int64
}

// StatusCountRow holds a single status/state → count pair from an
// aggregate SQL query (rounds by status, OOR sessions by state).
type StatusCountRow struct {
	// Status is the grouping label (e.g. "pending", "confirmed",
	// "cosigned", "finalized").
	Status string

	// Count is the number of rows with this status.
	Count int64
}

// WalletBalanceInfo holds confirmed and unconfirmed wallet balances
// in satoshis.
type WalletBalanceInfo struct {
	// Confirmed is the total confirmed balance in satoshis.
	Confirmed int64

	// Unconfirmed is the total unconfirmed balance in satoshis.
	Unconfirmed int64
}

// SystemStatsQuerier is the interface the collector needs to read
// system health statistics. Each method returns lightweight aggregate
// data; implementations may query the database, the LND wallet, or
// both.
type SystemStatsQuerier interface {
	// GetVTXOStatsByStatus returns VTXO counts and total values
	// grouped by status.
	GetVTXOStatsByStatus(ctx context.Context) ([]VTXOStatRow, error)

	// GetRoundStatsByStatus returns round counts grouped by
	// status (e.g. pending, confirmed).
	GetRoundStatsByStatus(ctx context.Context) ([]StatusCountRow, error)

	// GetOORSessionStatsByState returns OOR session counts
	// grouped by state (e.g. cosigned, finalized, failed).
	GetOORSessionStatsByState(
		ctx context.Context,
	) ([]StatusCountRow, error)

	// GetWalletBalance returns the operator wallet's confirmed
	// and unconfirmed balances.
	GetWalletBalance(ctx context.Context) (*WalletBalanceInfo, error)
}

// Metric descriptors for all scrape-driven gauges.
var (
	// VTXO gauges.
	vtxoCountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "vtxos"),
		"Number of VTXOs by status.",
		[]string{"status"}, nil,
	)
	vtxoValueDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "vtxos_value_satoshis"),
		"Total VTXO value by status in satoshis.",
		[]string{"status"}, nil,
	)

	// Round gauges.
	roundCountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "rounds_by_status"),
		"Number of rounds by status.",
		[]string{"status"}, nil,
	)

	// OOR session gauges.
	oorSessionCountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(
			namespace, "", "oor_sessions_by_state",
		),
		"Number of OOR sessions by state.",
		[]string{"state"}, nil,
	)

	// Wallet balance gauges.
	walletConfirmedDesc = prometheus.NewDesc(
		prometheus.BuildFQName(
			namespace, "", "wallet_confirmed_satoshis",
		),
		"Operator wallet confirmed balance in satoshis.",
		nil, nil,
	)
	walletUnconfirmedDesc = prometheus.NewDesc(
		prometheus.BuildFQName(
			namespace, "", "wallet_unconfirmed_satoshis",
		),
		"Operator wallet unconfirmed balance in satoshis.",
		nil, nil,
	)
)

// SystemCollector implements prometheus.Collector by querying the
// database and wallet on each Prometheus scrape. This ensures gauge
// values are always fresh at scrape time rather than relying on a
// periodic ticker.
type SystemCollector struct {
	querier SystemStatsQuerier
	log     btclog.Logger
}

// NewSystemCollector creates a new collector that queries system
// stats on each Prometheus scrape.
func NewSystemCollector(querier SystemStatsQuerier,
	log fn.Option[btclog.Logger]) *SystemCollector {

	return &SystemCollector{
		querier: querier,
		log:     log.UnwrapOr(btclog.Disabled),
	}
}

// Describe sends the metric descriptors to the channel. This is
// called once during registration.
func (c *SystemCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- vtxoCountDesc
	ch <- vtxoValueDesc
	ch <- roundCountDesc
	ch <- oorSessionCountDesc
	ch <- walletConfirmedDesc
	ch <- walletUnconfirmedDesc
}

// Collect queries the database and wallet for current statistics and
// sends the resulting metrics to the channel. Called on each
// Prometheus scrape. Each data source is queried independently so a
// failure in one doesn't block the others.
func (c *SystemCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(
		context.Background(), defaultCollectTimeout,
	)
	defer cancel()

	c.collectVTXOStats(ctx, ch)
	c.collectRoundStats(ctx, ch)
	c.collectOORStats(ctx, ch)
	c.collectWalletBalance(ctx, ch)
}

// collectVTXOStats queries VTXO counts and values by status.
func (c *SystemCollector) collectVTXOStats(ctx context.Context,
	ch chan<- prometheus.Metric) {

	rows, err := c.querier.GetVTXOStatsByStatus(ctx)
	if err != nil {
		c.log.Warnf("VTXO stats query failed during scrape: %v",
			err)
		return
	}

	for _, row := range rows {
		ch <- prometheus.MustNewConstMetric(
			vtxoCountDesc, prometheus.GaugeValue,
			float64(row.Count), row.Status,
		)
		ch <- prometheus.MustNewConstMetric(
			vtxoValueDesc, prometheus.GaugeValue,
			float64(row.TotalValue), row.Status,
		)
	}
}

// collectRoundStats queries round counts by status.
func (c *SystemCollector) collectRoundStats(ctx context.Context,
	ch chan<- prometheus.Metric) {

	rows, err := c.querier.GetRoundStatsByStatus(ctx)
	if err != nil {
		c.log.Warnf("Round stats query failed during scrape: %v",
			err)
		return
	}

	for _, row := range rows {
		ch <- prometheus.MustNewConstMetric(
			roundCountDesc, prometheus.GaugeValue,
			float64(row.Count), row.Status,
		)
	}
}

// collectOORStats queries OOR session counts by state.
func (c *SystemCollector) collectOORStats(ctx context.Context,
	ch chan<- prometheus.Metric) {

	rows, err := c.querier.GetOORSessionStatsByState(ctx)
	if err != nil {
		c.log.Warnf("OOR session stats query failed during "+
			"scrape: %v", err)
		return
	}

	for _, row := range rows {
		ch <- prometheus.MustNewConstMetric(
			oorSessionCountDesc, prometheus.GaugeValue,
			float64(row.Count), row.Status,
		)
	}
}

// collectWalletBalance queries the operator wallet balance.
func (c *SystemCollector) collectWalletBalance(ctx context.Context,
	ch chan<- prometheus.Metric) {

	balance, err := c.querier.GetWalletBalance(ctx)
	if err != nil {
		c.log.Warnf("Wallet balance query failed during "+
			"scrape: %v", err)
		return
	}

	ch <- prometheus.MustNewConstMetric(
		walletConfirmedDesc, prometheus.GaugeValue,
		float64(balance.Confirmed),
	)
	ch <- prometheus.MustNewConstMetric(
		walletUnconfirmedDesc, prometheus.GaugeValue,
		float64(balance.Unconfirmed),
	)
}

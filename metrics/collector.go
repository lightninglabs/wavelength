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

// VTXOStatsQuerier is the narrow interface the scrape-driven collector
// needs to read VTXO inventory. Implementations query the client's VTXO
// store; the interface keeps the metrics package free of any database
// dependency and makes the collector trivially testable with a mock.
type VTXOStatsQuerier interface {
	// GetVTXOStatsByStatus returns VTXO counts and total values
	// grouped by status.
	GetVTXOStatsByStatus(ctx context.Context) ([]VTXOStatRow, error)
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
		"Total value in satoshis of spendable (live) VTXOs.", nil, nil,
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
	querier VTXOStatsQuerier
	log     btclog.Logger
}

// NewSystemCollector creates a collector that queries VTXO inventory on
// each Prometheus scrape.
func NewSystemCollector(querier VTXOStatsQuerier,
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
}

// Collect queries the VTXO store and emits the inventory metrics. It is
// called on each Prometheus scrape. A query failure is logged and
// produces no samples for the affected scrape rather than failing the
// whole endpoint.
func (c *SystemCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(
		context.Background(), defaultCollectTimeout,
	)
	defer cancel()

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
		spendableBalanceDesc, prometheus.GaugeValue,
		float64(spendable),
	)
}

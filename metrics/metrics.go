package metrics

import (
	"errors"

	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// namespace is the Prometheus namespace prefixing every waved
	// client metric. It mirrors the daemon's own product name (the
	// "waved" binary and the WAVED env prefix) so dashboards group
	// client metrics under the same identifier operators already use.
	namespace = "waved"
)

var (
	// RoundsJoinedTotal counts the rounds the client has attempted to
	// join, regardless of eventual outcome. Pairs with
	// RoundsCompletedTotal to derive a join-to-completion ratio.
	RoundsJoinedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "rounds_joined_total",
			Help:      "Total rounds the client attempted to join.",
		},
	)

	// RoundsCompletedTotal counts settlement rounds the client
	// finished, labelled by outcome so operators can alert on a
	// sustained failure rate.
	RoundsCompletedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "rounds_completed_total",
			Help: "Total settlement rounds completed by " +
				"outcome.",
		},
		[]string{"status"},
	)

	// OORTransfersSentTotal counts out-of-round (async) transfers the
	// client originated, labelled by outcome.
	OORTransfersSentTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "oor_transfers_sent_total",
			Help: "Total out-of-round transfers sent by " +
				"outcome.",
		},
		[]string{"status"},
	)

	// OORTransfersReceivedTotal counts incoming out-of-round transfers
	// the client materialized, labelled by outcome.
	OORTransfersReceivedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "oor_transfers_received_total",
			Help: "Total out-of-round transfers received by " +
				"outcome.",
		},
		[]string{"status"},
	)

	// BoardingEventsTotal counts boarding (on-chain to VTXO) intents
	// the client submitted, labelled by outcome.
	BoardingEventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "boarding_events_total",
			Help:      "Total boarding events by outcome.",
		},
		[]string{"status"},
	)

	// ServerSyncTimestamp records the Unix time of the last poll that
	// observed the direct gRPC connection to the ark operator in the
	// Ready transport state. It is a transport-liveness signal, not a
	// completed application round-trip: a stale value relative to
	// wall-clock time signals the client has lost transport contact
	// with the server.
	ServerSyncTimestamp = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "server_sync_timestamp_seconds",
			Help: "Unix timestamp of the last poll that observed " +
				"the ark operator connection in the Ready " +
				"transport state.",
		},
	)

	// ServerConnectionUp is 1 while the direct gRPC connection to the
	// ark operator is believed healthy and 0 otherwise.
	ServerConnectionUp = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "server_connection_up",
			Help: "1 when the direct gRPC connection to the ark " +
				"operator is up, 0 otherwise.",
		},
	)

	// BackgroundTaskErrorsTotal counts errors hit by daemon-owned
	// background tasks (sync loops, watchers), labelled by task so
	// operators can locate a failing subsystem.
	BackgroundTaskErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "background_task_errors_total",
			Help:      "Background task errors by task name.",
		},
		[]string{"task"},
	)

	// OORTransferDurationSeconds observes the wall-clock duration of
	// outgoing OOR (async) transfers from the SendOOR call entry to its
	// terminal outcome, labelled by status. The duration is measured at
	// the call site and carried on the metric message so the metrics
	// actor stays stateless. Buckets mirror the lumosd server's OOR
	// transfer histogram for dashboard consistency.
	OORTransferDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "oor_transfer_duration_seconds",
			Help: "Duration of outgoing OOR transfers in " +
				"seconds by outcome.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 11),
		},
		[]string{"status"},
	)

	// GRPCClientMetrics provides per-method client-side gRPC metrics
	// (request count, error rate, handling-time histograms) for calls
	// the client makes to the ark operator. Wired as unary and stream
	// interceptors on the operator gRPC connection.
	GRPCClientMetrics = grpcprom.NewClientMetrics(
		grpcprom.WithClientCounterOptions(
			grpcprom.WithNamespace(namespace),
		),
		grpcprom.WithClientHandlingTimeHistogram(
			grpcprom.WithHistogramNamespace(namespace),
			grpcprom.WithHistogramBuckets(
				prometheus.ExponentialBuckets(
					0.001, 2, 16,
				),
			),
		),
	)
)

// allCollectors returns every event-driven waved client metric
// collector for registration. The scrape-driven SystemCollector is
// registered separately by the caller because it needs a querier.
func allCollectors() []prometheus.Collector {
	return []prometheus.Collector{
		RoundsJoinedTotal,
		RoundsCompletedTotal,
		OORTransfersSentTotal,
		OORTransfersReceivedTotal,
		OORTransferDurationSeconds,
		BoardingEventsTotal,
		ServerSyncTimestamp,
		ServerConnectionUp,
		BackgroundTaskErrorsTotal,
		GRPCClientMetrics,
	}
}

// RegisterAll registers all event-driven waved client metrics with
// the given registerer. Typically called with
// prometheus.DefaultRegisterer during daemon startup. Duplicate
// registrations are tolerated so multiple daemons sharing a test
// process do not panic.
func RegisterAll(reg prometheus.Registerer) {
	for _, c := range allCollectors() {
		err := reg.Register(c)
		if err == nil {
			continue
		}

		// Ignore duplicate registration errors so multiple Server
		// instances in the same test process don't panic.
		var alreadyReg prometheus.AlreadyRegisteredError
		if !errors.As(err, &alreadyReg) {
			panic(err)
		}
	}
}

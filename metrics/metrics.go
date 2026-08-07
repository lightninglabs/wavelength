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

	// ServerConnLastIngressPollTimestamp records the Unix time of the last
	// Pull that returned to the connector's ingress loop. That loop is the
	// process's only consumer of the server mailbox, so this is the
	// liveness signal for the loop itself: an idle client keeps it fresh at
	// the long-poll cadence, and a loop that has parked or died stops
	// updating it immediately.
	//
	// It answers "is the loop still looping", which is narrower than "is
	// the client still hearing the operator". A target actor that has
	// stopped draining leaves this gauge fresh — the loop keeps polling and
	// re-pulling the envelope it cannot deliver — and shows up on
	// ServerConnIngressDeferredTotal instead. Keeping the two separable is
	// the reason this is stamped before dispatch rather than after.
	//
	// It is set directly by serverconn rather than through the metrics
	// actor, for the same reason as the two gauges above: the connector
	// owns no metrics sink, and the ingress loop is precisely the goroutine
	// that must not acquire a new actor edge to report that it is alive.
	ServerConnLastIngressPollTimestamp = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name: "serverconn_last_ingress_poll_timestamp_" +
				"seconds",
			Help: "Unix timestamp of the last Pull that returned " +
				"to the connector ingress loop.",
		},
	)

	// ServerConnLastIngressEventTimestamp records the Unix time of the last
	// pulled batch the ingress loop delivered and committed. Unlike the
	// poll gauge it only advances on real inbound traffic, so on its own it
	// cannot tell an idle client from a wedged one; read together, a fresh
	// poll stamp with a stale event stamp says the loop is running but
	// dispatch is not getting through.
	ServerConnLastIngressEventTimestamp = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name: "serverconn_last_ingress_event_timestamp_" +
				"seconds",
			Help: "Unix timestamp of the last inbound envelope " +
				"batch the connector ingress loop committed.",
		},
	)

	// ServerConnIngressDeferredTotal counts the redrives a full target
	// mailbox turned away, labelled by the route of the envelope that was
	// refused. A non-zero rate means a local actor has stopped keeping up
	// with the server; a rate that does not fall back to zero means it has
	// stopped entirely, and this is the only signal that fires for that —
	// the poll gauge above stays fresh throughout, because the loop is
	// still looping.
	//
	// It counts refused redrives rather than refused envelopes because
	// those are the only refusals that exist: the loop stops at the first
	// envelope the target will not take, so whatever is queued behind it is
	// never attempted and cannot be counted.
	ServerConnIngressDeferredTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "serverconn_ingress_dispatch_deferred_total",
			Help: "Ingress redrives refused by a full target " +
				"actor mailbox, by the refused envelope's " +
				"route.",
		},
		[]string{"service", "method"},
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

	// DeadLettersObservedTotal counts dead letters as the dead-letter
	// monitor first observes them. The waved_dead_letters gauge reports
	// what is parked right now (and falls when entries are requeued or
	// purged); this counter is the monotone history a rate() alert needs
	// to catch messages that were parked and later cleared between
	// scrapes.
	//
	// Deliberately unlabelled: actor IDs include per-session durable
	// actors, so an actor_id label would accumulate one counter child
	// per session for the process lifetime. Per-actor attribution lives
	// on the self-limiting waved_actor_dead_letters gauge and in the
	// monitor's log lines.
	DeadLettersObservedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "dead_letters_observed_total",
			Help: "Dead-lettered messages observed by the " +
				"monitor.",
		},
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
		ServerConnLastIngressPollTimestamp,
		ServerConnLastIngressEventTimestamp,
		ServerConnIngressDeferredTotal,
		BackgroundTaskErrorsTotal,
		DeadLettersObservedTotal,
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

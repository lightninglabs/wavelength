package metrics

import (
	"errors"

	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// namespace is the Prometheus namespace for all arkd metrics.
	namespace = "arkd"
)

var (
	// RoundsTotal counts the total number of completed rounds
	// by outcome (confirmed or failed).
	RoundsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "rounds_total",
			Help: "Total completed rounds by " +
				"outcome.",
		},
		[]string{"status"},
	)

	// RoundsCreated counts the total number of rounds that have
	// been created, including those still in progress.
	RoundsCreated = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "rounds_created_total",
			Help:      "Total rounds created.",
		},
	)

	// RoundsActive tracks the number of in-progress rounds.
	RoundsActive = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "rounds_active",
			Help:      "Number of currently active rounds.",
		},
	)

	// RoundDurationSeconds records the duration of each completed
	// round from creation to confirmation or failure.
	RoundDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "round_duration_seconds",
			Help:      "Duration of completed rounds in seconds.",
			Buckets:   prometheus.ExponentialBuckets(1, 2, 12),
		},
		[]string{"status"},
	)

	// RoundTicksTotal counts periodic round-tick fires by outcome.
	// "sealed" means the tick passed the participants + seal-predicate
	// gate and the round was sealed. "skipped_empty" means no clients
	// had registered yet so the tick was a no-op. "skipped_predicate"
	// means at least one client had joined but the configured seal
	// predicate rejected the current registrations. Operators alert on
	// a sustained skipped_empty rate to detect stuck rounds.
	RoundTicksTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "round_ticks_total",
			Help:      "Periodic round-tick outcomes.",
		},
		[]string{"result"},
	)

	// RoundChangeRequiredForBoardingTotal counts rounds that failed
	// because LND's coin selection produced no change output while
	// boarding inputs were present. The round needs change to absorb
	// the witness-weight delta fee (LND under-charges boarding inputs
	// because EstimateInputWeight rejects taproot script-path
	// externals). Operators should alert on a sustained rate here:
	// each increment indicates an operator-side liquidity gap that
	// will keep failing rounds until the hot wallet is topped up so
	// coin selection produces change.
	RoundChangeRequiredForBoardingTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "round_change_required_for_boarding_total",
			Help: "Rounds failed because FundPsbt returned no " +
				"change output while boarding inputs were " +
				"present. Operator should top up hot wallet.",
		},
	)

	// OORTransfersTotal counts completed out-of-round transfers by
	// outcome (finalized or failed).
	OORTransfersTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "oor_transfers_total",
			Help:      "Total out-of-round transfers by outcome.",
		},
		[]string{"status"},
	)

	// ConnectedClients tracks the number of currently connected
	// mailbox clients.
	ConnectedClients = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "connected_clients",
			Help: "Number of currently connected " +
				"mailbox clients.",
		},
	)

	// BlockHeight tracks the latest block height seen by the chain
	// backend.
	BlockHeight = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "block_height",
			Help: "Latest block height observed " +
				"by the chain backend.",
		},
	)

	// RoundBatchBuildDuration records the time spent building the
	// batch commitment transaction (wallet funding, tree
	// construction, fee estimation).
	RoundBatchBuildDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "round_batch_build_duration_seconds",
			Help: "Time spent building the batch commitment " +
				"transaction.",
			Buckets: prometheus.ExponentialBuckets(
				0.1, 2, 12,
			),
		},
		[]string{"status"},
	)

	// RoundRegistrationDuration records the time from round
	// creation to registration seal (timeout or predicate).
	RoundRegistrationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "round_registration_duration_seconds",
			Help: "Time from round creation to registration " +
				"seal.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 10),
		},
		[]string{"status"},
	)

	// RoundNonceExchangeDuration records the time spent waiting
	// for all clients to submit VTXO nonces.
	RoundNonceExchangeDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name: "round_vtxo_nonce_exchange_duration" +
				"_seconds",
			Help: "Time waiting for VTXO nonce submissions " +
				"from all clients.",
			Buckets: prometheus.ExponentialBuckets(
				0.1, 2, 10,
			),
		},
	)

	// RoundInputSigDuration records the time spent waiting for
	// input signatures from all participating clients.
	RoundInputSigDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name: "round_input_sig_collection_duration" +
				"_seconds",
			Help: "Time waiting for input signature " +
				"submissions from all clients.",
			Buckets: prometheus.ExponentialBuckets(
				0.1, 2, 10,
			),
		},
		[]string{"status"},
	)

	// RoundClientsJoined records the number of clients that
	// participated in each completed round.
	RoundClientsJoined = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "round_clients_joined",
			Help: "Number of clients per completed " +
				"round.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 10),
		},
		[]string{"status"},
	)

	// RoundBoardingInputs records the number of boarding inputs
	// per completed round.
	RoundBoardingInputs = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "round_boarding_inputs",
			Help: "Boarding inputs per completed " +
				"round.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 8),
		},
		[]string{"status"},
	)

	// RoundLeaveOutputs records the number of leave (withdrawal)
	// outputs per completed round.
	RoundLeaveOutputs = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "round_leave_outputs",
			Help: "Number of leave outputs per completed " +
				"round.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 8),
		},
		[]string{"status"},
	)

	// RoundVTXOsGenerated records the number of VTXOs generated
	// per completed round.
	RoundVTXOsGenerated = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "round_vtxos_generated",
			Help: "Number of VTXOs generated per completed " +
				"round.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 12),
		},
		[]string{"status"},
	)

	// OORTransferDuration records the end-to-end duration of OOR
	// transfers from submit to finalize or failure.
	OORTransferDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "oor_transfer_duration_seconds",
			Help: "End-to-end duration of OOR transfers " +
				"in seconds.",
			Buckets: prometheus.ExponentialBuckets(
				0.05, 2, 12,
			),
		},
		[]string{"status"},
	)

	// VTXOLockDuration records how long VTXO lock acquisitions
	// take.
	VTXOLockDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "vtxo_lock_duration_seconds",
			Help: "Time spent acquiring VTXO locks in " +
				"seconds.",
			Buckets: prometheus.ExponentialBuckets(
				0.001, 2, 12,
			),
		},
		[]string{"owner"},
	)

	// VTXOLockFailures counts failed VTXO lock attempts by
	// reason.
	VTXOLockFailures = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "vtxo_lock_failures_total",
			Help:      "Total failed VTXO lock attempts by reason.",
		},
		[]string{"reason"},
	)

	// DispatchDuration records per-method envelope dispatch
	// latency in the clientconn ingress loop.
	DispatchDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name: "clientconn_dispatch_duration" +
				"_seconds",
			Help: "Per-method envelope dispatch latency " +
				"in seconds.",
			Buckets: prometheus.ExponentialBuckets(
				0.001, 2, 12,
			),
		},
		[]string{"service_method"},
	)

	// GRPCServerMetrics provides per-method gRPC server metrics
	// (request count, error rate, handling time histograms). Both
	// the admin and client RPC servers use this shared instance
	// for their unary and stream interceptors.
	GRPCServerMetrics = grpcprom.NewServerMetrics(
		grpcprom.WithServerCounterOptions(
			grpcprom.WithNamespace(namespace),
		),
		grpcprom.WithServerHandlingTimeHistogram(
			grpcprom.WithHistogramNamespace(namespace),
			grpcprom.WithHistogramBuckets(
				prometheus.ExponentialBuckets(
					0.001, 2, 16,
				),
			),
		),
	)
)

// RegisterAll registers all arkd metrics with the given registerer.
// Typically called with prometheus.DefaultRegisterer during server
// startup. The shared GRPCServerMetrics collector is also registered
// so per-method request count, error rate, and handling time
// histograms are available for both admin and client RPC servers.
// allCollectors returns all arkd metric collectors for
// registration.
func allCollectors() []prometheus.Collector {
	return []prometheus.Collector{
		RoundsTotal,
		RoundsCreated,
		RoundsActive,
		RoundTicksTotal,
		RoundChangeRequiredForBoardingTotal,
		RoundDurationSeconds,
		RoundBatchBuildDuration,
		RoundRegistrationDuration,
		RoundNonceExchangeDuration,
		RoundInputSigDuration,
		RoundClientsJoined,
		RoundBoardingInputs,
		RoundLeaveOutputs,
		RoundVTXOsGenerated,
		OORTransfersTotal,
		OORTransferDuration,
		ConnectedClients,
		BlockHeight,
		VTXOLockDuration,
		VTXOLockFailures,
		DispatchDuration,
		GRPCServerMetrics,
	}
}

// RegisterAll registers all arkd metrics with the given registerer.
// Typically called with prometheus.DefaultRegisterer during server
// startup. In integration tests where multiple servers share a
// process, duplicate registrations are silently ignored.
func RegisterAll(reg prometheus.Registerer) {
	for _, c := range allCollectors() {
		err := reg.Register(c)
		if err == nil {
			continue
		}

		// Ignore duplicate registration errors so multiple
		// Server instances in the same test process don't
		// panic.
		var alreadyReg prometheus.AlreadyRegisteredError
		if !errors.As(err, &alreadyReg) {
			panic(err)
		}
	}
}

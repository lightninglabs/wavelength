package darepod

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/metrics"
	"github.com/lightninglabs/darepo-client/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/prometheus/client_golang/prometheus"
)

// metricsActorWorkers is the size of the metrics actor pool spawned by
// startMetricsServer. The metrics actor is stateless (it only increments
// Prometheus counters, which are concurrency-safe), so multiple workers
// drain metric events in parallel behind the round-robin sink router.
const metricsActorWorkers = 4

// startMetricsServer registers the daemon's Prometheus collectors and
// starts the optional /metrics HTTP server. It is a no-op when no metrics
// listen address is configured: RegisterAll and the SystemCollector are
// skipped entirely so a disabled daemon carries no collector wiring. The
// scrape-driven SystemCollector reads VTXO inventory through the store
// available by this point in startup.
func (s *Server) startMetricsServer(ctx context.Context) error {
	log := s.subLogger(MetricsSubsystem)

	if s.cfg.Metrics.ListenAddr == "" {
		log.DebugS(ctx, "Metrics server disabled")

		return nil
	}

	// Use an isolated registry per daemon instance rather than the
	// global prometheus.DefaultRegisterer. Multiple daemons in one
	// process (integration and unit tests) and a stopped-then-restarted
	// daemon would otherwise collide on AlreadyRegisteredError; in
	// particular the scrape-driven SystemCollector holds a reference to
	// this daemon's vtxoStore, so a silently-dropped re-registration
	// would leave /metrics querying the prior daemon's (possibly closed)
	// store.
	reg := prometheus.NewRegistry()

	// Register the event-driven collectors and the gRPC client metrics
	// on the registry that the /metrics handler serves.
	metrics.RegisterAll(reg)

	// Spawn a small pool of metrics actors that own all event-driven
	// counters and register them under one service key, mirroring the
	// arkd server's design. Lifecycle emission sites Tell through the
	// sink rather than touching Prometheus directly. The actors are
	// in-memory (no durable mailbox): a dropped metric event is
	// acceptable, and the actor system shutdown registered during
	// startup drains them.
	//
	// The actor is stateless — it only increments Prometheus counters,
	// which are internally synchronized — so several workers can drain
	// metric events concurrently without any locking. NewSink resolves
	// the service key to a round-robin router (the framework default),
	// so a producer's Tells fan out across the pool with no change at
	// the call sites. A small pool keeps a burst of lifecycle events
	// from queueing behind a single mailbox while adding negligible
	// overhead when idle.
	for i := 0; i < metricsActorWorkers; i++ {
		metricsActor := metrics.NewMetricsActor(metrics.ActorConfig{
			Log: fn.Some(log),
		})
		actor.RegisterWithSystem(
			s.actorSystem,
			fmt.Sprintf("%s-%d", metrics.ActorName, i),
			metrics.ActorKey, metricsActor,
		)
	}
	s.metricsSink = fn.Some(metrics.NewSink(s.actorSystem))

	// Register the scrape-driven VTXO inventory collector, backed by the
	// daemon's VTXO store via the adapter.
	collector := metrics.NewSystemCollector(
		&vtxoStatsAdapter{
			store: s.vtxoStore,
		},
		fn.Some(log),
	)
	// The registry is freshly created per daemon, so the collector
	// cannot collide; any registration error is a real fault.
	if err := reg.Register(collector); err != nil {
		return fmt.Errorf("register VTXO collector: %w", err)
	}

	s.metricsSrv = metrics.NewServer(s.cfg.Metrics, log, reg)

	return s.metricsSrv.Start(ctx)
}

// stopMetricsServer gracefully shuts down the metrics HTTP server. It is
// a no-op when the server was never started.
func (s *Server) stopMetricsServer(ctx context.Context) error {
	if s.metricsSrv == nil {
		return nil
	}

	return s.metricsSrv.Stop(ctx)
}

// emitMetric forwards a metric event to the metrics actor through the
// sink. It is a no-op when metrics are disabled (sink is None) and never
// returns an error to callers: instrumentation is a side observation and
// must never block or fail the operation being recorded. A Tell failure
// is logged at debug level only.
func (s *Server) emitMetric(ctx context.Context, msg metrics.Msg) {
	s.metricsSink.WhenSome(func(sink metrics.Sink) {
		if err := sink.Tell(ctx, msg); err != nil {
			s.log.DebugS(ctx, "Failed to emit metric event",
				err)
		}
	})
}

// vtxoStatsAdapter implements metrics.VTXOStatsQuerier on top of the
// daemon's VTXO persistence store. The metrics package stays free of any
// database dependency; this adapter is the single seam translating the
// store's per-status listing into the aggregate rows the scrape-driven
// collector expects.
type vtxoStatsAdapter struct {
	store *db.VTXOPersistenceStore
}

// allVTXOStatuses is the set of VTXO statuses the metrics collector
// reports. Each maps to one labelled gauge sample per scrape. The list
// is explicit (rather than derived) so adding a new status enum value is
// a deliberate dashboard-affecting change.
var allVTXOStatuses = []vtxo.VTXOStatus{
	vtxo.VTXOStatusLive,
	vtxo.VTXOStatusPendingForfeit,
	vtxo.VTXOStatusForfeiting,
	vtxo.VTXOStatusForfeited,
	vtxo.VTXOStatusSpent,
	vtxo.VTXOStatusUnilateralExit,
	vtxo.VTXOStatusFailed,
	vtxo.VTXOStatusSpending,
}

// GetVTXOStatsByStatus returns VTXO counts and total values grouped by
// status. It queries the store once per status and aggregates each
// listing into a single row. The store has no aggregate query, so this
// adapter does the grouping in Go; the per-status VTXO set is small
// enough for a client wallet that this stays cheap at scrape time.
func (a *vtxoStatsAdapter) GetVTXOStatsByStatus(ctx context.Context) (
	[]metrics.VTXOStatRow, error) {

	rows := make([]metrics.VTXOStatRow, 0, len(allVTXOStatuses))
	for _, status := range allVTXOStatuses {
		descs, err := a.store.ListVTXOsByStatus(ctx, status)
		if err != nil {
			return nil, fmt.Errorf("list VTXOs by status %s: %w",
				status, err)
		}

		// Skip statuses with no VTXOs so the scrape only carries
		// label values that currently exist, keeping cardinality
		// proportional to live inventory.
		if len(descs) == 0 {
			continue
		}

		var total int64
		for _, desc := range descs {
			total += int64(desc.Amount)
		}

		rows = append(rows, metrics.VTXOStatRow{
			Status:     status.String(),
			Count:      int64(len(descs)),
			TotalValue: total,
		})
	}

	return rows, nil
}

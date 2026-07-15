package metrics

import (
	"context"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// ActorName is the actor-system identifier and service-key name for
	// the metrics actor.
	ActorName = "metrics"
)

// ActorKey is the service key the metrics actor registers under. Other
// actors and the daemon resolve this key to obtain a Sink for
// fire-and-forget metric events.
var ActorKey = actor.NewServiceKey[Msg, Resp](ActorName)

// Sink is the fire-and-forget reference producers hold to forward metric
// events to the metrics actor. It is Tell-only because Resp is always
// nil; callers Tell, never Ask. Mirrors ledger.Sink so subsystem configs
// can embed an fn.Option[metrics.Sink] the same way they embed an
// fn.Option[ledger.Sink].
type Sink = actor.TellOnlyRef[Msg]

// NewSink resolves the metrics service key against the supplied actor
// system and returns a Tell-only reference suitable for embedding in
// subsystem actor configs. A Tell on a system with no registered metrics
// actor returns an error, which callers should log but never propagate:
// instrumentation is a side observation, never a precondition for the
// operation being recorded.
func NewSink(system *actor.ActorSystem) Sink {
	return ActorKey.Ref(system)
}

// ActorConfig holds configuration for the metrics actor.
type ActorConfig struct {
	// Log is an optional logger for the metrics actor.
	Log fn.Option[btclog.Logger]
}

// MetricsActor implements actor.ActorBehavior for centralized
// event-driven Prometheus instrumentation. All lifecycle counters live
// behind this single actor; subsystems Tell typed messages rather than
// touching Prometheus directly, so every event-driven update happens in
// one auditable place (matching the lumosd server's MetricsActor).
type MetricsActor struct {
	cfg ActorConfig
	log btclog.Logger
}

// NewMetricsActor creates a new metrics actor with the given config.
func NewMetricsActor(cfg ActorConfig) *MetricsActor {
	return &MetricsActor{
		cfg: cfg,
		log: cfg.Log.UnwrapOr(btclog.Disabled),
	}
}

// Receive processes a metric message and updates the corresponding
// Prometheus counter. This is the single place where event-driven
// Prometheus instrumentation happens. Scrape-driven gauges (VTXO
// inventory) are handled separately by SystemCollector.
func (a *MetricsActor) Receive(_ context.Context, msg Msg) fn.Result[Resp] {
	switch m := msg.(type) {
	case *RoundJoinedMsg:
		RoundsJoinedTotal.Inc()

	case *RoundCompletedMsg:
		RoundsCompletedTotal.WithLabelValues(m.Status).Inc()

	case *OORTransferSentMsg:
		OORTransfersSentTotal.WithLabelValues(m.Status).Inc()

		// Observe the transfer duration when the call site measured
		// one. A zero duration means the producer did not time the
		// call, so it is left unobserved rather than skewing the
		// histogram toward zero.
		if m.Duration > 0 {
			OORTransferDurationSeconds.WithLabelValues(
				m.Status,
			).Observe(m.Duration.Seconds())
		}

	case *OORTransferReceivedMsg:
		OORTransfersReceivedTotal.WithLabelValues(m.Status).Inc()

	case *BoardingEventMsg:
		BoardingEventsTotal.WithLabelValues(m.Status).Inc()

	case *BackgroundTaskErrorMsg:
		BackgroundTaskErrorsTotal.WithLabelValues(m.Task).Inc()

	default:
		a.log.Warnf("Unknown metrics message type: %T", msg)
	}

	return fn.Ok[Resp](nil)
}

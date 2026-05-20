package metrics

import (
	"context"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// ActorName is the actor system identifier for the metrics
	// actor.
	ActorName = "metrics"
)

// RoundID is a type alias for round identifiers used as map keys
// in the metrics actor's internal tracking state.
type RoundID = string

// SessionID is a type alias for OOR session identifiers used as
// map keys in the metrics actor's internal tracking state.
type SessionID = string

// PhaseName is a type alias for round phase names used as map keys
// in the metrics actor's internal tracking state.
type PhaseName = string

// ActorKey is the service key for the metrics actor. Other actors
// use this to discover and send metric events.
var ActorKey = actor.NewServiceKey[Msg, Resp](ActorName)

// ActorConfig holds configuration for the metrics actor.
type ActorConfig struct {
	// Log is an optional logger for the metrics actor.
	Log fn.Option[btclog.Logger]
}

// MetricsActor implements ActorBehavior for centralized Prometheus
// instrumentation. All subsystem actors send typed metric messages
// here instead of calling Prometheus directly. All metric-specific
// aggregation state lives here, not in the subsystem actors.
type MetricsActor struct {
	cfg ActorConfig
	log btclog.Logger

	// roundStartTimes tracks when each round was created for
	// overall duration metrics.
	roundStartTimes map[RoundID]time.Time

	// roundPhaseTimes tracks per-round, per-phase start times
	// for phase duration histograms. Outer key is round ID,
	// inner key is phase name.
	roundPhaseTimes map[RoundID]map[PhaseName]time.Time

	// roundClientCounts tracks the number of clients that joined
	// each round for the per-round size histogram.
	roundClientCounts map[RoundID]int

	// oorStartTimes tracks when each OOR transfer session began
	// for end-to-end duration metrics.
	oorStartTimes map[SessionID]time.Time
}

// NewMetricsActor creates a new metrics actor with the given config.
func NewMetricsActor(cfg ActorConfig) *MetricsActor {
	return &MetricsActor{
		cfg:               cfg,
		log:               cfg.Log.UnwrapOr(btclog.Disabled),
		roundStartTimes:   make(map[RoundID]time.Time),
		roundPhaseTimes:   make(map[RoundID]map[PhaseName]time.Time),
		roundClientCounts: make(map[RoundID]int),
		oorStartTimes:     make(map[SessionID]time.Time),
	}
}

// Receive processes a metric message and updates the corresponding
// Prometheus counters, gauges, and histograms. This is the single
// place where all Prometheus instrumentation happens.
func (a *MetricsActor) Receive(ctx context.Context, msg Msg) fn.Result[Resp] {
	switch m := msg.(type) {
	case *RoundCreatedMsg:
		RoundsCreated.Inc()
		RoundsActive.Inc()
		a.roundStartTimes[m.RoundID] = time.Now()
		a.startPhase(m.RoundID, "registration")

	case *ClientJoinedRoundMsg:
		a.roundClientCounts[m.RoundID]++

	case *RoundSealedMsg:
		// Observe registration phase duration.
		status := "completed"
		if m.TimedOut {
			status = "timeout"
		}
		if d := a.observePhase(m.RoundID, "registration"); d > 0 {
			RoundRegistrationDuration.WithLabelValues(
				status,
			).Observe(d.Seconds())
		}

		// Start batch build phase.
		a.startPhase(m.RoundID, "batch_build")

	case *RoundBatchBuiltMsg:
		// Observe batch build duration.
		if d := a.observePhase(
			m.RoundID, "batch_build",
		); d > 0 {

			RoundBatchBuildDuration.WithLabelValues(
				"success",
			).Observe(d.Seconds())
		}

		// Record batch composition counts.
		RoundBoardingInputs.WithLabelValues("success").Observe(
			float64(m.BoardingInputs),
		)
		RoundLeaveOutputs.WithLabelValues("success").Observe(
			float64(m.LeaveOutputs),
		)
		RoundVTXOsGenerated.WithLabelValues("success").Observe(
			float64(m.VTXOsGenerated),
		)

	case *RoundBatchBuildFailedMsg:
		if d := a.observePhase(
			m.RoundID, "batch_build",
		); d > 0 {

			RoundBatchBuildDuration.WithLabelValues(
				"failed",
			).Observe(d.Seconds())
		}

	case *PhaseStartedMsg:
		a.startPhase(m.RoundID, m.Phase)

	case *RoundTickFiredMsg:
		RoundTicksTotal.WithLabelValues(m.Result).Inc()

	case *PhaseEndedMsg:
		status := "completed"
		if m.TimedOut {
			status = "timeout"
		}
		if d := a.observePhase(m.RoundID, m.Phase); d > 0 {
			a.recordPhaseDuration(m.Phase, status, d)
		}

	case *RoundCompletedMsg:
		// Always count completions regardless of whether
		// the round was tracked at startup (restored rounds
		// skip RoundCreatedMsg so they won't be in
		// roundStartTimes).
		RoundsTotal.WithLabelValues(m.Status).Inc()

		// Only adjust the active gauge and observe duration
		// for rounds we actually tracked from creation.
		if t, ok := a.roundStartTimes[m.RoundID]; ok {
			RoundsActive.Dec()
			RoundDurationSeconds.WithLabelValues(
				m.Status,
			).Observe(time.Since(t).Seconds())
		}

		// Observe client count for the round.
		if count, ok := a.roundClientCounts[m.RoundID]; ok {
			RoundClientsJoined.WithLabelValues(
				m.Status,
			).Observe(float64(count))
		}

		if m.BlockHeight > 0 {
			BlockHeight.Set(float64(m.BlockHeight))
		}

		// Clean up all tracking state for this round.
		a.cleanupRound(m.RoundID)

	case *OORTransferStartedMsg:
		a.oorStartTimes[m.SessionID] = time.Now()

	case *OORTransferCompletedMsg:
		OORTransfersTotal.WithLabelValues(m.Status).Inc()
		if t, ok := a.oorStartTimes[m.SessionID]; ok {
			OORTransferDuration.WithLabelValues(
				m.Status,
			).Observe(time.Since(t).Seconds())
			delete(a.oorStartTimes, m.SessionID)
		}

	case *VTXOLockResultMsg:
		if m.Success {
			VTXOLockDuration.WithLabelValues(
				m.Owner,
			).Observe(m.Duration.Seconds())
		} else {
			VTXOLockFailures.WithLabelValues(
				m.Reason,
			).Inc()
		}

	case *DispatchCompletedMsg:
		DispatchDuration.WithLabelValues(
			m.ServiceMethod,
		).Observe(m.Duration.Seconds())

	case *BatchWatcherRegisterFailedMsg:
		// Bump the counter once per failed batch so the operator
		// alert fires proportionally to the number of unwatched
		// trees, not just per-round. The handler in `rounds`
		// guarantees BatchCount >= 1.
		BatchWatcherRegisterFailures.Add(float64(m.BatchCount))

	case *ClientStatusChangedMsg:
		if m.Online {
			ConnectedClients.Inc()
		} else {
			ConnectedClients.Dec()
		}

	default:
		a.log.Warnf("Unknown metrics message type: %T", msg)
	}

	return fn.Ok[Resp](nil)
}

// startPhase records the start time of a named phase for a round.
func (a *MetricsActor) startPhase(roundID RoundID, phase PhaseName) {
	if a.roundPhaseTimes[roundID] == nil {
		a.roundPhaseTimes[roundID] = make(map[PhaseName]time.Time)
	}
	a.roundPhaseTimes[roundID][phase] = time.Now()
}

// observePhase returns the elapsed time since the phase started and
// removes the entry. Returns zero if the phase was not tracked.
func (a *MetricsActor) observePhase(roundID RoundID,
	phase PhaseName) time.Duration {

	phases, ok := a.roundPhaseTimes[roundID]
	if !ok {
		return 0
	}

	start, ok := phases[phase]
	if !ok {
		return 0
	}

	delete(phases, phase)

	return time.Since(start)
}

// recordPhaseDuration emits the appropriate histogram observation
// for the given phase.
func (a *MetricsActor) recordPhaseDuration(phase PhaseName, status string,
	d time.Duration) {

	switch phase {
	case "nonce_exchange":
		RoundNonceExchangeDuration.Observe(d.Seconds())

	case "input_sigs":
		RoundInputSigDuration.WithLabelValues(status).Observe(
			d.Seconds(),
		)
	}
}

// cleanupRound removes all internal tracking state for a round.
func (a *MetricsActor) cleanupRound(roundID RoundID) {
	delete(a.roundStartTimes, roundID)
	delete(a.roundPhaseTimes, roundID)
	delete(a.roundClientCounts, roundID)
}

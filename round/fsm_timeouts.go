package round

import (
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// TimeoutPhase identifies which FSM phase owns a timeout.
type TimeoutPhase string

const (
	// TimeoutPhaseRefreshRegistration coalesces expiry-driven refreshes
	// before registering their assembling round.
	TimeoutPhaseRefreshRegistration TimeoutPhase = "refresh-registration"

	// TimeoutPhaseForfeitCollection is the timeout phase for
	// ForfeitSignaturesCollectingState.
	TimeoutPhaseForfeitCollection TimeoutPhase = "forfeit-collection"

	// TimeoutPhaseRegistration is the timeout phase for IntentSentState.
	// It bounds how long the client waits for the server to acknowledge a
	// JoinRoundRequest (the RoundJoined admission watermark). Without it a
	// silent or unresponsive server would leave the round parked in
	// IntentSentState forever, stranding any forfeit-reserved VTXOs in
	// pending-forfeit (see wavelength#653).
	TimeoutPhaseRegistration TimeoutPhase = "registration"

	// TimeoutPhaseStatusReconcile is the timeout phase for
	// InputSigSentState's round-status reconcile (wavelength#844). It is
	// armed on entry to InputSigSentState — for every checkpointed round,
	// including a boarding-only one that submits no forfeit signatures
	// (wavelength#1051) — and re-armed on every reconcile probe, so both a
	// received round failure and total operator silence (the lumos#618
	// crash door) eventually drive a QueryRoundStatus. The reservation is
	// released only on an authoritative dead answer, never on the timeout
	// alone.
	TimeoutPhaseStatusReconcile TimeoutPhase = "status-reconcile"
)

// cancelForfeitTimeout builds a single-element outbox slice that
// cancels the forfeit collection timeout for the given round.
func cancelForfeitTimeout(roundID RoundID) []ClientOutMsg {
	return []ClientOutMsg{
		&CancelTimeoutReq{
			RoundKey: RoundKeyStr(roundID.KeyString()),
			Phase:    TimeoutPhaseForfeitCollection,
		},
	}
}

// statusReconcileMaxBackoffShift caps the exponential backoff applied to
// repeated reconcile probes at base<<4 (16x, 24 minutes at the 90 second
// default). An operator that predates the QueryRoundStatus RPC never
// answers, so without a ceiling the doubling would push the next probe
// arbitrarily far out; with one, the client keeps converging on a bounded,
// low-rate probe cadence for as long as the reservation is parked.
const statusReconcileMaxBackoffShift = 4

// statusReconcileProbeOutbox builds the outbox pair for one round-status
// reconcile probe: the QueryRoundStatus ask to the operator, plus a
// re-arm of the status-reconcile timeout so an unanswered probe retries.
// Scheduling a timeout under an existing ID replaces it, so re-arming
// from a probe never stacks timers. The re-arm duration doubles with each
// unanswered probe (capped by statusReconcileMaxBackoffShift), bounding
// the probe traffic aimed at an operator that never answers, e.g. one
// running a release that predates the QueryRoundStatus RPC.
func statusReconcileProbeOutbox(roundID RoundID, env *ClientEnvironment,
	probes uint32) []ClientOutMsg {

	shift := min(probes, statusReconcileMaxBackoffShift)
	duration := env.StatusReconcileTimeout << shift

	return []ClientOutMsg{
		&QueryRoundStatusOutbox{
			RoundID: roundID,
		},
		&StartTimeoutReq{
			RoundKey: RoundKeyStr(roundID.KeyString()),
			Phase:    TimeoutPhaseStatusReconcile,
			Duration: duration,
		},
	}
}

// cancelStatusReconcileTimeout builds the outbox message that disarms the
// status-reconcile timeout, for the paths that resolve the round's fate: a
// confirmation, an authoritative dead answer, or a failure the operator
// delivered directly (which carries the same authority a probe would return).
func cancelStatusReconcileTimeout(roundID RoundID) ClientOutMsg {
	return &CancelTimeoutReq{
		RoundKey: RoundKeyStr(roundID.KeyString()),
		Phase:    TimeoutPhaseStatusReconcile,
	}
}

// reconcileDisarmEvents builds the emitted-event option that disarms the
// status-reconcile clock on an exit from InputSigSentState. The clock is armed
// for the whole of that state, so every exit disarms it; the gate mirrors the
// arm sites so no cancel is emitted for a timer that was never scheduled.
func reconcileDisarmEvents(roundID RoundID,
	env *ClientEnvironment) fn.Option[ClientEmittedEvent] {

	if env.StatusReconcileTimeout <= 0 {
		return fn.None[ClientEmittedEvent]()
	}

	return fn.Some(ClientEmittedEvent{
		Outbox: []ClientOutMsg{
			cancelStatusReconcileTimeout(roundID),
		},
	})
}

// appendReconcileDisarm adds the status-reconcile disarm to the end of a
// transition's outbox, for exits that have already queued messages of their
// own. Disarming is cleanup and must stay behind every delivery: processOutbox
// abandons the rest of the outbox on the first failing Tell, and a cancel is
// one of the few entries that can fail, so a cancel sitting ahead of a
// notification lets a saturated timeout actor suppress it. Trailing costs
// nothing, since a cancel that never lands only leaks a one-shot timer, which
// fires into a terminal state and self-loops.
func appendReconcileDisarm(transition *ClientStateTransition, roundID RoundID,
	env *ClientEnvironment) *ClientStateTransition {

	if env.StatusReconcileTimeout <= 0 {
		return transition
	}

	emitted := transition.NewEvents.UnwrapOr(ClientEmittedEvent{})
	emitted.Outbox = append(
		emitted.Outbox, cancelStatusReconcileTimeout(roundID),
	)
	transition.NewEvents = fn.Some(emitted)

	return transition
}

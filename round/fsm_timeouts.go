package round

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
	// armed when the forfeit signatures leave the box and re-armed on
	// every reconcile probe, so both a received round failure and total
	// operator silence (the lumos#618 crash door) eventually drive a
	// QueryRoundStatus. The reservation is released only on an
	// authoritative dead answer, never on the timeout alone.
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
// status-reconcile timeout, for the paths that resolve the round's fate
// (confirmation, or an authoritative dead answer).
func cancelStatusReconcileTimeout(roundID RoundID) ClientOutMsg {
	return &CancelTimeoutReq{
		RoundKey: RoundKeyStr(roundID.KeyString()),
		Phase:    TimeoutPhaseStatusReconcile,
	}
}

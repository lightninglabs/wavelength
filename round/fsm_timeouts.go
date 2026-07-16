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

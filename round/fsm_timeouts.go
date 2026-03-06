package round

// TimeoutPhase identifies which FSM phase owns a timeout.
type TimeoutPhase string

const (
	// TimeoutPhaseForfeitCollection is the timeout phase for
	// ForfeitSignaturesCollectingState.
	TimeoutPhaseForfeitCollection TimeoutPhase = "forfeit-collection"
)

// cancelForfeitTimeout builds a single-element outbox slice that
// cancels the forfeit collection timeout for the given round.
func cancelForfeitTimeout(roundID RoundID) []ClientOutMsg {
	return []ClientOutMsg{
		&CancelTimeoutReq{
			RoundID: roundID,
			Phase:   TimeoutPhaseForfeitCollection,
		},
	}
}

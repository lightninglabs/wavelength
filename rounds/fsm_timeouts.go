package rounds

// TimeoutPhase identifies which FSM phase scheduled a timeout. This is used to
// create composite timeout IDs that prevent stale timeouts from being processed
// by the wrong state.
type TimeoutPhase string

const (
	// TimeoutPhaseRegistration is the phase for registration timeouts.
	TimeoutPhaseRegistration TimeoutPhase = "registration"

	// TimeoutPhaseBoardingSigs is the phase for boarding signature
	// collection timeouts.
	TimeoutPhaseBoardingSigs TimeoutPhase = "boarding-signatures"
)

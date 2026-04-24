package rounds

// TimeoutPhase identifies which FSM phase scheduled a timeout. This is used to
// create composite timeout IDs that prevent stale timeouts from being processed
// by the wrong state.
type TimeoutPhase string

const (
	// TimeoutPhaseRegistration is the phase for registration timeouts.
	TimeoutPhaseRegistration TimeoutPhase = "registration"

	// TimeoutPhaseInputSigs is the phase for input signature
	// collection timeouts.
	TimeoutPhaseInputSigs TimeoutPhase = "boarding-signatures"

	// TimeoutPhaseVTXONonces is the phase for VTXO nonce collection
	// timeouts.
	TimeoutPhaseVTXONonces TimeoutPhase = "vtxo-nonces"

	// TimeoutPhaseVTXOSignatures is the phase for VTXO partial signature
	// collection timeouts.
	TimeoutPhaseVTXOSignatures TimeoutPhase = "vtxo-signatures"

	// TimeoutPhaseQuote is the phase for seal-time quote acceptance
	// timeouts. Each pending client in QuoteSentState is scheduled a
	// QuoteTimeoutEvent under this phase at QuoteTTL; the actor then
	// translates the timeout to a per-client QuoteTimeoutEvent
	// carrying the pending quote_id.
	TimeoutPhaseQuote TimeoutPhase = "quote"
)

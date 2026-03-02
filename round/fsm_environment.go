package round

import (
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/types"
)

// ClientEnvironment provides the client round interaction state machine
// with read-only configuration for state transitions. All I/O side
// effects (persistence, signing, key derivation) are handled by the
// OutboxHandler, keeping the FSM pure and testable.
type ClientEnvironment struct {
	// OperatorTerms contains the operator's parameters including
	// sweep keys, fee targets, confirmation thresholds, and amount
	// limits.
	OperatorTerms *types.OperatorTerms

	// Log is the logger for FSM transitions and operations.
	Log btclog.Logger

	// StartHeight is the block height when the FSM was created.
	// This is used as a HeightHint for confirmation registration,
	// ensuring the chain backend scans from the correct starting
	// point. This avoids missing confirmations if the transaction
	// was broadcast before the registration request is processed.
	StartHeight uint32
}

// Name returns the unique identifier for this FSM instance.
func (e *ClientEnvironment) Name() string {
	return "round_fsm"
}

// NewClientEnvironment creates a new client environment with the
// provided read-only configuration. All I/O dependencies (stores,
// wallet, height queries) are handled by the OutboxHandler and are
// not part of the FSM environment.
func NewClientEnvironment(terms *types.OperatorTerms,
	logger btclog.Logger,
	startHeight uint32) *ClientEnvironment {

	return &ClientEnvironment{
		OperatorTerms: terms,
		Log:           logger,
		StartHeight:   startHeight,
	}
}

package round

import (
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/types"
)

// ClientEnvironment provides the client round interaction state machine with
// access to external systems and storage. This follows the protofsm pattern
// where the environment contains all dependencies needed for state transitions.
//
// Note: Boarding address and intent persistence is handled by the wallet actor.
// The FSM only needs RoundStore for round checkpointing.
type ClientEnvironment struct {
	// RoundStore provides persistence for round coordination and
	// checkpointing.
	RoundStore RoundStore

	// VTXOStore provides persistence for off-chain balance.
	VTXOStore VTXOStore

	// Wallet provides signing capabilities for round participation.
	Wallet ClientWallet

	// OperatorTerms contains the operator's parameters including sweep
	// keys, fee targets, confirmation thresholds, and amount limits.
	OperatorTerms *types.OperatorTerms

	// ChainParams are the Bitcoin network parameters.
	ChainParams *chaincfg.Params

	// Log is the logger for FSM transitions and operations.
	Log btclog.Logger
}

// Name returns the unique identifier for this FSM instance.
func (e *ClientEnvironment) Name() string {
	return "round_fsm"
}

// NewClientEnvironment creates a new client environment with the provided
// dependencies.
func NewClientEnvironment(roundStore RoundStore, vtxoStore VTXOStore,
	wallet ClientWallet, terms *types.OperatorTerms,
	chainParams *chaincfg.Params, logger btclog.Logger) *ClientEnvironment {

	return &ClientEnvironment{
		RoundStore:    roundStore,
		VTXOStore:     vtxoStore,
		Wallet:        wallet,
		OperatorTerms: terms,
		ChainParams:   chainParams,
		Log:           logger,
	}
}

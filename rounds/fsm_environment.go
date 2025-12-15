package rounds

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/batch"
)

// Environment provides the round state machine with access to external systems
// and storage. This follows the protofsm pattern where the environment contains
// all dependencies needed for state transitions.
type Environment struct {
	// RoundID identifies this FSM instance.
	RoundID RoundID

	// ChainParams specifies the Bitcoin network parameters.
	ChainParams *chaincfg.Params

	// BoardingInputLocker manages locks on boarding inputs to prevent
	// double-spending across concurrent rounds.
	BoardingInputLocker BoardingInputLocker

	// ChainSource provides access to blockchain data for UTXO validation.
	ChainSource ChainSource

	// Terms contains the operator's terms for this round.
	Terms *batch.Terms

	// Log is the logger for the FSM.
	Log btclog.Logger
}

// Name returns the unique identifier for this FSM instance.
func (e *Environment) Name() string {
	return fmt.Sprintf("server_round_fsm_%s", e.RoundID)
}

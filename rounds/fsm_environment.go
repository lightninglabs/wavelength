package rounds

import (
	"fmt"
)

// Environment provides the round state machine with access to external systems
// and storage. This follows the protofsm pattern where the environment contains
// all dependencies needed for state transitions.
type Environment struct {
	// RoundID identifies this FSM instance.
	RoundID RoundID
}

// Name returns the unique identifier for this FSM instance.
func (e *Environment) Name() string {
	return fmt.Sprintf("server_round_fsm_%s", e.RoundID)
}

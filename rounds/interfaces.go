package rounds

import (
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
)

// StateTransition is a type alias for the verbose protofsm.StateTransition
// type used throughout the round FSM. This makes function signatures more
// readable and easier to maintain. The baselib protofsm uses 3
// type parameters: InternalEvent, OutboxEvent, and Env. In our case:
//   - InternalEvent = Event (events that drive the FSM).
//   - OutboxEvent = OutboxEvent (outbox messages emitted by transitions).
//   - Env = *Environment.
type StateTransition = protofsm.StateTransition[
	Event, OutboxEvent, *Environment,
]

// EmittedEvent is a type alias for the verbose protofsm.EmittedEvent type
// used when state transitions emit new events or outbox messages. This
// improves readability of state transition return values.
type EmittedEvent = protofsm.EmittedEvent[Event, OutboxEvent]

// StateMachine is a type alias for the server rounds FSM.
type StateMachine = protofsm.StateMachine[
	Event, OutboxEvent, *Environment,
]

// StateMachineCfg is a type alias for the server FSM configuration.
type StateMachineCfg = protofsm.StateMachineCfg[
	Event, OutboxEvent, *Environment,
]

// RoundID is a type alias for round identifiers.
type RoundID uuid.UUID

// NewRoundID generates a new unique RoundID using cryptographic randomness.
func NewRoundID() (RoundID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return RoundID{}, err
	}

	return RoundID(id), nil
}

// LogPrefix returns a short string representation of the RoundID for logging.
// It uses the last 4 bytes (32 bits) of the UUIDv7, which are high-entropy
// random bits.
func (id RoundID) LogPrefix() string {
	// Last 4 bytes = 32 bits of pure randomness.
	return fmt.Sprintf("round(%v)", hex.EncodeToString(id[12:16]))
}

// RoundFSM wraps a state machine instance for a specific round.
type RoundFSM struct {
	// FSM is the state machine for this round.
	FSM *StateMachine

	// RoundID is the unique identifier for this round.
	RoundID RoundID
}

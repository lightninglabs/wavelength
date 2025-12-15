package rounds

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/wire"
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

// BoardingInputLocker provides thread-safe locking of boarding inputs
// across concurrent rounds to prevent double-spending.
type BoardingInputLocker interface {
	// Lock attempts to lock a boarding input for the specified round.
	// Returns an error if the input is already locked by another round.
	Lock(ctx context.Context, outpoint *wire.OutPoint,
		roundID RoundID) error

	// Unlock releases the lock on a boarding input for the specified round.
	// Only the round that locked the input can unlock it.
	Unlock(ctx context.Context, outpoint *wire.OutPoint,
		roundID RoundID) error

	// IsLocked checks if an input is locked and returns the locking round
	// ID if it is locked.
	IsLocked(ctx context.Context,
		outpoint *wire.OutPoint) (bool, RoundID, error)
}

// ChainSource provides access to blockchain data for UTXO validation.
type ChainSource interface {
	// GetUTXO fetches the UTXO for the given outpoint. Returns an error
	// if the UTXO doesn't exist or has been spent.
	GetUTXO(outpoint wire.OutPoint) (*UTXO, error)
}

// UTXO represents a UTXO along with its confirmation count.
type UTXO struct {
	// Output is the transaction output.
	Output *wire.TxOut

	// Confirmations is the number of confirmations for this UTXO.
	Confirmations int64
}

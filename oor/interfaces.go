package oor

import (
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
)

// StateTransition is a type alias for the verbose protofsm.StateTransition type
// used throughout the OOR client transfer FSM.
type StateTransition = protofsm.StateTransition[
	Event, OutboxEvent, *Environment,
]

// EmittedEvent is a type alias for the verbose protofsm.EmittedEvent type used
// when state transitions emit new events or outbox messages.
type EmittedEvent = protofsm.EmittedEvent[Event, OutboxEvent]

// SessionState is the common protofsm state interface implemented by both the
// outgoing and incoming OOR session state machines.
type SessionState = protofsm.State[Event, OutboxEvent, *Environment]

// StateMachine is a type alias for the OOR client transfer FSM.
type StateMachine = protofsm.StateMachine[
	Event, OutboxEvent, *Environment,
]

// StateMachineCfg is a type alias for the OOR client transfer FSM
// configuration.
type StateMachineCfg = protofsm.StateMachineCfg[
	Event, OutboxEvent, *Environment,
]

// SessionID uniquely identifies an out-of-round transfer session.
//
// In v0, we use the Ark txid as the stable session identifier.
type SessionID chainhash.Hash

// String returns the full string representation of the session id.
func (id SessionID) String() string {
	hash := chainhash.Hash(id)

	return hash.String()
}

// LogPrefix returns a short string representation of the session id for logs.
func (id SessionID) LogPrefix() string {
	hash := chainhash.Hash(id)

	return fmt.Sprintf("oor(%s)", hex.EncodeToString(hash[:4]))
}

// Environment provides the transfer FSM with access to external systems and
// storage.
//
// The FSM itself should remain mostly pure: it emits outbox requests and
// expects the actor boundary to translate them into follow-up events.
type Environment struct {
	// SessionID identifies this FSM instance.
	SessionID SessionID
}

// Name returns the unique identifier for this FSM instance.
func (e *Environment) Name() string {
	return fmt.Sprintf("oor_transfer_fsm_%s", e.SessionID)
}

package oor

import (
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
)

// StateTransition is a type alias for the verbose protofsm.StateTransition type
// used throughout the OOR transfer session FSM.
//
// This mirrors the `rounds/` package style, making the FSM code easier to read.
type StateTransition = protofsm.StateTransition[
	Event, OutboxEvent, *Environment,
]

// EmittedEvent is a type alias for the verbose protofsm.EmittedEvent type used
// when state transitions emit new events or outbox messages.
type EmittedEvent = protofsm.EmittedEvent[Event, OutboxEvent]

// StateMachine is a type alias for the OOR transfer session FSM.
type StateMachine = protofsm.StateMachine[
	Event, OutboxEvent, *Environment,
]

// StateMachineCfg is a type alias for the OOR transfer session FSM
// configuration.
type StateMachineCfg = protofsm.StateMachineCfg[
	Event, OutboxEvent, *Environment,
]

// SessionID uniquely identifies an OOR transfer session.
//
// In v0, the session identifier is the Ark txid.
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

// Environment provides the OOR session state machine with access to external
// systems and storage.
//
// The FSM itself should remain mostly pure: it emits outbox requests, and the
// actor (or other subsystem) turns those into inbox events.
type Environment struct {
	// SessionID identifies this FSM instance.
	SessionID SessionID

	// Log is the logger for the FSM.
	Log btclog.Logger

	// CheckpointPolicy is the operator policy used for submit validation.
	CheckpointPolicy arkscript.CheckpointPolicy
}

// Name returns the unique identifier for this FSM instance.
func (e *Environment) Name() string {
	return fmt.Sprintf("oor_session_fsm_%s", e.SessionID)
}

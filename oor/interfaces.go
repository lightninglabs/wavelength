package oor

import (
	"encoding/hex"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightningnetwork/lnd/clock"
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

	// Clock supplies the current time to transitions that need to reason
	// about elapsed wall-clock time (e.g. the bounded transient
	// submit-reject retry window). It is injected rather than read from
	// time.Now() so transitions stay deterministic under test. A nil clock
	// defaults to a real clock via Now(), so a directly-constructed
	// Environment (e.g. in a unit test) still advances time.
	Clock clock.Clock

	// MaxTransientSubmitRetry bounds the cumulative wall-clock time the
	// outgoing FSM keeps re-driving a transient submit rejection while
	// awaiting submit acceptance before failing the session terminally. A
	// zero value disables the bound (unbounded, legacy behavior), which is
	// what a directly-constructed Environment without a configured cap
	// gets.
	MaxTransientSubmitRetry time.Duration
}

// Name returns the unique identifier for this FSM instance.
func (e *Environment) Name() string {
	return fmt.Sprintf("oor_transfer_fsm_%s", e.SessionID)
}

// Now returns the environment's current time. It defaults to a real clock when
// no clock was injected so a directly-constructed Environment still advances
// time, keeping the FSM usable without explicit clock wiring in tests.
func (e *Environment) Now() time.Time {
	if e == nil || e.Clock == nil {
		return clock.NewDefaultClock().Now()
	}

	return e.Clock.Now()
}

// EnvConfig carries the deterministic-time clock and transient submit-reject
// retry budget injected into a session FSM Environment at construction. A nil
// Clock defaults to a real clock so callers that do not inject one (tests,
// simple harnesses) still work; a zero MaxTransientSubmitRetry leaves the retry
// bound disabled.
type EnvConfig struct {
	// Clock is the deterministic time source for the session FSM.
	Clock clock.Clock

	// MaxTransientSubmitRetry is the cumulative retry-window cap for
	// transient submit rejections.
	MaxTransientSubmitRetry time.Duration
}

// newEnvironment builds a session FSM Environment from this config, defaulting
// a nil clock to a real clock so the returned Environment always has a usable
// time source.
func (c EnvConfig) newEnvironment(sessionID SessionID) *Environment {
	clk := c.Clock
	if clk == nil {
		clk = clock.NewDefaultClock()
	}

	return &Environment{
		SessionID:               sessionID,
		Clock:                   clk,
		MaxTransientSubmitRetry: c.MaxTransientSubmitRetry,
	}
}

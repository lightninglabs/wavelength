package rounds

import (
	"github.com/lightninglabs/darepo-client/baselib/actor"
)

// ActorMsg is the sealed interface for all messages that can be sent to the
// server round Actor.
type ActorMsg interface {
	actor.Message

	// actorMsgSealed marks this interface as sealed, preventing external
	// implementations.
	actorMsgSealed()
}

// ActorResp is the sealed interface for all response messages from a server
// rounds Actor.
type ActorResp interface {
	actor.Message

	// actorRespSealed marks this interface as sealed, preventing external
	// implementations.
	actorRespSealed()
}

// RoundMsg is a wrapper message that forwards an Event to a specific
// round's FSM.
type RoundMsg struct {
	actor.BaseMessage

	// RoundID identifies which round this event is for.
	RoundID RoundID

	// Event is the event to forward to the round's FSM.
	Event
}

// MessageType returns the type name of this message.
func (m *RoundMsg) MessageType() string {
	return "RoundMsg"
}

// actorMsgSealed marks this message as part of the ActorMsg sealed interface.
func (m *RoundMsg) actorMsgSealed() {}

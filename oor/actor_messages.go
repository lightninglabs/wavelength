package oor

import (
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
)

// ActorMsg is a sealed interface for messages that can be sent to the
// OORClientActor.
type ActorMsg interface {
	actor.Message
	actorMsgSealed()
}

// ActorResp is a sealed interface for responses produced by the OORClientActor.
type ActorResp interface {
	actor.Message
	actorRespSealed()
}

// StartTransferRequest asks the actor to start a new outgoing OOR transfer
// session by building a submit package and sending it via the outbox boundary.
type StartTransferRequest struct {
	actor.BaseMessage

	// Policy defines the checkpoint output tap tree policy.
	Policy scripts.CheckpointPolicy

	// Inputs are the VTXO inputs to convert into checkpoint txs.
	Inputs []oortx.CheckpointInput

	// Recipients are the Ark tx recipient outputs.
	Recipients []oortx.RecipientOutput
}

// MessageType returns the type of this message.
func (m *StartTransferRequest) MessageType() string {
	return "StartTransferRequest"
}

// actorMsgSealed marks this as implementing the sealed ActorMsg interface.
func (m *StartTransferRequest) actorMsgSealed() {}

// StartTransferResponse returns the created session identifier.
type StartTransferResponse struct {
	actor.BaseMessage

	SessionID SessionID
}

// MessageType returns the type of this message.
func (m *StartTransferResponse) MessageType() string {
	return "StartTransferResponse"
}

// actorRespSealed marks this as implementing the sealed ActorResp interface.
func (m *StartTransferResponse) actorRespSealed() {}

// DriveEventRequest asks the actor to feed an event into an existing session.
//
// This is the generic adapter boundary for future RPC/server notifications.
type DriveEventRequest struct {
	actor.BaseMessage

	// SessionID selects the session to drive.
	SessionID SessionID

	// Event is the follow-up event produced by an outbox handler, or by a
	// higher-level notification mechanism.
	Event Event
}

// MessageType returns the type of this message.
func (m *DriveEventRequest) MessageType() string {
	return "DriveEventRequest"
}

// actorMsgSealed marks this as implementing the sealed ActorMsg interface.
func (m *DriveEventRequest) actorMsgSealed() {}

// DriveEventResponse acknowledges the event was processed.
type DriveEventResponse struct {
	actor.BaseMessage
}

// MessageType returns the type of this message.
func (m *DriveEventResponse) MessageType() string {
	return "DriveEventResponse"
}

// actorRespSealed marks this as implementing the sealed ActorResp interface.
func (m *DriveEventResponse) actorRespSealed() {}

// GetStateRequest asks the actor for the current state of a session.
type GetStateRequest struct {
	actor.BaseMessage

	SessionID SessionID
}

// MessageType returns the type of this message.
func (m *GetStateRequest) MessageType() string {
	return "GetStateRequest"
}

// actorMsgSealed marks this as implementing the sealed ActorMsg interface.
func (m *GetStateRequest) actorMsgSealed() {}

// GetStateResponse returns the current session FSM state.
type GetStateResponse struct {
	actor.BaseMessage

	State State
}

// MessageType returns the type of this message.
func (m *GetStateResponse) MessageType() string {
	return "GetStateResponse"
}

// actorRespSealed marks this as implementing the sealed ActorResp interface.
func (m *GetStateResponse) actorRespSealed() {}

package oor

import (
	"github.com/btcsuite/btcd/btcutil/psbt"
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

	// Policy defines the operator checkpoint policy used to build the
	// transfer package.
	Policy scripts.CheckpointPolicy

	// Inputs are the VTXOs to transfer.
	//
	// Each input includes enough context for the outbox boundary to request
	// wallet signatures deterministically.
	Inputs []TransferInput

	// Recipients are the Ark tx output scripts/amounts.
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

	// SessionID is the stable v0 session identifier (Ark txid).
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

	// SessionID identifies the session to drive.
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

	// SessionID identifies the session to query.
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

	// State is the current session state machine state.
	State State
}

// MessageType returns the type of this message.
func (m *GetStateResponse) MessageType() string {
	return "GetStateResponse"
}

// actorRespSealed marks this as implementing the sealed ActorResp interface.
func (m *GetStateResponse) actorRespSealed() {}

// RestoreSessionRequest asks the actor to restore an outgoing transfer session
// from a previously exported snapshot.
type RestoreSessionRequest struct {
	actor.BaseMessage

	// Snapshot is the durable-ish client-side snapshot for an outgoing
	// transfer.
	Snapshot *OutgoingSnapshot
}

// MessageType returns the type of this message.
func (m *RestoreSessionRequest) MessageType() string {
	return "RestoreSessionRequest"
}

// actorMsgSealed marks this as implementing the sealed ActorMsg interface.
func (m *RestoreSessionRequest) actorMsgSealed() {}

// RestoreSessionResponse returns the restored session identifier.
type RestoreSessionResponse struct {
	actor.BaseMessage

	// SessionID is the restored session identifier.
	SessionID SessionID
}

// MessageType returns the type of this message.
func (m *RestoreSessionResponse) MessageType() string {
	return "RestoreSessionResponse"
}

// actorRespSealed marks this as implementing the sealed ActorResp interface.
func (m *RestoreSessionResponse) actorRespSealed() {}

// ResumeSessionRequest asks the actor to re-emit the outbox request implied by
// the current session state.
//
// This supports retries after app restart or temporary transport failures (for
// example, re-sending submit/finalize requests).
type ResumeSessionRequest struct {
	actor.BaseMessage

	// SessionID identifies the session to resume.
	SessionID SessionID
}

// MessageType returns the type of this message.
func (m *ResumeSessionRequest) MessageType() string {
	return "ResumeSessionRequest"
}

// actorMsgSealed marks this as implementing the sealed ActorMsg interface.
func (m *ResumeSessionRequest) actorMsgSealed() {}

// ResumeSessionResponse acknowledges the resume request.
type ResumeSessionResponse struct {
	actor.BaseMessage
}

// MessageType returns the type of this message.
func (m *ResumeSessionResponse) MessageType() string {
	return "ResumeSessionResponse"
}

// actorRespSealed marks this as implementing the sealed ActorResp interface.
func (m *ResumeSessionResponse) actorRespSealed() {}

// ExportSnapshotRequest asks the actor to export a snapshot for the requested
// session.
type ExportSnapshotRequest struct {
	actor.BaseMessage

	// SessionID identifies the session to snapshot.
	SessionID SessionID
}

// MessageType returns the type of this message.
func (m *ExportSnapshotRequest) MessageType() string {
	return "ExportSnapshotRequest"
}

// actorMsgSealed marks this as implementing the sealed ActorMsg interface.
func (m *ExportSnapshotRequest) actorMsgSealed() {}

// ExportSnapshotResponse returns an exported outgoing session snapshot.
type ExportSnapshotResponse struct {
	actor.BaseMessage

	// Snapshot is the exported outgoing snapshot.
	Snapshot *OutgoingSnapshot
}

// MessageType returns the type of this message.
func (m *ExportSnapshotResponse) MessageType() string {
	return "ExportSnapshotResponse"
}

// actorRespSealed marks this as implementing the sealed ActorResp interface.
func (m *ExportSnapshotResponse) actorRespSealed() {}

// ReceiveTransferRequest asks the actor to process an incoming transfer
// notification.
type ReceiveTransferRequest struct {
	actor.BaseMessage

	// SessionID is the stable incoming session identifier.
	SessionID SessionID

	// ArkPSBT is the canonical incoming Ark transaction package.
	ArkPSBT *psbt.Packet

	// FinalCheckpointPSBTs are finalized checkpoint packages for this transfer.
	FinalCheckpointPSBTs []*psbt.Packet
}

// MessageType returns the type of this message.
func (m *ReceiveTransferRequest) MessageType() string {
	return "ReceiveTransferRequest"
}

// actorMsgSealed marks this as implementing the sealed ActorMsg interface.
func (m *ReceiveTransferRequest) actorMsgSealed() {}

// ReceiveTransferResponse acknowledges incoming-transfer processing.
type ReceiveTransferResponse struct {
	actor.BaseMessage
}

// MessageType returns the type of this message.
func (m *ReceiveTransferResponse) MessageType() string {
	return "ReceiveTransferResponse"
}

// actorRespSealed marks this as implementing the sealed ActorResp interface.
func (m *ReceiveTransferResponse) actorRespSealed() {}

// ResumeIncomingRequest asks the actor to re-emit pending incoming outbox work
// from durable state.
type ResumeIncomingRequest struct {
	actor.BaseMessage

	// SessionID identifies the incoming session to resume.
	SessionID SessionID
}

// MessageType returns the type of this message.
func (m *ResumeIncomingRequest) MessageType() string {
	return "ResumeIncomingRequest"
}

// actorMsgSealed marks this as implementing the sealed ActorMsg interface.
func (m *ResumeIncomingRequest) actorMsgSealed() {}

// ResumeIncomingResponse acknowledges incoming resume processing.
type ResumeIncomingResponse struct {
	actor.BaseMessage
}

// MessageType returns the type of this message.
func (m *ResumeIncomingResponse) MessageType() string {
	return "ResumeIncomingResponse"
}

// actorRespSealed marks this as implementing the sealed ActorResp interface.
func (m *ResumeIncomingResponse) actorRespSealed() {}

// GetIncomingStateRequest asks the actor for the current incoming session
// state.
type GetIncomingStateRequest struct {
	actor.BaseMessage

	// SessionID identifies the incoming session to query.
	SessionID SessionID
}

// MessageType returns the type of this message.
func (m *GetIncomingStateRequest) MessageType() string {
	return "GetIncomingStateRequest"
}

// actorMsgSealed marks this as implementing the sealed ActorMsg interface.
func (m *GetIncomingStateRequest) actorMsgSealed() {}

// GetIncomingStateResponse returns the current incoming session FSM state.
type GetIncomingStateResponse struct {
	actor.BaseMessage

	// State is the current incoming session state.
	State ReceiveState
}

// MessageType returns the type of this message.
func (m *GetIncomingStateResponse) MessageType() string {
	return "GetIncomingStateResponse"
}

// actorRespSealed marks this as implementing the sealed ActorResp interface.
func (m *GetIncomingStateResponse) actorRespSealed() {}

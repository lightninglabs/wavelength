package oor

import (
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/lightninglabs/darepo-client/baselib/actor"
)

// ActorMsg is the sealed interface for all messages that can be sent to the
// OORTransferCoordinator actor.
type ActorMsg interface {
	actor.Message

	// actorMsgSealed marks this interface as sealed.
	actorMsgSealed()
}

// ActorResp is the sealed interface for all responses from the
// OORTransferCoordinator actor.
type ActorResp interface {
	actor.Message

	// actorRespSealed marks this interface as sealed.
	actorRespSealed()
}

// SubmitOORRequest requests starting (or resuming) an OOR transfer session.
//
// Submit package vocabulary:
//   - ArkPSBT is the transfer intent transaction.
//   - CheckpointPSBTs are per-input checkpoint transactions before finalize
//     signatures are attached by the client.
type SubmitOORRequest struct {
	actor.BaseMessage

	// ArkPSBT is the transfer intent transaction.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs are the per-input checkpoint transactions.
	CheckpointPSBTs []*psbt.Packet
}

// MessageType returns the type of this message.
func (m *SubmitOORRequest) MessageType() string {
	return "SubmitOORRequest"
}

// actorMsgSealed marks this message as part of the ActorMsg sealed interface.
func (m *SubmitOORRequest) actorMsgSealed() {}

// SubmitOORResponse is returned after the submit request is processed.
//
// In v0, this is an internal boundary. A future RPC adapter can translate this
// into an RPC response.
type SubmitOORResponse struct {
	actor.BaseMessage

	// SessionID identifies the OOR session.
	SessionID SessionID
}

// MessageType returns the type of this message.
func (m *SubmitOORResponse) MessageType() string {
	return "SubmitOORResponse"
}

// actorRespSealed marks this message as part of the ActorResp sealed interface.
func (m *SubmitOORResponse) actorRespSealed() {}

// FinalizeOORRequest requests finalizing an existing OOR transfer session.
//
// Finalize package vocabulary:
//   - FinalCheckpointPSBTs are the same checkpoint transactions with client
//     finalize signature material attached.
type FinalizeOORRequest struct {
	actor.BaseMessage

	// SessionID identifies the session to finalize.
	SessionID SessionID

	// FinalCheckpointPSBTs are checkpoint txs fully signed by the client.
	FinalCheckpointPSBTs []*psbt.Packet
}

// MessageType returns the type of this message.
func (m *FinalizeOORRequest) MessageType() string {
	return "FinalizeOORRequest"
}

// actorMsgSealed marks this message as part of the ActorMsg sealed interface.
func (m *FinalizeOORRequest) actorMsgSealed() {}

// FinalizeOORResponse is returned after the finalize request is processed.
type FinalizeOORResponse struct {
	actor.BaseMessage

	// SessionID identifies the finalized session.
	SessionID SessionID
}

// MessageType returns the type of this message.
func (m *FinalizeOORResponse) MessageType() string {
	return "FinalizeOORResponse"
}

// actorRespSealed marks this message as part of the ActorResp sealed interface.
func (m *FinalizeOORResponse) actorRespSealed() {}

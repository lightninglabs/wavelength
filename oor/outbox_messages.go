package oor

import (
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"google.golang.org/protobuf/proto"
)

// OutboxEvent is a sealed interface for side-effect requests emitted by the
// OOR transfer FSM.
//
// Outbox messages are the explicit I/O boundary for the FSM:
// - transport (submit/finalize/ack) lives behind this interface
// - wallet signing lives behind this interface
// - chain confirmation monitoring lives behind this interface
//
// Keeping these side effects out of the FSM makes transitions deterministic
// and testable, and it makes it possible to implement restart-safe behavior by
// re-emitting the outbox implied by the current state.
type OutboxEvent interface {
	outboxType() string
	outboxSealed()
}

// SendSubmitPackageRequest asks the transport layer to send the submit package
// (Ark PSBT + checkpoint PSBTs) to the server.
type SendSubmitPackageRequest struct {
	actor.BaseMessage

	// ArkPSBT is the canonical unsigned Ark transfer PSBT.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs are unsigned checkpoint PSBTs for the submit phase.
	//
	// In v0, client signing happens only after the server returns operator
	// co-signed checkpoints.
	CheckpointPSBTs []*psbt.Packet

	// TransferInputs carry the VTXO descriptors and scripts for the inputs
	// referenced by the checkpoint PSBTs. This is used by in-process test
	// adaptors, and will later be mapped to RPC request fields.
	TransferInputs []TransferInput
}

// outboxType returns a stable identifier for this outbox message.
func (m *SendSubmitPackageRequest) outboxType() string {
	return "SendSubmitPackageRequest"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (m *SendSubmitPackageRequest) outboxSealed() {}

// ToProto converts SendSubmitPackageRequest to a protobuf message.
//
// TODO: Implement once OOR RPC definitions exist.
func (m *SendSubmitPackageRequest) ToProto() proto.Message {
	return nil
}

// RequestCheckpointSignatures asks the signing layer to add client signature
// material to the co-signed checkpoint PSBTs.
type RequestCheckpointSignatures struct {
	actor.BaseMessage

	// ArkPSBT is the canonical Ark PSBT used to derive signing metadata.
	ArkPSBT *psbt.Packet

	// CoSignedCheckpointPSBTs are operator-co-signed checkpoint PSBTs.
	//
	// The signer should append client signature material directly in
	// PSBT input witness/signature fields and return finalized
	// checkpoint PSBTs.
	CoSignedCheckpointPSBTs []*psbt.Packet

	// TransferInputs carry the client-side VTXO signing context. These are
	// required to construct taproot script-spend signing descriptors.
	TransferInputs []TransferInput
}

// outboxType returns a stable identifier for this outbox message.
func (m *RequestCheckpointSignatures) outboxType() string {
	return "RequestCheckpointSignatures"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (m *RequestCheckpointSignatures) outboxSealed() {}

// SendFinalizePackageRequest asks the transport layer to send finalized
// checkpoint PSBTs back to the server.
type SendFinalizePackageRequest struct {
	actor.BaseMessage

	// ArkPSBT is the canonical Ark tx PSBT for this session.
	ArkPSBT *psbt.Packet

	// FinalCheckpointPSBTs are fully signed checkpoint PSBTs.
	FinalCheckpointPSBTs []*psbt.Packet
}

// outboxType returns a stable identifier for this outbox message.
func (m *SendFinalizePackageRequest) outboxType() string {
	return "SendFinalizePackageRequest"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (m *SendFinalizePackageRequest) outboxSealed() {}

// ToProto converts SendFinalizePackageRequest to a protobuf message.
//
// TODO: Implement once OOR RPC definitions exist.
func (m *SendFinalizePackageRequest) ToProto() proto.Message {
	return nil
}

// MarkInputsSpentRequest asks the persistence layer to mark the OOR inputs as
// spent in the local VTXO store.
//
// This outbox request exists to make the FSM crash-resilient: after a crash,
// the application can re-emit the outbox implied by the current state and
// retry local persistence until it succeeds.
type MarkInputsSpentRequest struct {
	actor.BaseMessage

	// Outpoints are the VTXO outpoints that were consumed as inputs to this
	// OOR session.
	Outpoints []wire.OutPoint
}

// outboxType returns a stable identifier for this outbox message.
func (m *MarkInputsSpentRequest) outboxType() string {
	return "MarkInputsSpentRequest"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (m *MarkInputsSpentRequest) outboxSealed() {}

// ToProto converts MarkInputsSpentRequest to a protobuf message.
//
// TODO: Implement once OOR RPC definitions exist.
func (m *MarkInputsSpentRequest) ToProto() proto.Message {
	return nil
}

// IncomingTransferNotification is emitted when an incoming transfer has been
// validated structurally and should be surfaced to the application/UI layer.
//
// This message is meant for "show/notify" semantics (eg. display a summary,
// badge a notification, or queue a UX flow). It is not expected to persist
// wallet state.
type IncomingTransferNotification struct {
	actor.BaseMessage

	// SessionID is the stable v0 session identifier (Ark txid).
	SessionID SessionID

	// ArkPSBT is the canonical Ark tx PSBT.
	ArkPSBT *psbt.Packet

	// Recipients are the non-anchor recipient outputs in the Ark tx.
	Recipients []ArkRecipientOutput
}

// outboxType returns a stable identifier for this outbox message.
func (m *IncomingTransferNotification) outboxType() string {
	return "IncomingTransferNotification"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (m *IncomingTransferNotification) outboxSealed() {}

// MaterializeIncomingVTXOsRequest asks the wallet/state layer to materialize
// the incoming transfer into local VTXO records.
//
// This message is meant for "persist/track" semantics: decide which recipient
// outputs belong to the local wallet and persist the corresponding VTXO state.
//
// This is the interface boundary where we eventually construct full VTXO
// descriptors and hand them to the vtxo.Manager for lifecycle tracking.
type MaterializeIncomingVTXOsRequest struct {
	actor.BaseMessage

	// SessionID identifies the incoming transfer session.
	SessionID SessionID

	// ArkPSBT is the canonical Ark tx PSBT.
	ArkPSBT *psbt.Packet

	// Recipients are the non-anchor recipient outputs in the Ark tx.
	Recipients []ArkRecipientOutput
}

// outboxType returns a stable identifier for this outbox message.
func (m *MaterializeIncomingVTXOsRequest) outboxType() string {
	return "MaterializeIncomingVTXOsRequest"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (m *MaterializeIncomingVTXOsRequest) outboxSealed() {}

// SendIncomingAckRequest requests the transport layer to ack receipt of the
// incoming transfer to the server.
//
// In the future this becomes an RPC call. For now it is left as an interface
// boundary so client-side FSMs can be tested without a transport.
type SendIncomingAckRequest struct {
	actor.BaseMessage

	// SessionID identifies the transfer being acknowledged.
	SessionID SessionID
}

// outboxType returns a stable identifier for this outbox message.
func (m *SendIncomingAckRequest) outboxType() string {
	return "SendIncomingAckRequest"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (m *SendIncomingAckRequest) outboxSealed() {}

// ToProto converts SendIncomingAckRequest to a protobuf message.
//
// TODO: Implement once OOR RPC definitions exist.
func (m *SendIncomingAckRequest) ToProto() proto.Message {
	return nil
}

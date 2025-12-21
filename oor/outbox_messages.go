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
	// The signer should append client signature material directly in the PSBT
	// input witness/signature fields and return finalized checkpoint PSBTs.
	CoSignedCheckpointPSBTs []*psbt.Packet
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

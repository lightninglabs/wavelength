package oor

import (
	"bytes"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/rpc/oorpb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	oorOutboxProtoTypeURLPrefix = "type.lightninglabs.dev/wavelength/" +
		"oor/"
)

const (
	retryPayloadAfterNanosRecordType tlv.Type = 1
	retryPayloadReasonRecordType     tlv.Type = 3
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

// RequestArkSignatures asks the signing layer to attach client signature
// material for Ark inputs before submit.
//
// This is the explicit boundary where the client signs the Ark PSBT. The
// resulting signed Ark PSBT is then forwarded in SendSubmitPackageRequest.
type RequestArkSignatures struct {
	actor.BaseMessage

	// ArkPSBT is the canonical Ark transfer PSBT to sign.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs are the unsigned checkpoint PSBTs that correspond
	// to the Ark inputs. These are needed to map each Ark input back to
	// the transfer input that provides the client signing key.
	CheckpointPSBTs []*psbt.Packet

	// TransferInputs carry client signing context needed to authorize Ark
	// inputs.
	TransferInputs []TransferInput
}

// outboxType returns a stable identifier for this outbox message.
func (m *RequestArkSignatures) outboxType() string {
	return "RequestArkSignatures"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (m *RequestArkSignatures) outboxSealed() {}

// SendSubmitPackageRequest asks the transport layer to send the submit package
// (Ark PSBT + checkpoint PSBTs) to the server.
type SendSubmitPackageRequest struct {
	actor.BaseMessage

	// ArkPSBT is the canonical Ark transfer PSBT with client signature
	// material already attached for submit.
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

	// Recipients are the canonical non-anchor Ark outputs for the submit
	// package. When present, they carry optional output policy metadata for
	// operator-side persistence.
	Recipients []oortx.RecipientOutput
}

// outboxType returns a stable identifier for this outbox message.
func (m *SendSubmitPackageRequest) outboxType() string {
	return "SendSubmitPackageRequest"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (m *SendSubmitPackageRequest) outboxSealed() {}

// ServiceMethod returns the mailbox routing metadata for SubmitPackage.
func (m *SendSubmitPackageRequest) ServiceMethod() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{
		Service: oorpb.ServiceName,
		Method:  oorpb.MethodSubmitPackage,
	}
}

// sessionCorrelationKey is the canonical per-session FIFO key for
// client-to-server OOR outbox events. The client maintains a single
// durable serverconn mailbox per operator, so distinguishing sessions
// is sufficient to keep submit / finalize / ack for the same session
// from reordering under transient Edge.Send failure. The "oor/" prefix
// distinguishes the namespace from round keys so a session id cannot
// collide with a round id by accident.
func sessionCorrelationKey(sessionID SessionID) string {
	return "oor/" + sessionID.String()
}

// CorrelationKey returns the per-session FIFO key derived from the
// Ark PSBT. Falls back to the unkeyed lane if session derivation
// fails (e.g. a malformed PSBT) — the message would error out at the
// ToProto step anyway, so dropping FIFO enforcement on that pathological
// case is acceptable.
func (m *SendSubmitPackageRequest) CorrelationKey() string {
	sessionID, err := sessionIDFromArk(m.ArkPSBT)
	if err != nil {
		return ""
	}

	return sessionCorrelationKey(sessionID)
}

// ToProto converts SendSubmitPackageRequest to the concrete proto type
// expected by the server-side OOR dispatcher.
func (m *SendSubmitPackageRequest) ToProto() fn.Result[proto.Message] {
	descs := make([]oorpb.SigningDescriptor, 0, len(m.TransferInputs))
	for _, ti := range m.TransferInputs {
		vtxoPolicyTemplate, err := ti.EffectiveVTXOPolicyTemplate()
		if err != nil {
			return fn.Err[proto.Message](err)
		}

		spendPath, err := ti.EffectiveSpendPath()
		if err != nil {
			return fn.Err[proto.Message](err)
		}

		spendPathRaw, err := spendPath.Encode()
		if err != nil {
			return fn.Err[proto.Message](err)
		}

		desc := oorpb.SigningDescriptor{
			Outpoint:           ti.VTXO.Outpoint,
			VTXOPolicyTemplate: vtxoPolicyTemplate,
			SpendPath:          spendPathRaw,
			OwnerLeafPolicy:    ti.OwnerLeafPolicy,
		}
		descs = append(descs, desc)
	}

	req, err := oorpb.NewSubmitPackageRequest(
		m.ArkPSBT, m.CheckpointPSBTs, descs, m.Recipients,
	)
	if err != nil {
		return fn.Err[proto.Message](err)
	}

	// Stamp the OOR flow version this transfer is conducted under so the
	// operator records the same value. Every transfer is V1 today.
	//
	// TODO(v2): once a second OOR flow exists, stamp the session's own
	// version here instead of the V1 constant.
	req.FlowVersion = uint32(oorpb.FlowVersionV1)

	return fn.Ok[proto.Message](req)
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

// ServiceMethod returns the mailbox routing metadata for FinalizePackage.
func (m *SendFinalizePackageRequest) ServiceMethod() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{
		Service: oorpb.ServiceName,
		Method:  oorpb.MethodFinalizePackage,
	}
}

// CorrelationKey returns the per-session FIFO key so finalize lands
// in emission order behind the matching submit.
func (m *SendFinalizePackageRequest) CorrelationKey() string {
	sessionID, err := sessionIDFromArk(m.ArkPSBT)
	if err != nil {
		return ""
	}

	return sessionCorrelationKey(sessionID)
}

// ToProto converts SendFinalizePackageRequest to the concrete proto type
// expected by the server-side OOR dispatcher.
func (m *SendFinalizePackageRequest) ToProto() fn.Result[proto.Message] {
	sessionID, err := sessionIDFromArk(m.ArkPSBT)
	if err != nil {
		return fn.Err[proto.Message](err)
	}

	req, err := oorpb.NewFinalizePackageRequest(
		chainhash.Hash(sessionID), m.FinalCheckpointPSBTs,
	)
	if err != nil {
		return fn.Err[proto.Message](err)
	}

	return fn.Ok[proto.Message](req)
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

// ReleaseInputsRequest asks the persistence layer to release the spend
// reservation on the session's input VTXOs, returning them from
// SpendingState to LiveState. Emitted when an outgoing session fails
// terminally BEFORE the point of no return (the server co-signing the
// checkpoints), e.g. on a typed submit rejection: the server never
// locked the inputs, so holding the local reservation until a restart
// sweep would strand spendable funds for no reason.
type ReleaseInputsRequest struct {
	actor.BaseMessage

	// Outpoints are the reserved input VTXO outpoints to release.
	Outpoints []wire.OutPoint
}

// outboxType returns a stable identifier for this outbox message.
func (m *ReleaseInputsRequest) outboxType() string {
	return "ReleaseInputsRequest"
}

// outboxSealed marks this as implementing the sealed OutboxEvent
// interface.
func (m *ReleaseInputsRequest) outboxSealed() {}

// ToProto converts MarkInputsSpentRequest to a protobuf message.
func (m *MarkInputsSpentRequest) ToProto() fn.Result[proto.Message] {
	payload, err := encodeOutpoints(m.Outpoints)
	if err != nil {
		return fn.Err[proto.Message](err)
	}

	return fn.Ok[proto.Message](
		protoEnvelope("MarkInputsSpentRequest", payload),
	)
}

// ToProto converts ReleaseInputsRequest to a protobuf message.
func (m *ReleaseInputsRequest) ToProto() fn.Result[proto.Message] {
	payload, err := encodeOutpoints(m.Outpoints)
	if err != nil {
		return fn.Err[proto.Message](err)
	}

	return fn.Ok[proto.Message](
		protoEnvelope("ReleaseInputsRequest", payload),
	)
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

// QueryIncomingTransferRequest asks the transport layer to resolve a
// lightweight incoming OOR hint into the full Ark/checkpoint package for this
// session.
type QueryIncomingTransferRequest struct {
	actor.BaseMessage

	// SessionID identifies the incoming transfer session.
	SessionID SessionID

	// RecipientPkScript identifies the locally controlled output that the
	// server notified us about.
	RecipientPkScript []byte

	// RecipientEventID identifies the authoritative recipient event row to
	// resolve from the indexer.
	RecipientEventID uint64
}

// outboxType returns a stable identifier for this outbox message.
func (m *QueryIncomingTransferRequest) outboxType() string {
	return "QueryIncomingTransferRequest"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (m *QueryIncomingTransferRequest) outboxSealed() {}

// QueryIncomingMetadataRequest asks the transport layer to query the
// authoritative indexer inventory for the incoming Ark outputs referenced by
// this session.
type QueryIncomingMetadataRequest struct {
	actor.BaseMessage

	// SessionID identifies the incoming transfer session.
	SessionID SessionID

	// ArkPSBT is the canonical Ark tx PSBT.
	ArkPSBT *psbt.Packet

	// FinalCheckpointPSBTs are the finalized checkpoint packages associated
	// with this Ark transfer.
	FinalCheckpointPSBTs []*psbt.Packet

	// Recipients are the non-anchor recipient outputs in the Ark tx.
	Recipients []ArkRecipientOutput
}

// outboxType returns a stable identifier for this outbox message.
func (m *QueryIncomingMetadataRequest) outboxType() string {
	return "QueryIncomingMetadataRequest"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (m *QueryIncomingMetadataRequest) outboxSealed() {}

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

	// FinalCheckpointPSBTs are the finalized checkpoint packages associated
	// with this Ark transfer.
	FinalCheckpointPSBTs []*psbt.Packet

	// Recipients are the non-anchor recipient outputs in the Ark tx.
	Recipients []ArkRecipientOutput

	// MetadataMatches carries the authoritative lineage
	// metadata resolved for the current Ark outputs.
	MetadataMatches []IncomingMetadataMatch

	// AncestorPackages are finalized OOR packages needed to unroll the
	// incoming VTXO's OOR parent chain.
	AncestorPackages []PackageArtifact
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

// ServiceMethod returns the mailbox routing metadata for IncomingAck.
func (m *SendIncomingAckRequest) ServiceMethod() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{
		Service: oorpb.ServiceName,
		Method:  oorpb.MethodIncomingAck,
	}
}

// CorrelationKey returns the per-session FIFO key.
func (m *SendIncomingAckRequest) CorrelationKey() string {
	return sessionCorrelationKey(m.SessionID)
}

// ToProto converts SendIncomingAckRequest to a protobuf message.
func (m *SendIncomingAckRequest) ToProto() fn.Result[proto.Message] {
	payload, err := encodeSessionPayload(m.SessionID)
	if err != nil {
		return fn.Err[proto.Message](err)
	}

	return fn.Ok[proto.Message](
		protoEnvelope("SendIncomingAckRequest", payload),
	)
}

// ScheduleRetryRequest asks the runtime to resume the session after the
// requested delay.
type ScheduleRetryRequest struct {
	actor.BaseMessage

	After  time.Duration
	Reason string
}

// outboxType returns a stable identifier for this outbox message.
func (m *ScheduleRetryRequest) outboxType() string {
	return "ScheduleRetryRequest"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (m *ScheduleRetryRequest) outboxSealed() {}

// ToProto converts ScheduleRetryRequest to a protobuf message.
func (m *ScheduleRetryRequest) ToProto() fn.Result[proto.Message] {
	payload, err := encodeRetryPayload(m.After, m.Reason)
	if err != nil {
		return fn.Err[proto.Message](err)
	}

	return fn.Ok[proto.Message](
		protoEnvelope("ScheduleRetryRequest", payload),
	)
}

func protoEnvelope(typeName string, payload []byte) proto.Message {
	return &anypb.Any{
		TypeUrl: oorOutboxProtoTypeURLPrefix + typeName,
		Value:   payload,
	}
}

func encodeRetryPayload(after time.Duration, reason string) ([]byte, error) {
	if after < 0 {
		return nil, fmt.Errorf("retry delay must be non-negative")
	}

	afterNanos := uint64(after)
	reasonBytes := []byte(reason)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			retryPayloadAfterNanosRecordType, &afterNanos,
		),
		tlv.MakePrimitiveRecord(
			retryPayloadReasonRecordType, &reasonBytes,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

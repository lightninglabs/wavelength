package oor

import (
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightninglabs/darepo-client/rpc/oorpb"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightningnetwork/lnd/tlv"
	"google.golang.org/protobuf/proto"
)

// OORActorServiceKeyName is the receptionist key used to discover
// the OOR server-side actor in the actor system.
const OORActorServiceKeyName = "oor-server"

// NewServiceKey returns the service key for looking up the OOR
// server actor via the receptionist.
func NewServiceKey() actor.ServiceKey[OORDurableMsg, ActorResp] {
	return actor.NewServiceKey[OORDurableMsg, ActorResp](
		OORActorServiceKeyName,
	)
}

// TLV type constants for OOR actor messages. Each ActorMsg type has a stable
// identifier used for durable mailbox serialization. The 0x7xxx range avoids
// collisions with the actor framework's reserved types.
const (
	submitOORRequestTLVType   tlv.Type = 0x7001
	finalizeOORRequestTLVType tlv.Type = 0x7002
)

// TLV record type constants for submit request fields.
const (
	submitClientIDRecordType           tlv.Type = 0
	submitArkPSBTRecordType            tlv.Type = 1
	submitCheckpointPSBTsRecordType    tlv.Type = 2
	submitSigningDescriptorsRecordType tlv.Type = 3
)

// TLV record type constants for finalize request fields.
const (
	finalizeClientIDRecordType        tlv.Type = 0
	finalizeSessionIDRecordType       tlv.Type = 1
	finalizeCheckpointPSBTsRecordType tlv.Type = 2
)

// OORDurableMsg is the message constraint for the OOR durable actor mailbox.
// It embeds actor.TLVMessage so both application-level ActorMsg types and the
// framework-injected RestartMessage satisfy this interface. This gives a
// tighter, domain-specific type bound than raw actor.TLVMessage while still
// admitting the framework's restart path.
type OORDurableMsg interface {
	actor.TLVMessage
}

// ActorMsg is the sealed interface for all messages that can be sent to the
// TransferCoordinatorActor. It extends OORDurableMsg so each message type
// handles its own TLV serialization directly, allowing the durable actor to
// persist and dispatch messages without an intermediate envelope layer.
type ActorMsg interface {
	OORDurableMsg

	// actorMsgSealed marks this interface as sealed.
	actorMsgSealed()
}

// ActorResp is the sealed interface for all responses from the
// TransferCoordinatorActor.
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

	// ClientID identifies the submitting client for response routing
	// via clientconn. The bridge or RPC layer sets this before sending
	// the request to the coordinator actor.
	ClientID clientconn.ClientID

	// ArkPSBT is the transfer intent transaction.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs are the per-input checkpoint transactions.
	CheckpointPSBTs []*psbt.Packet

	// VTXOSigningDescriptors carry enough information for the operator to
	// co-sign each checkpoint tx by spending the collaborative leaf of the
	// input VTXO script.
	VTXOSigningDescriptors []VTXOSigningDescriptor
}

// MessageType returns the type of this message.
func (m *SubmitOORRequest) MessageType() string {
	return "SubmitOORRequest"
}

// actorMsgSealed marks this message as part of the ActorMsg sealed interface.
func (m *SubmitOORRequest) actorMsgSealed() {}

// TLVType returns the unique TLV type identifier for durable mailbox
// serialization.
func (m *SubmitOORRequest) TLVType() tlv.Type {
	return submitOORRequestTLVType
}

// Encode serializes the submit request to the provided writer.
func (m *SubmitOORRequest) Encode(w io.Writer) error {
	if m.ArkPSBT == nil {
		return fmt.Errorf("ark psbt must be provided")
	}

	clientIDBytes := []byte(m.ClientID)

	arkPSBTBytes, err := serializePSBT(m.ArkPSBT)
	if err != nil {
		return err
	}

	checkpointPSBTs, err := serializePSBTList(m.CheckpointPSBTs)
	if err != nil {
		return err
	}

	checkpointPSBTsBlob, err := encodeTLVByteList(checkpointPSBTs)
	if err != nil {
		return err
	}

	signingDescs := make([][]byte, 0, len(m.VTXOSigningDescriptors))
	for i, desc := range m.VTXOSigningDescriptors {
		descBlob, err := encodeSigningDescriptor(desc)
		if err != nil {
			return fmt.Errorf(
				"encode signing descriptor %d: %w", i, err,
			)
		}

		signingDescs = append(signingDescs, descBlob)
	}

	signingDescsBlob, err := encodeTLVByteList(signingDescs)
	if err != nil {
		return err
	}

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			submitClientIDRecordType, &clientIDBytes,
		),
		tlv.MakePrimitiveRecord(
			submitArkPSBTRecordType, &arkPSBTBytes,
		),
		tlv.MakePrimitiveRecord(
			submitCheckpointPSBTsRecordType,
			&checkpointPSBTsBlob,
		),
		tlv.MakePrimitiveRecord(
			submitSigningDescriptorsRecordType,
			&signingDescsBlob,
		),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes the submit request from the provided reader.
func (m *SubmitOORRequest) Decode(r io.Reader) error {
	var (
		clientIDBytes      []byte
		arkPSBTBytes       []byte
		checkpointPSBTsTLV []byte
		signingDescsTLV    []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			submitClientIDRecordType, &clientIDBytes,
		),
		tlv.MakePrimitiveRecord(
			submitArkPSBTRecordType, &arkPSBTBytes,
		),
		tlv.MakePrimitiveRecord(
			submitCheckpointPSBTsRecordType, &checkpointPSBTsTLV,
		),
		tlv.MakePrimitiveRecord(
			submitSigningDescriptorsRecordType, &signingDescsTLV,
		),
	)
	if err != nil {
		return err
	}

	parsed, err := stream.DecodeWithParsedTypes(r)
	if err != nil {
		return err
	}

	if _, ok := parsed[submitArkPSBTRecordType]; !ok {
		return fmt.Errorf("ark psbt must be provided")
	}

	arkPSBT, err := deserializePSBT(arkPSBTBytes)
	if err != nil {
		return err
	}

	checkpointPSBTBytes, err := decodeTLVByteList(checkpointPSBTsTLV)
	if err != nil {
		return err
	}

	checkpointPSBTs, err := deserializePSBTList(checkpointPSBTBytes)
	if err != nil {
		return err
	}

	signingDescBytes, err := decodeTLVByteList(signingDescsTLV)
	if err != nil {
		return err
	}

	signingDescs := make(
		[]VTXOSigningDescriptor, 0, len(signingDescBytes),
	)
	for i, descBlob := range signingDescBytes {
		desc, err := decodeSigningDescriptor(descBlob)
		if err != nil {
			return fmt.Errorf(
				"decode signing descriptor %d: %w", i, err,
			)
		}

		signingDescs = append(signingDescs, desc)
	}

	m.ClientID = clientconn.ClientID(clientIDBytes)
	m.ArkPSBT = arkPSBT
	m.CheckpointPSBTs = checkpointPSBTs
	m.VTXOSigningDescriptors = signingDescs

	return nil
}

// SubmitOORResponse is returned after the submit request is processed.
type SubmitOORResponse struct {
	actor.BaseMessage

	// clientID identifies the target client for response routing via
	// clientconn.
	clientID clientconn.ClientID

	// SessionID identifies the OOR session.
	SessionID SessionID

	// CoSignedCheckpointPSBTs are the checkpoint PSBTs after the operator
	// has attached its signature material.
	CoSignedCheckpointPSBTs []*psbt.Packet
}

// MessageType returns the type of this message.
func (m *SubmitOORResponse) MessageType() string {
	return "SubmitOORResponse"
}

// actorRespSealed marks this message as part of the ActorResp sealed interface.
func (m *SubmitOORResponse) actorRespSealed() {}

// ClientID returns the target client identifier for response routing.
func (m *SubmitOORResponse) ClientID() clientconn.ClientID {
	return m.clientID
}

// ToProto returns the proto event payload for envelope body construction.
func (m *SubmitOORResponse) ToProto() proto.Message {
	resp, err := oorpb.NewSubmitPackageResponse(
		chainhash.Hash(m.SessionID), m.CoSignedCheckpointPSBTs,
	)
	if err != nil {
		// Return nil on serialization failure; the downstream
		// durable delivery path checks for nil and returns an
		// error rather than panicking.
		return nil
	}

	return resp
}

// ServiceMethod returns the routing key for client-side ingress
// dispatch. Uses the same method name as the request RPC so the
// client's EventRouter can match on a single (Service, Method) pair.
func (m *SubmitOORResponse) ServiceMethod() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{
		Service: oorpb.ServiceName,
		Method:  oorpb.MethodSubmitPackage,
	}
}

// FinalizeOORRequest requests finalizing an existing OOR transfer session.
//
// Finalize package vocabulary:
//   - FinalCheckpointPSBTs are the same checkpoint transactions with client
//     finalize signature material attached.
type FinalizeOORRequest struct {
	actor.BaseMessage

	// ClientID identifies the requesting client for response routing
	// via clientconn.
	ClientID clientconn.ClientID

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

// TLVType returns the unique TLV type identifier for durable mailbox
// serialization.
func (m *FinalizeOORRequest) TLVType() tlv.Type {
	return finalizeOORRequestTLVType
}

// Encode serializes the finalize request to the provided writer.
func (m *FinalizeOORRequest) Encode(w io.Writer) error {
	clientIDBytes := []byte(m.ClientID)
	sessionID := [chainhash.HashSize]byte(m.SessionID)

	finalCheckpointPSBTs, err := serializePSBTList(m.FinalCheckpointPSBTs)
	if err != nil {
		return err
	}

	finalCheckpointPSBTsBlob, err := encodeTLVByteList(
		finalCheckpointPSBTs,
	)
	if err != nil {
		return err
	}

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			finalizeClientIDRecordType, &clientIDBytes,
		),
		tlv.MakePrimitiveRecord(
			finalizeSessionIDRecordType, &sessionID,
		),
		tlv.MakePrimitiveRecord(
			finalizeCheckpointPSBTsRecordType,
			&finalCheckpointPSBTsBlob,
		),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes the finalize request from the provided reader.
func (m *FinalizeOORRequest) Decode(r io.Reader) error {
	var (
		clientIDBytes        []byte
		sessionID            [chainhash.HashSize]byte
		finalCheckpointPSBTs []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			finalizeClientIDRecordType, &clientIDBytes,
		),
		tlv.MakePrimitiveRecord(
			finalizeSessionIDRecordType, &sessionID,
		),
		tlv.MakePrimitiveRecord(
			finalizeCheckpointPSBTsRecordType,
			&finalCheckpointPSBTs,
		),
	)
	if err != nil {
		return err
	}

	parsed, err := stream.DecodeWithParsedTypes(r)
	if err != nil {
		return err
	}

	if _, ok := parsed[finalizeSessionIDRecordType]; !ok {
		return fmt.Errorf("session id must be provided")
	}

	checkpointPSBTBytes, err := decodeTLVByteList(finalCheckpointPSBTs)
	if err != nil {
		return err
	}

	checkpointPSBTs, err := deserializePSBTList(checkpointPSBTBytes)
	if err != nil {
		return err
	}

	var hash chainhash.Hash
	copy(hash[:], sessionID[:])

	m.ClientID = clientconn.ClientID(clientIDBytes)
	m.SessionID = SessionID(hash)
	m.FinalCheckpointPSBTs = checkpointPSBTs

	return nil
}

// FinalizeOORResponse is returned after the finalize request is processed.
type FinalizeOORResponse struct {
	actor.BaseMessage

	// clientID identifies the target client for response routing via
	// clientconn.
	clientID clientconn.ClientID

	// SessionID identifies the finalized session.
	SessionID SessionID
}

// MessageType returns the type of this message.
func (m *FinalizeOORResponse) MessageType() string {
	return "FinalizeOORResponse"
}

// actorRespSealed marks this message as part of the ActorResp sealed interface.
func (m *FinalizeOORResponse) actorRespSealed() {}

// ClientID returns the target client identifier for response routing.
func (m *FinalizeOORResponse) ClientID() clientconn.ClientID {
	return m.clientID
}

// ToProto returns the proto event payload for envelope body construction.
func (m *FinalizeOORResponse) ToProto() proto.Message {
	return oorpb.NewFinalizePackageResponse(
		chainhash.Hash(m.SessionID),
	)
}

// ServiceMethod returns the routing key for client-side ingress
// dispatch. Uses the same method name as the request RPC so the
// client's EventRouter can match on a single (Service, Method) pair.
func (m *FinalizeOORResponse) ServiceMethod() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{
		Service: oorpb.ServiceName,
		Method:  oorpb.MethodFinalizePackage,
	}
}

// newOORActorCodec builds the durable mailbox codec for the coordinator.
// Each message type is registered individually, allowing the durable actor
// to serialize and dispatch messages without an intermediate envelope.
func newOORActorCodec() *actor.MessageCodec {
	codec := actor.NewMessageCodec()

	codec.MustRegister(submitOORRequestTLVType, func() actor.TLVMessage {
		return &SubmitOORRequest{}
	})
	codec.MustRegister(
		finalizeOORRequestTLVType, func() actor.TLVMessage {
			return &FinalizeOORRequest{}
		},
	)
	codec.MustRegister(actor.RestartTLVType, func() actor.TLVMessage {
		return &actor.RestartMessage{}
	})

	return codec
}

// Compile-time interface checks.
var (
	_ clientconn.ClientMessage = (*SubmitOORResponse)(nil)
	_ clientconn.ClientMessage = (*FinalizeOORResponse)(nil)
	_ ActorMsg                 = (*SubmitOORRequest)(nil)
	_ ActorMsg                 = (*FinalizeOORRequest)(nil)
)

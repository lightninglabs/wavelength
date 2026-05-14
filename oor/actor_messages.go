package oor

import (
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	oorlib "github.com/lightninglabs/darepo-client/lib/tx/oor"
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
	submitRecipientOutputsRecordType   tlv.Type = 4
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

	// Recipients are the canonical non-anchor Ark outputs plus optional
	// semantic policy metadata for the created VTXOs.
	Recipients []oorlib.RecipientOutput
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
			return fmt.Errorf("encode signing descriptor %d: %w", i,
				err)
		}

		signingDescs = append(signingDescs, descBlob)
	}

	signingDescsBlob, err := encodeTLVByteList(signingDescs)
	if err != nil {
		return err
	}

	recipientBlobs := make([][]byte, 0, len(m.Recipients))
	for i := range m.Recipients {
		blob, err := encodeRecipientOutput(m.Recipients[i])
		if err != nil {
			return fmt.Errorf("encode recipient %d: %w", i, err)
		}

		recipientBlobs = append(recipientBlobs, blob)
	}

	recipientsBlob, err := encodeTLVByteList(recipientBlobs)
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
			submitCheckpointPSBTsRecordType, &checkpointPSBTsBlob,
		),
		tlv.MakePrimitiveRecord(
			submitSigningDescriptorsRecordType, &signingDescsBlob,
		),
		tlv.MakePrimitiveRecord(
			submitRecipientOutputsRecordType, &recipientsBlob,
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
		recipientsTLV      []byte
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
		tlv.MakePrimitiveRecord(
			submitRecipientOutputsRecordType, &recipientsTLV,
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
			return fmt.Errorf("decode signing descriptor %d: %w", i,
				err)
		}

		signingDescs = append(signingDescs, desc)
	}

	recipientBytes, err := decodeTLVByteList(recipientsTLV)
	if err != nil {
		return err
	}

	recipients := make(
		[]oorlib.RecipientOutput, 0, len(recipientBytes),
	)
	for i, recipientBlob := range recipientBytes {
		recipient, err := decodeRecipientOutput(recipientBlob)
		if err != nil {
			return fmt.Errorf("decode recipient %d: %w", i, err)
		}

		recipients = append(recipients, recipient)
	}

	m.ClientID = clientconn.ClientID(clientIDBytes)
	m.ArkPSBT = arkPSBT
	m.CheckpointPSBTs = checkpointPSBTs
	m.VTXOSigningDescriptors = signingDescs
	m.Recipients = recipients

	return nil
}

// SubmitOORResponse is returned after the submit request is processed.
//
// The response carries either a success (CoSignedCheckpointPSBTs
// populated) or a typed rejection (Rejection != nil); the actor
// constructs whichever the FSM produced and the client side recovers
// the typed rejection via oorpb.ParseSubmitPackageResponse.
type SubmitOORResponse struct {
	actor.BaseMessage

	// clientID identifies the target client for response routing via
	// clientconn.
	clientID clientconn.ClientID

	// SessionID identifies the OOR session.
	SessionID SessionID

	// CoSignedCheckpointPSBTs are the checkpoint PSBTs after the
	// operator has attached its signature material. Populated on the
	// success branch only.
	CoSignedCheckpointPSBTs []*psbt.Packet

	// Rejection carries the typed rejection on the failure branch.
	// nil on success; non-nil on failure with Code/Reason set so the
	// proto envelope emits the rejection branch and the client can
	// route on the typed code via errors.As(&ErrLineageTooLarge).
	Rejection *SubmitOORRejection
}

// SubmitOORRejection carries the typed rejection material for a
// failed SubmitOORResponse. The actor populates it from the
// FailedState.Code that drove the FSM into terminal failure so the
// proto SubmitPackageRejection branch lands at the client with the
// same typed code the operator-side cap check produced.
type SubmitOORRejection struct {
	// Code is the typed reject code, mirroring FailedState.Code.
	Code RejectCode

	// Reason is the human-readable failure reason for logs/UX.
	Reason string
}

// oorSessionCorrelationKey is the canonical per-mailbox FIFO key for
// OOR response events. Two responses for the same client + session are
// claim-ordered by emission order regardless of retry backoff. The
// "oor/" prefix distinguishes the namespace from the rounds keys so a
// session id cannot accidentally collide with a round id.
func oorSessionCorrelationKey(clientID clientconn.ClientID,
	sessionID SessionID) string {

	return fmt.Sprintf("%s/oor/%s", clientID, sessionID)
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

// ToProto returns the proto event payload for envelope body
// construction. Emits the typed rejection branch when m.Rejection is
// set so clients recover the typed code via
// oorpb.ParseSubmitPackageResponse / errors.As; otherwise emits the
// success branch with the co-signed checkpoint PSBTs.
func (m *SubmitOORResponse) ToProto() proto.Message {
	if m.Rejection != nil {
		return oorpb.NewSubmitPackageRejection(
			chainhash.Hash(m.SessionID),
			rejectCodeToProto(m.Rejection.Code), m.Rejection.Reason,
		)
	}

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

// rejectCodeToProto maps the FSM-side RejectCode onto its proto-wire
// counterpart. Unknown codes default to OOR_REJECT_UNSPECIFIED so the
// client treats an unrecognized code as a generic rejection rather
// than panicking on an unmapped enum value.
func rejectCodeToProto(code RejectCode) oorpb.OORRejectCode {
	switch code {
	case RejectCodeLineageTooLarge:
		return oorpb.OORRejectCode_OOR_REJECT_LINEAGE_TOO_LARGE

	default:
		return oorpb.OORRejectCode_OOR_REJECT_UNSPECIFIED
	}
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

// CorrelationKey returns the per-client/session FIFO key so the submit
// response is delivered ahead of any later same-session response
// (e.g. the finalize ack) even when the first send transiently fails.
func (m *SubmitOORResponse) CorrelationKey() string {
	return oorSessionCorrelationKey(m.clientID, m.SessionID)
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

// CorrelationKey returns the per-client/session FIFO key. Together
// with SubmitOORResponse.CorrelationKey, this keeps the two responses
// for one OOR session ordered against each other on the per-client
// mailbox even when a Send transiently fails.
func (m *FinalizeOORResponse) CorrelationKey() string {
	return oorSessionCorrelationKey(m.clientID, m.SessionID)
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

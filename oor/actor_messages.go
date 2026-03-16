package oor

import (
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightningnetwork/lnd/tlv"
)

// OORActorServiceKeyName is the receptionist key used to discover the OOR
// client actor. The serverconn ingress event router uses this key to dispatch
// incoming server messages (SubmitAccepted, FinalizeAccepted, etc.) to the
// OOR actor.
const OORActorServiceKeyName = "oor-client"

// NewServiceKey returns the service key for looking up the OOR client actor
// in the actor system's receptionist. This key is used by the serverconn
// event router to dispatch incoming server events to the OOR actor.
func NewServiceKey() actor.ServiceKey[OORDurableMsg, ActorResp] {
	return actor.NewServiceKey[OORDurableMsg, ActorResp](
		OORActorServiceKeyName,
	)
}

// TLV type constants for OOR actor messages. Each ActorMsg type has a stable
// identifier used for durable mailbox serialization. The 0x7xxx range avoids
// collisions with the actor framework's reserved types.
const (
	StartTransferRequestTLVType    tlv.Type = 0x7010
	DriveEventRequestTLVType       tlv.Type = 0x7011
	GetStateRequestTLVType         tlv.Type = 0x7012
	RestoreSessionRequestTLVType   tlv.Type = 0x7013
	ResumeSessionRequestTLVType    tlv.Type = 0x7014
	ExportSnapshotRequestTLVType   tlv.Type = 0x7015
	ResolveIncomingTransferTLVType tlv.Type = 0x7016
)

// OORDurableMsg is the message constraint for the OOR durable actor mailbox.
// It embeds actor.TLVMessage so both application-level ActorMsg types and the
// framework-injected RestartMessage satisfy this interface. The constraint is
// structurally equivalent to actor.TLVMessage but provides a nominal type
// that signals "messages accepted by the OOR durable actor," mirroring the
// serverconn.ServerConnMsg pattern.
type OORDurableMsg interface {
	actor.TLVMessage
}

// ActorMsg is a sealed interface for messages that can be sent to the
// OORClientActor. It extends OORDurableMsg so each message type handles its
// own serialization directly, allowing the durable actor to persist and
// dispatch messages without an intermediate envelope layer.
type ActorMsg interface {
	OORDurableMsg
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
	Policy arkscript.CheckpointPolicy

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

// TLVType returns the unique TLV type identifier for this message.
func (m *StartTransferRequest) TLVType() tlv.Type {
	return StartTransferRequestTLVType
}

// Encode serializes the message to the provided writer.
func (m *StartTransferRequest) Encode(w io.Writer) error {
	payload := startTransferPayload{
		CSVDelay:   m.Policy.CSVDelay,
		Recipients: make([]recipientPayload, 0, len(m.Recipients)),
		Inputs: make(
			[]*TransferInputSnapshot, 0, len(m.Inputs),
		),
	}

	if m.Policy.OperatorKey != nil {
		payload.OperatorPubKey = m.Policy.OperatorKey.
			SerializeCompressed()
	}

	for i := range m.Inputs {
		snap, err := m.Inputs[i].ToSnapshot()
		if err != nil {
			return err
		}

		payload.Inputs = append(payload.Inputs, snap)
	}

	for i := range m.Recipients {
		payload.Recipients = append(
			payload.Recipients, recipientPayload{
				PkScript: m.Recipients[i].PkScript,
				ValueSat: int64(m.Recipients[i].Value),
			},
		)
	}

	raw, err := encodeStartTransferPayload(payload)
	if err != nil {
		return err
	}

	_, err = w.Write(raw)

	return err
}

// Decode deserializes the message from the provided reader.
func (m *StartTransferRequest) Decode(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	payload, err := decodeStartTransferPayload(raw)
	if err != nil {
		return err
	}

	operatorKey, err := btcec.ParsePubKey(payload.OperatorPubKey)
	if err != nil {
		return err
	}

	m.Policy = arkscript.CheckpointPolicy{
		OperatorKey: operatorKey,
		CSVDelay:    payload.CSVDelay,
	}

	m.Inputs = make([]TransferInput, 0, len(payload.Inputs))
	for i := range payload.Inputs {
		in, err := TransferInputFromSnapshot(payload.Inputs[i])
		if err != nil {
			return err
		}

		m.Inputs = append(m.Inputs, in)
	}

	m.Recipients = make(
		[]oortx.RecipientOutput, 0, len(payload.Recipients),
	)
	for i := range payload.Recipients {
		recipient := payload.Recipients[i]
		m.Recipients = append(m.Recipients, oortx.RecipientOutput{
			PkScript: recipient.PkScript,
			Value:    btcutil.Amount(recipient.ValueSat),
		})
	}

	return nil
}

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

// TLVType returns the unique TLV type identifier for this message.
func (m *DriveEventRequest) TLVType() tlv.Type {
	return DriveEventRequestTLVType
}

// Encode serializes the message to the provided writer. The nil receiver
// check handles typed-nil pointers (e.g. (*DriveEventRequest)(nil)) that
// pass interface nil checks but would panic on field access.
func (m *DriveEventRequest) Encode(w io.Writer) error {
	if m == nil {
		return fmt.Errorf("drive event request must be provided")
	}

	raw, err := encodeDriveEventRequestPayload(m.SessionID, m.Event)
	if err != nil {
		return err
	}

	_, err = w.Write(raw)

	return err
}

// Decode deserializes the message from the provided reader.
func (m *DriveEventRequest) Decode(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	sessionID, event, err := decodeDriveEventRequestPayload(raw)
	if err != nil {
		return err
	}

	m.SessionID = sessionID
	m.Event = event

	return nil
}

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

// ResolveIncomingTransferRequest asks the actor to durably record a
// lightweight incoming OOR notification and then resolve the full Ark package
// asynchronously outside the live actor transaction.
//
// The serverconn dispatcher persists only the lightweight hint into the durable
// OOR mailbox. The actor checkpoints a resolve-pending receive state first,
// then performs the follow-up unary RPC on a detached callback path so restart
// can safely re-drive the work.
type ResolveIncomingTransferRequest struct {
	actor.BaseMessage

	// SessionID identifies the incoming transfer session.
	SessionID SessionID

	// RecipientPkScript is the registered receive script that matched the
	// incoming OOR output.
	RecipientPkScript []byte

	// RecipientEventID is the monotonic per-script event ID
	// for the incoming recipient event. The actor uses this as
	// the cursor hint when querying the indexer for the full
	// Ark PSBT payload.
	RecipientEventID uint64
}

// MessageType returns the type of this message.
func (m *ResolveIncomingTransferRequest) MessageType() string {
	return "ResolveIncomingTransferRequest"
}

// actorMsgSealed marks this as implementing the sealed ActorMsg interface.
func (m *ResolveIncomingTransferRequest) actorMsgSealed() {}

// TLVType returns the unique TLV type identifier for this message.
func (m *ResolveIncomingTransferRequest) TLVType() tlv.Type {
	return ResolveIncomingTransferTLVType
}

// Encode serializes the message to the provided writer.
func (m *ResolveIncomingTransferRequest) Encode(w io.Writer) error {
	if m == nil {
		return fmt.Errorf("resolve incoming transfer request must " +
			"be provided")
	}

	raw, err := encodeResolveIncomingTransferPayload(
		m.SessionID, m.RecipientPkScript, m.RecipientEventID,
	)
	if err != nil {
		return err
	}

	_, err = w.Write(raw)

	return err
}

// Decode deserializes the message from the provided reader.
func (m *ResolveIncomingTransferRequest) Decode(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	sessionID, recipientPkScript, recipientEventID, err :=
		decodeResolveIncomingTransferPayload(raw)
	if err != nil {
		return err
	}

	m.SessionID = sessionID
	m.RecipientPkScript = recipientPkScript
	m.RecipientEventID = recipientEventID

	return nil
}

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

// TLVType returns the unique TLV type identifier for this message.
func (m *GetStateRequest) TLVType() tlv.Type {
	return GetStateRequestTLVType
}

// Encode serializes the message to the provided writer.
func (m *GetStateRequest) Encode(w io.Writer) error {
	raw, err := encodeSessionPayload(m.SessionID)
	if err != nil {
		return err
	}

	_, err = w.Write(raw)

	return err
}

// Decode deserializes the message from the provided reader.
func (m *GetStateRequest) Decode(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	sessionID, err := decodeSessionPayload(raw)
	if err != nil {
		return err
	}

	m.SessionID = sessionID

	return nil
}

// GetStateResponse returns the current session FSM state.
type GetStateResponse struct {
	actor.BaseMessage

	// State is the current session state machine state.
	State SessionState
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

// TLVType returns the unique TLV type identifier for this message.
func (m *RestoreSessionRequest) TLVType() tlv.Type {
	return RestoreSessionRequestTLVType
}

// Encode serializes the message to the provided writer.
func (m *RestoreSessionRequest) Encode(w io.Writer) error {
	raw, err := encodeRestoreSnapshotPayload(m.Snapshot)
	if err != nil {
		return err
	}

	_, err = w.Write(raw)

	return err
}

// Decode deserializes the message from the provided reader.
func (m *RestoreSessionRequest) Decode(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	snapshot, err := decodeRestoreSnapshotPayload(raw)
	if err != nil {
		return err
	}

	m.Snapshot = snapshot

	return nil
}

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

// TLVType returns the unique TLV type identifier for this message.
func (m *ResumeSessionRequest) TLVType() tlv.Type {
	return ResumeSessionRequestTLVType
}

// Encode serializes the message to the provided writer.
func (m *ResumeSessionRequest) Encode(w io.Writer) error {
	raw, err := encodeSessionPayload(m.SessionID)
	if err != nil {
		return err
	}

	_, err = w.Write(raw)

	return err
}

// Decode deserializes the message from the provided reader.
func (m *ResumeSessionRequest) Decode(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	sessionID, err := decodeSessionPayload(raw)
	if err != nil {
		return err
	}

	m.SessionID = sessionID

	return nil
}

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

// TLVType returns the unique TLV type identifier for this message.
func (m *ExportSnapshotRequest) TLVType() tlv.Type {
	return ExportSnapshotRequestTLVType
}

// Encode serializes the message to the provided writer.
func (m *ExportSnapshotRequest) Encode(w io.Writer) error {
	raw, err := encodeSessionPayload(m.SessionID)
	if err != nil {
		return err
	}

	_, err = w.Write(raw)

	return err
}

// Decode deserializes the message from the provided reader.
func (m *ExportSnapshotRequest) Decode(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	sessionID, err := decodeSessionPayload(raw)
	if err != nil {
		return err
	}

	m.SessionID = sessionID

	return nil
}

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

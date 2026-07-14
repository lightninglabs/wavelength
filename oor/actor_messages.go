package oor

import (
	"fmt"
	"io"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
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

	// ListSessionsRequestTLVType identifies ListSessionsRequest messages
	// in the durable OOR actor mailbox.
	ListSessionsRequestTLVType tlv.Type = 0x7017

	// SessionTerminalNotificationTLVType identifies
	// SessionTerminalNotification messages sent from a per-session child
	// to the registry coordinator after a terminal commit.
	SessionTerminalNotificationTLVType tlv.Type = 0x7019

	// RestoreNonTerminalRequestTLVType identifies the boot-time control
	// message that runs the non-terminal session restore on the registry
	// goroutine.
	RestoreNonTerminalRequestTLVType tlv.Type = 0x701a
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

	limits ReceiveLimits

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

	// IdempotencyKey identifies this caller intent across crashes and
	// retries. Empty preserves the historical deterministic-session
	// behavior.
	IdempotencyKey string

	// PreparedSubmit is the optional asset-committed graph. Its PSBT and
	// sealed package bytes are persisted in this durable request before the
	// session starts.
	PreparedSubmit *PreparedSubmitPackage
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
		CSVDelay:       m.Policy.CSVDelay,
		IdempotencyKey: m.IdempotencyKey,
		Recipients:     make([]recipientPayload, 0, len(m.Recipients)),
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
				VTXOPolicyTemplate: m.Recipients[i].
					VTXOPolicyTemplate,
				TaprootAssetRoot: m.Recipients[i].
					TaprootAssetRoot,
			},
		)
	}

	if m.PreparedSubmit != nil {
		if err := m.PreparedSubmit.Validate(
			m.Inputs, m.Recipients,
		); err != nil {
			return err
		}

		submitRaw, err := oortx.MarshalSubmitPackage(
			&oortx.SubmitPackage{
				ArkPSBT: m.PreparedSubmit.ArkPSBT,
				CheckpointPSBTs: m.PreparedSubmit.
					CheckpointPSBTs,
			},
		)
		if err != nil {
			return err
		}
		assetRaw, err := m.PreparedSubmit.TaprootAssetTransfer.
			MarshalBinary()
		if err != nil {
			return err
		}

		payload.PreparedSubmit = submitRaw
		payload.AssetTransfer = assetRaw
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

	payload, err := decodeStartTransferPayloadWithLimits(
		raw, m.limits,
	)
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
	m.IdempotencyKey = payload.IdempotencyKey

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
			VTXOPolicyTemplate: recipient.
				VTXOPolicyTemplate,
			TaprootAssetRoot: recipient.TaprootAssetRoot,
		})
	}

	m.PreparedSubmit = nil
	if len(payload.PreparedSubmit) > 0 || len(payload.AssetTransfer) > 0 {
		if len(payload.PreparedSubmit) == 0 ||
			len(payload.AssetTransfer) == 0 {
			return fmt.Errorf("prepared submit and asset " +
				"transfer must both be provided")
		}

		submit, err := oortx.UnmarshalSubmitPackage(
			payload.PreparedSubmit,
		)
		if err != nil {
			return err
		}
		assetTransfer := &oortx.TaprootAssetTransfer{}
		if err := assetTransfer.UnmarshalBinary(
			payload.AssetTransfer,
		); err != nil {
			return err
		}

		m.PreparedSubmit = &PreparedSubmitPackage{
			ArkPSBT:              submit.ArkPSBT,
			CheckpointPSBTs:      submit.CheckpointPSBTs,
			TaprootAssetTransfer: assetTransfer,
		}
		if err := m.PreparedSubmit.Validate(
			m.Inputs, m.Recipients,
		); err != nil {
			return err
		}
	}

	return nil
}

// StartTransferResponse returns the session identifier for a start request.
type StartTransferResponse struct {
	actor.BaseMessage

	// SessionID is the stable v0 session identifier (Ark txid).
	SessionID SessionID

	// Existing is true when the actor returned an already-known session
	// instead of creating a new one for this request.
	Existing bool
}

// MessageType returns the type of this message.
func (m *StartTransferResponse) MessageType() string {
	return "StartTransferResponse"
}

// actorRespSealed marks this as implementing the sealed ActorResp interface.
func (m *StartTransferResponse) actorRespSealed() {}

// SessionDirection is a filter for listing locally known OOR sessions.
type SessionDirection uint8

const (
	// SessionDirectionAll includes outgoing and incoming sessions.
	SessionDirectionAll SessionDirection = iota

	// SessionDirectionOutgoing includes locally sent OOR sessions.
	SessionDirectionOutgoing

	// SessionDirectionIncoming includes locally received OOR sessions.
	SessionDirectionIncoming
)

// ListSessionsRequest asks the OOR actor for compact summaries of its
// in-memory sessions.
type ListSessionsRequest struct {
	actor.BaseMessage

	// Direction restricts the listed sessions by local direction.
	Direction SessionDirection

	// PendingOnly restricts the result to non-terminal sessions.
	PendingOnly bool
}

// MessageType returns the type of this message.
func (m *ListSessionsRequest) MessageType() string {
	return "ListSessionsRequest"
}

// actorMsgSealed marks this as implementing the sealed ActorMsg interface.
func (m *ListSessionsRequest) actorMsgSealed() {}

// TLVType returns the unique TLV type identifier for this message.
func (m *ListSessionsRequest) TLVType() tlv.Type {
	return ListSessionsRequestTLVType
}

// Encode serializes the message to the provided writer.
func (m *ListSessionsRequest) Encode(w io.Writer) error {
	if m == nil {
		return fmt.Errorf("list sessions request must be provided")
	}

	raw, err := encodeListSessionsPayload(m.Direction, m.PendingOnly)
	if err != nil {
		return err
	}

	_, err = w.Write(raw)

	return err
}

// Decode deserializes the message from the provided reader.
func (m *ListSessionsRequest) Decode(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	direction, pendingOnly, err := decodeListSessionsPayload(raw)
	if err != nil {
		return err
	}

	m.Direction = direction
	m.PendingOnly = pendingOnly

	return nil
}

// SessionSummary is a compact operation-status view of one OOR session.
type SessionSummary struct {
	// SessionID is the stable OOR session identifier.
	SessionID SessionID

	// Direction is from the local client perspective.
	Direction SessionDirection

	// Phase is the detailed FSM phase string.
	Phase string

	// Pending is true while the session is not terminal.
	Pending bool

	// RetryAfter is the pending retry delay, when one is scheduled.
	RetryAfter time.Duration

	// RetryReason is the pending retry or terminal failure reason.
	RetryReason string

	// InputOutpoints are locally known outgoing input VTXOs.
	InputOutpoints []wire.OutPoint

	// InputAmountSat is the total value of InputOutpoints.
	InputAmountSat int64

	// RecipientCount is the number of Ark recipients in an outgoing
	// transfer.
	RecipientCount int
}

// ListSessionsResponse returns OOR session summaries.
type ListSessionsResponse struct {
	actor.BaseMessage

	// Sessions contains matching OOR session summaries.
	Sessions []SessionSummary
}

// MessageType returns the type of this message.
func (m *ListSessionsResponse) MessageType() string {
	return "ListSessionsResponse"
}

// actorRespSealed marks this as implementing the sealed ActorResp interface.
func (m *ListSessionsResponse) actorRespSealed() {}

// DriveEventRequest asks the actor to feed an event into an existing session.
//
// This is the generic adapter boundary for future RPC/server notifications.
type DriveEventRequest struct {
	actor.BaseMessage

	limits ReceiveLimits

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

	sessionID, event, err := decodeDriveEventRequestPayloadWithLimits(
		raw, m.limits,
	)
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

	limits ReceiveLimits

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
		return fmt.Errorf("resolve incoming transfer request must be " +
			"provided")
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
		decodeResolveIncomingTransferPayloadWithLimits(
			raw, m.limits,
		)
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

	limits ReceiveLimits

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

	snapshot, err := decodeRestoreSnapshotPayloadWithLimits(
		raw, m.limits,
	)
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

	// FromRetryTimer is true when this resume was driven by a fired
	// give-up/retry timer, and false when it was driven by a boot restore.
	// Only a timer expiry advances the give-up attempt counter; a boot
	// resume re-arms the timer from the persisted count so repeated
	// restarts cannot amplify the attempt count past the time-based
	// schedule.
	FromRetryTimer bool
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
	raw, err := encodeResumePayload(m.SessionID, m.FromRetryTimer)
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

	sessionID, fromRetryTimer, err := decodeResumePayload(raw)
	if err != nil {
		return err
	}

	m.SessionID = sessionID
	m.FromRetryTimer = fromRetryTimer

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

// SessionTerminalNotification tells the registry coordinator that a session
// committed a terminal snapshot, so the registry can stop the per-session
// child and drop it from the active set. The registry re-checks the durable
// row before reaping, so a stale or duplicate notification is harmless.
type SessionTerminalNotification struct {
	actor.BaseMessage

	// SessionID identifies the session that reached a terminal status.
	SessionID SessionID
}

// MessageType returns the type of this message.
func (m *SessionTerminalNotification) MessageType() string {
	return "SessionTerminalNotification"
}

// actorMsgSealed marks this as implementing the sealed ActorMsg interface.
func (m *SessionTerminalNotification) actorMsgSealed() {}

// TLVType returns the unique TLV type identifier for this message.
func (m *SessionTerminalNotification) TLVType() tlv.Type {
	return SessionTerminalNotificationTLVType
}

// Encode serializes the message to the provided writer.
func (m *SessionTerminalNotification) Encode(w io.Writer) error {
	raw, err := encodeSessionPayload(m.SessionID)
	if err != nil {
		return err
	}

	_, err = w.Write(raw)

	return err
}

// Decode deserializes the message from the provided reader.
func (m *SessionTerminalNotification) Decode(r io.Reader) error {
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

// RestoreNonTerminalRequest asks the registry to respawn and resume every
// non-terminal session from the control-plane store. Routing it through the
// registry mailbox serializes the restore with any backlog the durable inbox
// redelivers at boot, so the active set is only ever touched on the registry
// goroutine.
type RestoreNonTerminalRequest struct {
	actor.BaseMessage
}

// MessageType returns the type of this message.
func (m *RestoreNonTerminalRequest) MessageType() string {
	return "RestoreNonTerminalRequest"
}

// actorMsgSealed marks this as implementing the sealed ActorMsg interface.
func (m *RestoreNonTerminalRequest) actorMsgSealed() {}

// TLVType returns the unique TLV type identifier for this message.
func (m *RestoreNonTerminalRequest) TLVType() tlv.Type {
	return RestoreNonTerminalRequestTLVType
}

// Encode serializes the message; it carries no payload.
func (m *RestoreNonTerminalRequest) Encode(io.Writer) error {
	return nil
}

// Decode deserializes the message; it carries no payload.
func (m *RestoreNonTerminalRequest) Decode(io.Reader) error {
	return nil
}

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

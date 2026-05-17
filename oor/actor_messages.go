package oor

import (
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
)

// OORActorServiceKeyName is the receptionist key used to discover the OOR
// client actor. The serverconn ingress event router uses this key to dispatch
// incoming server messages (SubmitAccepted, FinalizeAccepted, etc.) to the
// OOR actor.
const OORActorServiceKeyName = "oor-client"

// NewServiceKey returns the service key for looking up the OOR client actor
// in the actor system's receptionist. This key is used by the serverconn
// event router to dispatch incoming server events to the OOR actor.
func NewServiceKey() actor.ServiceKey[ActorMsg, ActorResp] {
	return actor.NewServiceKey[ActorMsg, ActorResp](
		OORActorServiceKeyName,
	)
}

// ActorMsg is a sealed interface for messages that can be sent to the
// OORClientActor. These messages are in-memory workflow commands; restart
// safety is moving to SQL domain/effect rows instead of local actor payloads.
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
}

// MessageType returns the type of this message.
func (m *StartTransferRequest) MessageType() string {
	return "StartTransferRequest"
}

// actorMsgSealed marks this as implementing the sealed ActorMsg interface.
func (m *StartTransferRequest) actorMsgSealed() {}

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

// FindOutgoingSessionByIdempotencyKeyRequest asks the actor whether an
// outgoing session already exists for a caller supplied idempotency key.
type FindOutgoingSessionByIdempotencyKeyRequest struct {
	actor.BaseMessage

	// IdempotencyKey identifies the caller intent to query.
	IdempotencyKey string
}

// MessageType returns the type of this message.
func (m *FindOutgoingSessionByIdempotencyKeyRequest) MessageType() string {
	return "FindOutgoingSessionByIdempotencyKeyRequest"
}

// actorMsgSealed marks this as implementing the sealed ActorMsg interface.
func (m *FindOutgoingSessionByIdempotencyKeyRequest) actorMsgSealed() {}

// FindOutgoingSessionByIdempotencyKeyResponse returns a keyed session lookup
// result.
type FindOutgoingSessionByIdempotencyKeyResponse struct {
	actor.BaseMessage

	// SessionID is the existing outgoing session identifier when Found is
	// true.
	SessionID SessionID

	// Found is true when the actor already knows the keyed outgoing
	// session.
	Found bool
}

// MessageType returns the type of this message.
func (m *FindOutgoingSessionByIdempotencyKeyResponse) MessageType() string {
	return "FindOutgoingSessionByIdempotencyKeyResponse"
}

// actorRespSealed marks this as implementing the sealed ActorResp interface.
func (m *FindOutgoingSessionByIdempotencyKeyResponse) actorRespSealed() {}

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

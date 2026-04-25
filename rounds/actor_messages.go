package rounds

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/timeout"
	"github.com/lightninglabs/darepo/clientconn"
)

// ActorMsg is the sealed interface for all messages that can be sent to the
// server round Actor.
type ActorMsg interface {
	actor.Message

	// actorMsgSealed marks this interface as sealed, preventing external
	// implementations.
	actorMsgSealed()
}

// ActorResp is the sealed interface for all response messages from a server
// rounds Actor.
type ActorResp interface {
	actor.Message

	// actorRespSealed marks this interface as sealed, preventing external
	// implementations.
	actorRespSealed()
}

// TimeoutMsg is sent to the actor when a timeout expires. The actor parses the
// composite timeout ID to extract the round ID and phase, then sends the
// appropriate phase-specific timeout event to the round's FSM.
type TimeoutMsg struct {
	actor.BaseMessage

	// TimeoutID is the composite ID of the timeout that expired. It has the
	// format "roundID:phase" (e.g., "abc-123:registration").
	TimeoutID timeout.ID
}

// MessageType returns the type name of this message.
func (m *TimeoutMsg) MessageType() string {
	return "TimeoutMsg"
}

// actorMsgSealed marks this message as part of the ActorMsg sealed interface.
func (m *TimeoutMsg) actorMsgSealed() {}

// RoundMsg is a wrapper message that forwards an Event to a specific
// round's FSM.
type RoundMsg struct {
	actor.BaseMessage

	// RoundID identifies which round this event is for.
	RoundID RoundID

	// Event is the event to forward to the round's FSM.
	Event
}

// MessageType returns the type name of this message.
func (m *RoundMsg) MessageType() string {
	return "RoundMsg"
}

// actorMsgSealed marks this message as part of the ActorMsg sealed interface.
func (m *RoundMsg) actorMsgSealed() {}

// JoinRoundRequest is sent by the RPC layer when a client wants to join a
// round.
type JoinRoundRequest struct {
	actor.BaseMessage

	// ClientID is the unique identifier for the client connection.
	ClientID clientconn.ClientID

	// Request contains the client's join round parameters.
	Request *types.JoinRoundRequest
}

// MessageType returns the type name of this message.
func (m *JoinRoundRequest) MessageType() string {
	return "JoinRoundRequest"
}

// actorMsgSealed marks this message as part of the ActorMsg sealed interface.
func (m *JoinRoundRequest) actorMsgSealed() {}

// ConfirmationMsg is sent to the actor when a round's commitment transaction
// has been confirmed on-chain. The actor forwards this as a
// TransactionConfirmedEvent to the appropriate round's FSM.
type ConfirmationMsg struct {
	actor.BaseMessage

	// RoundID identifies which round was confirmed.
	RoundID RoundID

	// BlockHeight is the height of the block containing the transaction.
	BlockHeight int32

	// BlockHash is the hash of the block containing the transaction.
	BlockHash chainhash.Hash

	// NumConfs is the number of confirmations at the time of notification.
	NumConfs uint32
}

// MessageType returns the type name of this message.
func (m *ConfirmationMsg) MessageType() string {
	return "ConfirmationMsg"
}

// actorMsgSealed marks this message as part of the ActorMsg sealed interface.
func (m *ConfirmationMsg) actorMsgSealed() {}

// TriggerBatchMsg is sent by the admin RPC to manually seal the current
// registration round. The actor identifies the live round internally and
// sends a SealEvent to its FSM.
type TriggerBatchMsg struct {
	actor.BaseMessage
}

// MessageType returns the type name of this message.
func (m *TriggerBatchMsg) MessageType() string {
	return "TriggerBatchMsg"
}

// actorMsgSealed marks this message as part of the ActorMsg sealed interface.
func (m *TriggerBatchMsg) actorMsgSealed() {}

// TriggerBatchResp is the response to a TriggerBatchMsg. It returns the
// round ID that was sealed.
type TriggerBatchResp struct {
	actor.BaseMessage

	// RoundID is the ID of the round that was sealed.
	RoundID RoundID
}

// MessageType returns the type name of this message.
func (m *TriggerBatchResp) MessageType() string {
	return "TriggerBatchResp"
}

// actorRespSealed marks this as part of the ActorResp sealed interface.
func (m *TriggerBatchResp) actorRespSealed() {}

// GetClientRoundsRequest requests the list of rounds a client is participating
// in. This goes through the actor for concurrency safety.
type GetClientRoundsRequest struct {
	actor.BaseMessage

	// ClientID is the client to query.
	ClientID clientconn.ClientID
}

// MessageType returns the type name of this message.
func (m *GetClientRoundsRequest) MessageType() string {
	return "GetClientRoundsRequest"
}

// actorMsgSealed marks this message as part of the ActorMsg sealed interface.
func (m *GetClientRoundsRequest) actorMsgSealed() {}

// GetClientRoundsResponse is the response to GetClientRoundsRequest.
type GetClientRoundsResponse struct {
	actor.BaseMessage

	// RoundIDs is the list of rounds the client is participating in.
	RoundIDs []RoundID
}

// MessageType returns the type name of this message.
func (m *GetClientRoundsResponse) MessageType() string {
	return "GetClientRoundsResponse"
}

// actorRespSealed marks this as part of the ActorResp sealed interface.
func (m *GetClientRoundsResponse) actorRespSealed() {}

// GetRoundStatusReq is sent by the admin RPC to fetch observability
// detail for a specific round (current state, quote-phase counts,
// current seal pass, quote expiry). The actor responds with a
// GetRoundStatusResp populated from a read-only snapshot of the
// target round's current FSM state.
type GetRoundStatusReq struct {
	actor.BaseMessage

	// RoundID identifies the round to query.
	RoundID RoundID
}

// MessageType returns the type name of this message.
func (m *GetRoundStatusReq) MessageType() string {
	return "GetRoundStatusReq"
}

// actorMsgSealed marks this message as part of the ActorMsg sealed interface.
func (m *GetRoundStatusReq) actorMsgSealed() {}

// GetRoundStatusResp is the response to GetRoundStatusReq. Every
// counter field defaults to zero for states that do not track the
// corresponding metric (e.g. QuotesAccepted is 0 until the round
// has entered QuoteSentState). RoundNotFound is set to true when no
// live FSM exists for the requested round_id — callers should check
// this before reading any other field.
type GetRoundStatusResp struct {
	actor.BaseMessage

	// RoundID echoes the request's round_id for correlation.
	RoundID RoundID

	// RoundNotFound is true when no live FSM exists for RoundID.
	RoundNotFound bool

	// StateName is the current FSM state name ("IntentCollecting",
	// "QuoteSent", "BatchBuilding", etc.). Empty when
	// RoundNotFound.
	StateName string

	// IntentCount is the number of clients registered in the
	// current round (maxes at the registration-timeout clamp).
	IntentCount uint32

	// QuotesSent is the number of quotes fanned out in the current
	// pass (zero outside QuoteSentState).
	QuotesSent uint32

	// QuotesAccepted is the number of clients in QuoteAccepted
	// status for the current pass.
	QuotesAccepted uint32

	// QuotesRejected is the number of clients in QuoteRejected
	// status for the current pass.
	QuotesRejected uint32

	// QuotesTimedOut is the number of clients in QuoteTimedOut
	// status for the current pass.
	QuotesTimedOut uint32

	// CurrentSealPass is the zero-indexed pass number (zero on
	// the initial SealEvent, incremented on every reseal).
	CurrentSealPass uint32

	// QuoteExpiresAt is the unix timestamp (seconds) at which the
	// current pass's quotes time out. Zero outside QuoteSentState.
	QuoteExpiresAt int64
}

// MessageType returns the type name of this message.
func (m *GetRoundStatusResp) MessageType() string {
	return "GetRoundStatusResp"
}

// actorRespSealed marks this as part of the ActorResp sealed interface.
func (m *GetRoundStatusResp) actorRespSealed() {}

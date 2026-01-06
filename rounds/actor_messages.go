package rounds

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/timeout"
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

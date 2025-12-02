package round

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/wallet"
)

// ClientMsg is the sealed interface for all messages that can be sent to a
// RoundClientActor.
type ClientMsg interface {
	actor.Message
	clientMsgSealed()
}

// ClientResp is the sealed interface for all response messages from a
// RoundClientActor.
type ClientResp interface {
	actor.Message
	clientRespSealed()
}

// WalletBoardingConfirmed wraps a wallet.BoardingUtxoConfirmedEvent to make it
// compatible with the ClientMsg sealed interface. This enables the round actor
// to receive boarding UTXO confirmations from the wallet actor.
type WalletBoardingConfirmed struct {
	actor.BaseMessage

	// Intent is the confirmed boarding intent from the wallet. Contains
	// the address, outpoint, chain info (amount, conf height/hash), and
	// status.
	Intent *wallet.BoardingIntent
}

func (m *WalletBoardingConfirmed) MessageType() string {
	return "WalletBoardingConfirmed"
}

func (m *WalletBoardingConfirmed) clientMsgSealed() {}

// ServerMessageNotification delivers a server FSM outbox message to the client.
type ServerMessageNotification struct {
	actor.BaseMessage

	// Message is a ClientEvent from the server (RoundJoined,
	// CommitmentTxBuilt, NoncesAggregated, OperatorSigned, BoardingFailed).
	Message ClientEvent
}

func (m *ServerMessageNotification) MessageType() string {
	return "ServerMessageNotification"
}

func (m *ServerMessageNotification) clientMsgSealed() {}

// ServerMessageResponse acknowledges receipt of a server message.
type ServerMessageResponse struct {
	actor.BaseMessage

	Success bool
	Error   string
}

func (m *ServerMessageResponse) MessageType() string {
	return "ServerMessageResponse"
}

func (m *ServerMessageResponse) clientRespSealed() {}

// GetClientStateRequest queries the current client state.
type GetClientStateRequest struct {
	actor.BaseMessage
}

func (m *GetClientStateRequest) MessageType() string {
	return "GetClientStateRequest"
}

func (m *GetClientStateRequest) clientMsgSealed() {}

// FSMStateInfo contains information about a single FSM's current state.
type FSMStateInfo struct {
	// State is the actual state object (any ClientState implementation).
	State ClientState

	// IsPrimary indicates whether this is the primary FSM.
	IsPrimary bool

	// RoundID is the round ID (empty for primary FSM).
	RoundID string
}

// GetClientStateResponse returns the current state of all FSMs.
type GetClientStateResponse struct {
	actor.BaseMessage

	// States maps FSM identifier to state info. Key "primary" for the
	// primary FSM, round IDs for round FSMs.
	States map[string]FSMStateInfo
}

func (m *GetClientStateResponse) MessageType() string {
	return "GetClientStateResponse"
}

func (m *GetClientStateResponse) clientRespSealed() {}

// CancelRoundRequest cancels participation in the current round.
type CancelRoundRequest struct {
	actor.BaseMessage
}

func (m *CancelRoundRequest) MessageType() string {
	return "CancelRoundRequest"
}

func (m *CancelRoundRequest) clientMsgSealed() {}

// CancelRoundResponse confirms cancellation.
type CancelRoundResponse struct {
	actor.BaseMessage

	Success bool
	Error   string
}

func (m *CancelRoundResponse) MessageType() string {
	return "CancelRoundResponse"
}

func (m *CancelRoundResponse) clientRespSealed() {}

// RegisterBoardingIntentRequest informs the FSM that the wallet has funded or
// will fund a specific boarding address so confirmations should be tracked.
type RegisterBoardingIntentRequest struct {
	actor.BaseMessage

	Address      *BoardingAddress
	VTXORequests []*types.VTXORequest
}

func (m *RegisterBoardingIntentRequest) MessageType() string {
	return "RegisterBoardingIntentRequest"
}

func (m *RegisterBoardingIntentRequest) clientMsgSealed() {}

// RegisterBoardingIntentResponse acknowledges the request.
type RegisterBoardingIntentResponse struct {
	actor.BaseMessage

	Success bool
	Error   string
}

func (m *RegisterBoardingIntentResponse) MessageType() string {
	return "RegisterBoardingIntentResponse"
}

func (m *RegisterBoardingIntentResponse) clientRespSealed() {}

// ConfirmationEvent wraps a chain confirmation event from ChainSource.
// This allows the actor to receive confirmation notifications.
type ConfirmationEvent struct {
	actor.BaseMessage

	// Txid is the transaction that was confirmed.
	Txid chainhash.Hash

	// BlockHeight is the height at which the transaction was confirmed.
	BlockHeight int32

	// BlockHash is the hash of the block containing the transaction.
	BlockHash chainhash.Hash

	// Confirmations is the number of confirmations.
	Confirmations uint32

	// Tx is the confirmed transaction. This allows the actor to scan
	// outputs to find the specific UTXO that matches the boarding address.
	Tx *wire.MsgTx
}

func (m *ConfirmationEvent) MessageType() string {
	return "ConfirmationEvent"
}

func (m *ConfirmationEvent) clientMsgSealed() {}

// ============================================================================
// Server Actor Messages
// ============================================================================

// ServerMsg is the sealed interface for all messages that can be sent to a
// RoundServerActor.
type ServerMsg interface {
	actor.Message
	serverMsgSealed()
}

// ServerResp is the sealed interface for all response messages from a
// RoundServerActor.
type ServerResp interface {
	actor.Message
	serverRespSealed()
}

// TODO: Add server actor message types (JoinRoundRequest, SubmitNoncesRequest,
// etc.) here. These are the actor-level messages, not the FSM outbox messages.

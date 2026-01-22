package round

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// ClientMsg embeds actormsg.RoundReceivable for messages that can be sent to a
// RoundClientActor. Both round-internal messages and messages from other actors
// (vtxo, wallet) implement the RoundReceivable marker method.
type ClientMsg interface {
	actormsg.RoundReceivable
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

// RoundReceivable implements actormsg.RoundReceivable marker interface.
func (m *WalletBoardingConfirmed) RoundReceivable() {}

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

// RoundReceivable implements actormsg.RoundReceivable marker interface.
func (m *ServerMessageNotification) RoundReceivable() {}

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

// RoundReceivable implements actormsg.RoundReceivable marker interface.
func (m *GetClientStateRequest) RoundReceivable() {}

// FSMStateInfo contains information about a single FSM's current state.
type FSMStateInfo struct {
	// State is the actual state object (any ClientState implementation).
	State ClientState

	// IsTemp indicates whether this round has a temp key (not yet assigned
	// a RoundID by the server). Temp-keyed rounds are in the process of
	// joining a round but haven't received a RoundJoined response yet.
	IsTemp bool

	// RoundID is the server-assigned round ID (zero value for temp-keyed
	// rounds).
	RoundID RoundID
}

// GetClientStateResponse returns the current state of all FSMs.
type GetClientStateResponse struct {
	actor.BaseMessage

	// States maps FSM identifier to state info. Keys are either temp key
	// strings (for pending rounds) or RoundID strings (for rounds that
	// have been assigned an ID by the server).
	States map[string]FSMStateInfo
}

func (m *GetClientStateResponse) MessageType() string {
	return "GetClientStateResponse"
}

func (m *GetClientStateResponse) clientRespSealed() {}

// CancelRoundRequest cancels participation in a round.
type CancelRoundRequest struct {
	actor.BaseMessage

	// RoundKey is the optional key of the round to cancel. If not
	// specified, the first temp-keyed round will be cancelled.
	RoundKey fn.Option[RoundKeyStr]
}

func (m *CancelRoundRequest) MessageType() string {
	return "CancelRoundRequest"
}

// RoundReceivable implements actormsg.RoundReceivable marker interface.
func (m *CancelRoundRequest) RoundReceivable() {}

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

// RegisterVTXORequestsRequest informs the FSM of VTXO request amounts to
// include in the next round registration.
type RegisterVTXORequestsRequest struct {
	actor.BaseMessage

	Amounts []btcutil.Amount
}

// MessageType returns the message type name.
func (m *RegisterVTXORequestsRequest) MessageType() string {
	return "RegisterVTXORequestsRequest"
}

// RoundReceivable implements actormsg.RoundReceivable marker interface.
func (m *RegisterVTXORequestsRequest) RoundReceivable() {}

// RegisterVTXORequestsResponse acknowledges the request.
type RegisterVTXORequestsResponse struct {
	actor.BaseMessage

	Success bool
	Error   string
}

// MessageType returns the message type name.
func (m *RegisterVTXORequestsResponse) MessageType() string {
	return "RegisterVTXORequestsResponse"
}

// clientRespSealed marks this as a client response message.
func (m *RegisterVTXORequestsResponse) clientRespSealed() {}

// RegisterLeaveRequestsRequest informs the FSM of leave outputs to include in
// the next round registration.
type RegisterLeaveRequestsRequest struct {
	actor.BaseMessage

	Outputs []*wire.TxOut
}

// MessageType returns the message type name.
func (m *RegisterLeaveRequestsRequest) MessageType() string {
	return "RegisterLeaveRequestsRequest"
}

// RoundReceivable implements actormsg.RoundReceivable marker interface.
func (m *RegisterLeaveRequestsRequest) RoundReceivable() {}

// RegisterLeaveRequestsResponse acknowledges the request.
type RegisterLeaveRequestsResponse struct {
	actor.BaseMessage

	Success bool
}

// MessageType returns the message type name.
func (m *RegisterLeaveRequestsResponse) MessageType() string {
	return "RegisterLeaveRequestsResponse"
}

// clientRespSealed marks this as a client response message.
func (m *RegisterLeaveRequestsResponse) clientRespSealed() {}

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

// RoundReceivable implements actormsg.RoundReceivable marker interface.
func (m *ConfirmationEvent) RoundReceivable() {}

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

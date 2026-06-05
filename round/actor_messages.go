package round

import (
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/timeout"
	"github.com/lightninglabs/wavelength/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// ClientMsg embeds actormsg.RoundReceivable for messages that can be sent to a
// RoundClientActor. Both round-internal messages and messages from other actors
// (vtxo, wallet) implement the RoundReceivable marker method.
type ClientMsg interface {
	actormsg.RoundReceivable
}

// ClientResp is the sealed interface for all response messages from a
// RoundClientActor. It implements actormsg.RoundActorResp to enable service
// key lookup from the wallet package.
type ClientResp interface {
	actor.Message
	actormsg.RoundActorResp

	clientRespSealed()
}

// serviceKeyConfig holds configuration for service key creation.
type serviceKeyConfig struct {
	suffix string
}

// ServiceKeyOption is a functional option for customizing the service key.
type ServiceKeyOption func(*serviceKeyConfig)

// WithSuffix adds a suffix to the service key name. This is useful in tests
// with multiple round actors that need unique service keys to avoid collisions.
func WithSuffix(suffix string) ServiceKeyOption {
	return func(cfg *serviceKeyConfig) {
		cfg.suffix = suffix
	}
}

// NewServiceKey returns the service key for looking up a round client actor.
// The service key uses actormsg interface types to match the round actor's
// Receive signature and ensure compatibility with wallet service key lookups.
//
// Use the WithSuffix option in tests to create unique service keys for
// multi-client scenarios:
//
//	key := round.NewServiceKey(round.WithSuffix("-1"))
func NewServiceKey(
	opts ...ServiceKeyOption,
) actor.ServiceKey[actormsg.RoundReceivable, actormsg.RoundActorResp] {

	cfg := &serviceKeyConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	keyName := actormsg.RoundActorServiceKeyName
	if cfg.suffix != "" {
		keyName += cfg.suffix
	}

	return actor.NewServiceKey[
		actormsg.RoundReceivable, actormsg.RoundActorResp,
	](
		keyName,
	)
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

// RoundActorResp implements actormsg.RoundActorResp marker interface.
func (m *ServerMessageResponse) RoundActorResp() {}

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

// RoundActorResp implements actormsg.RoundActorResp marker interface.
func (m *GetClientStateResponse) RoundActorResp() {}

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

// RoundActorResp implements actormsg.RoundActorResp marker interface.
func (m *CancelRoundResponse) RoundActorResp() {}

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

// RoundActorResp implements actormsg.RoundActorResp marker interface.
func (m *RegisterVTXORequestsResponse) RoundActorResp() {}

// RegisterVirtualChannelIntentRequest queues an operator-funded, channel-backed
// VTXO request into the next round.
type RegisterVirtualChannelIntentRequest struct {
	actor.BaseMessage

	BackingAmount  btcutil.Amount
	Capacity       btcutil.Amount
	Private        bool
	ZeroConf       bool
	IdempotencyKey string
}

// MessageType returns the message type name.
func (m *RegisterVirtualChannelIntentRequest) MessageType() string {
	return "RegisterVirtualChannelIntentRequest"
}

// RoundReceivable implements actormsg.RoundReceivable marker interface.
func (m *RegisterVirtualChannelIntentRequest) RoundReceivable() {}

// RegisterVirtualChannelIntentResponse acknowledges the request.
type RegisterVirtualChannelIntentResponse struct {
	actor.BaseMessage

	Success bool
}

// MessageType returns the message type name.
func (m *RegisterVirtualChannelIntentResponse) MessageType() string {
	return "RegisterVirtualChannelIntentResponse"
}

// RoundActorResp implements actormsg.RoundActorResp marker interface.
func (m *RegisterVirtualChannelIntentResponse) RoundActorResp() {}

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

// TimeoutMsg is sent to the round actor when a timeout expires.
type TimeoutMsg struct {
	actor.BaseMessage

	// TimeoutID identifies the expired timeout.
	TimeoutID timeout.ID
}

// MessageType returns the message type name.
func (m *TimeoutMsg) MessageType() string {
	return "TimeoutMsg"
}

// RoundReceivable implements actormsg.RoundReceivable marker interface.
func (m *TimeoutMsg) RoundReceivable() {}

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

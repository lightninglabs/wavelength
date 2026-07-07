package chainsource

import (
	"iter"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// ChainSourceMsg is the sealed interface for all messages that can be sent to
// the ChainSource actor. The sealed interface pattern ensures type safety by
// preventing external packages from implementing the interface.
type ChainSourceMsg interface {
	actor.Message

	chainSourceMsgSealed()
}

// ChainSourceResp is the sealed interface for all response messages from the
// ChainSource actor.
type ChainSourceResp interface {
	actor.Message

	chainSourceRespSealed()
}

// FeeEstimateRequest requests a fee estimation for a given confirmation
// target. The fee estimator will provide the satoshis per vbyte needed to
// confirm within the target number of blocks.
type FeeEstimateRequest struct {
	actor.BaseMessage

	// TargetConf is the desired number of blocks within which the
	// transaction should confirm.
	TargetConf uint32
}

// MessageType returns the message type identifier for logging and debugging.
func (m *FeeEstimateRequest) MessageType() string {
	return "FeeEstimateRequest"
}

// chainSourceMsgSealed implements the sealed ChainSourceMsg interface.
func (m *FeeEstimateRequest) chainSourceMsgSealed() {}

// FeeEstimateResponse contains the estimated fee rate in satoshis per vbyte
// for the requested confirmation target.
type FeeEstimateResponse struct {
	actor.BaseMessage

	// SatPerVByte is the estimated fee rate in satoshis per virtual byte.
	SatPerVByte btcutil.Amount
}

// MessageType returns the message type identifier for logging and debugging.
func (m *FeeEstimateResponse) MessageType() string {
	return "FeeEstimateResponse"
}

// chainSourceRespSealed implements the sealed ChainSourceResp interface.
func (m *FeeEstimateResponse) chainSourceRespSealed() {}

// BestHeightRequest requests the current best known block height and hash
// from the blockchain backend.
type BestHeightRequest struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier for logging and debugging.
func (m *BestHeightRequest) MessageType() string {
	return "BestHeightRequest"
}

// chainSourceMsgSealed implements the sealed ChainSourceMsg interface.
func (m *BestHeightRequest) chainSourceMsgSealed() {}

// BestHeightResponse contains the current best block height and hash from the
// blockchain.
type BestHeightResponse struct {
	actor.BaseMessage

	// Height is the current best block height.
	Height int32

	// Hash is the current best block hash.
	Hash chainhash.Hash
}

// MessageType returns the message type identifier for logging and debugging.
func (m *BestHeightResponse) MessageType() string {
	return "BestHeightResponse"
}

// chainSourceRespSealed implements the sealed ChainSourceResp interface.
func (m *BestHeightResponse) chainSourceRespSealed() {}

// TestMempoolAcceptRequest requests a test of whether one or more
// transactions would be accepted by the mempool without actually
// broadcasting them. Passing more than one transaction asks the backend
// to evaluate the set as a package (per Bitcoin Core's
// testmempoolaccept RPC).
type TestMempoolAcceptRequest struct {
	actor.BaseMessage

	// Txs are the transactions to test for mempool acceptance. One
	// transaction performs a single-tx test; multiple transactions
	// request a package test.
	Txs []*wire.MsgTx
}

// MessageType returns the message type identifier for logging and debugging.
func (m *TestMempoolAcceptRequest) MessageType() string {
	return "TestMempoolAcceptRequest"
}

// chainSourceMsgSealed implements the sealed ChainSourceMsg interface.
func (m *TestMempoolAcceptRequest) chainSourceMsgSealed() {}

// TestMempoolAcceptResponse contains the per-transaction results of a
// mempool acceptance test.
type TestMempoolAcceptResponse struct {
	actor.BaseMessage

	// Results has one entry per tx in the original request, in the
	// same order.
	Results []MempoolAcceptResult
}

// MessageType returns the message type identifier for logging and debugging.
func (m *TestMempoolAcceptResponse) MessageType() string {
	return "TestMempoolAcceptResponse"
}

// chainSourceRespSealed implements the sealed ChainSourceResp interface.
func (m *TestMempoolAcceptResponse) chainSourceRespSealed() {}

// BroadcastTxRequest requests that a transaction be broadcast to the network.
// An optional label can be provided for wallet tracking purposes.
type BroadcastTxRequest struct {
	actor.BaseMessage

	// Tx is the transaction to broadcast.
	Tx *wire.MsgTx

	// Label is an optional label for tracking this transaction in the
	// wallet. Can be empty.
	Label string
}

// MessageType returns the message type identifier for logging and debugging.
func (m *BroadcastTxRequest) MessageType() string {
	return "BroadcastTxRequest"
}

// chainSourceMsgSealed implements the sealed ChainSourceMsg interface.
func (m *BroadcastTxRequest) chainSourceMsgSealed() {}

// BroadcastTxResponse contains the transaction ID of the successfully
// broadcast transaction.
type BroadcastTxResponse struct {
	actor.BaseMessage

	// Txid is the transaction ID of the broadcast transaction.
	Txid chainhash.Hash
}

// MessageType returns the message type identifier for logging and debugging.
func (m *BroadcastTxResponse) MessageType() string {
	return "BroadcastTxResponse"
}

// chainSourceRespSealed implements the sealed ChainSourceResp interface.
func (m *BroadcastTxResponse) chainSourceRespSealed() {}

// SubmitPackageRequest requests atomic submission of a parent+child
// transaction package. The parents must be in dependency order and the
// child pays the package fee.
type SubmitPackageRequest struct {
	actor.BaseMessage

	// Parents is the list of parent transactions in dependency order.
	Parents []*wire.MsgTx

	// Child is the fee-paying child transaction.
	Child *wire.MsgTx
}

// MessageType returns the message type identifier for logging and debugging.
func (m *SubmitPackageRequest) MessageType() string {
	return "SubmitPackageRequest"
}

// chainSourceMsgSealed implements the sealed ChainSourceMsg interface.
func (m *SubmitPackageRequest) chainSourceMsgSealed() {}

// SubmitPackageResponse indicates successful package submission.
type SubmitPackageResponse struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier for logging and debugging.
func (m *SubmitPackageResponse) MessageType() string {
	return "SubmitPackageResponse"
}

// chainSourceRespSealed implements the sealed ChainSourceResp interface.
func (m *SubmitPackageResponse) chainSourceRespSealed() {}

// ConfMsg is the sealed interface for all messages that can be sent to a
// ConfActor sub-actor for confirmation monitoring.
type ConfMsg interface {
	actor.Message

	confMsgSealed()
}

// ConfResp is the sealed interface for all response messages from a ConfActor.
type ConfResp interface {
	actor.Message

	confRespSealed()
}

// RegisterConfRequest requests monitoring of a transaction for a specified
// number of confirmations. The actor supports dual-mode operation: if
// NotifyActor is None, a Future is returned for blocking await; if NotifyActor
// is Some, events are sent to that actor asynchronously.
type RegisterConfRequest struct {
	actor.BaseMessage

	// CallerID is a unique identifier provided by the caller. This is used
	// to construct the service key for the dedicated actor, enabling
	// deterministic cancellation.
	CallerID string

	// Txid is the transaction ID to monitor. Can be nil to match by script
	// only.
	Txid *chainhash.Hash

	// PkScript is the public key script to monitor.
	PkScript []byte

	// TargetConfs is the number of confirmations to wait for.
	TargetConfs uint32

	// HeightHint is an optional height hint indicating the earliest block
	// that could contain the transaction. This is an optimization for
	// light clients. Set to 0 if unknown.
	HeightHint uint32

	// IncludeBlock indicates whether the confirmation event should include
	// the full block containing the transaction. This is needed for
	// constructing merkle proofs. When true, the ConfirmationEvent.Block
	// field will be populated.
	IncludeBlock bool

	// NotifyActor is an optional actor reference. If Some, confirmation
	// events will be sent to this actor asynchronously. If None, a Future
	// is returned in the response for blocking await.
	NotifyActor fn.Option[actor.TellOnlyRef[ConfirmationEvent]]
}

// MessageType returns the message type identifier for logging and debugging.
func (m *RegisterConfRequest) MessageType() string {
	return "RegisterConfRequest"
}

// chainSourceMsgSealed implements the ChainSourceMsg interface so confirmation
// requests can be routed through the ChainSource actor.
func (m *RegisterConfRequest) chainSourceMsgSealed() {}

// confMsgSealed implements the sealed ConfMsg interface.
func (m *RegisterConfRequest) confMsgSealed() {}

// RegisterConfResponse contains either a Future for blocking on confirmation
// or nothing if actor-mode notification was requested. The subscription can
// be cancelled later using UnregisterConfRequest with the original CallerID.
type RegisterConfResponse struct {
	actor.BaseMessage

	// Future provides a blocking await interface for confirmation. Only
	// present if NotifyActor was None in the request.
	Future actor.Future[ConfirmationEvent]
}

// MessageType returns the message type identifier for logging and debugging.
func (m *RegisterConfResponse) MessageType() string {
	return "RegisterConfResponse"
}

// confRespSealed implements the sealed ConfResp interface.
func (m *RegisterConfResponse) confRespSealed() {}

// chainSourceRespSealed implements the ChainSourceResp interface so responses
// can be returned via the ChainSource actor.
func (m *RegisterConfResponse) chainSourceRespSealed() {}

// ConfirmationEvent is sent when a monitored transaction reaches the target
// number of confirmations. This can be delivered either via a Future (for
// blocking await) or by sending to a registered actor (for async notification).
type ConfirmationEvent struct {
	actor.BaseMessage

	// Txid is the transaction ID.
	Txid chainhash.Hash

	// BlockHeight is the height of the block containing the transaction.
	BlockHeight int32

	// BlockHash is the hash of the block containing the transaction.
	BlockHash chainhash.Hash

	// NumConfs is the actual number of confirmations at the time of this
	// event.
	NumConfs uint32

	// Tx is the confirmed transaction. This allows consumers to inspect
	// transaction outputs without making additional chain queries.
	Tx *wire.MsgTx

	// Block is the full block containing the confirmed transaction. This
	// is only populated when the confirmation was registered with
	// IncludeBlock=true. Used for constructing merkle proofs.
	Block *wire.MsgBlock
}

// MessageType returns the message type identifier for logging and debugging.
func (m ConfirmationEvent) MessageType() string {
	return "ConfirmationEvent"
}

// UnregisterConfRequest requests cancellation of a confirmation subscription.
// The ChainSource actor uses the fields to construct the service key and
// cancel the dedicated actor.
type UnregisterConfRequest struct {
	actor.BaseMessage

	// CallerID is the unique identifier provided in the original
	// RegisterConfRequest. Required to reconstruct the service key.
	CallerID string

	// Txid is the transaction ID being monitored. Can be nil to match by
	// script only (must match the original registration).
	Txid *chainhash.Hash

	// PkScript is the public key script being monitored. Required if Txid
	// is nil.
	PkScript []byte

	// TargetConfs is the number of confirmations from the original
	// request. Required to reconstruct the service key.
	TargetConfs uint32
}

// MessageType returns the message type identifier for logging and debugging.
func (m *UnregisterConfRequest) MessageType() string {
	return "UnregisterConfRequest"
}

// chainSourceMsgSealed implements the ChainSourceMsg interface.
func (m *UnregisterConfRequest) chainSourceMsgSealed() {}

// UnregisterConfResponse indicates successful cancellation.
type UnregisterConfResponse struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier for logging and debugging.
func (m *UnregisterConfResponse) MessageType() string {
	return "UnregisterConfResponse"
}

// chainSourceRespSealed implements the ChainSourceResp interface.
func (m *UnregisterConfResponse) chainSourceRespSealed() {}

// SpendMsg is the sealed interface for all messages that can be sent to a
// SpendActor sub-actor for spend monitoring.
type SpendMsg interface {
	actor.Message

	spendMsgSealed()
}

// SpendResp is the sealed interface for all response messages from a
// SpendActor.
type SpendResp interface {
	actor.Message

	spendRespSealed()
}

// RegisterSpendRequest requests monitoring of an outpoint for spend events.
// The actor supports dual-mode operation similar to RegisterConfRequest.
type RegisterSpendRequest struct {
	actor.BaseMessage

	// CallerID is a unique identifier provided by the caller. This is used
	// to construct the service key for the dedicated actor, enabling
	// deterministic cancellation.
	CallerID string

	// Outpoint is the transaction output to monitor for spends. Can be nil
	// to match by script only.
	Outpoint *wire.OutPoint

	// PkScript is the public key script of the output to monitor.
	PkScript []byte

	// HeightHint is an optional height hint indicating the earliest block
	// that could contain a spending transaction. Set to 0 if unknown.
	HeightHint uint32

	// NotifyActor is an optional actor reference. If Some, spend events
	// will be sent to this actor asynchronously. If None, a Future is
	// returned for blocking await.
	NotifyActor fn.Option[actor.TellOnlyRef[SpendEvent]]
}

// MessageType returns the message type identifier for logging and debugging.
func (m *RegisterSpendRequest) MessageType() string {
	return "RegisterSpendRequest"
}

// chainSourceMsgSealed implements the ChainSourceMsg interface.
func (m *RegisterSpendRequest) chainSourceMsgSealed() {}

// spendMsgSealed implements the sealed SpendMsg interface.
func (m *RegisterSpendRequest) spendMsgSealed() {}

// RegisterSpendResponse contains either a Future for blocking on spend
// notification or nothing if actor-mode notification was requested. The
// subscription can be cancelled later using UnregisterSpendRequest with the
// original CallerID.
type RegisterSpendResponse struct {
	actor.BaseMessage

	// Future provides a blocking await interface for spend notification.
	// Only present if NotifyActor was None in the request.
	Future actor.Future[SpendEvent]
}

// MessageType returns the message type identifier for logging and debugging.
func (m *RegisterSpendResponse) MessageType() string {
	return "RegisterSpendResponse"
}

// spendRespSealed implements the sealed SpendResp interface.
func (m *RegisterSpendResponse) spendRespSealed() {}

// chainSourceRespSealed implements the ChainSourceResp interface.
func (m *RegisterSpendResponse) chainSourceRespSealed() {}

// SpendEvent is sent when a monitored outpoint is spent in a transaction with
// at least one confirmation.
type SpendEvent struct {
	actor.BaseMessage

	// Outpoint is the output that was spent.
	Outpoint wire.OutPoint

	// SpendingTxid is the transaction ID of the spending transaction.
	SpendingTxid chainhash.Hash

	// SpendingTx is the full spending transaction.
	SpendingTx *wire.MsgTx

	// SpenderInputIndex is the input index within the spending transaction
	// that consumes the monitored outpoint.
	SpenderInputIndex uint32

	// SpendingHeight is the block height where the spending transaction
	// was confirmed.
	SpendingHeight int32
}

// MessageType returns the message type identifier for logging and debugging.
func (m SpendEvent) MessageType() string {
	return "SpendEvent"
}

// UnregisterSpendRequest requests cancellation of a spend subscription.
// The ChainSource actor uses the fields to construct the service key and
// cancel the dedicated actor.
type UnregisterSpendRequest struct {
	actor.BaseMessage

	// CallerID is the unique identifier provided in the original
	// RegisterSpendRequest. Required to reconstruct the service key.
	CallerID string

	// Outpoint is the transaction output being monitored. Can be nil to
	// match by script only (must match the original registration).
	Outpoint *wire.OutPoint

	// PkScript is the public key script being monitored. Required if
	// Outpoint is nil.
	PkScript []byte
}

// MessageType returns the message type identifier for logging and debugging.
func (m *UnregisterSpendRequest) MessageType() string {
	return "UnregisterSpendRequest"
}

// chainSourceMsgSealed implements the ChainSourceMsg interface.
func (m *UnregisterSpendRequest) chainSourceMsgSealed() {}

// UnregisterSpendResponse indicates successful cancellation.
type UnregisterSpendResponse struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier for logging and debugging.
func (m *UnregisterSpendResponse) MessageType() string {
	return "UnregisterSpendResponse"
}

// chainSourceRespSealed implements the ChainSourceResp interface.
func (m *UnregisterSpendResponse) chainSourceRespSealed() {}

// EpochMsg is the sealed interface for all messages that can be sent to a
// BlockEpochActor sub-actor for block subscription.
type EpochMsg interface {
	actor.Message

	epochMsgSealed()
}

// EpochResp is the sealed interface for all response messages from a
// BlockEpochActor.
type EpochResp interface {
	actor.Message

	epochRespSealed()
}

// SubscribeBlocksRequest requests subscription to new block notifications.
// The actor supports dual-mode operation: if NotifyActor is None, an iterator
// is returned for range loops; if NotifyActor is Some, events are sent to
// that actor asynchronously.
type SubscribeBlocksRequest struct {
	actor.BaseMessage

	// CallerID is a unique identifier provided by the caller. This is used
	// to construct the service key for the dedicated actor, enabling
	// deterministic cancellation.
	CallerID string

	// NotifyActor is an optional actor reference. If Some, block events
	// will be sent to this actor asynchronously. If None, an iterator is
	// returned for use in range loops.
	NotifyActor fn.Option[actor.TellOnlyRef[BlockEpoch]]
}

// MessageType returns the message type identifier for logging and debugging.
func (m *SubscribeBlocksRequest) MessageType() string {
	return "SubscribeBlocksRequest"
}

// chainSourceMsgSealed implements the ChainSourceMsg interface.
func (m *SubscribeBlocksRequest) chainSourceMsgSealed() {}

// epochMsgSealed implements the sealed EpochMsg interface.
func (m *SubscribeBlocksRequest) epochMsgSealed() {}

// SubscribeBlocksResponse contains either an iterator for range loops or
// nothing if actor-mode notification was requested. The subscription can
// be cancelled later using UnsubscribeBlocksRequest with the original CallerID.
type SubscribeBlocksResponse struct {
	actor.BaseMessage

	// Iterator provides an iterator for iterating over block epochs using
	// Go's range-over-function feature (Go 1.23+). Only present if
	// NotifyActor was None in the request.
	Iterator iter.Seq[BlockEpoch]

	// Cancel terminates the subscription and cleans up backend resources.
	// It is safe to call multiple times.
	Cancel func()
}

// MessageType returns the message type identifier for logging and debugging.
func (m *SubscribeBlocksResponse) MessageType() string {
	return "SubscribeBlocksResponse"
}

// epochRespSealed implements the sealed EpochResp interface.
func (m *SubscribeBlocksResponse) epochRespSealed() {}

// chainSourceRespSealed implements the ChainSourceResp interface.
func (m *SubscribeBlocksResponse) chainSourceRespSealed() {}

// BlockEpoch represents a new block connected to the blockchain. This event is
// sent for each new block and can include backfilled blocks if the client
// fell behind.
type BlockEpoch struct {
	actor.BaseMessage

	// Height is the block height.
	Height int32

	// Hash is the block hash.
	Hash chainhash.Hash

	// Timestamp is the block timestamp from the header.
	Timestamp int64
}

// MessageType returns the message type identifier for logging and debugging.
func (m BlockEpoch) MessageType() string {
	return "BlockEpoch"
}

// UnsubscribeBlocksRequest requests cancellation of a block subscription.
// The ChainSource actor uses the CallerID to construct the service key and
// cancel the dedicated actor.
type UnsubscribeBlocksRequest struct {
	actor.BaseMessage

	// CallerID is the unique identifier provided in the original
	// SubscribeBlocksRequest. Required to reconstruct the service key.
	CallerID string
}

// MessageType returns the message type identifier for logging and debugging.
func (m *UnsubscribeBlocksRequest) MessageType() string {
	return "UnsubscribeBlocksRequest"
}

// chainSourceMsgSealed implements the ChainSourceMsg interface.
func (m *UnsubscribeBlocksRequest) chainSourceMsgSealed() {}

// UnsubscribeBlocksResponse indicates successful cancellation.
type UnsubscribeBlocksResponse struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier for logging and debugging.
func (m *UnsubscribeBlocksResponse) MessageType() string {
	return "UnsubscribeBlocksResponse"
}

// chainSourceRespSealed implements the ChainSourceResp interface.
func (m *UnsubscribeBlocksResponse) chainSourceRespSealed() {}

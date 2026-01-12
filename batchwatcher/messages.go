package batchwatcher

import (
	"github.com/btcsuite/btcd/wire"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/tree"
)

// BatchID uniquely identifies a batch being watched.
type BatchID uuid.UUID

// String returns the string representation of the batch ID.
func (b BatchID) String() string {
	return uuid.UUID(b).String()
}

// BatchWatcherMsg is the sealed interface for all messages that can be sent to
// the BatchWatcherActor. The sealed interface pattern ensures type safety by
// preventing external packages from implementing the interface.
type BatchWatcherMsg interface {
	actor.Message
	batchWatcherMsgSealed()
}

// BatchWatcherResp is the sealed interface for all response messages from the
// BatchWatcherActor.
type BatchWatcherResp interface {
	actor.Message
	batchWatcherRespSealed()
}

// RegisterBatchRequest is sent by the Rounds actor to register a new batch for
// monitoring. The BatchWatcher will begin tracking the tree state and watching
// for spends on the batch output.
type RegisterBatchRequest struct {
	actor.BaseMessage

	// BatchID is the unique identifier for this batch.
	BatchID BatchID

	// Tree is the complete pre-signed VTXO tree for this batch.
	Tree *tree.Tree

	// ExpiryHeight is the block height at which this batch expires and
	// becomes sweepable by the operator.
	ExpiryHeight uint32
}

// MessageType returns the message type identifier for logging and debugging.
func (m *RegisterBatchRequest) MessageType() string {
	return "RegisterBatchRequest"
}

// batchWatcherMsgSealed implements the sealed BatchWatcherMsg interface.
func (m *RegisterBatchRequest) batchWatcherMsgSealed() {}

// RegisterBatchResponse acknowledges successful batch registration.
type RegisterBatchResponse struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier for logging and debugging.
func (m *RegisterBatchResponse) MessageType() string {
	return "RegisterBatchResponse"
}

// batchWatcherRespSealed implements the sealed BatchWatcherResp interface.
func (m *RegisterBatchResponse) batchWatcherRespSealed() {}

// GetTreeStateRequest queries the current on-chain tree state for a batch.
// This is used by BatchSweeper to determine which outputs need to be swept.
type GetTreeStateRequest struct {
	actor.BaseMessage

	// BatchID identifies the batch to query.
	BatchID BatchID
}

// MessageType returns the message type identifier for logging and debugging.
func (m *GetTreeStateRequest) MessageType() string {
	return "GetTreeStateRequest"
}

// batchWatcherMsgSealed implements the sealed BatchWatcherMsg interface.
func (m *GetTreeStateRequest) batchWatcherMsgSealed() {}

// GetTreeStateResponse contains the current on-chain tree state for a batch.
type GetTreeStateResponse struct {
	actor.BaseMessage

	// Found indicates whether the batch was found in the watcher's state.
	Found bool

	// TreeState contains the current state if Found is true.
	TreeState *BatchTreeState
}

// MessageType returns the message type identifier for logging and debugging.
func (m *GetTreeStateResponse) MessageType() string {
	return "GetTreeStateResponse"
}

// batchWatcherRespSealed implements the sealed BatchWatcherResp interface.
func (m *GetTreeStateResponse) batchWatcherRespSealed() {}

// NodeSpendDetected is an internal message sent when the ChainSource detects
// that a watched output has been spent. This triggers tree state updates and
// child output watching.
type NodeSpendDetected struct {
	actor.BaseMessage

	// BatchID identifies which batch this spend belongs to.
	BatchID BatchID

	// SpentOutpoint is the outpoint that was spent.
	SpentOutpoint wire.OutPoint

	// SpendingTx is the transaction that spent the output.
	SpendingTx *wire.MsgTx

	// SpendingHeight is the block height where the spend was confirmed.
	SpendingHeight int32
}

// MessageType returns the message type identifier for logging and debugging.
func (m *NodeSpendDetected) MessageType() string {
	return "NodeSpendDetected"
}

// batchWatcherMsgSealed implements the sealed BatchWatcherMsg interface.
func (m *NodeSpendDetected) batchWatcherMsgSealed() {}

// NewBlockReceived is sent when a new block is connected to the chain. The
// BatchWatcher uses this to check for expired batches.
type NewBlockReceived struct {
	actor.BaseMessage

	// Height is the height of the new block.
	Height int32
}

// MessageType returns the message type identifier for logging and debugging.
func (m *NewBlockReceived) MessageType() string {
	return "NewBlockReceived"
}

// batchWatcherMsgSealed implements the sealed BatchWatcherMsg interface.
func (m *NewBlockReceived) batchWatcherMsgSealed() {}

// UnregisterBatchRequest removes a batch from monitoring. This is typically
// sent after a batch has been fully swept or is no longer needed.
type UnregisterBatchRequest struct {
	actor.BaseMessage

	// BatchID identifies the batch to remove.
	BatchID BatchID
}

// MessageType returns the message type identifier for logging and debugging.
func (m *UnregisterBatchRequest) MessageType() string {
	return "UnregisterBatchRequest"
}

// batchWatcherMsgSealed implements the sealed BatchWatcherMsg interface.
func (m *UnregisterBatchRequest) batchWatcherMsgSealed() {}

// UnregisterBatchResponse acknowledges batch removal.
type UnregisterBatchResponse struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier for logging and debugging.
func (m *UnregisterBatchResponse) MessageType() string {
	return "UnregisterBatchResponse"
}

// batchWatcherRespSealed implements the sealed BatchWatcherResp interface.
func (m *UnregisterBatchResponse) batchWatcherRespSealed() {}

// ===== Outgoing notification messages (sent to child actors) =====

// FraudDetectorMsg is the sealed interface for messages sent to the
// FraudDetector actor.
type FraudDetectorMsg interface {
	actor.Message
	fraudDetectorMsgSealed()
}

// VTXOOnChainNotification is sent to the FraudDetector when a VTXO leaf
// appears on-chain. This indicates the tree has been unrolled to the leaf
// level and the VTXO is now spendable.
type VTXOOnChainNotification struct {
	actor.BaseMessage

	// BatchID identifies which batch this VTXO belongs to.
	BatchID BatchID

	// VTXOOutpoint is the on-chain outpoint of the VTXO.
	VTXOOutpoint wire.OutPoint

	// VTXOOutput contains the output details (value, pkScript).
	VTXOOutput *wire.TxOut
}

// MessageType returns the message type identifier for logging and debugging.
func (m *VTXOOnChainNotification) MessageType() string {
	return "VTXOOnChainNotification"
}

// fraudDetectorMsgSealed implements the sealed FraudDetectorMsg interface.
func (m *VTXOOnChainNotification) fraudDetectorMsgSealed() {}

// BatchSweeperMsg is the sealed interface for messages sent to the
// BatchSweeper actor.
type BatchSweeperMsg interface {
	actor.Message
	batchSweeperMsgSealed()
}

// BatchExpiredNotification is sent to the BatchSweeper when a batch reaches
// its expiry height. The sweeper can then build sweep transactions for any
// unspent outputs.
type BatchExpiredNotification struct {
	actor.BaseMessage

	// BatchID identifies which batch has expired.
	BatchID BatchID

	// ExpiryHeight is the height at which the batch expired.
	ExpiryHeight uint32
}

// MessageType returns the message type identifier for logging and debugging.
func (m *BatchExpiredNotification) MessageType() string {
	return "BatchExpiredNotification"
}

// batchSweeperMsgSealed implements the sealed BatchSweeperMsg interface.
func (m *BatchExpiredNotification) batchSweeperMsgSealed() {}

// TreeStateChangedNotification is sent to the BatchSweeper when the on-chain
// tree state changes (new outputs appear or existing outputs are spent).
type TreeStateChangedNotification struct {
	actor.BaseMessage

	// BatchID identifies which batch's tree state changed.
	BatchID BatchID
}

// MessageType returns the message type identifier for logging and debugging.
func (m *TreeStateChangedNotification) MessageType() string {
	return "TreeStateChangedNotification"
}

// batchSweeperMsgSealed implements the sealed BatchSweeperMsg interface.
func (m *TreeStateChangedNotification) batchSweeperMsgSealed() {}

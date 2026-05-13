package batchsweeper

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo/batchwatcher"
)

// Msg is the sealed interface for all messages that can be sent to the
// BatchSweeperActor.
type Msg interface {
	actor.Message

	batchSweeperMsgSealed()
}

// Resp is the sealed interface for all responses from the BatchSweeperActor.
type Resp interface {
	actor.Message

	batchSweeperRespSealed()
}

// BatchExpiredEvent wraps a BatchWatcher expiry notification for internal
// BatchSweeper processing.
type BatchExpiredEvent struct {
	actor.BaseMessage

	// Notification is the original notification from the BatchWatcher.
	Notification *batchwatcher.BatchExpiredNotification
}

// MessageType returns the message type identifier for logging and debugging.
func (m *BatchExpiredEvent) MessageType() string {
	return "BatchExpiredEvent"
}

// batchSweeperMsgSealed implements the sealed Msg interface.
func (m *BatchExpiredEvent) batchSweeperMsgSealed() {}

// TreeStateChangedEvent wraps a BatchWatcher tree-state change notification for
// internal BatchSweeper processing.
type TreeStateChangedEvent struct {
	actor.BaseMessage

	// Notification is the original notification from the BatchWatcher.
	Notification *batchwatcher.TreeStateChangedNotification
}

// MessageType returns the message type identifier for logging and debugging.
func (m *TreeStateChangedEvent) MessageType() string {
	return "TreeStateChangedEvent"
}

// batchSweeperMsgSealed implements the sealed Msg interface.
func (m *TreeStateChangedEvent) batchSweeperMsgSealed() {}

// SweepRetryEvent is an internal message that triggers a retry of sweeping for
// a batch. This is typically scheduled via the timeout actor.
type SweepRetryEvent struct {
	actor.BaseMessage

	// BatchID identifies which batch should be re-attempted.
	BatchID batchwatcher.BatchID
}

// MessageType returns the message type identifier for logging and debugging.
func (m *SweepRetryEvent) MessageType() string {
	return "SweepRetryEvent"
}

// batchSweeperMsgSealed implements the sealed Msg interface.
func (m *SweepRetryEvent) batchSweeperMsgSealed() {}

// SweepConfirmedEvent is an internal message that indicates a sweep
// transaction has confirmed. This triggers cleanup of tracking state.
type SweepConfirmedEvent struct {
	actor.BaseMessage

	// BatchID identifies the batch whose sweep confirmed.
	BatchID batchwatcher.BatchID

	// Txid is the confirmed sweep transaction ID.
	Txid [32]byte

	// BlockHeight is the height at which the sweep confirmed.
	BlockHeight int32
}

// MessageType returns the message type identifier for logging and debugging.
func (m *SweepConfirmedEvent) MessageType() string {
	return "SweepConfirmedEvent"
}

// batchSweeperMsgSealed implements the sealed Msg interface.
func (m *SweepConfirmedEvent) batchSweeperMsgSealed() {}

// BatchSweptEvent wraps a BatchWatcher swept notification for internal
// BatchSweeper processing. The watcher sends this after detecting that the
// batch root was spent by a non-tree tx and no outputs remain.
type BatchSweptEvent struct {
	actor.BaseMessage

	// Notification is the original notification from the BatchWatcher.
	// The Tree field carries the pre-signed VTXO tree for extracting
	// leaf outpoints.
	Notification *batchwatcher.BatchSweptNotification
}

// MessageType returns the message type identifier for logging and debugging.
func (m *BatchSweptEvent) MessageType() string {
	return "BatchSweptEvent"
}

// batchSweeperMsgSealed implements the sealed Msg interface.
func (m *BatchSweptEvent) batchSweeperMsgSealed() {}

// BatchSweptCallbackRetryEvent is an internal message that retries the
// OnBatchSwept callback for a batch whose callback previously failed. The
// watcher unregisters the batch as soon as it enqueues the original
// BatchSweptEvent, so without an in-process retry a transient OnBatchSwept
// failure would leave the tree's VTXOs marked live until operator restart.
type BatchSweptCallbackRetryEvent struct {
	actor.BaseMessage

	// BatchID identifies which batch's callback should be retried.
	BatchID batchwatcher.BatchID
}

// MessageType returns the message type identifier for logging and debugging.
func (m *BatchSweptCallbackRetryEvent) MessageType() string {
	return "BatchSweptCallbackRetryEvent"
}

// batchSweeperMsgSealed implements the sealed Msg interface.
func (m *BatchSweptCallbackRetryEvent) batchSweeperMsgSealed() {}

// BatchSubtreeSweptEvent wraps a BatchWatcher subtree-swept notification for
// internal BatchSweeper processing. The watcher sends this after detecting
// that an exposed mid-tree branch output was spent by a non-tree tx after
// expiry. The descendant VTXO leaves under the swept subtree must be marked
// expired in storage.
type BatchSubtreeSweptEvent struct {
	actor.BaseMessage

	// Notification is the original notification from the BatchWatcher.
	// The SubtreeRoot field carries the tree node whose on-chain output
	// was just swept; its descendant leaves identify the VTXOs to mark
	// expired.
	Notification *batchwatcher.BatchSubtreeSweptNotification
}

// MessageType returns the message type identifier for logging and debugging.
func (m *BatchSubtreeSweptEvent) MessageType() string {
	return "BatchSubtreeSweptEvent"
}

// batchSweeperMsgSealed implements the sealed Msg interface.
func (m *BatchSubtreeSweptEvent) batchSweeperMsgSealed() {}

// BatchSubtreeSweptCallbackRetryEvent is an internal message that retries the
// OnBatchSwept callback for a (batch, subtree-root) pair whose callback
// previously failed. The watcher does not redeliver subtree-sweep
// notifications once it has notified successfully, so an in-process retry
// surface is needed for the durable VTXO-expiry transition just like the
// root-sweep path. See handleBatchSubtreeSwept.
type BatchSubtreeSweptCallbackRetryEvent struct {
	actor.BaseMessage

	// BatchID identifies which batch the subtree belongs to.
	BatchID batchwatcher.BatchID

	// SubtreeTxid is the txid of the swept subtree's root node and acts
	// as the key disambiguating multiple independent subtree sweeps for
	// the same batch.
	SubtreeTxid chainhash.Hash
}

// MessageType returns the message type identifier for logging and debugging.
func (m *BatchSubtreeSweptCallbackRetryEvent) MessageType() string {
	return "BatchSubtreeSweptCallbackRetryEvent"
}

// batchSweeperMsgSealed implements the sealed Msg interface.
func (m *BatchSubtreeSweptCallbackRetryEvent) batchSweeperMsgSealed() {}

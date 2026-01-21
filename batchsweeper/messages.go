package batchsweeper

import (
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

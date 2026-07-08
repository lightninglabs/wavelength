package batchcanon

import (
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
)

// ManagerMsg is the sealed inbound message interface for the
// BatchCanonicalityManager. It covers both the public register/query API and
// the internal chain-observation messages re-wrapped from chainsource.
type ManagerMsg interface {
	actor.Message

	managerMsgSealed()
}

// ManagerResp is the sealed response interface for the manager.
type ManagerResp interface {
	actor.Message

	managerRespSealed()
}

// RegisterBatchRequest registers (or re-registers, idempotently) a batch with
// the manager: it persists a canonicality record, registers a reorg-aware
// confirmation watch on the batch tx, and a reorg-aware spend watch on every
// consumed input. Calling it again for the same batch txid merges the
// dependent VTXOs into the record without duplicating watches.
type RegisterBatchRequest struct {
	actor.BaseMessage

	// BatchTxID is the batch (commitment) transaction id.
	BatchTxID chainhash.Hash

	// ConfirmationPkScript is the pkScript of the batch-tx output the
	// confirmation watch keys on. Required for light-client backends and
	// persisted for restart re-registration.
	ConfirmationPkScript []byte

	// CSVExpiryDelta is the batch's CSV-relative expiry timeout in blocks.
	CSVExpiryDelta int32

	// ConsumedInputs are the outpoints the batch tx spends. Each gets a
	// reorg-aware spend watch so a conflicting double-spend is detected.
	ConsumedInputs []wire.OutPoint

	// DependentVTXOs are the VTXO outpoints anchored by this batch.
	DependentVTXOs []wire.OutPoint
}

// MessageType returns the message type identifier.
func (m *RegisterBatchRequest) MessageType() string {
	return "batchcanon.RegisterBatchRequest"
}

func (m *RegisterBatchRequest) managerMsgSealed() {}

// RegisterBatchResponse is the reply to RegisterBatchRequest.
type RegisterBatchResponse struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier.
func (m *RegisterBatchResponse) MessageType() string {
	return "batchcanon.RegisterBatchResponse"
}

func (m *RegisterBatchResponse) managerRespSealed() {}

// GetBatchStateRequest reads the current canonicality record for a batch.
type GetBatchStateRequest struct {
	actor.BaseMessage

	// BatchTxID is the batch tx to look up.
	BatchTxID chainhash.Hash
}

// MessageType returns the message type identifier.
func (m *GetBatchStateRequest) MessageType() string {
	return "batchcanon.GetBatchStateRequest"
}

func (m *GetBatchStateRequest) managerMsgSealed() {}

// GetBatchStateResponse carries the looked-up record, if present.
type GetBatchStateResponse struct {
	actor.BaseMessage

	// Record is the canonicality record. Nil when Found is false.
	Record *Record

	// Found reports whether a record existed for the batch.
	Found bool
}

// MessageType returns the message type identifier.
func (m *GetBatchStateResponse) MessageType() string {
	return "batchcanon.GetBatchStateResponse"
}

func (m *GetBatchStateResponse) managerRespSealed() {}

// ackResponse is the no-op reply for internal Tell-delivered messages.
type ackResponse struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier.
func (m *ackResponse) MessageType() string {
	return "batchcanon.ackResponse"
}

func (m *ackResponse) managerRespSealed() {}

// batchConfirmedMsg is the internal re-wrap of a chainsource ConfirmationEvent
// for a watched batch tx.
type batchConfirmedMsg struct {
	actor.BaseMessage

	txid        chainhash.Hash
	blockHeight int32
	blockHash   chainhash.Hash
}

// MessageType returns the message type identifier.
func (m *batchConfirmedMsg) MessageType() string {
	return "batchcanon.batchConfirmedMsg"
}

func (m *batchConfirmedMsg) managerMsgSealed() {}

// batchReorgedMsg is the internal re-wrap of a chainsource ConfReorgedEvent.
type batchReorgedMsg struct {
	actor.BaseMessage

	txid chainhash.Hash
}

// MessageType returns the message type identifier.
func (m *batchReorgedMsg) MessageType() string {
	return "batchcanon.batchReorgedMsg"
}

func (m *batchReorgedMsg) managerMsgSealed() {}

// batchDoneMsg is the internal re-wrap of a chainsource ConfDoneEvent: the
// batch confirmation has matured past the reorg-safety depth (policy
// finality).
type batchDoneMsg struct {
	actor.BaseMessage

	txid chainhash.Hash
}

// MessageType returns the message type identifier.
func (m *batchDoneMsg) MessageType() string {
	return "batchcanon.batchDoneMsg"
}

func (m *batchDoneMsg) managerMsgSealed() {}

// inputSpentMsg is the internal re-wrap of a chainsource SpendEvent on a
// consumed batch input.
type inputSpentMsg struct {
	actor.BaseMessage

	outpoint     wire.OutPoint
	spendingTxid chainhash.Hash
	spendHeight  int32
}

// MessageType returns the message type identifier.
func (m *inputSpentMsg) MessageType() string {
	return "batchcanon.inputSpentMsg"
}

func (m *inputSpentMsg) managerMsgSealed() {}

// inputSpendReorgedMsg is the internal re-wrap of a chainsource
// SpendReorgedEvent: a previously observed spend left the best chain.
type inputSpendReorgedMsg struct {
	actor.BaseMessage

	outpoint wire.OutPoint
}

// MessageType returns the message type identifier.
func (m *inputSpendReorgedMsg) MessageType() string {
	return "batchcanon.inputSpendReorgedMsg"
}

func (m *inputSpendReorgedMsg) managerMsgSealed() {}

// inputSpendDoneMsg is the internal re-wrap of a chainsource SpendDoneEvent:
// the spend observation matured past the reorg-safety depth.
type inputSpendDoneMsg struct {
	actor.BaseMessage

	outpoint wire.OutPoint
}

// MessageType returns the message type identifier.
func (m *inputSpendDoneMsg) MessageType() string {
	return "batchcanon.inputSpendDoneMsg"
}

func (m *inputSpendDoneMsg) managerMsgSealed() {}

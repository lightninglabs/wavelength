package batchcanon

import (
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
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

	// BatchTx is the serialized batch transaction. Registration derives its
	// txid and exact consumed-input set from these bytes instead of
	// trusting a caller-supplied subset.
	BatchTx []byte

	// BatchOutputIndex selects the batch transaction output watched for
	// confirmation. Its script must exactly match ConfirmationPkScript.
	BatchOutputIndex uint32

	// ConfirmationPkScript is the pkScript of the batch-tx output the
	// confirmation watch keys on. Required for light-client backends and
	// persisted for restart re-registration.
	ConfirmationPkScript []byte

	// WatchHeightHint is the best-chain height from before the batch could
	// have confirmed. It is persisted so both initial registration and
	// restart reconciliation scan across confirmations that raced ahead of
	// watch installation.
	WatchHeightHint uint32

	// CSVExpiryDelta is the batch's CSV-relative expiry timeout in blocks.
	CSVExpiryDelta int32

	// ConsumedInputs are the exact inputs the batch tx spends, each
	// carrying the authenticated value and pkScript of the spent output.
	// Each gets a reorg-aware spend watch so a conflicting double-spend is
	// detected; the pkScript is required because lnd's spend notifier
	// filters by output script.
	ConsumedInputs []ConsumedInput

	// DependentVTXOs are the VTXO outpoints anchored by this batch.
	DependentVTXOs []wire.OutPoint

	// ConsumedVTXOs are logical VTXOs from prior batches that this batch
	// forfeits. Each edge binds the exact business revision and complete
	// creator lineage needed for conditional restoration. This is distinct
	// from ConsumedInputs, the batch transaction's actual Bitcoin inputs.
	ConsumedVTXOs []ConsumerEdge

	// ForfeitedVTXOs is the legacy outpoint-only producer shape. Production
	// registration rejects it because it lacks creator lineage and the
	// exact expected business revision. It remains temporarily for
	// compile-time compatibility while producer wiring moves to
	// ConsumedVTXOs.
	ForfeitedVTXOs []wire.OutPoint
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

// LineageRevision binds one batch's ready generation and availability
// revision into an AdmissionToken.
type LineageRevision struct {
	BatchTxID  chainhash.Hash
	Generation uint64
	Revision   uint64
}

// AdmissionToken is returned only for a ready, usable complete lineage. A
// critical side effect must validate the token through the manager immediately
// before crossing its point of no return.
type AdmissionToken struct {
	Lineage []LineageRevision
}

// QueryLineageRequest asks for the fail-closed availability of a complete
// inherited lineage.
type QueryLineageRequest struct {
	actor.BaseMessage

	BatchTxIDs []chainhash.Hash
}

// MessageType returns the message type identifier.
func (m *QueryLineageRequest) MessageType() string {
	return "batchcanon.QueryLineageRequest"
}

func (m *QueryLineageRequest) managerMsgSealed() {}

// QueryLineageResponse carries availability and, only when admitted, a token
// binding the current ready generation and revision of every ancestor.
type QueryLineageResponse struct {
	actor.BaseMessage

	Availability Availability
	Token        *AdmissionToken
}

// MessageType returns the message type identifier.
func (m *QueryLineageResponse) MessageType() string {
	return "batchcanon.QueryLineageResponse"
}

func (m *QueryLineageResponse) managerRespSealed() {}

// ValidateAdmissionRequest revalidates a previously issued token immediately
// before a critical side effect.
type ValidateAdmissionRequest struct {
	actor.BaseMessage

	Token AdmissionToken
}

// MessageType returns the message type identifier.
func (m *ValidateAdmissionRequest) MessageType() string {
	return "batchcanon.ValidateAdmissionRequest"
}

func (m *ValidateAdmissionRequest) managerMsgSealed() {}

// ValidateAdmissionResponse reports whether the exact token remains current.
// Availability carries the latest fail-closed result when it is stale.
type ValidateAdmissionResponse struct {
	actor.BaseMessage

	Valid        bool
	Availability Availability
}

// MessageType returns the message type identifier.
func (m *ValidateAdmissionResponse) MessageType() string {
	return "batchcanon.ValidateAdmissionResponse"
}

func (m *ValidateAdmissionResponse) managerRespSealed() {}

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
	generation  uint64
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

	txid       chainhash.Hash
	generation uint64
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

	txid       chainhash.Hash
	generation uint64
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

	batchTxid    chainhash.Hash
	generation   uint64
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

	batchTxid  chainhash.Hash
	generation uint64
	outpoint   wire.OutPoint
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

	batchTxid  chainhash.Hash
	generation uint64
	outpoint   wire.OutPoint
}

// MessageType returns the message type identifier.
func (m *inputSpendDoneMsg) MessageType() string {
	return "batchcanon.inputSpendDoneMsg"
}

func (m *inputSpendDoneMsg) managerMsgSealed() {}

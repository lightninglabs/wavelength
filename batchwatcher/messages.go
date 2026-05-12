package batchwatcher

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
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

// BatchWatcherServiceKeyName is the receptionist key used to
// discover the batch watcher actor in the actor system.
const BatchWatcherServiceKeyName = "batch-watcher"

// NewServiceKey returns the service key for looking up the batch
// watcher actor via the receptionist.
func NewServiceKey() actor.ServiceKey[
	BatchWatcherMsg, BatchWatcherResp] {

	return actor.NewServiceKey[
		BatchWatcherMsg, BatchWatcherResp,
	](
		BatchWatcherServiceKeyName,
	)
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

	// ConfirmationHeight is the block height at which the commitment
	// transaction for this batch was confirmed. This is used for CSV
	// maturity calculations when sweeping operator-controlled outputs.
	ConfirmationHeight uint32

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

// SpendClassification discriminates between the different fraud-response
// flows an UnexpectedSpendNotification may trigger. Each value determines the
// meaning of the ResponseTxID field and the action the fraud detector should
// take.
type SpendClassification uint8

const (
	// SpendClassificationUnknown is the zero value; it should never be
	// emitted in a real notification.
	SpendClassificationUnknown SpendClassification = iota

	// SpendClassificationMissedBranchTx indicates a non-leaf tracked
	// output was spent by a transaction that is not the expected
	// presigned branch tx. ResponseTxID carries the presigned branch
	// txid that should have spent TrackedOutput.
	SpendClassificationMissedBranchTx

	// SpendClassificationForfeitedLeaf indicates a leaf VTXO that was
	// already forfeited was revealed on-chain. ResponseTxID carries the
	// stored forfeit txid that the operator must broadcast.
	SpendClassificationForfeitedLeaf

	// SpendClassificationOORCheckpointLeaf indicates a leaf VTXO with
	// a stored OOR checkpoint was revealed on-chain. ResponseTxID carries
	// the stored checkpoint txid that the operator must broadcast to
	// race the client's CSV delay.
	SpendClassificationOORCheckpointLeaf

	// SpendClassificationSpentLeaf indicates a leaf VTXO whose rounds-DB
	// status is already 'spent' (OOR finalization completed) was revealed
	// on-chain. ResponseTxID carries the stored OOR checkpoint txid; the
	// fraud detector MUST broadcast the checkpoint before the CSV delay
	// expires.
	SpendClassificationSpentLeaf

	// SpendClassificationExpiredLeaf indicates a leaf VTXO whose rounds-DB
	// status is 'expired' was revealed on-chain. Per ARK-04 this is a
	// legitimate race outcome (client won vs operator sweep) and no fraud
	// response is required. ResponseTxID is unset.
	SpendClassificationExpiredLeaf

	// SpendClassificationInFlightLeaf indicates a leaf VTXO was locked by
	// an active round or OOR session (status 'in_flight') when it was
	// revealed on-chain. ResponseTxID carries the stored OOR checkpoint
	// txid if one exists, else is unset.
	SpendClassificationInFlightLeaf
)

// String returns a human-readable label for the SpendClassification.
func (c SpendClassification) String() string {
	switch c {
	case SpendClassificationMissedBranchTx:
		return "missed_branch_tx"

	case SpendClassificationForfeitedLeaf:
		return "forfeited_leaf"

	case SpendClassificationOORCheckpointLeaf:
		return "oor_checkpoint_leaf"

	case SpendClassificationSpentLeaf:
		return "spent_leaf"

	case SpendClassificationExpiredLeaf:
		return "expired_leaf"

	case SpendClassificationInFlightLeaf:
		return "in_flight_leaf"

	default:
		return "unknown"
	}
}

// UnexpectedSpendNotification is sent to the FraudDetector when a watched
// output confirms a spend that does not match the next presigned branch
// transaction. This is the hand-off point for future fraud-response logic.
type UnexpectedSpendNotification struct {
	actor.BaseMessage

	// BatchID identifies which batch this spend belongs to.
	BatchID BatchID

	// TrackedOutput is the watched output that was consumed on-chain.
	TrackedOutput *Output

	// Classification describes which fraud-response flow applies. The
	// fraud detector switches on this value to decide how to interpret
	// ResponseTxID and which response transaction to broadcast.
	Classification SpendClassification

	// ResponseTxID is the txid the fraud detector should act on. Its
	// meaning depends on Classification:
	//
	//   - MissedBranchTx: presigned branch txid that should have spent
	//     TrackedOutput.
	//   - ForfeitedLeaf: forfeit tx that must be broadcast.
	//   - OORCheckpointLeaf / SpentLeaf: OOR checkpoint tx that must be
	//     broadcast to race the client's CSV.
	//   - ExpiredLeaf / InFlightLeaf (no checkpoint): zero.
	ResponseTxID chainhash.Hash

	// ResponseTx is the broadcastable transaction matching ResponseTxID
	// when the classification requires an immediate fraud response. It is
	// populated for forfeit and OOR-checkpoint responses so the fraud
	// detector does not need to repeat recovery-store lookups after the
	// batch watcher has already classified the spend.
	ResponseTx *wire.MsgTx

	// SpendingTx is the confirmed transaction that actually spent the
	// watched output.
	SpendingTx *wire.MsgTx

	// SpendingHeight is the block height where the unexpected spend
	// confirmed.
	SpendingHeight int32
}

// MessageType returns the message type identifier for logging and debugging.
func (m *UnexpectedSpendNotification) MessageType() string {
	return "UnexpectedSpendNotification"
}

// fraudDetectorMsgSealed implements the sealed FraudDetectorMsg interface.
func (m *UnexpectedSpendNotification) fraudDetectorMsgSealed() {}

// CheckpointSweepNotification asks the fraud responder to submit the operator
// timeout sweep for checkpoint output 0. Batchwatcher emits this after it has
// tracked the checkpoint output as part of the active frontier and observed
// that no spend arrived before CSV maturity.
type CheckpointSweepNotification struct {
	actor.BaseMessage

	// BatchID identifies which batch led to this checkpoint output.
	BatchID BatchID

	// InputOutpoint is the original spent OOR VTXO input consumed by the
	// checkpoint transaction.
	InputOutpoint wire.OutPoint

	// CheckpointOutpoint is the checkpoint output being swept. Step 1
	// always uses output 0.
	CheckpointOutpoint wire.OutPoint

	// MaturityHeight is the first block height at which the timeout sweep
	// is valid.
	MaturityHeight uint32
}

// MessageType returns the message type identifier for logging and debugging.
func (m *CheckpointSweepNotification) MessageType() string {
	return "CheckpointSweepNotification"
}

// fraudDetectorMsgSealed implements the sealed FraudDetectorMsg interface.
func (m *CheckpointSweepNotification) fraudDetectorMsgSealed() {}

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
// tree state changes (new outputs appear or existing outputs are spent). This
// allows the sweeper to re-attempt sweeping for batches that have already
// expired when additional operator-controlled outputs become available on-
// chain due to progressive unrolls.
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

// BatchSweptNotification is sent to the BatchSweeper when the watcher detects
// that a batch root output has been spent by a non-tree transaction (operator
// sweep) and no unspent outputs remain in the tree. The watcher self-
// unregisters after sending this, so the sweeper must not query the watcher
// for tree state after receiving it.
type BatchSweptNotification struct {
	actor.BaseMessage

	// BatchID identifies which batch was swept.
	BatchID BatchID

	// Tree is the full pre-signed VTXO tree so the sweeper can extract
	// leaf outpoints without querying back to the watcher.
	Tree *tree.Tree
}

// MessageType returns the message type identifier for logging and debugging.
func (m *BatchSweptNotification) MessageType() string {
	return "BatchSweptNotification"
}

// batchSweeperMsgSealed implements the sealed BatchSweeperMsg interface.
func (m *BatchSweptNotification) batchSweeperMsgSealed() {}

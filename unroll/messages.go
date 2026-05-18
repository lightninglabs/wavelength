package unroll

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/unrollplan"
)

// Actor priorities. Admission stays at the default priority so a restored
// actor loads before low-value block churn, while concrete chain observations
// run ahead of polling reads.
const (
	unrollProgressPriority = 100
	unrollHeightPriority   = -10
	unrollStatusPriority   = -20
)

// StartTrigger identifies what caused the unroll actor to start.
type StartTrigger int32

const (
	// TriggerManual indicates an operator-triggered start.
	TriggerManual StartTrigger = iota

	// TriggerCriticalExpiry indicates a VTXO critical-expiry handoff.
	TriggerCriticalExpiry

	// TriggerRestart indicates a restored in-flight job.
	TriggerRestart

	// TriggerFraudSpend indicates the job was started because the
	// target outpoint was seen spent externally and the actor needs to
	// escalate to fraud-handling.
	TriggerFraudSpend
)

// Phase identifies the coarse durable phase of the unroll actor.
type Phase string

const (
	// PhasePending indicates the actor exists but has not started work.
	PhasePending Phase = "pending"

	// PhaseMaterializing indicates proof transactions are still being
	// materialized or confirmed.
	PhaseMaterializing Phase = "materializing"

	// PhaseCSVPending indicates the target confirmed and the actor is
	// waiting for CSV maturity.
	PhaseCSVPending Phase = "csv_pending"

	// PhaseSweepBroadcast indicates the sweep is ready and is being
	// submitted to txconfirm.
	PhaseSweepBroadcast Phase = "sweep_broadcast"

	// PhaseSweepConfirmation indicates the sweep has been broadcast and is
	// awaiting confirmation.
	PhaseSweepConfirmation Phase = "sweep_confirmation"

	// PhaseCompleted indicates the sweep confirmed successfully.
	PhaseCompleted Phase = "completed"

	// PhaseFailed indicates the actor reached terminal failure.
	PhaseFailed Phase = "failed"
)

// Msg is the in-memory message surface accepted by the VTXO unroll actor.
// Restart safety comes from the SQL checkpoint store, not from serialized
// actor mailbox payloads.
type Msg interface {
	actor.Message

	unrollMsgSealed()
}

// Resp is the response surface returned by the VTXO unroll actor.
type Resp interface {
	actor.Message

	unrollRespSealed()
}

// StartUnrollRequest starts the actor at the given best height.
type StartUnrollRequest struct {
	actor.BaseMessage

	// Height is the current best height.
	Height int32

	// Trigger identifies why the unroll started.
	Trigger StartTrigger
}

// MessageType returns the stable message type identifier.
func (m *StartUnrollRequest) MessageType() string {
	return "StartUnrollRequest"
}

// unrollMsgSealed seals StartUnrollRequest into the message surface.
func (m *StartUnrollRequest) unrollMsgSealed() {}

// ResumeUnrollRequest resumes the actor from a durable checkpoint.
type ResumeUnrollRequest struct {
	actor.BaseMessage

	// Height is the current best height at resume time.
	Height int32
}

// MessageType returns the stable message type identifier.
func (m *ResumeUnrollRequest) MessageType() string {
	return "ResumeUnrollRequest"
}

// unrollMsgSealed seals ResumeUnrollRequest into the message surface.
func (m *ResumeUnrollRequest) unrollMsgSealed() {}

// HeightObservedMsg reports a new best height to the actor.
type HeightObservedMsg struct {
	actor.BaseMessage

	// Height is the latest observed best height.
	Height int32
}

// MessageType returns the stable message type identifier.
func (m *HeightObservedMsg) MessageType() string {
	return "HeightObservedMsg"
}

// Priority returns the priority for block-height ticks.
func (m *HeightObservedMsg) Priority() int {
	return unrollHeightPriority
}

// unrollMsgSealed seals HeightObservedMsg into the message surface.
func (m *HeightObservedMsg) unrollMsgSealed() {}

// TxConfirmedMsg reports that txconfirm observed one transaction confirmed.
type TxConfirmedMsg struct {
	actor.BaseMessage

	// Txid is the confirmed transaction hash.
	Txid chainhash.Hash

	// Height is the block height where the transaction confirmed.
	Height int32

	// NumConfs is the observed confirmation count.
	NumConfs uint32
}

// MessageType returns the stable message type identifier.
func (m *TxConfirmedMsg) MessageType() string {
	return "TxConfirmedMsg"
}

// Priority returns the priority for confirmation progress.
func (m *TxConfirmedMsg) Priority() int {
	return unrollProgressPriority
}

// unrollMsgSealed seals TxConfirmedMsg into the message surface.
func (m *TxConfirmedMsg) unrollMsgSealed() {}

// TxFailedMsg reports a terminal txconfirm failure for one transaction.
type TxFailedMsg struct {
	actor.BaseMessage

	// Txid identifies the failed transaction.
	Txid chainhash.Hash

	// Reason is the stable human-readable failure reason.
	Reason string
}

// MessageType returns the stable message type identifier.
func (m *TxFailedMsg) MessageType() string {
	return "TxFailedMsg"
}

// Priority returns the priority for terminal tx failures.
func (m *TxFailedMsg) Priority() int {
	return unrollProgressPriority
}

// unrollMsgSealed seals TxFailedMsg into the message surface.
func (m *TxFailedMsg) unrollMsgSealed() {}

// SpendObservedMsg reports that a watched outpoint was spent on-chain.
type SpendObservedMsg struct {
	actor.BaseMessage

	// Outpoint is the watched output that was spent.
	Outpoint wire.OutPoint

	// SpendingTxid is the transaction that spent the watched outpoint.
	SpendingTxid chainhash.Hash

	// SpendingHeight is the block height of the spending transaction.
	SpendingHeight int32
}

// MessageType returns the stable message type identifier.
func (m *SpendObservedMsg) MessageType() string {
	return "SpendObservedMsg"
}

// Priority returns the priority for target-spend progress.
func (m *SpendObservedMsg) Priority() int {
	return unrollProgressPriority
}

// unrollMsgSealed seals SpendObservedMsg into the message surface.
func (m *SpendObservedMsg) unrollMsgSealed() {}

// GetStateRequest asks the actor for its current in-memory state summary.
type GetStateRequest struct {
	actor.BaseMessage
}

// MessageType returns the stable message type identifier.
func (m *GetStateRequest) MessageType() string {
	return "GetStateRequest"
}

// Priority returns the priority for read-only status probes.
func (m *GetStateRequest) Priority() int {
	return unrollStatusPriority
}

// unrollMsgSealed seals GetStateRequest into the message surface.
func (m *GetStateRequest) unrollMsgSealed() {}

// AckResp is a trivial response used by Tell-first workflows.
type AckResp struct {
	actor.BaseMessage
}

// MessageType returns the stable message type identifier.
func (m *AckResp) MessageType() string {
	return "AckResp"
}

// unrollRespSealed seals AckResp into the response surface.
func (m *AckResp) unrollRespSealed() {}

// GetStateResp reports the actor's current durable and derived state.
type GetStateResp struct {
	actor.BaseMessage

	// Started reports whether the actor has been started.
	Started bool

	// Trigger identifies why the actor was started.
	Trigger StartTrigger

	// Height is the current best height tracked by the actor.
	Height int32

	// Phase is the coarse phase derived from planner state.
	Phase Phase

	// PlannerState is the durable planner-owned progress state.
	PlannerState unrollplan.State

	// FailReason records the terminal failure reason, if any.
	FailReason string

	// SweepTxid records the sweep txid when the actor has built one.
	SweepTxid *chainhash.Hash
}

// MessageType returns the stable message type identifier.
func (m *GetStateResp) MessageType() string {
	return "GetStateResp"
}

// unrollRespSealed seals GetStateResp into the response surface.
func (m *GetStateResp) unrollRespSealed() {}

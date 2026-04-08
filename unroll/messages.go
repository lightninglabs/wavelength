package unroll

import (
	"encoding/json"
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/unrollplan"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	startUnrollRequestTLVType  tlv.Type = 0x7900
	resumeUnrollRequestTLVType tlv.Type = 0x7901
	heightObservedMsgTLVType   tlv.Type = 0x7902
	txConfirmedMsgTLVType      tlv.Type = 0x7903
	txFailedMsgTLVType         tlv.Type = 0x7904
	getStateRequestTLVType     tlv.Type = 0x7905
	spendObservedMsgTLVType    tlv.Type = 0x7906
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
)

// Phase identifies the coarse durable phase of the new unroll actor.
type Phase string

const (
	// PhasePending indicates the actor exists but has not started work.
	PhasePending Phase = "pending"

	// PhaseMaterializing indicates proof transactions are still being
	// materialized or confirmed.
	PhaseMaterializing Phase = "materializing"

	// PhaseCSVPending indicates the target confirmed and the actor
	// is waiting
	// for CSV maturity.
	PhaseCSVPending Phase = "csv_pending"

	// PhaseSweepBroadcast indicates the sweep is ready and is being
	// submitted
	// to txconfirm.
	PhaseSweepBroadcast Phase = "sweep_broadcast"

	// PhaseSweepConfirmation indicates the sweep has been broadcast and is
	// awaiting confirmation.
	PhaseSweepConfirmation Phase = "sweep_confirmation"

	// PhaseCompleted indicates the sweep confirmed successfully.
	PhaseCompleted Phase = "completed"

	// PhaseFailed indicates the actor reached terminal failure.
	PhaseFailed Phase = "failed"
)

// Msg is the durable mailbox surface accepted by the VTXO unroll actor.
type Msg interface {
	actor.TLVMessage
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

// TLVType returns the durable mailbox type ID.
func (m *StartUnrollRequest) TLVType() tlv.Type {
	return startUnrollRequestTLVType
}

// Encode serializes the message.
func (m *StartUnrollRequest) Encode(w io.Writer) error {
	return encodeMessage(w, m)
}

// Decode deserializes the message.
func (m *StartUnrollRequest) Decode(r io.Reader) error {
	return decodeMessage(r, m)
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

// TLVType returns the durable mailbox type ID.
func (m *ResumeUnrollRequest) TLVType() tlv.Type {
	return resumeUnrollRequestTLVType
}

// Encode serializes the message.
func (m *ResumeUnrollRequest) Encode(w io.Writer) error {
	return encodeMessage(w, m)
}

// Decode deserializes the message.
func (m *ResumeUnrollRequest) Decode(r io.Reader) error {
	return decodeMessage(r, m)
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

// TLVType returns the durable mailbox type ID.
func (m *HeightObservedMsg) TLVType() tlv.Type {
	return heightObservedMsgTLVType
}

// Encode serializes the message.
func (m *HeightObservedMsg) Encode(w io.Writer) error {
	return encodeMessage(w, m)
}

// Decode deserializes the message.
func (m *HeightObservedMsg) Decode(r io.Reader) error {
	return decodeMessage(r, m)
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

// TLVType returns the durable mailbox type ID.
func (m *TxConfirmedMsg) TLVType() tlv.Type {
	return txConfirmedMsgTLVType
}

// Encode serializes the message.
func (m *TxConfirmedMsg) Encode(w io.Writer) error {
	return encodeMessage(w, m)
}

// Decode deserializes the message.
func (m *TxConfirmedMsg) Decode(r io.Reader) error {
	return decodeMessage(r, m)
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

// TLVType returns the durable mailbox type ID.
func (m *TxFailedMsg) TLVType() tlv.Type {
	return txFailedMsgTLVType
}

// Encode serializes the message.
func (m *TxFailedMsg) Encode(w io.Writer) error {
	return encodeMessage(w, m)
}

// Decode deserializes the message.
func (m *TxFailedMsg) Decode(r io.Reader) error {
	return decodeMessage(r, m)
}

// unrollMsgSealed seals TxFailedMsg into the message surface.
func (m *TxFailedMsg) unrollMsgSealed() {}

// SpendObservedMsg reports that the target outpoint was spent on-chain.
type SpendObservedMsg struct {
	actor.BaseMessage

	// SpendingTxid is the transaction that spent the target.
	SpendingTxid chainhash.Hash

	// SpendingHeight is the block height of the spending transaction.
	SpendingHeight int32
}

// MessageType returns the stable message type identifier.
func (m *SpendObservedMsg) MessageType() string {
	return "SpendObservedMsg"
}

// TLVType returns the durable mailbox type ID.
func (m *SpendObservedMsg) TLVType() tlv.Type {
	return spendObservedMsgTLVType
}

// Encode serializes the message.
func (m *SpendObservedMsg) Encode(w io.Writer) error {
	return encodeMessage(w, m)
}

// Decode deserializes the message.
func (m *SpendObservedMsg) Decode(r io.Reader) error {
	return decodeMessage(r, m)
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

// TLVType returns the durable mailbox type ID.
func (m *GetStateRequest) TLVType() tlv.Type {
	return getStateRequestTLVType
}

// Encode serializes the message.
func (m *GetStateRequest) Encode(w io.Writer) error {
	return encodeMessage(w, m)
}

// Decode deserializes the message.
func (m *GetStateRequest) Decode(r io.Reader) error {
	return decodeMessage(r, m)
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

// encodeMessage serializes one durable actor message as JSON.
func encodeMessage(w io.Writer, value any) error {
	return json.NewEncoder(w).Encode(value)
}

// decodeMessage deserializes one durable actor message from JSON.
func decodeMessage(r io.Reader, value any) error {
	return json.NewDecoder(r).Decode(value)
}

// newCodec creates a message codec with every unroll durable message type
// registered.
func newCodec() *actor.MessageCodec {
	codec := actor.NewMessageCodec()

	codec.MustRegister(startUnrollRequestTLVType,
		func() actor.TLVMessage { return &StartUnrollRequest{} },
	)
	codec.MustRegister(resumeUnrollRequestTLVType,
		func() actor.TLVMessage { return &ResumeUnrollRequest{} },
	)
	codec.MustRegister(heightObservedMsgTLVType,
		func() actor.TLVMessage { return &HeightObservedMsg{} },
	)
	codec.MustRegister(txConfirmedMsgTLVType,
		func() actor.TLVMessage { return &TxConfirmedMsg{} },
	)
	codec.MustRegister(txFailedMsgTLVType,
		func() actor.TLVMessage { return &TxFailedMsg{} },
	)
	codec.MustRegister(getStateRequestTLVType,
		func() actor.TLVMessage { return &GetStateRequest{} },
	)
	codec.MustRegister(spendObservedMsgTLVType,
		func() actor.TLVMessage { return &SpendObservedMsg{} },
	)

	return codec
}

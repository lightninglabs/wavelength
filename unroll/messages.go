package unroll

import (
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/unrollplan"
	"github.com/lightningnetwork/lnd/tlv"
)

// messages.go defines the durable mailbox surface for the per-target
// unroll actor. Every [Msg] here implements [actor.TLVMessage] with a
// hand-written Encode/Decode pair so that the delivery-store codec can
// persist and later restore the exact message bytes, with no JSON
// round-tripping.
//
// Two TLV layers are in play:
//
//  1. Outer identifiers (0x7900..) tell the durable-mailbox codec
//     which message variant is being carried. These are globally unique
//     across the actor's mailbox.
//
//  2. Inner record types (1, 3, 5, ...) namespace the payload fields
//     of each message. Because the outer codec has already resolved the
//     variant, inner types can restart at 1 per message, and odd
//     numbering leaves slots for additive even-typed extensions
//     (lightning-style schema evolution).
//
// Round-trip tests in messages_test.go pin the byte layout of every
// message so a change in encoding semantics cannot slip past review.

// Outer TLV identifiers carried by the durable mailbox codec.
const (
	startUnrollRequestTLVType  tlv.Type = 0x7900
	resumeUnrollRequestTLVType tlv.Type = 0x7901
	heightObservedMsgTLVType   tlv.Type = 0x7902
	txConfirmedMsgTLVType      tlv.Type = 0x7903
	txFailedMsgTLVType         tlv.Type = 0x7904
	getStateRequestTLVType     tlv.Type = 0x7905
	spendObservedMsgTLVType    tlv.Type = 0x7906
)

// Durable mailbox priorities. Admission stays at the default priority so a
// restored actor loads before low-value block churn, while concrete chain
// observations run ahead of polling reads.
const (
	unrollProgressPriority = 100
	unrollHeightPriority   = -10
	unrollStatusPriority   = -20
)

// Inner payload TLV record types. Each message has its own namespace — the
// outer mailbox codec already identifies the message, so inner types can
// start at 1. Odd values leave room for additive even-typed extensions.
const (
	startUnrollHeightRecType         tlv.Type = 1
	startUnrollTriggerRecType        tlv.Type = 3
	startUnrollExitPolicyKindRecType tlv.Type = 5
	startUnrollExitPolicyRefRecType  tlv.Type = 7

	resumeUnrollHeightRecType tlv.Type = 1

	heightObservedHeightRecType tlv.Type = 1

	txConfirmedTxidRecType     tlv.Type = 1
	txConfirmedHeightRecType   tlv.Type = 3
	txConfirmedNumConfsRecType tlv.Type = 5

	txFailedTxidRecType   tlv.Type = 1
	txFailedReasonRecType tlv.Type = 3

	spendObservedTxidRecType     tlv.Type = 1
	spendObservedHeightRecType   tlv.Type = 3
	spendObservedOutHashRecType  tlv.Type = 5
	spendObservedOutIndexRecType tlv.Type = 7
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
	// escalate to fraud-handling. The DB side reserves status=3 on the
	// trigger column for this case; earlier revisions silently
	// downgraded FraudSpend rows to TriggerManual on restore.
	TriggerFraudSpend
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

	// ExitPolicyKind identifies the final spend policy to persist for this
	// target. Empty requests use the standard VTXO timeout policy.
	ExitPolicyKind ExitPolicyKind

	// ExitPolicyRef is the policy-specific durable reference.
	ExitPolicyRef string
}

// MessageType returns the stable message type identifier.
func (m *StartUnrollRequest) MessageType() string {
	return "StartUnrollRequest"
}

// TLVType returns the durable mailbox type ID.
func (m *StartUnrollRequest) TLVType() tlv.Type {
	return startUnrollRequestTLVType
}

// Encode serializes the message as a TLV stream.
func (m *StartUnrollRequest) Encode(w io.Writer) error {
	height := uint32(m.Height)
	trigger := uint32(m.Trigger)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(startUnrollHeightRecType, &height),
		tlv.MakePrimitiveRecord(startUnrollTriggerRecType, &trigger),
	}

	if m.ExitPolicyKind != "" {
		policyKind := []byte(m.ExitPolicyKind)
		records = append(
			records, tlv.MakePrimitiveRecord(
				startUnrollExitPolicyKindRecType, &policyKind,
			),
		)
	}

	if m.ExitPolicyRef != "" {
		policyRef := []byte(m.ExitPolicyRef)
		records = append(
			records, tlv.MakePrimitiveRecord(
				startUnrollExitPolicyRefRecType, &policyRef,
			),
		)
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return fmt.Errorf("create stream: %w", err)
	}

	return stream.Encode(w)
}

// Decode deserializes the message from a TLV stream.
func (m *StartUnrollRequest) Decode(r io.Reader) error {
	var height, trigger uint32
	var policyKind, policyRef []byte

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(startUnrollHeightRecType, &height),
		tlv.MakePrimitiveRecord(startUnrollTriggerRecType, &trigger),
		tlv.MakePrimitiveRecord(
			startUnrollExitPolicyKindRecType, &policyKind,
		),
		tlv.MakePrimitiveRecord(
			startUnrollExitPolicyRefRecType, &policyRef,
		),
	)
	if err != nil {
		return fmt.Errorf("create stream: %w", err)
	}

	parsed, err := stream.DecodeWithParsedTypes(r)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	if _, ok := parsed[startUnrollHeightRecType]; !ok {
		return fmt.Errorf("start unroll request missing height")
	}

	if _, ok := parsed[startUnrollTriggerRecType]; !ok {
		return fmt.Errorf("start unroll request missing trigger")
	}

	m.Height = int32(height)
	m.Trigger = StartTrigger(int32(trigger))
	if _, ok := parsed[startUnrollExitPolicyKindRecType]; ok {
		m.ExitPolicyKind = ExitPolicyKind(policyKind)
	}
	if _, ok := parsed[startUnrollExitPolicyRefRecType]; ok {
		m.ExitPolicyRef = string(policyRef)
	}

	return nil
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

// Encode serializes the message as a TLV stream.
func (m *ResumeUnrollRequest) Encode(w io.Writer) error {
	height := uint32(m.Height)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(resumeUnrollHeightRecType, &height),
	)
	if err != nil {
		return fmt.Errorf("create stream: %w", err)
	}

	return stream.Encode(w)
}

// Decode deserializes the message from a TLV stream.
func (m *ResumeUnrollRequest) Decode(r io.Reader) error {
	var height uint32

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(resumeUnrollHeightRecType, &height),
	)
	if err != nil {
		return fmt.Errorf("create stream: %w", err)
	}

	if err := stream.Decode(r); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	m.Height = int32(height)

	return nil
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

// Priority returns the durable mailbox priority for block-height ticks.
func (m *HeightObservedMsg) Priority() int {
	return unrollHeightPriority
}

// Encode serializes the message as a TLV stream.
func (m *HeightObservedMsg) Encode(w io.Writer) error {
	height := uint32(m.Height)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(heightObservedHeightRecType, &height),
	)
	if err != nil {
		return fmt.Errorf("create stream: %w", err)
	}

	return stream.Encode(w)
}

// Decode deserializes the message from a TLV stream.
func (m *HeightObservedMsg) Decode(r io.Reader) error {
	var height uint32

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(heightObservedHeightRecType, &height),
	)
	if err != nil {
		return fmt.Errorf("create stream: %w", err)
	}

	if err := stream.Decode(r); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	m.Height = int32(height)

	return nil
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

// Priority returns the durable mailbox priority for confirmation progress.
func (m *TxConfirmedMsg) Priority() int {
	return unrollProgressPriority
}

// Encode serializes the message as a TLV stream.
func (m *TxConfirmedMsg) Encode(w io.Writer) error {
	txid := [32]byte(m.Txid)
	height := uint32(m.Height)
	numConfs := m.NumConfs

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(txConfirmedTxidRecType, &txid),
		tlv.MakePrimitiveRecord(txConfirmedHeightRecType, &height),
		tlv.MakePrimitiveRecord(
			txConfirmedNumConfsRecType, &numConfs,
		),
	)
	if err != nil {
		return fmt.Errorf("create stream: %w", err)
	}

	return stream.Encode(w)
}

// Decode deserializes the message from a TLV stream.
func (m *TxConfirmedMsg) Decode(r io.Reader) error {
	var (
		txid     [32]byte
		height   uint32
		numConfs uint32
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(txConfirmedTxidRecType, &txid),
		tlv.MakePrimitiveRecord(txConfirmedHeightRecType, &height),
		tlv.MakePrimitiveRecord(
			txConfirmedNumConfsRecType, &numConfs,
		),
	)
	if err != nil {
		return fmt.Errorf("create stream: %w", err)
	}

	if err := stream.Decode(r); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	m.Txid = chainhash.Hash(txid)
	m.Height = int32(height)
	m.NumConfs = numConfs

	return nil
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

// Priority returns the durable mailbox priority for terminal tx failures.
func (m *TxFailedMsg) Priority() int {
	return unrollProgressPriority
}

// Encode serializes the message as a TLV stream.
func (m *TxFailedMsg) Encode(w io.Writer) error {
	txid := [32]byte(m.Txid)
	reason := []byte(m.Reason)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(txFailedTxidRecType, &txid),
		tlv.MakePrimitiveRecord(txFailedReasonRecType, &reason),
	)
	if err != nil {
		return fmt.Errorf("create stream: %w", err)
	}

	return stream.Encode(w)
}

// Decode deserializes the message from a TLV stream.
func (m *TxFailedMsg) Decode(r io.Reader) error {
	var (
		txid   [32]byte
		reason []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(txFailedTxidRecType, &txid),
		tlv.MakePrimitiveRecord(txFailedReasonRecType, &reason),
	)
	if err != nil {
		return fmt.Errorf("create stream: %w", err)
	}

	if err := stream.Decode(r); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	m.Txid = chainhash.Hash(txid)
	m.Reason = string(reason)

	return nil
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

// TLVType returns the durable mailbox type ID.
func (m *SpendObservedMsg) TLVType() tlv.Type {
	return spendObservedMsgTLVType
}

// Priority returns the durable mailbox priority for target-spend progress.
func (m *SpendObservedMsg) Priority() int {
	return unrollProgressPriority
}

// Encode serializes the message as a TLV stream.
func (m *SpendObservedMsg) Encode(w io.Writer) error {
	txid := [32]byte(m.SpendingTxid)
	height := uint32(m.SpendingHeight)
	outHash := [32]byte(m.Outpoint.Hash)
	outIndex := m.Outpoint.Index

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(spendObservedTxidRecType, &txid),
		tlv.MakePrimitiveRecord(spendObservedHeightRecType, &height),
		tlv.MakePrimitiveRecord(spendObservedOutHashRecType, &outHash),
		tlv.MakePrimitiveRecord(
			spendObservedOutIndexRecType, &outIndex,
		),
	)
	if err != nil {
		return fmt.Errorf("create stream: %w", err)
	}

	return stream.Encode(w)
}

// Decode deserializes the message from a TLV stream.
func (m *SpendObservedMsg) Decode(r io.Reader) error {
	var (
		txid     [32]byte
		height   uint32
		outHash  [32]byte
		outIndex uint32
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(spendObservedTxidRecType, &txid),
		tlv.MakePrimitiveRecord(spendObservedHeightRecType, &height),
		tlv.MakePrimitiveRecord(spendObservedOutHashRecType, &outHash),
		tlv.MakePrimitiveRecord(
			spendObservedOutIndexRecType, &outIndex,
		),
	)
	if err != nil {
		return fmt.Errorf("create stream: %w", err)
	}

	if err := stream.Decode(r); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	m.SpendingTxid = chainhash.Hash(txid)
	m.SpendingHeight = int32(height)
	m.Outpoint = wire.OutPoint{
		Hash:  chainhash.Hash(outHash),
		Index: outIndex,
	}

	return nil
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

// Priority returns the durable mailbox priority for read-only status probes.
func (m *GetStateRequest) Priority() int {
	return unrollStatusPriority
}

// Encode serializes the empty-payload message. An empty TLV stream
// decodes cleanly on the other end, so we don't need to emit any
// records.
func (m *GetStateRequest) Encode(_ io.Writer) error {
	return nil
}

// Decode deserializes the empty-payload message. The codec consumes any
// bytes already framed for the outer type so we can drain and drop.
func (m *GetStateRequest) Decode(r io.Reader) error {
	_, err := io.Copy(io.Discard, r)

	return err
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

	// ExitPolicyKind identifies the final spend policy for this target.
	ExitPolicyKind ExitPolicyKind

	// ExitPolicyRef is the policy-specific durable reference.
	ExitPolicyRef string

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

// newCodec creates a message codec with every unroll durable message type
// registered.
func newCodec() *actor.MessageCodec {
	codec := actor.NewMessageCodec()

	codec.MustRegister(
		startUnrollRequestTLVType,
		func() actor.TLVMessage { return &StartUnrollRequest{} },
	)
	codec.MustRegister(
		resumeUnrollRequestTLVType,
		func() actor.TLVMessage { return &ResumeUnrollRequest{} },
	)
	codec.MustRegister(
		heightObservedMsgTLVType,
		func() actor.TLVMessage { return &HeightObservedMsg{} },
	)
	codec.MustRegister(
		txConfirmedMsgTLVType,
		func() actor.TLVMessage { return &TxConfirmedMsg{} },
	)
	codec.MustRegister(
		txFailedMsgTLVType,
		func() actor.TLVMessage { return &TxFailedMsg{} },
	)
	codec.MustRegister(
		getStateRequestTLVType,
		func() actor.TLVMessage { return &GetStateRequest{} },
	)
	codec.MustRegister(
		spendObservedMsgTLVType,
		func() actor.TLVMessage { return &SpendObservedMsg{} },
	)

	return codec
}

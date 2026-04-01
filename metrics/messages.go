package metrics

import (
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
)

// Msg is the sealed interface for all messages sent to the metrics
// actor. Subsystem actors send typed metric events here instead of
// calling Prometheus directly, keeping all instrumentation in one
// auditable place.
type Msg interface {
	actor.Message

	// metricsMsgSealed prevents external implementations.
	metricsMsgSealed()
}

// Resp is a placeholder response type for the metrics actor. All
// metric messages are fire-and-forget, so no meaningful response is
// returned.
type Resp = any

// RoundCreatedMsg is sent when a new round is created and enters
// the registration phase. The metrics actor starts tracking the
// round's lifetime and registration phase duration.
type RoundCreatedMsg struct {
	actor.BaseMessage

	// RoundID identifies the newly created round.
	RoundID string
}

// MessageType implements actor.Message.
func (m *RoundCreatedMsg) MessageType() string {
	return "metrics.RoundCreated"
}

func (m *RoundCreatedMsg) metricsMsgSealed() {}

// ClientJoinedRoundMsg is sent each time a client successfully
// joins a round. The metrics actor increments its internal client
// count for the round.
type ClientJoinedRoundMsg struct {
	actor.BaseMessage

	// RoundID identifies the round the client joined.
	RoundID string
}

// MessageType implements actor.Message.
func (m *ClientJoinedRoundMsg) MessageType() string {
	return "metrics.ClientJoinedRound"
}

func (m *ClientJoinedRoundMsg) metricsMsgSealed() {}

// RoundSealedMsg is sent when registration closes. The metrics
// actor observes the registration duration and starts the batch
// build phase timer.
type RoundSealedMsg struct {
	actor.BaseMessage

	// RoundID identifies the sealed round.
	RoundID string

	// TimedOut is true if the registration phase ended by timeout
	// rather than by a seal predicate.
	TimedOut bool
}

// MessageType implements actor.Message.
func (m *RoundSealedMsg) MessageType() string {
	return "metrics.RoundSealed"
}

func (m *RoundSealedMsg) metricsMsgSealed() {}

// RoundBatchBuiltMsg is sent when the commitment transaction is
// built successfully. The metrics actor observes the build duration
// and records batch composition counts.
type RoundBatchBuiltMsg struct {
	actor.BaseMessage

	// RoundID identifies the round whose batch was built.
	RoundID string

	// BoardingInputs is the number of boarding inputs in the
	// batch.
	BoardingInputs int

	// LeaveOutputs is the number of leave (withdrawal) outputs
	// in the batch.
	LeaveOutputs int

	// VTXOsGenerated is the number of VTXOs created by the batch.
	VTXOsGenerated int

	// TreeCount is the number of VTXO trees in the batch.
	TreeCount int
}

// MessageType implements actor.Message.
func (m *RoundBatchBuiltMsg) MessageType() string {
	return "metrics.RoundBatchBuilt"
}

func (m *RoundBatchBuiltMsg) metricsMsgSealed() {}

// RoundBatchBuildFailedMsg is sent when batch building fails. The
// metrics actor observes the build duration from its internal timer.
type RoundBatchBuildFailedMsg struct {
	actor.BaseMessage

	// RoundID identifies the round whose batch build failed.
	RoundID string
}

// MessageType implements actor.Message.
func (m *RoundBatchBuildFailedMsg) MessageType() string {
	return "metrics.RoundBatchBuildFailed"
}

func (m *RoundBatchBuildFailedMsg) metricsMsgSealed() {}

// PhaseStartedMsg is sent when a signing phase begins. The metrics
// actor starts an internal timer for this phase.
type PhaseStartedMsg struct {
	actor.BaseMessage

	// RoundID identifies the round this phase belongs to.
	RoundID string

	// Phase is the phase name: "nonce_exchange", "input_sigs",
	// or "vtxo_sigs".
	Phase string
}

// MessageType implements actor.Message.
func (m *PhaseStartedMsg) MessageType() string {
	return "metrics.PhaseStarted"
}

func (m *PhaseStartedMsg) metricsMsgSealed() {}

// PhaseEndedMsg is sent when a signing phase ends (either by
// successful completion or timeout). The metrics actor observes the
// duration from its internal timer.
type PhaseEndedMsg struct {
	actor.BaseMessage

	// RoundID identifies the round this phase belongs to.
	RoundID string

	// Phase is the phase name: "nonce_exchange", "input_sigs",
	// or "vtxo_sigs".
	Phase string

	// TimedOut is true if the phase ended by timeout rather than
	// successful collection.
	TimedOut bool
}

// MessageType implements actor.Message.
func (m *PhaseEndedMsg) MessageType() string {
	return "metrics.PhaseEnded"
}

func (m *PhaseEndedMsg) metricsMsgSealed() {}

// RoundCompletedMsg is sent when a round reaches a terminal state
// (confirmed or failed). The metrics actor observes the total
// duration from its internal timer and emits the client count.
type RoundCompletedMsg struct {
	actor.BaseMessage

	// RoundID identifies the completed round.
	RoundID string

	// Status is the round outcome: "confirmed" or "failed".
	Status string

	// BlockHeight is the confirmation height. Non-zero only for
	// confirmed rounds.
	BlockHeight uint32
}

// MessageType implements actor.Message.
func (m *RoundCompletedMsg) MessageType() string {
	return "metrics.RoundCompleted"
}

func (m *RoundCompletedMsg) metricsMsgSealed() {}

// OORTransferStartedMsg is sent when a new OOR transfer session
// begins. The metrics actor starts an internal timer.
type OORTransferStartedMsg struct {
	actor.BaseMessage

	// SessionID identifies the OOR transfer session.
	SessionID string
}

// MessageType implements actor.Message.
func (m *OORTransferStartedMsg) MessageType() string {
	return "metrics.OORTransferStarted"
}

func (m *OORTransferStartedMsg) metricsMsgSealed() {}

// OORTransferCompletedMsg is sent when an OOR transfer reaches a
// terminal state (finalized or failed). The metrics actor observes
// the duration from its internal timer.
type OORTransferCompletedMsg struct {
	actor.BaseMessage

	// SessionID identifies the OOR transfer session.
	SessionID string

	// Status is the transfer outcome: "finalized" or "failed".
	Status string
}

// MessageType implements actor.Message.
func (m *OORTransferCompletedMsg) MessageType() string {
	return "metrics.OORTransferCompleted"
}

func (m *OORTransferCompletedMsg) metricsMsgSealed() {}

// VTXOLockResultMsg is sent after a VTXO lock attempt completes.
type VTXOLockResultMsg struct {
	actor.BaseMessage

	// Owner identifies who requested the lock (e.g. "round" or
	// "oor").
	Owner string

	// Duration is the time spent acquiring the lock.
	Duration time.Duration

	// Success is true if the lock was acquired.
	Success bool

	// Reason is the failure reason when Success is false.
	Reason string
}

// MessageType implements actor.Message.
func (m *VTXOLockResultMsg) MessageType() string {
	return "metrics.VTXOLockResult"
}

func (m *VTXOLockResultMsg) metricsMsgSealed() {}

// DispatchCompletedMsg is sent after an envelope dispatch completes
// in the clientconn ingress loop.
type DispatchCompletedMsg struct {
	actor.BaseMessage

	// ServiceMethod is the dispatched RPC service method name.
	ServiceMethod string

	// Duration is the time spent dispatching the envelope.
	Duration time.Duration
}

// MessageType implements actor.Message.
func (m *DispatchCompletedMsg) MessageType() string {
	return "metrics.DispatchCompleted"
}

func (m *DispatchCompletedMsg) metricsMsgSealed() {}

// ClientStatusChangedMsg is sent when a client transitions between
// online and offline status.
type ClientStatusChangedMsg struct {
	actor.BaseMessage

	// Online is true when a client came online, false when it
	// went offline.
	Online bool
}

// MessageType implements actor.Message.
func (m *ClientStatusChangedMsg) MessageType() string {
	return "metrics.ClientStatusChanged"
}

func (m *ClientStatusChangedMsg) metricsMsgSealed() {}

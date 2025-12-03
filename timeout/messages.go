package timeout

import (
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
)

// ID is a unique identifier for a scheduled timeout. This is intentionally
// generic so that it can be used by any component that needs timeout
// scheduling (e.g., rounds, sessions, etc.).
type ID string

// Msg is the sealed interface for messages that can be sent to the
// TimeoutActor.
type Msg interface {
	actor.Message
	timeoutMsgSealed()
}

// Resp is the sealed interface for responses from the TimeoutActor.
type Resp interface {
	actor.Message
	timeoutRespSealed()
}

// ScheduleTimeoutRequest requests the timeout actor to schedule a timeout.
// When the timeout expires, an ExpiredMsg will be sent to the Callback.
type ScheduleTimeoutRequest struct {
	actor.BaseMessage

	// ID is the unique identifier for this timeout.
	ID ID

	// Duration is how long to wait before the timeout expires.
	Duration time.Duration

	// Callback is the actor reference to notify when the timeout expires.
	Callback actor.TellOnlyRef[Msg]
}

// MessageType returns the type of this message.
func (m *ScheduleTimeoutRequest) MessageType() string {
	return "ScheduleTimeoutRequest"
}

// timeoutMsgSealed marks this as implementing the sealed Msg interface.
func (m *ScheduleTimeoutRequest) timeoutMsgSealed() {}

// CancelTimeoutRequest requests the timeout actor to cancel a pending timeout.
type CancelTimeoutRequest struct {
	actor.BaseMessage

	// ID is the unique identifier of the timeout to cancel.
	ID ID
}

// MessageType returns the type of this message.
func (m *CancelTimeoutRequest) MessageType() string {
	return "CancelTimeoutRequest"
}

// timeoutMsgSealed marks this as implementing the sealed Msg interface.
func (m *CancelTimeoutRequest) timeoutMsgSealed() {}

// ExpiredMsg is sent back to the callback when a timeout expires.
// This message is also a valid Msg so it can be received by the rounds actor.
type ExpiredMsg struct {
	actor.BaseMessage

	// ID is the unique identifier of the timeout that expired.
	ID ID
}

// MessageType returns the type of this message.
func (m *ExpiredMsg) MessageType() string {
	return "ExpiredMsg"
}

// timeoutMsgSealed marks this as implementing the sealed Msg interface.
func (m *ExpiredMsg) timeoutMsgSealed() {}

// AckResponse acknowledges a schedule or cancel request.
type AckResponse struct {
	actor.BaseMessage

	// Success indicates whether the operation succeeded.
	Success bool

	// Error contains an error message if Success is false.
	Error string
}

// MessageType returns the type of this message.
func (m *AckResponse) MessageType() string {
	return "AckResponse"
}

// timeoutRespSealed marks this as implementing the sealed Resp interface.
func (m *AckResponse) timeoutRespSealed() {}

package metrics

import (
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
)

// Msg is the sealed interface for all messages sent to the metrics
// actor. Subsystem actors and the daemon send typed metric events here
// instead of calling Prometheus directly, keeping all event-driven
// instrumentation in one auditable place. This mirrors the lumosd server's
// metrics actor design.
type Msg interface {
	actor.Message

	// metricsMsgSealed prevents external implementations.
	metricsMsgSealed()
}

// Resp is the placeholder response type for the metrics actor. All
// metric messages are fire-and-forget, so no meaningful response is
// returned and callers Tell, never Ask.
type Resp = any

// RoundJoinedMsg is sent when the client attempts to join a settlement
// round. The metrics actor increments RoundsJoinedTotal.
type RoundJoinedMsg struct {
	actor.BaseMessage

	// RoundID identifies the round being joined. Empty is allowed when
	// the join is triggered before a round id is known.
	RoundID string
}

// MessageType implements actor.Message.
func (m *RoundJoinedMsg) MessageType() string {
	return "metrics.RoundJoined"
}

func (m *RoundJoinedMsg) metricsMsgSealed() {}

// RoundCompletedMsg is sent when a settlement round the client
// participated in reaches a terminal outcome. The metrics actor
// increments RoundsCompletedTotal labelled by Status.
type RoundCompletedMsg struct {
	actor.BaseMessage

	// RoundID identifies the completed round.
	RoundID string

	// Status is the round outcome: "confirmed" or "failed".
	Status string
}

// MessageType implements actor.Message.
func (m *RoundCompletedMsg) MessageType() string {
	return "metrics.RoundCompleted"
}

func (m *RoundCompletedMsg) metricsMsgSealed() {}

// OORTransferSentMsg is sent when an outgoing out-of-round (async)
// transfer reaches a terminal outcome. The metrics actor increments
// OORTransfersSentTotal labelled by Status.
type OORTransferSentMsg struct {
	actor.BaseMessage

	// SessionID identifies the OOR transfer session.
	SessionID string

	// Status is the transfer outcome: "submitted" or "failed".
	Status string

	// Duration is the wall-clock time from the SendOOR call entry to
	// this terminal outcome, measured at the call site and observed
	// into OORTransferDurationSeconds. Carrying it on the message keeps
	// the metrics actor stateless (no per-session start-time tracking).
	// A zero value is not observed.
	Duration time.Duration
}

// MessageType implements actor.Message.
func (m *OORTransferSentMsg) MessageType() string {
	return "metrics.OORTransferSent"
}

func (m *OORTransferSentMsg) metricsMsgSealed() {}

// OORTransferReceivedMsg is sent when an incoming out-of-round transfer
// reaches a terminal outcome. The metrics actor increments
// OORTransfersReceivedTotal labelled by Status.
type OORTransferReceivedMsg struct {
	actor.BaseMessage

	// Status is the receive outcome: "materialized" or "failed".
	Status string
}

// MessageType implements actor.Message.
func (m *OORTransferReceivedMsg) MessageType() string {
	return "metrics.OORTransferReceived"
}

func (m *OORTransferReceivedMsg) metricsMsgSealed() {}

// BoardingEventMsg is sent when a boarding (on-chain to VTXO) intent
// reaches a terminal submit outcome. The metrics actor increments
// BoardingEventsTotal labelled by Status.
type BoardingEventMsg struct {
	actor.BaseMessage

	// Status is the boarding outcome: "submitted", "skipped", or
	// "failed".
	Status string
}

// MessageType implements actor.Message.
func (m *BoardingEventMsg) MessageType() string {
	return "metrics.BoardingEvent"
}

func (m *BoardingEventMsg) metricsMsgSealed() {}

// BackgroundTaskErrorMsg is sent when a daemon-owned background task hits
// an error. The metrics actor increments BackgroundTaskErrorsTotal
// labelled by Task.
type BackgroundTaskErrorMsg struct {
	actor.BaseMessage

	// Task names the failing background task (e.g.
	// "boarding_sweep_watcher").
	Task string
}

// MessageType implements actor.Message.
func (m *BackgroundTaskErrorMsg) MessageType() string {
	return "metrics.BackgroundTaskError"
}

func (m *BackgroundTaskErrorMsg) metricsMsgSealed() {}

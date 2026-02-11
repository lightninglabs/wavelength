package oor

import (
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
)

// ReceiveState is a sealed interface for all states in the incoming transfer
// FSM.
//
// This FSM is separate from the sender transfer FSM. It exists so a client can
// validate and acknowledge incoming transfers in a restart-friendly way.
type ReceiveState interface {
	protofsm.State[Event, OutboxEvent, *Environment]
	receiveStateSealed()
}

// ReceiveIdle is the initial state for handling incoming transfers.
type ReceiveIdle struct{}

// String returns a human-readable representation of ReceiveIdle.
func (s *ReceiveIdle) String() string {
	return "ReceiveIdle"
}

// IsTerminal returns false as ReceiveIdle is not terminal.
func (s *ReceiveIdle) IsTerminal() bool {
	return false
}

// receiveStateSealed marks ReceiveIdle as implementing ReceiveState.
func (s *ReceiveIdle) receiveStateSealed() {}

// ReceiveNotified indicates the client has validated and surfaced an incoming
// transfer and is waiting to ack it.
type ReceiveNotified struct {
	SessionID SessionID

	ArkPSBT *psbt.Packet

	// FinalCheckpointPSBTs carries finalized checkpoint packages that anchor
	// this incoming transfer. We keep these in-state so restart/resume paths
	// can re-drive deterministic materialization without re-fetching metadata.
	FinalCheckpointPSBTs []*psbt.Packet
}

// String returns a human-readable representation of ReceiveNotified.
func (s *ReceiveNotified) String() string {
	return "ReceiveNotified"
}

// IsTerminal returns false as ReceiveNotified is not terminal.
func (s *ReceiveNotified) IsTerminal() bool {
	return false
}

// receiveStateSealed marks ReceiveNotified as implementing ReceiveState.
func (s *ReceiveNotified) receiveStateSealed() {}

// ReceiveAwaitingAck indicates incoming VTXOs were materialized locally and the
// client is waiting for ack transport completion.
type ReceiveAwaitingAck struct {
	SessionID SessionID
}

// String returns a human-readable representation of ReceiveAwaitingAck.
func (s *ReceiveAwaitingAck) String() string {
	return "ReceiveAwaitingAck"
}

// IsTerminal returns false as ReceiveAwaitingAck is not terminal.
func (s *ReceiveAwaitingAck) IsTerminal() bool {
	return false
}

// receiveStateSealed marks ReceiveAwaitingAck as implementing ReceiveState.
func (s *ReceiveAwaitingAck) receiveStateSealed() {}

// ReceiveCompleted is the terminal success state for an incoming transfer.
type ReceiveCompleted struct{}

// String returns a human-readable representation of ReceiveCompleted.
func (s *ReceiveCompleted) String() string {
	return "ReceiveCompleted"
}

// IsTerminal returns true as ReceiveCompleted is terminal.
func (s *ReceiveCompleted) IsTerminal() bool {
	return true
}

// receiveStateSealed marks ReceiveCompleted as implementing ReceiveState.
func (s *ReceiveCompleted) receiveStateSealed() {}

package oor

import (
	"github.com/btcsuite/btcd/psbt/v2"
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

// ReceiveResolving indicates the client durably recorded a lightweight
// incoming-transfer hint and still needs to fetch the full Ark/checkpoint
// package outside the live durable actor transaction.
type ReceiveResolving struct {
	SessionID SessionID

	RecipientPkScript []byte

	RecipientEventID uint64

	// ResolveAttempts counts how many times the phase-1 hint resolution has
	// been re-issued for this session without a response. It drives the
	// exponential backoff and the terminal give-up in handleResolveRetry,
	// and is persisted in the snapshot so the bound survives restarts.
	ResolveAttempts uint32
}

// String returns a human-readable representation of ReceiveResolving.
func (s *ReceiveResolving) String() string {
	return "ReceiveResolving"
}

// IsTerminal returns false as ReceiveResolving is not terminal.
func (s *ReceiveResolving) IsTerminal() bool {
	return false
}

// receiveStateSealed marks ReceiveResolving as implementing ReceiveState.
func (s *ReceiveResolving) receiveStateSealed() {}

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
// transfer and is waiting for local materialization to finish.
type ReceiveNotified struct {
	SessionID SessionID

	ArkPSBT *psbt.Packet

	FinalCheckpointPSBTs []*psbt.Packet

	AncestorPackages []PackageArtifact

	// Recipients are the structurally validated Ark outputs plus optional
	// semantic policy metadata needed to materialize custom VTXOs.
	Recipients []ArkRecipientOutput

	// MetadataAttempts counts how many times the authoritative metadata
	// resolution has failed retryably for this session. It drives the
	// exponential backoff and terminal give-up in handleReceiveOutboxError
	// and is persisted in the snapshot so the bound survives restarts.
	MetadataAttempts uint32
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

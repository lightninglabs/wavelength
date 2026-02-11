package oor

import (
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
)

// State is a sealed interface for all states in the OOR transfer session FSM.
type State interface {
	protofsm.State[Event, OutboxEvent, *Environment]

	// stateSealed marks this interface as sealed.
	stateSealed()
}

// IdleState is the initial state of an OOR transfer session.
type IdleState struct{}

// String returns a human-readable representation of IdleState.
func (s *IdleState) String() string {
	return "IdleState"
}

// IsTerminal returns false as IdleState is not terminal.
func (s *IdleState) IsTerminal() bool {
	return false
}

// stateSealed marks IdleState as implementing the sealed State interface.
func (s *IdleState) stateSealed() {}

// RequestedState indicates a submit request has been received and inputs are in
// the process of being locked.
type RequestedState struct {
	// Inputs are the VTXO outpoints spent by the checkpoint transactions.
	Inputs []wire.OutPoint

	// ArkPSBT is the pre-validated Ark tx PSBT.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs are the pre-validated checkpoint PSBTs.
	CheckpointPSBTs []*psbt.Packet
}

// String returns a human-readable representation of RequestedState.
func (s *RequestedState) String() string {
	return "RequestedState"
}

// IsTerminal returns false as RequestedState is not terminal.
func (s *RequestedState) IsTerminal() bool {
	return false
}

// stateSealed marks RequestedState as implementing the sealed State interface.
func (s *RequestedState) stateSealed() {}

// ValidatedState indicates validation has succeeded and the operator can sign.
type ValidatedState struct {
	// Inputs are the VTXO outpoints spent by the checkpoint transactions.
	Inputs []wire.OutPoint

	// ArkPSBT is the pre-validated Ark tx PSBT.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs are the pre-validated checkpoint PSBTs.
	CheckpointPSBTs []*psbt.Packet
}

// String returns a human-readable representation of ValidatedState.
func (s *ValidatedState) String() string {
	return "ValidatedState"
}

// IsTerminal returns false as ValidatedState is not terminal.
func (s *ValidatedState) IsTerminal() bool {
	return false
}

// stateSealed marks ValidatedState as implementing the sealed State interface.
func (s *ValidatedState) stateSealed() {}

// CoSignedState indicates the operator has co-signed the package and the
// session has reached its point-of-no-return.
type CoSignedState struct {
	// ArkPSBT is carried forward for finalize package validation.
	ArkPSBT *psbt.Packet
}

// String returns a human-readable representation of CoSignedState.
func (s *CoSignedState) String() string {
	return "CoSignedState"
}

// IsTerminal returns false as CoSignedState is not terminal.
func (s *CoSignedState) IsTerminal() bool {
	return false
}

// stateSealed marks CoSignedState as implementing the sealed State interface.
func (s *CoSignedState) stateSealed() {}

// AwaitingFinalCheckpointsState indicates we are waiting for the client to
// submit fully signed checkpoint PSBTs.
type AwaitingFinalCheckpointsState struct{}

// String returns a human-readable representation of
// AwaitingFinalCheckpointsState.
func (s *AwaitingFinalCheckpointsState) String() string {
	return "AwaitingFinalCheckpointsState"
}

// IsTerminal returns false as AwaitingFinalCheckpointsState is not terminal.
func (s *AwaitingFinalCheckpointsState) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingFinalCheckpointsState as implementing the sealed
// State interface.
func (s *AwaitingFinalCheckpointsState) stateSealed() {}

// FinalizedState is the terminal success state.
type FinalizedState struct{}

// String returns a human-readable representation of FinalizedState.
func (s *FinalizedState) String() string {
	return "FinalizedState"
}

// IsTerminal returns true as FinalizedState is terminal.
func (s *FinalizedState) IsTerminal() bool {
	return true
}

// stateSealed marks FinalizedState as implementing the sealed State interface.
func (s *FinalizedState) stateSealed() {}

// FailedState is the terminal failure state.
type FailedState struct {
	// Reason is a human-readable reason for failure.
	Reason string
}

// String returns a human-readable representation of FailedState.
func (s *FailedState) String() string {
	return "FailedState"
}

// IsTerminal returns true as FailedState is terminal.
func (s *FailedState) IsTerminal() bool {
	return true
}

// stateSealed marks FailedState as implementing the sealed State interface.
func (s *FailedState) stateSealed() {}

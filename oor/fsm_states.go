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

// AwaitingInputsLockState indicates a submit request has been accepted and the
// FSM is waiting for the input lock side effect to complete.
type AwaitingInputsLockState struct {
	// Inputs are the VTXO outpoints spent by the checkpoint transactions.
	Inputs []wire.OutPoint

	// ArkPSBT is the submitted Ark tx PSBT.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs are the submitted checkpoint PSBTs.
	CheckpointPSBTs []*psbt.Packet
}

// String returns a human-readable representation of AwaitingInputsLockState.
func (s *AwaitingInputsLockState) String() string {
	return "AwaitingInputsLockState"
}

// IsTerminal returns false as AwaitingInputsLockState is not terminal.
func (s *AwaitingInputsLockState) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingInputsLockState as implementing the sealed State
// interface.
func (s *AwaitingInputsLockState) stateSealed() {}

// AwaitingSubmitValidationState indicates inputs are locked and the FSM is
// waiting for submit package validation.
type AwaitingSubmitValidationState struct {
	// Inputs are the VTXO outpoints spent by the checkpoint transactions.
	Inputs []wire.OutPoint

	// ArkPSBT is the submitted Ark tx PSBT.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs are the submitted checkpoint PSBTs.
	CheckpointPSBTs []*psbt.Packet
}

// String returns a human-readable representation of
// AwaitingSubmitValidationState.
func (s *AwaitingSubmitValidationState) String() string {
	return "AwaitingSubmitValidationState"
}

// IsTerminal returns false as AwaitingSubmitValidationState is not terminal.
func (s *AwaitingSubmitValidationState) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingSubmitValidationState as implementing the sealed
// State interface.
func (s *AwaitingSubmitValidationState) stateSealed() {}

// ValidatedState indicates validation has succeeded and the operator can sign.
type ValidatedState struct {
	// Inputs are the VTXO outpoints spent by the checkpoint transactions.
	Inputs []wire.OutPoint

	// ArkPSBT is the validated Ark tx PSBT.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs are the validated checkpoint PSBTs.
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
//
// Finalize requests are accepted in this state. The finalize request provides
// fully signed checkpoint PSBTs, and the next state validates that package.
type CoSignedState struct{}

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

// AwaitingFinalizeValidationState indicates finalize package validation is in
// progress.
type AwaitingFinalizeValidationState struct{}

// String returns a human-readable representation of
// AwaitingFinalizeValidationState.
func (s *AwaitingFinalizeValidationState) String() string {
	return "AwaitingFinalizeValidationState"
}

// IsTerminal returns false as AwaitingFinalizeValidationState is not terminal.
func (s *AwaitingFinalizeValidationState) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingFinalizeValidationState as implementing the sealed
// State interface.
func (s *AwaitingFinalizeValidationState) stateSealed() {}

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

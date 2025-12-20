package oor

import (
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
)

// State is a sealed interface for all states in the OOR client transfer FSM.
//
// States are protocol stages, not just implementation details. In particular,
// the outgoing transfer FSM is designed so that:
// - the submit package is deterministic (stable Ark txid); and
// - the client can resume by re-sending the outbox implied by the state.
type State interface {
	protofsm.State[Event, OutboxEvent, *Environment]
	stateSealed()
}

// Idle is the initial state of a client-side OOR transfer session.
type Idle struct{}

// String returns a human-readable representation of Idle.
func (s *Idle) String() string {
	return "Idle"
}

// IsTerminal returns false as Idle is not terminal.
func (s *Idle) IsTerminal() bool {
	return false
}

// stateSealed marks Idle as implementing the sealed State interface.
func (s *Idle) stateSealed() {}

// AwaitingSubmitAccepted is reached after the client has built a submit
// package and emitted an outbox request to send it to the server.
type AwaitingSubmitAccepted struct {
	// AwaitingSubmitAccepted is the crash-sensitive phase where a submit
	// request may have been sent, while the client has not yet observed
	// the server's co-sign response.

	// InputOutpoints are the VTXO outpoints consumed by this OOR session.
	//
	// The FSM carries these through to the terminal state so it can emit a
	// crash-resilient local persistence step after the server accepts
	// finalize.
	InputOutpoints []wire.OutPoint

	// ArkPSBT is the Ark tx PSBT for this session.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs are the checkpoint tx PSBTs for this session.
	CheckpointPSBTs []*psbt.Packet

	// TransferInputs are the vtxo descriptors and scripts needed later on
	// to sign the checkpoint PSBTs.
	TransferInputs []TransferInput
}

// String returns a human-readable representation of AwaitingSubmitAccepted.
func (s *AwaitingSubmitAccepted) String() string {
	return "AwaitingSubmitAccepted"
}

// IsTerminal returns false as AwaitingSubmitAccepted is not terminal.
func (s *AwaitingSubmitAccepted) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingSubmitAccepted as implementing the sealed State
// interface.
func (s *AwaitingSubmitAccepted) stateSealed() {}

// AwaitingCheckpointSignatures indicates the server has accepted and co-signed
// the package and the client must attach its own signature material to
// checkpoints.
type AwaitingCheckpointSignatures struct {
	// AwaitingCheckpointSignatures means the server has accepted submit and
	// returned operator co-signed checkpoint PSBTs. The next step is to
	// attach the client's signature material and build a finalize package.

	// SessionID is the stable session identifier (Ark txid).
	SessionID SessionID

	// InputOutpoints are the VTXO outpoints consumed by this OOR session.
	InputOutpoints []wire.OutPoint

	// ArkPSBT is the Ark tx PSBT, needed to finalize checkpoint metadata.
	ArkPSBT *psbt.Packet

	// CoSignedCheckpointPSBTs are the operator co-signed checkpoint PSBTs.
	CoSignedCheckpointPSBTs []*psbt.Packet

	// TransferInputs carry the client-side VTXO signing context.
	TransferInputs []TransferInput
}

// String returns a human-readable representation of
// AwaitingCheckpointSignatures.
func (s *AwaitingCheckpointSignatures) String() string {
	return "AwaitingCheckpointSignatures"
}

// IsTerminal returns false as AwaitingCheckpointSignatures is not terminal.
func (s *AwaitingCheckpointSignatures) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingCheckpointSignatures as implementing the sealed
// State interface.
func (s *AwaitingCheckpointSignatures) stateSealed() {}

// AwaitingFinalizeAccepted indicates the client has produced finalized
// checkpoint PSBTs and is waiting for the server to accept the finalize
// package.
type AwaitingFinalizeAccepted struct {
	// AwaitingFinalizeAccepted means the client has sent fully signed
	// checkpoint PSBTs back to the server and is waiting for ack.

	// SessionID is the stable session identifier (Ark txid).
	SessionID SessionID

	// InputOutpoints are the VTXO outpoints consumed by this OOR session.
	InputOutpoints []wire.OutPoint

	// ArkPSBT is the Ark tx PSBT.
	ArkPSBT *psbt.Packet

	// FinalCheckpointPSBTs are the final checkpoint PSBTs sent to the
	// server.
	FinalCheckpointPSBTs []*psbt.Packet
}

// String returns a human-readable representation of AwaitingFinalizeAccepted.
func (s *AwaitingFinalizeAccepted) String() string {
	return "AwaitingFinalizeAccepted"
}

// IsTerminal returns false as AwaitingFinalizeAccepted is not terminal.
func (s *AwaitingFinalizeAccepted) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingFinalizeAccepted as implementing the sealed State
// interface.
func (s *AwaitingFinalizeAccepted) stateSealed() {}

// AwaitingLocalVTXOUpdate indicates the server has accepted the finalize
// package and the client must update its local VTXO persistence state.
type AwaitingLocalVTXOUpdate struct {
	// AwaitingLocalVTXOUpdate means the off-chain OOR protocol has
	// completed successfully at the server boundary, but local wallet
	// state still needs to be updated to reflect spent inputs.

	// SessionID is the stable session identifier (Ark txid).
	SessionID SessionID

	// InputOutpoints are the VTXO outpoints consumed by this OOR session.
	InputOutpoints []wire.OutPoint
}

// String returns a human-readable representation of AwaitingLocalVTXOUpdate.
func (s *AwaitingLocalVTXOUpdate) String() string {
	return "AwaitingLocalVTXOUpdate"
}

// IsTerminal returns false as AwaitingLocalVTXOUpdate is not terminal.
func (s *AwaitingLocalVTXOUpdate) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingLocalVTXOUpdate as implementing the sealed State
// interface.
func (s *AwaitingLocalVTXOUpdate) stateSealed() {}

// Completed is the terminal success state for the OOR client transfer session.
type Completed struct{}

// String returns a human-readable representation of Completed.
func (s *Completed) String() string {
	return "Completed"
}

// IsTerminal returns true as Completed is terminal.
func (s *Completed) IsTerminal() bool {
	return true
}

// stateSealed marks Completed as implementing the sealed State interface.
func (s *Completed) stateSealed() {}

// Failed is the terminal failure state for the OOR client transfer session.
type Failed struct {
	Reason string
}

// String returns a human-readable representation of Failed.
func (s *Failed) String() string {
	return "Failed"
}

// IsTerminal returns true as Failed is terminal.
func (s *Failed) IsTerminal() bool {
	return true
}

// stateSealed marks Failed as implementing the sealed State interface.
func (s *Failed) stateSealed() {}

package oor

import (
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
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

// AwaitingArkSignatures indicates the submit package has been built and the
// client must attach Ark signatures before submit can be sent.
type AwaitingArkSignatures struct {
	// ArkPSBT is the canonical Ark tx PSBT.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs are unsigned checkpoint PSBTs for the submit phase.
	CheckpointPSBTs []*psbt.Packet

	// TransferInputs carry client-side signing context.
	TransferInputs []TransferInput

	// RecipientOutputs are the canonical non-anchor Ark outputs
	// plus optional semantic policy metadata for the created
	// VTXOs.
	RecipientOutputs []oortx.RecipientOutput

	// IdempotencyKey identifies the caller intent that created this
	// outgoing session, when provided.
	IdempotencyKey string
}

// String returns a human-readable representation of AwaitingArkSignatures.
func (s *AwaitingArkSignatures) String() string {
	return "AwaitingArkSignatures"
}

// IsTerminal returns false as AwaitingArkSignatures is not terminal.
func (s *AwaitingArkSignatures) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingArkSignatures as implementing the sealed State
// interface.
func (s *AwaitingArkSignatures) stateSealed() {}

// AwaitingSubmitAccepted is reached after the client has built a submit
// package and emitted an outbox request to send it to the server.
type AwaitingSubmitAccepted struct {
	// AwaitingSubmitAccepted is the crash-sensitive phase where a submit
	// request may have been sent, while the client has not yet observed
	// the server's co-sign response.

	// ArkPSBT is the Ark tx PSBT for this session.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs are the checkpoint tx PSBTs for this session.
	CheckpointPSBTs []*psbt.Packet

	// TransferInputs carry the VTXO descriptors and scripts needed to
	// sign checkpoint PSBTs at the co-sign step.
	//
	// These are not used by the FSM's transition logic. They are threaded
	// through the state so the FSM can emit complete outbox events (which
	// need the signing context) and so checkpoint snapshots capture them
	// for crash-resume.
	TransferInputs []TransferInput

	// RecipientOutputs are the canonical non-anchor Ark
	// outputs plus optional semantic policy metadata for the
	// created VTXOs.
	RecipientOutputs []oortx.RecipientOutput

	// IdempotencyKey identifies the caller intent that created this
	// outgoing session, when provided.
	IdempotencyKey string
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

	// ArkPSBT is the Ark tx PSBT, needed to finalize checkpoint metadata.
	ArkPSBT *psbt.Packet

	// CoSignedCheckpointPSBTs are the operator co-signed checkpoint PSBTs.
	CoSignedCheckpointPSBTs []*psbt.Packet

	// TransferInputs carry the client-side VTXO signing context needed
	// for the checkpoint signing outbox event.
	//
	// See AwaitingSubmitAccepted.TransferInputs for rationale on why
	// this is carried on the FSM state.
	TransferInputs []TransferInput

	// IdempotencyKey identifies the caller intent that created this
	// outgoing session, when provided.
	IdempotencyKey string
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

	// ArkPSBT is the Ark tx PSBT.
	ArkPSBT *psbt.Packet

	// FinalCheckpointPSBTs are the final checkpoint PSBTs sent to the
	// server.
	//
	// These are persisted so resume/unilateral-exit paths can reconstruct
	// checkpoint lineage without depending on a fresh server response.
	FinalCheckpointPSBTs []*psbt.Packet

	// TransferInputs carry the VTXO descriptors consumed by this session.
	// The local bookkeeping phase derives spent outpoints from these.
	TransferInputs []TransferInput

	// IdempotencyKey identifies the caller intent that created this
	// outgoing session, when provided.
	IdempotencyKey string
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

	// TransferInputs carry the VTXO descriptors consumed by this session.
	// The local persistence step derives spent outpoints from these.
	TransferInputs []TransferInput

	// IdempotencyKey identifies the caller intent that created this
	// outgoing session, when provided.
	IdempotencyKey string
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
type Completed struct {
	// IdempotencyKey identifies the caller intent that created this
	// outgoing session, when provided.
	IdempotencyKey string
}

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
	// Reason is a human-readable failure reason intended for logs and
	// tests.
	Reason string

	// IdempotencyKey identifies the caller intent that created this
	// outgoing session, when provided.
	IdempotencyKey string
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

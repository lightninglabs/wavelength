package oor

import (
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
)

// OutboxEvent is a sealed interface for all side-effect requests emitted by the
// OOR session FSM.
//
// The coordinator actor should execute these requests and translate results
// back into inbox events.
//
// Package vocabulary:
//   - submit package: Ark PSBT + checkpoint PSBTs before finalize signatures,
//   - finalize package: Ark PSBT + checkpoint PSBTs with client finalize
//     signatures attached.
//
// We keep structural and policy/state-aware validation at this boundary so the
// FSM orchestration remains deterministic and all retryable side effects follow
// one pattern.
type OutboxEvent interface {
	// OutboxType returns a stable string identifier for this outbox event.
	OutboxType() string

	// outboxSealed marks this interface as sealed.
	outboxSealed()
}

// LockInputsReq requests locking VTXO inputs for a session.
type LockInputsReq struct {
	// Inputs are the VTXO outpoints to lock.
	Inputs []wire.OutPoint
}

// OutboxType returns the type of this outbox event.
func (e *LockInputsReq) OutboxType() string {
	return "LockInputsReq"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (e *LockInputsReq) outboxSealed() {}

// UnlockInputsReq requests unlocking VTXO inputs for a session.
type UnlockInputsReq struct {
	// Inputs are the VTXO outpoints to unlock.
	Inputs []wire.OutPoint
}

// OutboxType returns the type of this outbox event.
func (e *UnlockInputsReq) OutboxType() string {
	return "UnlockInputsReq"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (e *UnlockInputsReq) outboxSealed() {}

// ValidateSubmitReq asks the validator to validate a submit package.
type ValidateSubmitReq struct {
	// ArkPSBT is the Ark tx PSBT.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs are the checkpoint tx PSBTs.
	CheckpointPSBTs []*psbt.Packet
}

// OutboxType returns the type of this outbox event.
func (e *ValidateSubmitReq) OutboxType() string {
	return "ValidateSubmitReq"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (e *ValidateSubmitReq) outboxSealed() {}

// CoSignReq asks the signing subsystem to co-sign the validated package.
type CoSignReq struct{}

// OutboxType returns the type of this outbox event.
func (e *CoSignReq) OutboxType() string {
	return "CoSignReq"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (e *CoSignReq) outboxSealed() {}

// ValidateFinalizeReq asks the validator to validate a finalize package.
type ValidateFinalizeReq struct {
	// FinalCheckpointPSBTs are checkpoint txs fully signed by the client.
	FinalCheckpointPSBTs []*psbt.Packet
}

// OutboxType returns the type of this outbox event.
func (e *ValidateFinalizeReq) OutboxType() string {
	return "ValidateFinalizeReq"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (e *ValidateFinalizeReq) outboxSealed() {}

// FinalizeReq requests atomic VTXO set updates for a finalized transfer.
type FinalizeReq struct{}

// OutboxType returns the type of this outbox event.
func (e *FinalizeReq) OutboxType() string {
	return "FinalizeReq"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (e *FinalizeReq) outboxSealed() {}

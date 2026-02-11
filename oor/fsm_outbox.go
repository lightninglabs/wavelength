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

// CoSignReq asks the signing subsystem to co-sign the package.
type CoSignReq struct{}

// OutboxType returns the type of this outbox event.
func (e *CoSignReq) OutboxType() string {
	return "CoSignReq"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (e *CoSignReq) outboxSealed() {}

// ValidateFinalizeReq asks the validator to validate a finalize package.
type ValidateFinalizeReq struct {
	// ArkPSBT is the canonical Ark tx PSBT for this session.
	ArkPSBT *psbt.Packet

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

package oor

import (
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
)

// OutboxEvent is a sealed interface for all side-effect requests emitted by the
// OOR session FSM.
//
// The coordinator actor should execute these requests and translate results
// back into inbox events.
//
// This follows an explicit outbox-events pattern:
//   - the FSM itself remains (mostly) pure and deterministic; and
//   - all I/O (locks, validation, signing, persistence, notifications) happens
//     behind this boundary.
//
// This makes the core protocol logic testable and prepares for a future
// durable actor runtime where outbox requests can be persisted and retried.
type OutboxEvent interface {
	// OutboxType returns a stable string identifier for this outbox event.
	OutboxType() string

	// outboxSealed marks this interface as sealed.
	outboxSealed()
}

// Package vocabulary used by outbox requests:
//
// - Submit package: Ark PSBT + checkpoint PSBTs before finalize signatures.
//   The server validates structure and policy against authoritative state, then
//   moves the session to co-signing.
//
// - Finalize package: Ark PSBT + finalized checkpoint PSBTs with client
//   signature material attached. The server validates finalize bindings and
//   atomically applies VTXO set updates.
//
// Validation is intentionally modeled as outbox work so the FSM keeps one
// consistent side-effect boundary for locking, validation, signing, and durable
// persistence.

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

	// VTXOSigningDescriptors carry authoritative per-input owner and policy
	// metadata used for server-side checkpoint policy validation.
	VTXOSigningDescriptors []VTXOSigningDescriptor

	// CheckpointPolicy is the operator policy used to derive checkpoint
	// output scripts.
	CheckpointPolicy scripts.CheckpointPolicy
}

// OutboxType returns the type of this outbox event.
func (e *ValidateSubmitReq) OutboxType() string {
	return "ValidateSubmitReq"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (e *ValidateSubmitReq) outboxSealed() {}

// CoSignReq asks the signing subsystem to co-sign the validated package.
//
// Co-sign is the most sensitive boundary for durability: once the operator has
// attached its signature material, submit retries must be answered idempotently
// by returning the exact same co-signed PSBT bytes.
type CoSignReq struct {
	// Inputs are the VTXO outpoints to transition to in-flight at the
	// point-of-no-return.
	Inputs []wire.OutPoint

	// ArkPSBT is the validated Ark tx PSBT.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs are the validated checkpoint PSBTs.
	CheckpointPSBTs []*psbt.Packet

	// VTXOSigningDescriptors carry the per-input signing metadata needed
	// for operator co-signing.
	VTXOSigningDescriptors []VTXOSigningDescriptor
}

// OutboxType returns the type of this outbox event.
func (e *CoSignReq) OutboxType() string {
	return "CoSignReq"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (e *CoSignReq) outboxSealed() {}

// ValidateFinalizeReq asks the validator to validate a finalize package.
//
// Finalize package vocabulary in outbox terms:
//   - ArkPSBT is the canonical session Ark transaction.
//   - FinalCheckpointPSBTs are checkpoint txs with finalize signatures from the
//     client.
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
type FinalizeReq struct {
	// ArkPSBT is the canonical Ark tx PSBT. Its non-anchor outputs are
	// materialized as new VTXOs in v0.
	ArkPSBT *psbt.Packet

	// FinalCheckpointPSBTs are checkpoint txs fully signed by the client.
	FinalCheckpointPSBTs []*psbt.Packet

	// Inputs are the VTXO outpoints consumed by this finalized transfer.
	Inputs []wire.OutPoint
}

// OutboxType returns the type of this outbox event.
func (e *FinalizeReq) OutboxType() string {
	return "FinalizeReq"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (e *FinalizeReq) outboxSealed() {}

// NotifyRecipientsReq requests a durable notification for transfer recipients.
type NotifyRecipientsReq struct {
	// ArkPSBT is the canonical Ark tx PSBT used to derive recipient
	// outputs.
	ArkPSBT *psbt.Packet
}

// OutboxType returns the type of this outbox event.
func (e *NotifyRecipientsReq) OutboxType() string {
	return "NotifyRecipientsReq"
}

// outboxSealed marks this as implementing the sealed OutboxEvent interface.
func (e *NotifyRecipientsReq) outboxSealed() {}

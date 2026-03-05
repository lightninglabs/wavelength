package oor

import (
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// Event is a sealed interface for all events that can drive an OOR session FSM.
//
// Events are split into:
//   - external requests (submit/finalize) that enter the coordinator actor, and
//   - internal events produced by outbox processing (locks acquired, signing
//     results, finalize validation results).
//
// The intent is to make the session FSM deterministic: the only "inputs" are
// these events, and the only "outputs" are outbox requests (plus the next
// state). This structure keeps the protocol semantics testable without
// requiring a real wallet, database, or RPC service.
type Event interface {
	// EventType returns a stable string identifier for this event.
	EventType() string

	// eventSealed marks this interface as sealed.
	eventSealed()
}

// SubmitRequestedEvent begins an OOR transfer session.
//
// It corresponds to the "submit" operation at the coordinator boundary. The
// actor translates an inbound request into this event, which kicks off locking
// and validation.
type SubmitRequestedEvent struct {
	// ArkPSBT is the transfer intent transaction.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs are the per-input checkpoint transactions.
	CheckpointPSBTs []*psbt.Packet

	// VTXOSigningDescriptors are the input VTXO signing descriptors for
	// operator co-signing.
	VTXOSigningDescriptors []VTXOSigningDescriptor
}

// EventType returns the type of this event.
func (e *SubmitRequestedEvent) EventType() string {
	return "SubmitRequestedEvent"
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *SubmitRequestedEvent) eventSealed() {}

// InputsLockSucceededEvent indicates the VTXO inputs are locked for this
// session.
type InputsLockSucceededEvent struct{}

// EventType returns the type of this event.
func (e *InputsLockSucceededEvent) EventType() string {
	return "InputsLockSucceededEvent"
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *InputsLockSucceededEvent) eventSealed() {}

// InputsLockFailedEvent indicates input locking failed.
type InputsLockFailedEvent struct {
	// Reason is a human-readable error string for logs/tests.
	Reason string
}

// EventType returns the type of this event.
func (e *InputsLockFailedEvent) EventType() string {
	return "InputsLockFailedEvent"
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *InputsLockFailedEvent) eventSealed() {}

// LockFailedEvent indicates input locking failed.
//
// This is a pre-point-of-no-return failure. No unlock request is required if
// the lock operation is atomic (all-or-nothing), but the coordinator may still
// choose to issue an idempotent unlock as a best-effort cleanup.
type LockFailedEvent struct {
	// Reason is a human-readable error string for logs/tests.
	Reason string
}

// EventType returns the type of this event.
func (e *LockFailedEvent) EventType() string {
	return "LockFailedEvent"
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *LockFailedEvent) eventSealed() {}

// SubmitValidatedEvent indicates submit package validation succeeded.
//
// It is emitted after ValidateSubmitReq succeeds. We keep the validated txid
// here for logging and future auditing, but the current v0 coordinator derives
// the session id from the Ark txid.
type SubmitValidatedEvent struct {
	// ArkTxid is the computed session identifier.
	ArkTxid chainhash.Hash
}

// EventType returns the type of this event.
func (e *SubmitValidatedEvent) EventType() string {
	return "SubmitValidatedEvent"
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *SubmitValidatedEvent) eventSealed() {}

// SubmitFailedEvent indicates submit package validation failed.
type SubmitFailedEvent struct {
	// Reason is a human-readable error string for logs/tests.
	Reason string
}

// EventType returns the type of this event.
func (e *SubmitFailedEvent) EventType() string {
	return "SubmitFailedEvent"
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *SubmitFailedEvent) eventSealed() {}

// OperatorSignedEvent indicates the operator has co-signed the package.
type OperatorSignedEvent struct{}

// EventType returns the type of this event.
func (e *OperatorSignedEvent) EventType() string {
	return "OperatorSignedEvent"
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *OperatorSignedEvent) eventSealed() {}

// SignFailedEvent indicates the operator signing step failed.
type SignFailedEvent struct {
	// Reason is a human-readable error string for logs/tests.
	Reason string
}

// EventType returns the type of this event.
func (e *SignFailedEvent) EventType() string {
	return "SignFailedEvent"
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *SignFailedEvent) eventSealed() {}

// FinalizeRequestedEvent begins the finalize phase for an existing session.
//
// It corresponds to the "finalize" operation at the coordinator boundary. The
// server receives fully signed checkpoint PSBTs from the client and validates
// them using tap tree metadata from the Ark PSBT.
type FinalizeRequestedEvent struct {
	// FinalCheckpointPSBTs are checkpoint txs fully signed by the client.
	FinalCheckpointPSBTs []*psbt.Packet
}

// EventType returns the type of this event.
func (e *FinalizeRequestedEvent) EventType() string {
	return "FinalizeRequestedEvent"
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *FinalizeRequestedEvent) eventSealed() {}

// FinalizeValidatedEvent indicates finalize package validation succeeded.
type FinalizeValidatedEvent struct{}

// EventType returns the type of this event.
func (e *FinalizeValidatedEvent) EventType() string {
	return "FinalizeValidatedEvent"
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *FinalizeValidatedEvent) eventSealed() {}

// FinalizeFailedEvent indicates finalize validation failed.
type FinalizeFailedEvent struct {
	// Reason is a human-readable error string for logs/tests.
	Reason string
}

// EventType returns the type of this event.
func (e *FinalizeFailedEvent) EventType() string {
	return "FinalizeFailedEvent"
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *FinalizeFailedEvent) eventSealed() {}

// FinalizeSucceededEvent indicates VTXO set finalization succeeded.
//
// The event carries the finalized checkpoint PSBTs so that downstream
// states (AwaitingRecipientsNotifyState) can thread them into the
// NotifyRecipientsReq without requiring a SessionStore lookup.
type FinalizeSucceededEvent struct {
	// FinalCheckpointPSBTs are the fully signed checkpoint PSBTs from
	// the finalize phase, needed for recipient notification delivery.
	FinalCheckpointPSBTs []*psbt.Packet
}

// EventType returns the type of this event.
func (e *FinalizeSucceededEvent) EventType() string {
	return "FinalizeSucceededEvent"
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *FinalizeSucceededEvent) eventSealed() {}

// NotifyRecipientsSucceededEvent indicates recipient notification persistence
// completed successfully.
type NotifyRecipientsSucceededEvent struct{}

// EventType returns the type of this event.
func (e *NotifyRecipientsSucceededEvent) EventType() string {
	return "NotifyRecipientsSucceededEvent"
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *NotifyRecipientsSucceededEvent) eventSealed() {}

// NotifyRecipientsFailedEvent indicates recipient notification persistence
// failed.
type NotifyRecipientsFailedEvent struct {
	// Reason is a human-readable error string for logs/tests.
	Reason string
}

// EventType returns the type of this event.
func (e *NotifyRecipientsFailedEvent) EventType() string {
	return "NotifyRecipientsFailedEvent"
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *NotifyRecipientsFailedEvent) eventSealed() {}

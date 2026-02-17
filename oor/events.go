package oor

import (
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
)

// Event is a sealed interface for all events that can drive the OOR transfer
// client FSM.
//
// The outgoing transfer FSM is intentionally deterministic:
// - events are the only inputs to transitions; and
// - outbox requests are the only way to do I/O (RPC, signing, timers).
//
// This is a foundational design choice for mobile safety: after a crash, the
// application can restore a persisted snapshot and re-drive the outbox implied
// by the current state.
type Event interface {
	eventSealed()
}

// StartTransferEvent requests starting an OOR transfer by building a submit
// package (checkpoint PSBTs + Ark PSBT).
type StartTransferEvent struct {
	// VTXOInputs is the set of client VTXOs to spend for this transfer.
	VTXOInputs []TransferInput

	// RecipientOutputs are the Ark tx outputs to produce.
	RecipientOutputs []oortx.RecipientOutput

	// Policy defines the checkpoint output tap tree policy.
	Policy scripts.CheckpointPolicy
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *StartTransferEvent) eventSealed() {}

// ArkSignedEvent is emitted after the client signs the Ark PSBT.
type ArkSignedEvent struct {
	// ArkPSBT is the signed Ark PSBT.
	ArkPSBT *psbt.Packet
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *ArkSignedEvent) eventSealed() {}

// SubmitAcceptedEvent is emitted when the server accepts the submit package and
// co-signs checkpoint PSBTs (the point-of-no-return for the outgoing flow).
//
// After this event, the client must be able to resume and obtain the same
// co-signed checkpoint artifacts even if the submit response was lost.
type SubmitAcceptedEvent struct {
	// SessionID is the session identifier (Ark txid).
	SessionID SessionID

	// ArkPSBT is the canonical session artifact for consistency checks and
	// stateless finalize retries. The operator does not add Ark signature
	// material in submit-accepted.
	// Operator co-signing applies to checkpoints.
	ArkPSBT *psbt.Packet

	// CoSignedCheckpointPSBTs are checkpoint PSBTs co-signed by the
	// operator.
	CoSignedCheckpointPSBTs []*psbt.Packet
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *SubmitAcceptedEvent) eventSealed() {}

// CheckpointsSignedEvent is emitted after the client has attached signature
// material to the co-signed checkpoint PSBTs.
type CheckpointsSignedEvent struct {
	// FinalCheckpointPSBTs are the finalized checkpoint PSBTs.
	FinalCheckpointPSBTs []*psbt.Packet
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *CheckpointsSignedEvent) eventSealed() {}

// FinalizeAcceptedEvent is emitted once the server has accepted the finalize
// package and updated its VTXO set.
type FinalizeAcceptedEvent struct{}

// eventSealed marks this as implementing the sealed Event interface.
func (e *FinalizeAcceptedEvent) eventSealed() {}

// InputsMarkedSpentEvent is emitted once the local wallet state has been
// updated to reflect that the input VTXOs were spent by this OOR session.
//
// This is an off-chain bookkeeping step: the OOR protocol does not imply any
// on-chain confirmation in the happy path.
type InputsMarkedSpentEvent struct{}

// eventSealed marks this as implementing the sealed Event interface.
func (e *InputsMarkedSpentEvent) eventSealed() {}

// RetryDueEvent indicates that a previously requested retry backoff timer has
// elapsed and the session can resume from the stored retry snapshot.
type RetryDueEvent struct{}

// eventSealed marks this as implementing the sealed Event interface.
func (e *RetryDueEvent) eventSealed() {}

// FailEvent forces the session to enter a terminal failure state.
type FailEvent struct {
	Reason string
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *FailEvent) eventSealed() {}

// IncomingTransferEvent notifies the client about an incoming OOR transfer.
//
// This event is intended to be delivered by some higher layer (RPC push,
// polling, or push-notification wakeup) once the server has accepted and
// finalized the transfer.
//
// NOTE: This event is expected to be delivered only for transfers where the
// server believes the client is a recipient. The client-side receive FSM still
// performs structural/canonical validation, and the application/wallet layer is
// responsible for filtering/materializing only the outputs that belong to the
// local wallet.
//
// The incoming transfer FSM is intentionally separate from the outgoing FSM so
// applications can handle notifications and acknowledgements independently of
// initiating transfers.
type IncomingTransferEvent struct {
	// SessionID is the stable v0 session identifier (Ark txid).
	SessionID SessionID

	// ArkPSBT is the canonical Ark tx PSBT for this transfer.
	ArkPSBT *psbt.Packet

	// FinalCheckpointPSBTs are the finalized checkpoint packages associated
	// with the Ark PSBT.
	// These can be used by the materialization boundary to derive parent
	// lineage and future unroll proofs.
	FinalCheckpointPSBTs []*psbt.Packet
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *IncomingTransferEvent) eventSealed() {}

// IncomingHandledEvent indicates the application/wallet has processed the
// incoming transfer notification.
type IncomingHandledEvent struct{}

// eventSealed marks this as implementing the sealed Event interface.
func (e *IncomingHandledEvent) eventSealed() {}

package oor

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
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
	// StartTransferEvent is the one-and-only "kick off a session" event.
	// After this, the FSM should have a deterministic submit package so
	// retries produce a stable session id (Ark txid).

	// CheckpointInputs is the set of VTXO inputs to convert into
	// checkpoints.
	CheckpointInputs []TransferInput

	// RecipientOutputs are the Ark tx outputs to produce.
	RecipientOutputs []oortx.RecipientOutput

	// Policy defines the checkpoint output tap tree policy.
	Policy scripts.CheckpointPolicy

	// PrebuiltArkPSBT is an optional prebuilt Ark PSBT. If provided, the
	// FSM should use it instead of rebuilding the submit package.
	PrebuiltArkPSBT *psbt.Packet

	// PrebuiltCheckpointPSBTs are optional prebuilt checkpoint PSBTs. If
	// provided together with PrebuiltArkPSBT, the FSM should use them
	// instead of rebuilding the submit package.
	PrebuiltCheckpointPSBTs []*psbt.Packet

	// AnchorAmount is reserved for future extensions. v0 uses a fixed P2A
	// anchor output with 0 sats.
	AnchorAmount btcutil.Amount
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

// SubmitAcceptedEvent is emitted when the server accepts and co-signs the
// submit package.
type SubmitAcceptedEvent struct {
	// SubmitAcceptedEvent is the client's view of the "point-of-no-return".
	//
	// Once the operator co-signs the checkpoint PSBTs, the client must be
	// able to resume and obtain the same co-signed artifacts even if it
	// did not receive the response due to a crash or transport loss.

	// SessionID is the session identifier (Ark txid).
	SessionID SessionID

	// ArkPSBT is echoed back for convenience and to allow stateless
	// finalization (tap tree metadata is bound to it).
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

// FailEvent forces the session to enter a terminal failure state.
type FailEvent struct {
	Reason string
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *FailEvent) eventSealed() {}

// OutboxErrorEvent notifies the FSM that a side effect request failed.
//
// The actor boundary is responsible for deciding whether the error is
// retryable and for populating RetryAfter appropriately.
//
// Encoding retry semantics as an event (rather than returning a special Go
// error) keeps the FSM deterministic and keeps all "should we retry" policy in
// one place: the state transition logic.
type OutboxErrorEvent struct {
	OutboxType  string
	Retryable   bool
	RetryAfter  time.Duration
	ErrorReason string
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *OutboxErrorEvent) eventSealed() {}

// RetryDueEvent indicates a previously scheduled retry is due.
type RetryDueEvent struct{}

// eventSealed marks this as implementing the sealed Event interface.
func (e *RetryDueEvent) eventSealed() {}

// IncomingTransferEvent notifies the client about an incoming OOR transfer.
//
// This event is intended to be delivered by some higher layer (RPC push,
// polling, or push-notification wakeup) once the server has accepted and
// finalized the transfer.
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
	// with the Ark PSBT. These can be used by the materialization boundary to
	// derive parent lineage and future unroll proofs.
	FinalCheckpointPSBTs []*psbt.Packet
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *IncomingTransferEvent) eventSealed() {}

// IncomingHandledEvent indicates the application/wallet has processed the
// incoming transfer notification.
type IncomingHandledEvent struct{}

// eventSealed marks this as implementing the sealed Event interface.
func (e *IncomingHandledEvent) eventSealed() {}

// IncomingAckSentEvent indicates the server ack has been sent for this
// incoming transfer session.
type IncomingAckSentEvent struct{}

// eventSealed marks this as implementing the sealed Event interface.
func (e *IncomingAckSentEvent) eventSealed() {}

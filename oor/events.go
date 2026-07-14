package oor

import (
	"time"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/vtxo"
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
	Policy arkscript.CheckpointPolicy

	// IdempotencyKey identifies this caller intent across crashes and
	// retries. Empty preserves the historical deterministic-session
	// behavior.
	IdempotencyKey string

	// PreparedSubmit is an optional asset-committed Bitcoin graph produced
	// before entering the deterministic FSM. Nil preserves the ordinary
	// Bitcoin-only builder path.
	PreparedSubmit *PreparedSubmitPackage
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

	// ArkPSBT is the operator-co-signed Ark PSBT returned in the
	// submit-success response. Clients persist this exact artifact so
	// unilateral recovery can reconstruct a spendable OOR transaction
	// without contacting the operator.
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

// RetryDueEvent indicates that a previously scheduled retry timer fired.
type RetryDueEvent struct{}

// eventSealed marks this as implementing the sealed Event interface.
func (e *RetryDueEvent) eventSealed() {}

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

	// AncestorPackages are finalized OOR packages that produced OOR inputs
	// consumed by this transfer. They are persisted during materialization
	// so chained receives can unroll without owning intermediate VTXOs.
	AncestorPackages []PackageArtifact

	// Recipients are the Ark outputs plus optional semantic policy metadata
	// supplied by the recipient event. If empty, receivers fall back to
	// structural extraction from ArkPSBT for compatibility with older
	// servers.
	Recipients []ArkRecipientOutput

	// TaprootAssetTransfer is the optional immutable container of sealed
	// checkpoint and Ark transition packages for this incoming transfer.
	TaprootAssetTransfer *oortx.TaprootAssetTransfer
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *IncomingTransferEvent) eventSealed() {}

// IncomingHandledEvent indicates the application/wallet has processed the
// incoming transfer notification. When VTXOs are materialized, the
// descriptors are attached so the actor can forward them to the VTXO
// manager for actor activation.
type IncomingHandledEvent struct {
	// MaterializedVTXOs contains descriptors that were durably
	// persisted during materialization. The OOR actor forwards these
	// to the VTXO manager so it can spawn monitoring actors.
	MaterializedVTXOs []*vtxo.Descriptor

	// MaterializedOutpoints identifies the persisted VTXOs by outpoint so
	// durable callback delivery can round-trip the event without embedding
	// the full descriptor payload in the mailbox record.
	MaterializedOutpoints []wire.OutPoint
}

// eventSealed marks this as implementing the sealed Event interface.
func (e *IncomingHandledEvent) eventSealed() {}

// IncomingAckSentEvent indicates the server ack has been sent for this
// incoming transfer session.
type IncomingAckSentEvent struct{}

// eventSealed marks this as implementing the sealed Event interface.
func (e *IncomingAckSentEvent) eventSealed() {}

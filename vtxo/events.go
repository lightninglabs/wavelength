package vtxo

import (
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/round"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// VTXOEvent embeds actormsg.VTXOActorMsg for all events that can be processed
// by the VTXO state machine. Message types are defined in the round package
// and implement the actormsg.VTXOActorMsg marker interface.
type VTXOEvent interface {
	actormsg.VTXOActorMsg
}

// Type aliases for VTXO events. These point to the canonical definitions in
// the round package, providing ergonomic access without the round. prefix.
type (
	// BlockEpochEvent is received when a new block is connected.
	BlockEpochEvent = round.BlockEpochEvent

	// ForfeitRequestEvent is received from the round actor when this VTXO
	// is being forfeited as part of a batch swap.
	ForfeitRequestEvent = round.ForfeitRequestEvent

	// ForfeitConfirmedEvent indicates the new commitment transaction has
	// been confirmed on-chain.
	ForfeitConfirmedEvent = round.ForfeitConfirmedEvent

	// ForfeitSignedEvent indicates the forfeit transaction has been signed
	// and submitted to the round.
	ForfeitSignedEvent = round.ForfeitSignedEvent

	// VTXOFailedEvent indicates an error occurred during VTXO processing.
	VTXOFailedEvent = round.VTXOFailedEvent

	// ResumeVTXOEvent is sent when resuming a VTXO actor from persisted
	// state.
	ResumeVTXOEvent = round.ResumeVTXOEvent

	// PendingForfeitEvent is sent when the round actor has committed this
	// VTXO to cooperative consumption and the VTXO should become
	// unavailable for other uses while awaiting concrete forfeit details.
	PendingForfeitEvent = round.PendingForfeitEvent

	// SpendReserveEvent claims a VTXO for an out-of-round (OOR) spend.
	SpendReserveEvent = round.SpendReserveEvent

	// SpendReleasedEvent releases a VTXO from spend reservation back to
	// LiveState.
	SpendReleasedEvent = round.SpendReleasedEvent

	// SpendCompletedEvent marks a VTXO as fully spent via an OOR
	// transaction.
	SpendCompletedEvent = round.SpendCompletedEvent

	// ForfeitReleasedEvent releases a VTXO from pending forfeit back to
	// LiveState.
	ForfeitReleasedEvent = round.ForfeitReleasedEvent
)

// ForceUnrollEvent is sent to a VTXO actor when a unilateral exit is
// requested (manual RPC, fraud spend, or vHTLC recovery). The VTXO actor
// transitions to UnilateralExitState and emits ExpiringNotification through
// the chain resolver seam, converging with the automatic critical-expiry
// path. The trigger and exit-policy identity ride along so the chain
// resolver bridge can admit the registry job under the right policy.
type ForceUnrollEvent struct {
	actor.BaseMessage

	// Reason explains why the unroll was requested.
	Reason string

	// Trigger identifies why the unroll was requested. The zero value
	// admits as critical expiry.
	Trigger actormsg.UnrollTrigger

	// ExitPolicy carries a non-standard exit-spend policy identity to
	// persist for this target. None selects the standard VTXO timeout
	// policy.
	ExitPolicy fn.Option[actormsg.ExitPolicy]
}

// VTXOActorMsg implements actormsg.VTXOActorMsg marker interface.
func (e *ForceUnrollEvent) VTXOActorMsg() {}

// MessageType returns the message type for logging.
func (e *ForceUnrollEvent) MessageType() string {
	return "ForceUnrollEvent"
}

// ExitFailedEvent is delivered to a VTXO actor in UnilateralExitState when
// the downstream unroll job terminated as a clean failure that left no
// on-chain footprint (no proof or sweep transaction was broadcast). The
// VTXO is rolled back to LiveState so the wallet's view re-converges with
// the operator's, which still considers the VTXO live. This is the
// recovery half of the darepo-client#602 fix.
type ExitFailedEvent struct {
	actor.BaseMessage

	// Reason explains why the unroll job failed, for logging and the
	// restored VTXO's audit trail.
	Reason string
}

// VTXOActorMsg implements actormsg.VTXOActorMsg marker interface.
func (e *ExitFailedEvent) VTXOActorMsg() {}

// MessageType returns the message type for logging.
func (e *ExitFailedEvent) MessageType() string {
	return "ExitFailedEvent"
}

// ExitConfirmedEvent is delivered to a VTXO actor in UnilateralExitState
// when the unilateral exit has been swept and confirmed on-chain. The VTXO
// is retired to the terminal SpentState and the actor is reaped. Unlike the
// original terminal UnilateralExitState, reaping now happens on this
// terminal on-chain event rather than on the user's intent to exit.
type ExitConfirmedEvent struct {
	actor.BaseMessage
}

// VTXOActorMsg implements actormsg.VTXOActorMsg marker interface.
func (e *ExitConfirmedEvent) VTXOActorMsg() {}

// MessageType returns the message type for logging.
func (e *ExitConfirmedEvent) MessageType() string {
	return "ExitConfirmedEvent"
}

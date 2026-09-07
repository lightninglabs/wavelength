package vtxo

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/round"
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

// criticalRefreshEvent tells LiveState or PendingForfeitState that the actor
// established that the automatic unilateral path is not currently viable.
// LiveState starts cooperative refresh; PendingForfeitState keeps waiting for
// that round. It is actor-local: external block notifications remain
// BlockEpochEvent, and a funded or unassessed critical VTXO follows the
// existing unilateral-exit transition.
type criticalRefreshEvent struct {
	actor.BaseMessage

	// Height is the critical block height used for the durable automatic
	// refresh reservation and repeated funding assessment.
	Height int32
}

// VTXOActorMsg implements actormsg.VTXOActorMsg.
func (e *criticalRefreshEvent) VTXOActorMsg() {}

// MessageType returns the message type used by actor diagnostics.
func (e *criticalRefreshEvent) MessageType() string {
	return "criticalRefreshEvent"
}

// CohortRefreshEvent asks an eligible sibling VTXO to join an automatic
// refresh already triggered by another VTXO from the same batch. The manager
// sends these requests with bounded Ask backpressure before forwarding the
// initiating request to the round actor.
type CohortRefreshEvent struct {
	actor.BaseMessage

	// Height is the chain height that triggered the cohort leader.
	Height int32

	// BatchExpiry is the shared absolute expiry used as a defensive
	// cross-check before the sibling leaves LiveState.
	BatchExpiry int32

	// LeaderOutpoint is the coordination generation token. A best-effort
	// rollback only applies if this leader reserved the sibling.
	LeaderOutpoint wire.OutPoint

	// Generation uniquely identifies this leader's coordination attempt. It
	// prevents a delayed rollback from an earlier retry releasing a newer
	// reservation led by the same outpoint.
	Generation uint64

	// OperatorKey is the leader's join-time operator-key snapshot. Every
	// sibling uses the same key so one cohort cannot mix operator terms.
	OperatorKey *btcec.PublicKey
}

// VTXOActorMsg implements actormsg.VTXOActorMsg.
func (e *CohortRefreshEvent) VTXOActorMsg() {}

// MessageType returns the message type for logging.
func (e *CohortRefreshEvent) MessageType() string {
	return "CohortRefreshEvent"
}

// CohortRefreshReleaseEvent rolls back only the automatic reservation owned
// by the named cohort leader. It cannot release a manual or competing round's
// pending forfeit if the store snapshot raced another admission.
type CohortRefreshReleaseEvent struct {
	actor.BaseMessage

	// LeaderOutpoint identifies the cohort whose reservation is released.
	LeaderOutpoint wire.OutPoint

	// Generation identifies the exact coordination attempt to release.
	Generation uint64
}

// VTXOActorMsg implements actormsg.VTXOActorMsg.
func (e *CohortRefreshReleaseEvent) VTXOActorMsg() {}

// MessageType returns the message type for logging.
func (e *CohortRefreshReleaseEvent) MessageType() string {
	return "CohortRefreshReleaseEvent"
}

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
// recovery half of the wavelength#602 fix.
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

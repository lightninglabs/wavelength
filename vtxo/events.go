package vtxo

import (
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/round"
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

	// RefreshAcknowledgedEvent is received when the round actor
	// acknowledges a refresh request.
	RefreshAcknowledgedEvent = round.RefreshAcknowledgedEvent

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

	// TriggerRefreshEvent is sent to manually trigger cooperative
	// forfeiture.
	TriggerRefreshEvent = round.TriggerRefreshEvent

	// TriggerLeaveEvent is sent to manually trigger a leave
	// (offboard).
	TriggerLeaveEvent = round.TriggerLeaveEvent
)

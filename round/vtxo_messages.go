package round

import (
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/arkscript"
)

// VTXOActorMsg embeds actormsg.VTXOActorMsg for messages exchanged between
// VTXO actors and the round actor. This includes both messages FROM VTXO
// actors (refresh requests, forfeit signatures) and messages TO VTXO actors
// (forfeit requests, confirmations).
type VTXOActorMsg interface {
	actormsg.VTXOActorMsg
}

// VTXOManagerMsg embeds actormsg.VTXOManagerMsg for messages sent to the VTXO
// manager. The manager receives notifications about VTXO creation and
// termination.
type VTXOManagerMsg interface {
	actormsg.VTXOManagerMsg
}

// =============================================================================
// Messages TO VTXO actors FROM round actor / chainsource
// =============================================================================

// BlockEpochEvent is received when a new block is connected to the blockchain.
// This triggers expiry monitoring logic to check if the VTXO needs to be
// refreshed or escalated to the chain resolver.
type BlockEpochEvent struct {
	actor.BaseMessage

	// Height is the block height.
	Height int32

	// Hash is the block hash.
	Hash chainhash.Hash

	// Timestamp is the block timestamp from the header.
	Timestamp int64
}

// VTXOActorMsg implements actormsg.VTXOActorMsg marker interface.
func (e *BlockEpochEvent) VTXOActorMsg() {}

// MessageType returns the message type for logging.
func (e *BlockEpochEvent) MessageType() string { return "BlockEpochEvent" }

// ForfeitRequestEvent is received from the round actor when this VTXO is being
// forfeited as part of a batch swap. The VTXO actor should sign the forfeit
// transaction and transition to the forfeiting state.
//
// The forfeit transaction structure:
//   - Input 0: VTXO being forfeited (collaborative spend - client + operator)
//   - Input 1: Connector output from new commitment tx (operator key spend)
//   - Output 0: Full VTXO value to operator's forfeit address
//   - Output 1: Anchor output (zero-value P2A for CPFP)
type ForfeitRequestEvent struct {
	actor.BaseMessage

	// RoundID is the new round where the refreshed VTXO will be created.
	RoundID string

	// ConnectorOutpoint is the connector output from the new commitment tx
	// that the forfeit tx must spend. This links the forfeit atomically to
	// the new round.
	ConnectorOutpoint wire.OutPoint

	// ConnectorPkScript is the scriptPubKey of the connector output.
	ConnectorPkScript []byte

	// ConnectorAmount is the value of the connector output in satoshis.
	ConnectorAmount int64

	// ServerForfeitPkScript is the operator's taproot script where the
	// forfeited VTXO value will be paid.
	ServerForfeitPkScript []byte

	// ForfeitSpend overrides the default standard-VTXO
	// collaborative leaf when the live output being settled uses
	// a custom script policy.
	ForfeitSpend *arkscript.SpendPath
}

// VTXOActorMsg implements actormsg.VTXOActorMsg marker interface.
func (e *ForfeitRequestEvent) VTXOActorMsg() {}

// MessageType returns the message type for logging.
func (e *ForfeitRequestEvent) MessageType() string {
	return "ForfeitRequestEvent"
}

// ForfeitConfirmedEvent indicates the new commitment transaction has been
// confirmed on-chain, meaning the forfeit is final. The old VTXO is now
// permanently forfeited and the new VTXO is live.
type ForfeitConfirmedEvent struct {
	actor.BaseMessage

	// CommitmentTxID is the new commitment transaction that was confirmed.
	CommitmentTxID chainhash.Hash

	// BlockHeight is the height at which confirmation occurred.
	BlockHeight int32
}

// VTXOActorMsg implements actormsg.VTXOActorMsg marker interface.
func (e *ForfeitConfirmedEvent) VTXOActorMsg() {}

// MessageType returns the message type for logging.
func (e *ForfeitConfirmedEvent) MessageType() string {
	return "ForfeitConfirmedEvent"
}

// =============================================================================
// Spend reservation events (sent by VTXO manager on behalf of wallet)
// =============================================================================

// SpendReserveEvent claims a VTXO for an out-of-round (OOR) spend. Only valid
// from LiveState. The VTXO becomes unavailable for cooperative forfeit until
// the spend completes (SpendCompletedEvent) or is released
// (SpendReleasedEvent).
type SpendReserveEvent struct {
	actor.BaseMessage
}

// VTXOActorMsg implements actormsg.VTXOActorMsg marker interface.
func (e *SpendReserveEvent) VTXOActorMsg() {}

// MessageType returns the message type for logging.
func (e *SpendReserveEvent) MessageType() string { return "SpendReserveEvent" }

// SpendReleasedEvent releases a VTXO from spend reservation back to LiveState.
// This is used when the OOR operation fails or is cancelled after the VTXO was
// reserved.
type SpendReleasedEvent struct {
	actor.BaseMessage
}

// VTXOActorMsg implements actormsg.VTXOActorMsg marker interface.
func (e *SpendReleasedEvent) VTXOActorMsg() {}

// MessageType returns the message type for logging.
func (e *SpendReleasedEvent) MessageType() string {
	return "SpendReleasedEvent"
}

// SpendCompletedEvent marks a VTXO as fully spent via an OOR transaction. This
// transitions the VTXO from SpendingState to terminal SpentState.
type SpendCompletedEvent struct {
	actor.BaseMessage
}

// VTXOActorMsg implements actormsg.VTXOActorMsg marker interface.
func (e *SpendCompletedEvent) VTXOActorMsg() {}

// MessageType returns the message type for logging.
func (e *SpendCompletedEvent) MessageType() string {
	return "SpendCompletedEvent"
}

// ForfeitReleasedEvent releases a VTXO from pending forfeit back to LiveState.
// This is used when cooperative round registration fails after the VTXO was
// admitted for forfeiture.
type ForfeitReleasedEvent struct {
	actor.BaseMessage
}

// VTXOActorMsg implements actormsg.VTXOActorMsg marker interface.
func (e *ForfeitReleasedEvent) VTXOActorMsg() {}

// MessageType returns the message type for logging.
func (e *ForfeitReleasedEvent) MessageType() string {
	return "ForfeitReleasedEvent"
}

// =============================================================================
// Internal VTXO actor events (not sent by round actor)
// =============================================================================

// ForfeitSignedEvent indicates the forfeit transaction has been signed and
// submitted to the round. This is an internal event triggered after the VTXO
// actor signs its portion of the forfeit transaction.
type ForfeitSignedEvent struct {
	actor.BaseMessage

	// ForfeitTxID is the txid of the forfeit transaction.
	ForfeitTxID chainhash.Hash
}

// VTXOActorMsg implements actormsg.VTXOActorMsg marker interface.
func (e *ForfeitSignedEvent) VTXOActorMsg() {}

// MessageType returns the message type for logging.
func (e *ForfeitSignedEvent) MessageType() string {
	return "ForfeitSignedEvent"
}

// VTXOFailedEvent indicates an error occurred during VTXO processing. This
// transitions the VTXO to the failed state.
type VTXOFailedEvent struct {
	actor.BaseMessage

	// Reason is a human-readable description of the failure.
	Reason string

	// Error is the underlying error, if any.
	Error error

	// Recoverable indicates whether the failure might be recoverable.
	Recoverable bool
}

// VTXOActorMsg implements actormsg.VTXOActorMsg marker interface.
func (e *VTXOFailedEvent) VTXOActorMsg() {}

// MessageType returns the message type for logging.
func (e *VTXOFailedEvent) MessageType() string { return "VTXOFailedEvent" }

// ResumeVTXOEvent is sent when resuming a VTXO actor from persisted state.
// This is used during startup to restore actors from the database.
type ResumeVTXOEvent struct {
	actor.BaseMessage
}

// VTXOActorMsg implements actormsg.VTXOActorMsg marker interface.
func (e *ResumeVTXOEvent) VTXOActorMsg() {}

// MessageType returns the message type for logging.
func (e *ResumeVTXOEvent) MessageType() string { return "ResumeVTXOEvent" }

// PendingForfeitEvent is sent to a VTXO actor when the round actor has
// accepted this VTXO for cooperative consumption in a pending round, but does
// not yet have the concrete connector/forfeit details needed for signing.
// This transitions the VTXO into PendingForfeitState without encoding whether
// the round intent is a refresh, leave, or another cooperative spend.
type PendingForfeitEvent struct {
	actor.BaseMessage
}

// VTXOActorMsg implements actormsg.VTXOActorMsg marker interface.
func (e *PendingForfeitEvent) VTXOActorMsg() {}

// MessageType returns the message type for logging.
func (e *PendingForfeitEvent) MessageType() string {
	return "PendingForfeitEvent"
}

// =============================================================================
// Messages TO VTXO Manager
// =============================================================================

// VTXOTerminatedMsg notifies the manager that a VTXO actor has reached a
// terminal state and should be removed from tracking.
type VTXOTerminatedMsg struct {
	actor.BaseMessage

	// Outpoint identifies the terminated VTXO.
	Outpoint wire.OutPoint

	// FinalState is the terminal state reached.
	FinalState string

	// Reason explains why the VTXO terminated.
	Reason string
}

// VTXOManagerMsg implements actormsg.VTXOManagerMsg marker interface.
func (m *VTXOTerminatedMsg) VTXOManagerMsg() {}

// MessageType returns the message type for logging.
func (m *VTXOTerminatedMsg) MessageType() string { return "VTXOTerminatedMsg" }

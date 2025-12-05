package vtxo

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
)

// VTXOEvent is a sealed interface for all events that can be processed by the
// VTXO state machine. The sealed pattern prevents external packages from
// adding unvalidated event types. Extends actor.Message for actor system
// compatibility.
type VTXOEvent interface {
	actor.Message
	vtxoEventSealed()
}

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

func (e *BlockEpochEvent) vtxoEventSealed() {}

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
}

func (e *ForfeitRequestEvent) vtxoEventSealed() {}

// MessageType returns the message type for logging.
func (e *ForfeitRequestEvent) MessageType() string { return "ForfeitRequestEvent" }

// ForfeitSignedEvent indicates the forfeit transaction has been signed and
// submitted to the round. This is an internal event triggered after the VTXO
// actor signs its portion of the forfeit transaction.
type ForfeitSignedEvent struct {
	actor.BaseMessage

	// ForfeitTxID is the txid of the forfeit transaction.
	ForfeitTxID chainhash.Hash
}

func (e *ForfeitSignedEvent) vtxoEventSealed() {}

// MessageType returns the message type for logging.
func (e *ForfeitSignedEvent) MessageType() string { return "ForfeitSignedEvent" }

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

func (e *ForfeitConfirmedEvent) vtxoEventSealed() {}

// MessageType returns the message type for logging.
func (e *ForfeitConfirmedEvent) MessageType() string { return "ForfeitConfirmedEvent" }

// RefreshAcknowledgedEvent is received when the round actor acknowledges a
// refresh request. This indicates the VTXO has been queued for inclusion in
// the next round but the forfeit request hasn't been sent yet.
type RefreshAcknowledgedEvent struct {
	actor.BaseMessage

	// RoundID is the round where the refresh will be processed.
	RoundID string
}

func (e *RefreshAcknowledgedEvent) vtxoEventSealed() {}

// MessageType returns the message type for logging.
func (e *RefreshAcknowledgedEvent) MessageType() string { return "RefreshAcknowledgedEvent" }

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

func (e *VTXOFailedEvent) vtxoEventSealed() {}

// MessageType returns the message type for logging.
func (e *VTXOFailedEvent) MessageType() string { return "VTXOFailedEvent" }

// ResumeVTXOEvent is sent when resuming a VTXO actor from persisted state.
// This is used during startup to restore actors from the database.
type ResumeVTXOEvent struct {
	actor.BaseMessage
}

func (e *ResumeVTXOEvent) vtxoEventSealed() {}

// MessageType returns the message type for logging.
func (e *ResumeVTXOEvent) MessageType() string { return "ResumeVTXOEvent" }

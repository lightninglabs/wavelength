package round

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/taproot-assets/proof"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// ClientEvent is a sealed interface for all events that can be processed by
// the client boarding state machine. The sealed interface pattern prevents
// external packages from implementing this interface, ensuring type safety
// and exhaustive pattern matching in state transitions.
type ClientEvent interface {
	// clientEventSealed is an unexported method that marks this interface
	// as sealed, preventing external implementations.
	clientEventSealed()
}

// ResumeBoardingIntents is emitted to instruct the FSM to resume monitoring
// boarding intents that were in progress. The intents are provided as a
// parameter (pre-filtered by the actor) rather than fetched from storage.
type ResumeBoardingIntents struct {
	// Intents are the boarding intents to resume, keyed by their outpoint.
	// These are pre-filtered to include only Confirmed-but-not-Adopted
	// intents.
	Intents map[wire.OutPoint]BoardingIntent
}

func (e *ResumeBoardingIntents) clientEventSealed() {}

// BoardingUTXOConfirmed is emitted when the boarding UTXO has received
// sufficient confirmations and is ready to be used for boarding.
type BoardingUTXOConfirmed struct {
	// Outpoint identifies the confirmed boarding UTXO.
	Outpoint wire.OutPoint

	// Address is the boarding address for this UTXO. Contains the keys,
	// tapscript, and exit delay needed to build the BoardingRequest.
	Address wallet.BoardingAddress

	// BlockHeight is the height at which the UTXO was confirmed.
	BlockHeight int32

	// BlockHash is the hash of the block containing the transaction.
	BlockHash chainhash.Hash

	// Confirmations is the number of confirmations the UTXO has.
	Confirmations int32

	// Tx is the confirmed transaction containing the boarding UTXO. This
	// allows the FSM to extract output details without additional chain
	// queries.
	Tx *wire.MsgTx

	// TxProof is the optional SPV proof for this boarding UTXO. Includes
	// merkle proof, block header, and output construction details. None if
	// the proof hasn't been constructed yet.
	TxProof fn.Option[proof.TxProof]
}

func (e *BoardingUTXOConfirmed) clientEventSealed() {}

// RegistrationRequested is emitted when the FSM is ready to join a round with
// the currently confirmed set of boarding intents. The actor should treat this
// as a batch request containing every confirmed intent.
type RegistrationRequested struct {
	Intents []BoardingIntent

	// RoundID allows resuming a previously assigned round when rejoining.
	RoundID string
}

func (e *RegistrationRequested) clientEventSealed() {}

// RoundJoined is emitted when the server accepts the client's registration
// and assigns them to a round. This event arrives via the Outbox from the
// server FSM.
type RoundJoined struct {
	// RoundID is the unique identifier for the round.
	RoundID string

	// SessionInfo contains any session-specific data from the operator.
	SessionInfo map[string][]byte
}

func (e *RoundJoined) clientEventSealed() {}

// CommitmentTxBuilt is emitted when the server sends the commitment
// transaction and VTXT path to the client. This event arrives via the Outbox
// from the server FSM. It embeds the common event type.
type CommitmentTxBuilt struct {
	CommitmentTxBuiltEvent
}

func (e *CommitmentTxBuilt) clientEventSealed() {}

// CommitmentTxValidated is emitted after the client successfully validates
// the commitment transaction and VTXT path. This is a critical security
// checkpoint - the client must verify:
//  1. Boarding UTXO is an input to commitment tx.
//  2. VTXT path is valid and leads to expected VTXOs.
//  3. VTXO amounts and scripts match requests.
type CommitmentTxValidated struct {
	// VTXTTree is the validated tree.
	VTXTTree *tree.Tree

	// ValidationInfo contains details about what was validated.
	ValidationInfo map[string]interface{}
}

func (e *CommitmentTxValidated) clientEventSealed() {}

// GenerateNonces is emitted when the client has validated the vtxt tree, and
// nonces should be generated for all the client trees.
type GenerateNonces struct {
}

func (e *GenerateNonces) clientEventSealed() {}

// NoncesAggregated is emitted when the server sends back the aggregated
// nonces from all participants. This event arrives via the Outbox from the
// server FSM. It embeds the common event type.
type NoncesAggregated struct {
	NoncesAggregatedEvent
}

func (e *NoncesAggregated) clientEventSealed() {}

// GeneratePartialSigs is emitted when the client has generated partial
// signatures for all transactions in their VTXT path using the aggregated
// nonces.
type GeneratePartialSigs struct {
	// PartialSigs are the MuSig2 partial signatures, one per transaction
	// in the client's VTXT path.
	PartialSigs [][]byte

	// SigningKey is the key used for signing.
	SigningKey *btcec.PublicKey
}

func (e *GeneratePartialSigs) clientEventSealed() {}

// OperatorSigned is emitted when the server sends the complete VTXT
// signatures after aggregating all partial signatures. This event arrives via
// the Outbox from the server FSM. It embeds the common event type.
type OperatorSigned struct {
	OperatorSignedEvent
}

func (e *OperatorSigned) clientEventSealed() {}

// BoardingConfirmed is emitted when the commitment transaction has been
// confirmed on-chain with sufficient confirmations.
type BoardingConfirmed struct {
	// TxID is the confirmed commitment transaction ID.
	TxID chainhash.Hash

	// BlockHeight is the height at which the transaction was confirmed.
	BlockHeight int32

	// Confirmations is the number of confirmations.
	Confirmations int32

	// VTXOs are the virtual UTXOs the client now owns. TODO: Evaluate if
	// we need to wrap lib VTXO types with boarding-specific metadata. For
	// now, storing as opaque data.
	VTXOs []byte
}

func (e *BoardingConfirmed) clientEventSealed() {}

// BoardingFailed is emitted when an error occurs during the boarding
// process.
type BoardingFailed struct {
	// Reason is a human-readable description of the failure.
	Reason string

	// Error is the underlying error.
	Error error

	// Recoverable indicates if the client can retry or if CSV recovery
	// is needed.
	Recoverable bool
}

func (e *BoardingFailed) clientEventSealed() {}

// RecoveryInitiated is emitted when the client initiates CSV timeout
// recovery to sweep their boarding UTXO back to their wallet.
type RecoveryInitiated struct {
	// Outpoint identifies the boarding UTXO being recovered.
	Outpoint wire.OutPoint

	// SweepTxID is the transaction ID of the sweep transaction.
	SweepTxID chainhash.Hash

	// Reason explains why recovery was initiated.
	Reason string
}

func (e *RecoveryInitiated) clientEventSealed() {}

// RoundComplete is an internal event emitted after a boarding round completes
// successfully. This triggers the FSM to transition back to Idle state to
// process new boarding addresses and intents.
type RoundComplete struct{}

func (e *RoundComplete) clientEventSealed() {}

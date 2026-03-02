package round

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
)

// ClientState is a sealed interface for all states in the client round
// interaction state machine. Each state implements ProcessEvent to handle
// events and transition to the next state. This FSM handles the client's
// participation in Ark rounds, including boarding, refresh, and offboard
// operations.
//
// The baselib protofsm.State interface has 3 type parameters:
//   - InternalEvent = ClientEvent.
//   - OutboxEvent = ClientOutMsg.
//   - Env = *ClientEnvironment.
type ClientState interface {
	protofsm.State[ClientEvent, ClientOutMsg, *ClientEnvironment]

	// clientStateSealed is an unexported method that marks this interface
	// as sealed, preventing external implementations.
	clientStateSealed()
}

// Idle is the initial state. No active boarding process is running.
type Idle struct{}

func (s *Idle) String() string {
	return "Idle"
}

func (s *Idle) IsTerminal() bool {
	return false
}

func (s *Idle) clientStateSealed() {}

// PendingRoundAssembly tracks all active boarding intents that have been
// funded on-chain but not yet fully confirmed. Intents are keyed by their
// on-chain outpoint for efficient lookup when confirmation events arrive. Once
// all intents reach the required confirmations, the FSM transitions to round
// registration.
//
// This state tracks four independent pools: boarding inputs, forfeit inputs,
// VTXO outputs, and leave outputs. The pools are validated at registration
// time by checking sum(inputs) >= sum(outputs) + fees.
type PendingRoundAssembly struct {
	// Boarding contains the collected boarding intents to include in the
	// next round.
	Boarding []BoardingIntent

	// VTXOs contains the collected VTXO requests to include in the next
	// round.
	VTXOs []types.VTXORequest

	// Forfeits tracks VTXOs being forfeited as inputs to this round.
	// Decoupled from outputs to enable many-to-many operations.
	Forfeits []types.ForfeitRequest

	// Leaves tracks on-chain exit outputs for this round. Decoupled from
	// forfeit inputs to enable many-to-many operations.
	Leaves []*types.LeaveRequest
}

func (s *PendingRoundAssembly) String() string {
	return "PendingRoundAssembly"
}

func (s *PendingRoundAssembly) IsTerminal() bool {
	return false
}

func (s *PendingRoundAssembly) clientStateSealed() {}

// RegistrationSentState indicates the client has sent a JoinRoundRequest
// to the server and is waiting for confirmation.
type RegistrationSentState struct {
	// Intents contains all the client's intents for this round.
	Intents Intents
}

func (s *RegistrationSentState) String() string {
	return "RegistrationSent"
}

func (s *RegistrationSentState) IsTerminal() bool {
	return false
}

func (s *RegistrationSentState) clientStateSealed() {}

// RoundJoinedState indicates the client has been accepted into a round and
// is waiting for the commitment transaction.
type RoundJoinedState struct {
	// RoundID is the unique identifier assigned by the server for this
	// round.
	RoundID RoundID

	// Intents contains all intents participating in this round.
	Intents Intents
}

func (s *RoundJoinedState) String() string {
	return "RoundJoined"
}

func (s *RoundJoinedState) IsTerminal() bool {
	return false
}

func (s *RoundJoinedState) clientStateSealed() {}

// CommitmentTxReceivedState indicates the client has received the commitment
// transaction and VTXT paths and must now validate them before proceeding.
type CommitmentTxReceivedState struct {
	// RoundID is the unique identifier for this round.
	RoundID RoundID

	// CommitmentTx is the unsigned commitment transaction as a PSBT.
	CommitmentTx *psbt.Packet

	// TxID is the transaction ID of the commitment transaction.
	TxID chainhash.Hash

	// VTXOTreePaths maps commitment tx output indices to VTXO tree paths.
	VTXOTreePaths map[int]*tree.Tree

	// Intents contains all the client's intents for this round.
	Intents Intents

	// ClientTrees maps signer keys (compressed pubkeys) to the client's
	// extracted sub-tree for that VTXO.
	ClientTrees map[SignerKey]*tree.Tree
}

func (s *CommitmentTxReceivedState) String() string {
	return "CommitmentTxReceived"
}

func (s *CommitmentTxReceivedState) IsTerminal() bool {
	return false
}

func (s *CommitmentTxReceivedState) clientStateSealed() {}

// CommitmentTxValidatedState indicates the client has validated the VTXT
// and is ready to participate in MuSig2 signing.
type CommitmentTxValidatedState struct {
	// RoundID is the unique identifier for this round.
	RoundID RoundID

	// CommitmentTx is the unsigned commitment transaction as a PSBT.
	CommitmentTx *psbt.Packet

	// VTXOTreePaths maps commitment tx output indices to VTXO tree paths.
	VTXOTreePaths map[int]*tree.Tree

	// Intents contains all the client's intents for this round.
	Intents Intents

	// ClientTrees maps signer keys (compressed pubkeys) to the client's
	// extracted sub-tree for that VTXO.
	ClientTrees map[SignerKey]*tree.Tree

	// BoardingInputIndices maps each boarding intent's outpoint to its
	// position in the commitment transaction inputs. Used for signing.
	BoardingInputIndices map[wire.OutPoint]int

	// ForfeitMappings maps VTXO outpoints to their connector info for
	// refresh rounds. Empty for boarding-only rounds. Carried forward
	// through MuSig2 signing states until forfeit collection.
	ForfeitMappings map[wire.OutPoint]*ConnectorLeafInfo
}

func (s *CommitmentTxValidatedState) String() string {
	return "CommitmentTxValidated"
}

func (s *CommitmentTxValidatedState) IsTerminal() bool {
	return false
}

func (s *CommitmentTxValidatedState) clientStateSealed() {}

// ForfeitSignaturesCollectingState indicates the client is waiting for forfeit
// signatures from VTXO actors after completing VTXO tree signing. This state
// is entered when the round includes refresh or leave requests (VTXOs being
// rolled over or exited). The FSM waits until all expected forfeit signatures
// are collected, then submits them to the server and transitions to boarding
// input signing.
//
// The forfeit flow ensures atomic refresh: old VTXOs are forfeited (locked to
// the new commitment tx via connectors) before new VTXOs become valid. This
// prevents double-spending while preserving client custody.
type ForfeitSignaturesCollectingState struct {
	// RoundID is the unique identifier for this round.
	RoundID RoundID

	// CommitmentTx is the unsigned commitment transaction as a PSBT.
	CommitmentTx *psbt.Packet

	// VTXOTreePaths maps commitment tx output indices to VTXO tree paths.
	VTXOTreePaths map[int]*tree.Tree

	// Intents contains all the client's intents for this round.
	Intents Intents

	// ClientTrees maps signer keys (compressed pubkeys) to the client's
	// extracted sub-tree for that VTXO.
	ClientTrees map[SignerKey]*tree.Tree

	// BoardingInputIndices maps each boarding intent's outpoint to its
	// position in the commitment transaction inputs. Used for signing.
	BoardingInputIndices map[wire.OutPoint]int

	// ExpectedForfeits maps VTXO outpoints to their connector info. These
	// are the VTXOs we're waiting for forfeit signatures from.
	ExpectedForfeits map[wire.OutPoint]*ConnectorLeafInfo

	// CollectedForfeits maps VTXO outpoints to their forfeit responses.
	// When len(CollectedForfeits) == len(ExpectedForfeits), we proceed.
	CollectedForfeits map[wire.OutPoint]*ForfeitSignatureResponse
}

func (s *ForfeitSignaturesCollectingState) String() string {
	return "ForfeitSignaturesCollecting"
}

func (s *ForfeitSignaturesCollectingState) IsTerminal() bool {
	return false
}

func (s *ForfeitSignaturesCollectingState) clientStateSealed() {}

// NoncesSentState indicates the client has sent nonces to the server and
// is waiting for aggregated nonces.
type NoncesSentState struct {
	// RoundID is the unique identifier for this round.
	RoundID RoundID

	// CommitmentTx is the unsigned commitment transaction as a PSBT.
	CommitmentTx *psbt.Packet

	// VTXOTreePaths maps commitment tx output indices to VTXO tree paths.
	VTXOTreePaths map[int]*tree.Tree

	// Intents contains all the client's intents for this round.
	Intents Intents

	// ClientTrees maps signer keys (compressed pubkeys) to the client's
	// extracted sub-tree for that VTXO.
	ClientTrees map[SignerKey]*tree.Tree

	// Musig2Sessions maps signer keys (compressed pubkeys) to the MuSig2
	// signing session for that VTXO.
	Musig2Sessions map[SignerKey]*tree.SignerSession

	// BoardingInputIndices maps each boarding intent's outpoint to its
	// position in the commitment transaction inputs. Used for signing.
	BoardingInputIndices map[wire.OutPoint]int

	// ForfeitMappings maps VTXO outpoints to their connector info for
	// refresh rounds. Carried forward until forfeit collection after
	// VTXO tree signing.
	ForfeitMappings map[wire.OutPoint]*ConnectorLeafInfo
}

func (s *NoncesSentState) String() string {
	return "NoncesSent"
}

func (s *NoncesSentState) IsTerminal() bool {
	return false
}

func (s *NoncesSentState) clientStateSealed() {}

// NoncesAggregatedState indicates the client has received aggregated nonces
// and is ready to generate partial signatures.
type NoncesAggregatedState struct {
	// RoundID is the unique identifier for this round.
	RoundID RoundID

	// CommitmentTx is the unsigned commitment transaction as a PSBT.
	CommitmentTx *psbt.Packet

	// VTXOTreePaths maps commitment tx output indices to VTXO tree paths.
	VTXOTreePaths map[int]*tree.Tree

	// Intents contains all the client's intents for this round.
	Intents Intents

	// ClientTrees maps signer keys (compressed pubkeys) to the client's
	// extracted sub-tree for that VTXO.
	ClientTrees map[SignerKey]*tree.Tree

	// Musig2Sessions maps signer keys (compressed pubkeys) to the MuSig2
	// signing session for that VTXO.
	Musig2Sessions map[SignerKey]*tree.SignerSession

	// AggNonces maps transaction IDs to aggregated MuSig2 public nonces.
	AggNonces map[tree.TxID]tree.Musig2PubNonce

	// BoardingInputIndices maps each boarding intent's outpoint to its
	// position in the commitment transaction inputs. Used for signing.
	BoardingInputIndices map[wire.OutPoint]int

	// ForfeitMappings maps VTXO outpoints to their connector info for
	// refresh rounds. Carried forward until forfeit collection after
	// VTXO tree signing.
	ForfeitMappings map[wire.OutPoint]*ConnectorLeafInfo
}

func (s *NoncesAggregatedState) String() string {
	return "NoncesAggregated"
}

func (s *NoncesAggregatedState) IsTerminal() bool {
	return false
}

func (s *NoncesAggregatedState) clientStateSealed() {}

// PartialSigsSentState indicates the client has sent partial signatures
// to the server and is waiting for the complete VTXT signatures.
type PartialSigsSentState struct {
	// RoundID is the unique identifier for this round.
	RoundID RoundID

	// CommitmentTx is the unsigned commitment transaction as a PSBT.
	CommitmentTx *psbt.Packet

	// VTXOTreePaths maps commitment tx output indices to VTXO tree paths.
	VTXOTreePaths map[int]*tree.Tree

	// Intents contains all the client's intents for this round.
	Intents Intents

	// ClientTrees maps signer keys (compressed pubkeys) to the client's
	// extracted sub-tree for that VTXO.
	ClientTrees map[SignerKey]*tree.Tree

	// Musig2Sessions maps signer keys (compressed pubkeys) to the MuSig2
	// signing session for that VTXO.
	Musig2Sessions map[SignerKey]*tree.SignerSession

	// BoardingInputIndices maps each boarding intent's outpoint to its
	// position in the commitment transaction inputs. Used for signing.
	BoardingInputIndices map[wire.OutPoint]int

	// ForfeitMappings maps VTXO outpoints to their connector info for
	// refresh rounds. After VTXO tree signature validation, if non-empty,
	// transitions to ForfeitSignaturesCollectingState.
	ForfeitMappings map[wire.OutPoint]*ConnectorLeafInfo
}

func (s *PartialSigsSentState) String() string {
	return "PartialSigsSent"
}

func (s *PartialSigsSentState) IsTerminal() bool {
	return false
}

func (s *PartialSigsSentState) clientStateSealed() {}

// InputSigSentState indicates the client has sent their boarding input
// signature and is waiting for the commitment tx to be broadcast.
type InputSigSentState struct {
	// RoundID is the unique identifier for this round.
	RoundID RoundID

	// CommitmentTx is the unsigned commitment transaction as a PSBT.
	CommitmentTx *psbt.Packet

	// VTXOTreePaths maps commitment tx output indices to VTXO tree paths.
	VTXOTreePaths map[int]*tree.Tree

	// Intents contains all the client's intents for this round.
	Intents Intents

	// ClientTrees maps signer keys (compressed pubkeys) to the client's
	// extracted sub-tree for that VTXO.
	ClientTrees map[SignerKey]*tree.Tree

	// InputSigs are the Schnorr signatures for the boarding inputs.
	InputSigs []*types.BoardingInputSignature

	// ForfeitedVTXOs contains outpoints of VTXOs being refreshed. When the
	// round confirms, ForfeitConfirmedToVTXO messages are emitted for each
	// so old VTXO actors can transition to the Forfeited terminal state.
	ForfeitedVTXOs []wire.OutPoint
}

func (s *InputSigSentState) String() string {
	return "InputSigSent"
}

func (s *InputSigSentState) IsTerminal() bool {
	return false
}

func (s *InputSigSentState) clientStateSealed() {}

// ConfirmedState is a terminal state indicating the boarding process has
// completed successfully. The client now owns VTXOs.
type ConfirmedState struct {
	// TxID is the confirmed commitment transaction ID.
	TxID chainhash.Hash

	// BlockHeight is the height at which the transaction was confirmed.
	BlockHeight int32

	// BlockHash is the hash of the block containing the confirmation.
	BlockHash chainhash.Hash

	// Confirmations is the number of confirmations.
	Confirmations int32

	// VTXOs are the virtual UTXOs created for this client.
	VTXOs []*ClientVTXO
}

func (s *ConfirmedState) String() string {
	return "Confirmed"
}

func (s *ConfirmedState) IsTerminal() bool {
	return true
}

func (s *ConfirmedState) clientStateSealed() {}

// ClientFailedState is a terminal state indicating the boarding process failed.
// The client may be able to retry or initiate CSV recovery.
type ClientFailedState struct {
	// Reason is a human-readable description of the failure.
	Reason string

	// Error is the underlying error that caused the failure.
	Error error

	// Recoverable indicates if the client can retry or if CSV recovery is
	// needed.
	Recoverable bool
}

func (s *ClientFailedState) String() string {
	return fmt.Sprintf("ClientFailed: %s", s.Reason)
}

func (s *ClientFailedState) IsTerminal() bool {
	// ClientFailedState is NOT terminal - it can recover by accepting the
	// same events as Idle (BoardingUTXOConfirmed, ResumeBoardingIntents).
	return false
}

func (s *ClientFailedState) clientStateSealed() {}

// RecoveryInitiatedState is a semi-terminal state where the client is
// recovering their boarding UTXO via CSV timeout sweep.
type RecoveryInitiatedState struct {
	// Outpoint identifies the boarding UTXO being recovered.
	Outpoint wire.OutPoint

	// SweepTxID is the transaction ID of the sweep transaction.
	SweepTxID chainhash.Hash

	// Reason explains why recovery was initiated.
	Reason string
}

func (s *RecoveryInitiatedState) String() string {
	return "RecoveryInitiated"
}

func (s *RecoveryInitiatedState) IsTerminal() bool {
	return true
}

func (s *RecoveryInitiatedState) clientStateSealed() {}

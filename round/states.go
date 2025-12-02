package round

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/lib/tree"
)

// ClientState is a sealed interface for all states in the client boarding
// state machine. Each state implements ProcessEvent to handle events and
// transition to the next state.
//
// The baselib protofsm.State interface has 3 type parameters:
//   - InternalEvent = ClientEvent.
//   - OutboxEvent = ClientEvent.
//   - Env = *ClientEnvironment.
type ClientState interface {
	protofsm.State[ClientEvent, ClientEvent, *ClientEnvironment]

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
type PendingRoundAssembly struct {
	// Intents maps outpoint to boarding intent. Only intents with on-chain
	// UTXOs (i.e., ChainInfo.OutPoint set) are included in this map.
	Intents map[wire.OutPoint]BoardingIntent
}

func (s *PendingRoundAssembly) String() string {
	return "BoardingIntents"
}

func (s *PendingRoundAssembly) IsTerminal() bool {
	return false
}

func (s *PendingRoundAssembly) clientStateSealed() {}

// RegistrationSentState indicates the client has sent a JoinRoundRequest
// to the server and is waiting for confirmation.
type RegistrationSentState struct {
	Intents []BoardingIntent
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
	RoundID string
	Intents []BoardingIntent
}

func (s *RoundJoinedState) String() string {
	return "RoundJoined"
}

func (s *RoundJoinedState) IsTerminal() bool {
	return false
}

func (s *RoundJoinedState) clientStateSealed() {}

// CommitmentTxReceivedState indicates the client has received the commitment
// transaction and VTXT and must now validate them before proceeding.
type CommitmentTxReceivedState struct {
	RoundID string

	CommitmentTx *wire.MsgTx

	TxID chainhash.Hash

	VTXTTree *tree.Tree

	Intents []BoardingIntent

	ClientTrees map[string]*tree.Tree
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
	RoundID string

	CommitmentTx *wire.MsgTx

	VTXTTree *tree.Tree

	Intents []BoardingIntent

	ClientTrees map[string]*tree.Tree

	// BoardingInputIndices maps each boarding intent's outpoint to its
	// position in the commitment transaction inputs. Used for signing.
	BoardingInputIndices map[wire.OutPoint]int
}

func (s *CommitmentTxValidatedState) String() string {
	return "CommitmentTxValidated"
}

func (s *CommitmentTxValidatedState) IsTerminal() bool {
	return false
}

func (s *CommitmentTxValidatedState) clientStateSealed() {}

// NoncesSentState indicates the client has sent nonces to the server and
// is waiting for aggregated nonces.
type NoncesSentState struct {
	RoundID string

	CommitmentTx *wire.MsgTx

	VTXTTree *tree.Tree

	Intents []BoardingIntent

	ClientTrees map[string]*tree.Tree

	Musig2Sessions map[string]*tree.SignerSession

	// BoardingInputIndices maps each boarding intent's outpoint to its
	// position in the commitment transaction inputs. Used for signing.
	BoardingInputIndices map[wire.OutPoint]int
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
	RoundID string

	CommitmentTx *wire.MsgTx

	VTXTTree *tree.Tree

	Intents []BoardingIntent

	ClientTrees map[string]*tree.Tree

	Musig2Sessions map[string]*tree.SignerSession

	AggregatedNonces [][]byte

	// BoardingInputIndices maps each boarding intent's outpoint to its
	// position in the commitment transaction inputs. Used for signing.
	BoardingInputIndices map[wire.OutPoint]int
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
	RoundID string

	CommitmentTx *wire.MsgTx

	VTXTTree *tree.Tree

	Intents []BoardingIntent

	ClientTrees map[string]*tree.Tree

	PartialSigs [][]byte

	Musig2Sessions map[string]*tree.SignerSession

	// BoardingInputIndices maps each boarding intent's outpoint to its
	// position in the commitment transaction inputs. Used for signing.
	BoardingInputIndices map[wire.OutPoint]int
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
	RoundID string

	CommitmentTx *wire.MsgTx

	VTXTTree *tree.Tree

	Intents []BoardingIntent

	ClientTrees map[string]*tree.Tree

	InputSigs [][]byte
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
	TxID          chainhash.Hash
	BlockHeight   int32
	Confirmations int32
	VTXOs         []*ClientVTXO
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
	Reason      string
	Error       error
	Recoverable bool
}

func (s *ClientFailedState) String() string {
	return fmt.Sprintf("ClientFailed: %s", s.Reason)
}

func (s *ClientFailedState) IsTerminal() bool {
	return true
}

func (s *ClientFailedState) clientStateSealed() {}

// RecoveryInitiatedState is a semi-terminal state where the client is
// recovering their boarding UTXO via CSV timeout sweep.
type RecoveryInitiatedState struct {
	Outpoint  wire.OutPoint
	SweepTxID chainhash.Hash
	Reason    string
}

func (s *RecoveryInitiatedState) String() string {
	return "RecoveryInitiated"
}

func (s *RecoveryInitiatedState) IsTerminal() bool {
	return true
}

func (s *RecoveryInitiatedState) clientStateSealed() {}

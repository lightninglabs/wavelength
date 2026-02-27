package rounds

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/clientconn"
)

// State is a sealed interface for all states in the round state machine.
// Each state implements ProcessEvent to handle events and transition to the
// next state.
type State interface {
	protofsm.State[Event, OutboxEvent, *Environment]

	// stateSealed is an unexported method that marks this interface as
	// sealed, preventing external implementations.
	stateSealed()
}

// CreatedState is the initial state of the round. No clients have joined yet.
type CreatedState struct{}

// String returns a human-readable representation of CreatedState.
func (s *CreatedState) String() string {
	return "CreatedState"
}

// IsTerminal returns false as CreatedState is not a terminal state.
func (s *CreatedState) IsTerminal() bool {
	return false
}

// stateSealed marks CreatedState as implementing the sealed State interface.
func (s *CreatedState) stateSealed() {}

// RegistrationState is the state where the FSM is accepting client join
// requests. The FSM accumulates client requests until a SealEvent is
// received.
type RegistrationState struct {
	// ClientRegistrations maps client IDs to their registration data.
	// This allows tracking which client submitted which requests, so we
	// can send appropriate data back to each client later.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration
}

// String returns a human-readable representation of the RegistrationState.
func (s *RegistrationState) String() string {
	return "RegistrationState"
}

// IsTerminal returns false as RegistrationState is not a terminal state.
func (s *RegistrationState) IsTerminal() bool {
	return false
}

// stateSealed marks RegistrationState as implementing the sealed State
// interface.
func (s *RegistrationState) stateSealed() {}

// newRegistrationState creates a new RegistrationState with the given client
// registrations.
func newRegistrationState(
	regs map[clientconn.ClientID]*ClientRegistration) *RegistrationState {

	return &RegistrationState{
		ClientRegistrations: regs,
	}
}

// isClientRegistered checks if a client is already registered in this round.
func (s *RegistrationState) isClientRegistered(
	clientID clientconn.ClientID) bool {

	_, exists := s.ClientRegistrations[clientID]
	return exists
}

// withNewClient returns a new RegistrationState with the given client added.
// The original state is not modified (immutable pattern).
func (s *RegistrationState) withNewClient(clientID clientconn.ClientID,
	result *JoinRequestResult) *RegistrationState {

	newRegs := make(map[clientconn.ClientID]*ClientRegistration)
	for id, reg := range s.ClientRegistrations {
		newRegs[id] = reg
	}

	newRegs[clientID] = newClientRegistration(clientID, result)

	return newRegistrationState(newRegs)
}

// getAllBoardingInputs returns all boarding inputs from all clients.
func (s *RegistrationState) getAllBoardingInputs() []*BoardingInput {
	var all []*BoardingInput
	for _, reg := range s.ClientRegistrations {
		all = append(all, reg.BoardingInputs...)
	}

	return all
}

// BatchBuildingState is a transitional state where the commitment transaction
// PSBT is being constructed. This state processes BuildBatchTxEvent to build
// the PSBT and immediately transitions to BatchBuiltState.
type BatchBuildingState struct {
	// ClientRegistrations maps client IDs to their registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration
}

// String returns a human-readable representation of BatchBuildingState.
func (s *BatchBuildingState) String() string {
	return "BatchBuildingState"
}

// IsTerminal returns false as BatchBuildingState is not a terminal state.
func (s *BatchBuildingState) IsTerminal() bool {
	return false
}

// stateSealed marks BatchBuildingState as implementing the sealed State
// interface.
func (s *BatchBuildingState) stateSealed() {}

// BatchBuiltState holds the funded PSBT after successful construction.
// The PSBT contains boarding inputs and leave outputs, plus wallet inputs
// and change added by FundPsbt. All inputs are unsigned at this point.
type BatchBuiltState struct {
	// ClientRegistrations maps client IDs to their registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration

	// PSBT is the funded but unsigned commitment transaction.
	PSBT *psbt.Packet

	// VTXOTrees maps commitment tx output indices to their VTXO trees.
	// This is nil if no VTXOs exist in the round.
	VTXOTrees map[int]*tree.Tree

	// ConnectorTrees maps commitment tx output indices to connector trees.
	// This is nil if no forfeits exist in the round.
	ConnectorTrees map[int]*tree.Tree

	// ConnectorAssignments maps forfeited outpoints to connector leaves.
	// This is nil if no forfeits exist in the round.
	ConnectorAssignments map[wire.OutPoint]*ConnectorLeafAssignment

	// ConnectorDescriptors describe connector outputs for this round.
	// This is nil if no forfeits exist in the round.
	ConnectorDescriptors []*ConnectorTreeDescriptor
}

// String returns a human-readable representation of BatchBuiltState.
func (s *BatchBuiltState) String() string {
	return "BatchBuiltState"
}

// IsTerminal returns false as BatchBuiltState is not a terminal state.
func (s *BatchBuiltState) IsTerminal() bool {
	return false
}

// stateSealed marks BatchBuiltState as implementing the sealed State interface.
func (s *BatchBuiltState) stateSealed() {}

// AwaitingInputSigsState waits for clients to submit their boarding
// input signatures. Each client must sign their boarding inputs so the
// commitment transaction can be finalized.
type AwaitingInputSigsState struct {
	// ClientRegistrations maps client IDs to their registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration

	// PSBT is the funded but unsigned commitment transaction.
	PSBT *psbt.Packet

	// VTXOTrees maps commitment tx output indices to their VTXO trees.
	// This is nil if no VTXOs exist in the round.
	VTXOTrees map[int]*tree.Tree

	// ConnectorTrees maps commitment tx output indices to connector trees.
	// This is nil if no forfeits exist in the round.
	ConnectorTrees map[int]*tree.Tree

	// ConnectorAssignments maps forfeited outpoints to connector leaves.
	// This is nil if no forfeits exist in the round.
	ConnectorAssignments map[wire.OutPoint]*ConnectorLeafAssignment

	// ConnectorDescriptors describe connector outputs for this round.
	// This is nil if no forfeits exist in the round.
	ConnectorDescriptors []*ConnectorTreeDescriptor

	// ClientsSubmitted tracks which clients have submitted all expected
	// boarding signatures and forfeit transactions. Once all registered
	// clients have submitted, the round transitions to ServerSigningState.
	ClientsSubmitted map[clientconn.ClientID]struct{}

	// CollectedSignatures stores the boarding signatures submitted by each
	// client. These are validated but not yet applied to the PSBT - that
	// happens during server signing.
	CollectedSignatures InputSigsMap

	// CollectedForfeitTxs stores the forfeit transactions submitted by each
	// client. These are validated but not yet signed by the server.
	CollectedForfeitTxs ForfeitTxsMap
}

// String returns a human-readable representation of
// AwaitingInputSigsState.
func (s *AwaitingInputSigsState) String() string {
	return "AwaitingInputSigsState"
}

// IsTerminal returns false as AwaitingInputSigsState is not a terminal
// state.
func (s *AwaitingInputSigsState) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingInputSigsState as implementing the sealed
// State interface.
func (s *AwaitingInputSigsState) stateSealed() {}

// allClientsSubmitted returns true if all registered clients with boarding
// inputs have submitted their signatures.
func (s *AwaitingInputSigsState) allClientsSubmitted() bool {
	// Count clients that have boarding inputs and thus need to submit sigs.
	clientsWithBoarding := 0
	for _, reg := range s.ClientRegistrations {
		if len(reg.BoardingInputs) > 0 {
			clientsWithBoarding++
		}
	}

	return len(s.ClientsSubmitted) >= clientsWithBoarding
}

// hasClientSubmitted checks if a client has already submitted their
// signatures.
func (s *AwaitingInputSigsState) hasClientSubmitted(
	clientID clientconn.ClientID) bool {

	_, exists := s.ClientsSubmitted[clientID]
	return exists
}

// AwaitingVTXONoncesState waits for clients to submit their MuSig2 nonces for
// VTXO tree transactions. This state is only entered if the round has VTXOs.
// Once all clients with VTXOs have submitted nonces, the FSM transitions to
// AwaitingVTXOSignaturesState for partial signature collection.
type AwaitingVTXONoncesState struct {
	// ClientRegistrations maps client IDs to their registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration

	// PSBT is the funded but unsigned commitment transaction.
	PSBT *psbt.Packet

	// VTXOTrees maps commitment tx output indices to their VTXO trees.
	VTXOTrees map[int]*tree.Tree

	// ConnectorTrees maps commitment tx output indices to connector trees.
	// This is nil if no forfeits exist in the round.
	ConnectorTrees map[int]*tree.Tree

	// ConnectorAssignments maps forfeited outpoints to connector leaves.
	// This is nil if no forfeits exist in the round.
	ConnectorAssignments map[wire.OutPoint]*ConnectorLeafAssignment

	// TreeSignCoordinators maps commitment tx output indices to their
	// MuSig2 signing coordinators. Each coordinator manages nonce and
	// signature collection for one VTXO tree.
	TreeSignCoordinators map[int]*batch.TreeSignCoordinator

	// ClientsWithNonces tracks which clients have submitted nonces.
	ClientsWithNonces map[clientconn.ClientID]struct{}
}

// String returns a human-readable representation of AwaitingVTXONoncesState.
func (s *AwaitingVTXONoncesState) String() string {
	return "AwaitingVTXONoncesState"
}

// IsTerminal returns false as AwaitingVTXONoncesState is not a terminal state.
func (s *AwaitingVTXONoncesState) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingVTXONoncesState as implementing the sealed State
// interface.
func (s *AwaitingVTXONoncesState) stateSealed() {}

// allClientsSubmittedNonces returns true if all registered clients with VTXOs
// have submitted their nonces.
func (s *AwaitingVTXONoncesState) allClientsSubmittedNonces() bool {
	for clientID, reg := range s.ClientRegistrations {
		if len(reg.VTXODescriptors) == 0 {
			continue
		}

		if !s.hasClientSubmittedNonces(clientID) {
			return false
		}
	}

	return true
}

// hasClientSubmittedNonces checks if a client has submitted any nonces.
func (s *AwaitingVTXONoncesState) hasClientSubmittedNonces(
	clientID clientconn.ClientID) bool {

	_, exists := s.ClientsWithNonces[clientID]
	return exists
}

// AwaitingVTXOSignaturesState waits for clients to submit their MuSig2 partial
// signatures for VTXO tree transactions. This state is entered after all
// clients with VTXOs have submitted their nonces and the aggregated nonces have
// been distributed. Once all clients have submitted partial signatures, the
// final signatures are aggregated and distributed, then the FSM transitions to
// AwaitingInputSigsState.
type AwaitingVTXOSignaturesState struct {
	// ClientRegistrations maps client IDs to their registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration

	// PSBT is the funded but unsigned commitment transaction.
	PSBT *psbt.Packet

	// VTXOTrees maps commitment tx output indices to their VTXO trees.
	VTXOTrees map[int]*tree.Tree

	// ConnectorTrees maps commitment tx output indices to connector trees.
	// This is nil if no forfeits exist in the round.
	ConnectorTrees map[int]*tree.Tree

	// ConnectorAssignments maps forfeited outpoints to connector leaves.
	// This is nil if no forfeits exist in the round.
	ConnectorAssignments map[wire.OutPoint]*ConnectorLeafAssignment

	// TreeSignCoordinators maps commitment tx output indices to their
	// MuSig2 signing coordinators. Each coordinator manages signature
	// collection for one VTXO tree.
	TreeSignCoordinators map[int]*batch.TreeSignCoordinator

	// ClientsWithSignatures tracks which clients have submitted their
	// partial signatures.
	ClientsWithSignatures map[clientconn.ClientID]struct{}
}

// String returns a human-readable representation of
// AwaitingVTXOSignaturesState.
func (s *AwaitingVTXOSignaturesState) String() string {
	return "AwaitingVTXOSignaturesState"
}

// IsTerminal returns false as AwaitingVTXOSignaturesState is not a terminal
// state.
func (s *AwaitingVTXOSignaturesState) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingVTXOSignaturesState as implementing the sealed
// State interface.
func (s *AwaitingVTXOSignaturesState) stateSealed() {}

// allClientsSubmittedSignatures returns true if all registered clients with
// VTXOs have submitted their partial signatures.
func (s *AwaitingVTXOSignaturesState) allClientsSubmittedSignatures() bool {
	for clientID, reg := range s.ClientRegistrations {
		if len(reg.VTXODescriptors) == 0 {
			continue
		}

		if !s.hasClientSubmittedSignatures(clientID) {
			return false
		}
	}

	return true
}

// hasClientSubmittedSignatures checks if a client has already submitted any
// partial signatures.
func (s *AwaitingVTXOSignaturesState) hasClientSubmittedSignatures(
	clientID clientconn.ClientID) bool {

	_, exists := s.ClientsWithSignatures[clientID]
	return exists
}

// ServerSigningState is where the server signs its wallet inputs on the
// commitment transaction. This occurs after all clients have submitted their
// boarding input signatures.
type ServerSigningState struct {
	// ClientRegistrations maps client IDs to their registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration

	// PSBT is the funded but unsigned commitment transaction.
	PSBT *psbt.Packet

	// VTXOTrees maps commitment tx output indices to their VTXO trees.
	VTXOTrees map[int]*tree.Tree

	// ConnectorAssignments maps forfeited outpoints to connector leaves.
	// This is nil if no forfeits exist in the round.
	ConnectorAssignments map[wire.OutPoint]*ConnectorLeafAssignment

	// ConnectorDescriptors describe connector outputs for this round.
	// This is nil if no forfeits exist in the round.
	ConnectorDescriptors []*ConnectorTreeDescriptor

	// CollectedSignatures contains all validated client boarding
	// signatures. These will be applied to the PSBT along with the
	// server's signatures.
	CollectedSignatures InputSigsMap

	// CollectedForfeitTxs contains all validated client forfeit
	// transactions. The server will sign these before finalization.
	CollectedForfeitTxs ForfeitTxsMap
}

// String returns a human-readable representation of ServerSigningState.
func (s *ServerSigningState) String() string {
	return "ServerSigningState"
}

// IsTerminal returns false as ServerSigningState is not a terminal state.
func (s *ServerSigningState) IsTerminal() bool {
	return false
}

// stateSealed marks ServerSigningState as implementing the sealed State
// interface.
func (s *ServerSigningState) stateSealed() {}

// AwaitingSignAndFinalizeState waits for the OutboxHandler to sign all
// boarding inputs, complete forfeit transactions, and finalize the PSBT.
// This intermediate state exists so that ServerSigningState remains pure
// — it emits a SignAndFinalizeRoundReq outbox event and transitions
// here, then the handler feeds back a success or failure event.
type AwaitingSignAndFinalizeState struct {
	// ClientRegistrations maps client IDs to their registration data.
	// Carried forward for persistence and failure transitions.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration

	// VTXOTrees maps commitment tx output indices to their VTXO trees.
	// Carried forward for the persistence outbox request.
	VTXOTrees map[int]*tree.Tree

	// ConnectorDescriptors describe connector outputs for this round.
	// Carried forward for the persistence outbox request.
	ConnectorDescriptors []*ConnectorTreeDescriptor

	// SweepKey is the operator public key used in VTXO sweep timeout
	// scripts. Carried forward for the persistence outbox request.
	SweepKey *btcec.PublicKey

	// CSVDelay is the relative timelock for the VTXO sweep timeout
	// path. Carried forward for the persistence outbox request.
	CSVDelay uint32

	// StartHeight is the block height when the round was created.
	// Carried forward for the BroadcastRoundReq.
	StartHeight uint32
}

// String returns a human-readable representation of
// AwaitingSignAndFinalizeState.
func (s *AwaitingSignAndFinalizeState) String() string {
	return "AwaitingSignAndFinalizeState"
}

// IsTerminal returns false as AwaitingSignAndFinalizeState is not a
// terminal state.
func (s *AwaitingSignAndFinalizeState) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingSignAndFinalizeState as implementing the
// sealed State interface.
func (s *AwaitingSignAndFinalizeState) stateSealed() {}

// AwaitingServerSignPersistState waits for the OutboxHandler to persist
// the round and VTXOs after server signing completes. This intermediate
// state exists so that handleServerSigning remains pure — it emits a
// PersistServerSigningReq outbox event and transitions here, then the
// handler feeds back a success or failure event.
type AwaitingServerSignPersistState struct {
	// ClientRegistrations maps client IDs to their registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration

	// FinalTx is the fully signed commitment transaction.
	FinalTx *wire.MsgTx

	// VTXOTrees maps commitment tx output indices to their VTXO trees.
	VTXOTrees map[int]*tree.Tree

	// ForfeitInfos maps forfeited VTXO outpoints to forfeit metadata.
	ForfeitInfos map[wire.OutPoint]*ForfeitInfo

	// StartHeight is the block height when the round was created. Needed
	// to construct the BroadcastRoundReq on persistence success.
	StartHeight uint32
}

// String returns a human-readable representation of
// AwaitingServerSignPersistState.
func (s *AwaitingServerSignPersistState) String() string {
	return "AwaitingServerSignPersistState"
}

// IsTerminal returns false as AwaitingServerSignPersistState is not a
// terminal state.
func (s *AwaitingServerSignPersistState) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingServerSignPersistState as implementing the
// sealed State interface.
func (s *AwaitingServerSignPersistState) stateSealed() {}

// FinalizedState holds the fully signed transaction ready for broadcast. The
// transaction has all boarding input signatures (client + operator) and wallet
// input signatures applied.
type FinalizedState struct {
	// ClientRegistrations maps client IDs to their registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration

	// FinalTx is the fully signed commitment transaction ready for
	// broadcast.
	FinalTx *wire.MsgTx

	// VTXOTrees maps commitment tx output indices to their VTXO trees.
	VTXOTrees map[int]*tree.Tree

	// ForfeitInfos maps forfeited VTXO outpoints to forfeit metadata.
	ForfeitInfos map[wire.OutPoint]*ForfeitInfo
}

// String returns a human-readable representation of FinalizedState.
func (s *FinalizedState) String() string {
	return "FinalizedState"
}

// IsTerminal returns false as FinalizedState is not a terminal state. The
// round waits for confirmation before completing.
func (s *FinalizedState) IsTerminal() bool {
	return false
}

// stateSealed marks FinalizedState as implementing the sealed State interface.
func (s *FinalizedState) stateSealed() {}

// AwaitingConfirmPersistState waits for the OutboxHandler to persist round
// confirmation data (mark VTXOs live, record forfeits, mark round confirmed).
// This intermediate state exists so that the FinalizedState transition remains
// pure — it emits a ConfirmRoundReq outbox event and transitions here, then
// the handler feeds back a success or failure event.
type AwaitingConfirmPersistState struct {
	// ClientRegistrations maps client IDs to their registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration

	// FinalTx is the fully signed commitment transaction.
	FinalTx *wire.MsgTx

	// VTXOTrees maps commitment tx output indices to their VTXO trees.
	VTXOTrees map[int]*tree.Tree

	// BlockHeight is the height of the confirming block.
	BlockHeight int32

	// BlockHash is the hash of the confirming block.
	BlockHash chainhash.Hash
}

// String returns a human-readable representation of
// AwaitingConfirmPersistState.
func (s *AwaitingConfirmPersistState) String() string {
	return "AwaitingConfirmPersistState"
}

// IsTerminal returns false as AwaitingConfirmPersistState is not a terminal
// state.
func (s *AwaitingConfirmPersistState) IsTerminal() bool {
	return false
}

// stateSealed marks AwaitingConfirmPersistState as implementing the sealed
// State interface.
func (s *AwaitingConfirmPersistState) stateSealed() {}

// FailedState is a terminal state indicating the round has failed. When
// entering this state, the FSM emits events to notify clients, unlock
// boarding inputs, and inform the actor of the failure.
type FailedState struct {
	// Reason describes why the round failed.
	Reason string
}

// String returns a human-readable representation of FailedState.
func (s *FailedState) String() string {
	return "FailedState"
}

// IsTerminal returns true as FailedState is a terminal state.
func (s *FailedState) IsTerminal() bool {
	return true
}

// stateSealed marks FailedState as implementing the sealed State interface.
func (s *FailedState) stateSealed() {}

// ConfirmedState is a terminal state reached after the commitment transaction
// has been confirmed on-chain with the required number of confirmations.
type ConfirmedState struct {
	// ClientRegistrations maps client IDs to their registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration

	// FinalTx is the fully signed commitment transaction.
	FinalTx *wire.MsgTx

	// VTXOTrees maps commitment tx output indices to their VTXO trees.
	// This is nil if no VTXOs exist in the round.
	VTXOTrees map[int]*tree.Tree

	// BlockHeight is the height of the block containing the transaction.
	BlockHeight int32

	// BlockHash is the hash of the block containing the transaction.
	BlockHash chainhash.Hash
}

// String returns a human-readable representation of ConfirmedState.
func (s *ConfirmedState) String() string {
	return "ConfirmedState"
}

// IsTerminal returns true as ConfirmedState is a terminal state.
func (s *ConfirmedState) IsTerminal() bool {
	return true
}

// stateSealed marks ConfirmedState as implementing the sealed State interface.
func (s *ConfirmedState) stateSealed() {}

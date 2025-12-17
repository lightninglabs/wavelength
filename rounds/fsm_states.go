package rounds

import (
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/lib/tree"
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
//
// NOTE: for now, we only deal with boarding and leave requests.
// TODO(elle): implement logic for:
//   - forfeit requests
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

	// ChangeOutputIndex is the index of the change output, or -1 if no
	// change was created.
	ChangeOutputIndex int32

	// VTXOTrees maps commitment tx output indices to their VTXO trees.
	// This is nil if no VTXOs exist in the round.
	VTXOTrees map[int]*tree.Tree
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

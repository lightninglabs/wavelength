package rounds

import (
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo/clientconn"
)

// Event is a sealed interface for all events that can be processed by the
// round state machine. The sealed interface pattern prevents external packages
// from implementing this interface, ensuring type safety and exhaustive pattern
// matching in state transitions.
type Event interface {
	// eventSealed is an unexported method that marks this interface as
	// sealed, preventing external implementations.
	eventSealed()
}

// ClientJoinRequestEvent is an event triggered when a client sends a request to
// join the current round.
type ClientJoinRequestEvent struct {
	// ClientID is the identifier of the client making the join request.
	// This should be used to correlate responses back to the client.
	ClientID clientconn.ClientID

	// Request contains the client's full join round request.
	Request *types.JoinRoundRequest
}

// eventSealed marks ClientJoinRequestEvent as implementing the sealed Event
// interface.
func (e *ClientJoinRequestEvent) eventSealed() {}

// SealEvent is an event that tells the FSM to seal the current batch and
// transition to commitment building. After this point, the round will not
// accept new join requests.
type SealEvent struct{}

// eventSealed marks SealEvent as implementing the sealed Event interface.
func (e *SealEvent) eventSealed() {}

// BuildBatchTxEvent triggers commitment transaction PSBT construction. It is
// sent as an internal event when transitioning to BatchBuildingState.
type BuildBatchTxEvent struct{}

// eventSealed marks BuildBatchTxEvent as implementing the sealed Event
// interface.
func (e *BuildBatchTxEvent) eventSealed() {}

// RegistrationTimeoutEvent is sent when the registration phase timeout expires.
// Only RegistrationState should handle this event.
type RegistrationTimeoutEvent struct{}

// eventSealed marks RegistrationTimeoutEvent as implementing the sealed Event
// interface.
func (e *RegistrationTimeoutEvent) eventSealed() {}

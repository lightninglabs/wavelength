package rounds

import (
	"time"

	"github.com/lightninglabs/darepo/clientconn"
	"google.golang.org/protobuf/proto"
)

// OutboxEvent is a sealed interface for all outbox messages emitted
// by the round FSM. The sealed interface pattern prevents external
// packages from implementing this interface, ensuring type safety
// and exhaustive pattern matching in state transitions.
type OutboxEvent interface {
	// outboxEventSealed is an unexported method that marks this interface
	// as sealed, preventing external implementations.
	outboxEventSealed()
}

// ClientErrorResp is an outbox message emitted by the FSM to send
// error responses back to clients via the ClientConnectionActor.
type ClientErrorResp struct {
	// Client is the identifier of the client to send the error to.
	Client clientconn.ClientID

	// ErrorMsg is the error message to send to the client.
	ErrorMsg string
}

// ClientID returns the identifier of the client to send the message to.
func (c *ClientErrorResp) ClientID() clientconn.ClientID {
	return c.Client
}

// ToProto converts ClientErrorResp to a protobuf message.
// TODO: Implement actual proto conversion once proto definitions are available.
func (c *ClientErrorResp) ToProto() proto.Message {
	return nil
}

// outboxEventSealed marks ClientErrorResp as implementing the sealed
// OutboxEvent interface.
func (c *ClientErrorResp) outboxEventSealed() {}

// ClientSuccessResp is an outbox message emitted by the FSM to send
// a successful join response back to a client via the ClientConnectionActor.
type ClientSuccessResp struct {
	// Client is the identifier of the client to send the response to.
	Client clientconn.ClientID

	// RoundID is the identifier of the round the client has joined.
	RoundID RoundID
}

// ClientID returns the identifier of the client to send the message to.
func (c *ClientSuccessResp) ClientID() clientconn.ClientID {
	return c.Client
}

// ToProto converts ClientSuccessResp to a protobuf message.
// TODO: Implement actual proto conversion once proto definitions are available.
func (c *ClientSuccessResp) ToProto() proto.Message {
	return nil
}

// outboxEventSealed marks ClientSuccessResp as implementing the sealed
// OutboxEvent interface.
func (c *ClientSuccessResp) outboxEventSealed() {}

// RoundSealedReq is emitted when a round has been sealed (registration closed).
// The actor should create a new round to accept new registrations.
type RoundSealedReq struct {
	// SealedRoundID is the ID of the round that was just sealed.
	SealedRoundID RoundID
}

// outboxEventSealed marks RoundSealedReq as implementing the sealed OutboxEvent
// interface.
func (r *RoundSealedReq) outboxEventSealed() {}

// StartTimeoutReq is emitted when the FSM wants to start a timeout. The
// duration is specified by the FSM based on the current state's requirements.
// The Phase field identifies which state scheduled this timeout, allowing the
// actor to send the appropriate phase-specific timeout event when it expires.
type StartTimeoutReq struct {
	// RoundID is the identifier of the round to schedule a timeout for.
	RoundID RoundID

	// Phase identifies which FSM phase is scheduling this timeout. This is
	// used to create a composite timeout ID and to determine which timeout
	// event type to send when the timeout expires.
	Phase TimeoutPhase

	// Duration is how long to wait before the timeout fires.
	Duration time.Duration
}

// outboxEventSealed marks StartTimeoutReq as implementing the sealed
// OutboxEvent interface.
func (s *StartTimeoutReq) outboxEventSealed() {}

// newStartTimeoutReq creates a StartTimeoutReq for the given phase. The
// duration is determined by the phase and the environment's terms.
func newStartTimeoutReq(env *Environment, phase TimeoutPhase) *StartTimeoutReq {
	var duration time.Duration

	if phase == TimeoutPhaseRegistration {
		duration = env.Terms.RegistrationTimeout
	}

	return &StartTimeoutReq{
		RoundID:  env.RoundID,
		Phase:    phase,
		Duration: duration,
	}
}

// CancelTimeoutReq is emitted when the FSM wants to cancel a pending timeout.
type CancelTimeoutReq struct {
	// RoundID is the identifier of the round to cancel the timeout for.
	RoundID RoundID

	// Phase identifies which FSM phase timeout to cancel. This is combined
	// with RoundID to form the composite timeout ID.
	Phase TimeoutPhase
}

// outboxEventSealed marks CancelTimeoutReq as implementing the sealed
// OutboxEvent interface.
func (c *CancelTimeoutReq) outboxEventSealed() {}

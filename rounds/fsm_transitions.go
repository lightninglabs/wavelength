package rounds

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightningnetwork/lnd/fn/v2"
)

var (
	// ErrJoinRequestInvalid is returned when a client's join request fails
	// validation.
	ErrJoinRequestInvalid = fmt.Errorf("join request invalid")
)

// unexpectedEvent returns a StateTransition that remains in the current state
// and logs a warning. This is used instead of returning an error to avoid
// crashing the FSM on unexpected events.
func unexpectedEvent(state State, stateName string, event Event,
	env *Environment) *StateTransition {

	env.Log.Warnf("%s: ignoring unexpected event: %T", stateName, event)

	return &StateTransition{
		NextState: state,
	}
}

// clientErrorTransition returns a StateTransition that remains in the current
// state and emits a ClientErrorResp to notify the client of an error.
func clientErrorTransition(state State, clientID ClientID,
	errMsg string) *StateTransition {

	return &StateTransition{
		NextState: state,
		NewEvents: fn.Some(EmittedEvent{
			Outbox: []OutboxEvent{
				&ClientErrorResp{
					Client:   clientID,
					ErrorMsg: errMsg,
				},
			},
		}),
	}
}

// lockBoardingInputs attempts to lock all boarding inputs for a client in the
// BoardingInputLocker. If any lock fails, it returns a StateTransition with
// a ClientErrorResp. If all locks succeed, it returns nil.
func lockBoardingInputs(ctx context.Context, env *Environment,
	inputs []*BoardingInput) error {

	for _, input := range inputs {
		err := env.BoardingInputLocker.Lock(
			ctx, input.Outpoint, env.RoundID,
		)
		if err != nil {
			// If we fail to lock the boarding input, return an
			// error to the client but remain in the current state.
			return fmt.Errorf("failed to lock boarding "+
				"input %v: %v", input.Outpoint, err)
		}
		return err
	}

	return nil
}

// newClientRegistration creates a ClientRegistration from a validated join
// request result.
func newClientRegistration(clientID ClientID,
	result *JoinRequestResult) *ClientRegistration {

	return &ClientRegistration{
		ClientID:        clientID,
		BoardingInputs:  result.BoardingInputs,
		LeaveOutputs:    result.RequiredOutputs,
		VTXODescriptors: result.VTXODescriptors,
	}
}

// ProcessEvent handles the events from the CreatedState state.
//
// Event handling:
//
//   - ClientJoinRequestEvent: Validates the join request. If validation fails,
//     remains in CreatedState and sends ClientErrorResp. On success,
//     transitions to RegistrationState with the first client registered,
//     sends ClientSuccessResp, requests boarding input locks, and starts
//     the registration timeout.
func (s *CreatedState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	switch evt := event.(type) {
	case *ClientJoinRequestEvent:
		// Validate the join request. If this fails, this is not an FSM
		// error, but we should respond to the client accordingly.
		result, err := ValidateJoinRequest(ctx, env, evt.Request)
		if err != nil {
			errMsg := fmt.Sprintf("%v: %v", ErrJoinRequestInvalid,
				err)

			return clientErrorTransition(s, evt.ClientID, errMsg),
				nil
		}

		// Attempt to lock all boarding inputs for this client.
		err = lockBoardingInputs(ctx, env, result.BoardingInputs)
		if err != nil {
			return clientErrorTransition(
				s, evt.ClientID, err.Error(),
			), nil
		}

		// Create the initial client registrations map with the first
		// client.
		reg := newClientRegistration(evt.ClientID, result)
		clientRegs := map[clientconn.ClientID]*ClientRegistration{
			evt.ClientID: reg,
		}

		return &StateTransition{
			NextState: newRegistrationState(clientRegs),
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&ClientSuccessResp{
						Client:  evt.ClientID,
						RoundID: env.RoundID,
					},
					newStartTimeoutReq(
						env, TimeoutPhaseRegistration,
					),
				},
			}),
		}, nil

	default:
		return unexpectedEvent(s, "created", event, env), nil
	}
}

// ProcessEvent handles the events from the RegistrationState state.
//
// Event handling:
//
//   - ClientJoinRequestEvent: Validates the join request. If the client is
//     already registered or validation fails, sends ClientErrorResp. On
//     success, adds the client to registrations, sends ClientSuccessResp,
//     and requests boarding input locks.
//
//   - RegistrationTimeoutEvent: Registration phase timed out. Emits
//     RoundSealedReq to notify actor, then internal SealEvent to seal.
//
//   - SealEvent: Transitions to BatchBuildingState with all accumulated
//     registrations, emits BuildBatchTxEvent to start batch construction.
func (s *RegistrationState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	switch evt := event.(type) {
	case *ClientJoinRequestEvent:
		// Check if client is already registered in this round.
		if s.isClientRegistered(evt.ClientID) {
			return clientErrorTransition(
				s, evt.ClientID, "client already registered",
			), nil
		}

		// Validate the join request.
		result, err := ValidateJoinRequest(ctx, env, evt.Request)
		if err != nil {
			errMsg := fmt.Sprintf("%v: %v", ErrJoinRequestInvalid,
				err)

			return clientErrorTransition(
				s, evt.ClientID, errMsg,
			), nil
		}

		// Attempt to lock all boarding inputs for this client.
		err = lockBoardingInputs(ctx, env, result.BoardingInputs)
		if err != nil {
			return clientErrorTransition(
				s, evt.ClientID, err.Error(),
			), nil
		}

		return &StateTransition{
			NextState: s.withNewClient(evt.ClientID, result),
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&ClientSuccessResp{
						Client:  evt.ClientID,
						RoundID: env.RoundID,
					},
				},
			}),
		}, nil

	case *RegistrationTimeoutEvent:
		// Registration timeout expired. Emit internal SealEvent to seal
		// the round and outbox RoundSealedReq to notify actor.
		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				InternalEvent: []Event{
					&SealEvent{},
				},
				Outbox: []OutboxEvent{
					&RoundSealedReq{
						SealedRoundID: env.RoundID,
					},
				},
			}),
		}, nil

	case *SealEvent:
		// Registration is closed. Transition to BatchBuildingState with
		// internal event to trigger PSBT construction.
		return &StateTransition{
			NextState: &BatchBuildingState{
				ClientRegistrations: s.ClientRegistrations,
			},
			NewEvents: fn.Some(EmittedEvent{
				InternalEvent: []Event{
					&BuildBatchTxEvent{},
				},
			}),
		}, nil

	default:
		return unexpectedEvent(s, "registration", event, env), nil
	}
}

// ProcessEvent handles the events from the BatchBuildingState state.
// This is a placeholder that will be fully implemented later.
func (s *BatchBuildingState) ProcessEvent(_ context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	switch event.(type) {
	case *BuildBatchTxEvent:
		// TODO: Implement batch building logic.
		// For now, just stay in this state.
		return &StateTransition{
			NextState: s,
		}, nil

	default:
		return unexpectedEvent(s, "batch-building", event, env), nil
	}
}

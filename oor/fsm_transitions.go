package oor

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// unexpectedEvent returns a StateTransition that remains in the current state
// and emits no outbox work for an unexpected event.
//
// We prefer this to returning an error because unexpected events can be a
// normal consequence of retries, timeouts, or races at the actor boundary.
func unexpectedEvent(state State, stateName string,
	event Event, env *Environment) *StateTransition {

	if env != nil && env.Log != nil {
		env.Log.WarnS(context.Background(), "Ignoring unexpected event", nil,
			slog.String("state", stateName),
			slog.String("event_type", fmt.Sprintf("%T", event)),
		)
	}

	return &StateTransition{
		NextState: state,
		NewEvents: fn.None[EmittedEvent](),
	}
}

// ProcessEvent handles events for IdleState.
func (s *IdleState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch evt := event.(type) {
	case *SubmitRequestedEvent:
		inputs, err := CollectCheckpointInputs(evt.CheckpointPSBTs)
		if err != nil {
			return nil, err
		}

		return &StateTransition{
			NextState: &RequestedState{
				Inputs:          inputs,
				ArkPSBT:         evt.ArkPSBT,
				CheckpointPSBTs: evt.CheckpointPSBTs,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&LockInputsReq{
						Inputs: inputs,
					},
				},
			}),
		}, nil

	default:
		return unexpectedEvent(s, s.String(), event, env), nil
	}
}

// ProcessEvent handles events for RequestedState.
func (s *RequestedState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch event.(type) {
	case *InputsLockedEvent:
		return &StateTransition{
			NextState: &ValidatedState{
				Inputs:          s.Inputs,
				ArkPSBT:         s.ArkPSBT,
				CheckpointPSBTs: s.CheckpointPSBTs,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{&CoSignReq{}},
			}),
		}, nil

	default:
		return unexpectedEvent(s, s.String(), event, env), nil
	}
}

// ProcessEvent handles events for ValidatedState.
func (s *ValidatedState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch event.(type) {
	case *OperatorSignedEvent:
		return &StateTransition{
			NextState: &CoSignedState{
				ArkPSBT: s.ArkPSBT,
			},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	case *SignFailedEvent:
		return &StateTransition{
			NextState: &FailedState{
				Reason: "sign failed",
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{&UnlockInputsReq{
					Inputs: s.Inputs,
				}},
			}),
		}, nil

	default:
		return unexpectedEvent(s, s.String(), event, env), nil
	}
}

// ProcessEvent handles events for CoSignedState.
//
// CoSignedState is the point-of-no-return. After this point, input VTXOs must
// not be unlocked by this session FSM.
func (s *CoSignedState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch evt := event.(type) {
	case *FinalizeRequestedEvent:
		if len(evt.FinalCheckpointPSBTs) == 0 {
			return nil, fmt.Errorf("final checkpoints must be " +
				"provided")
		}

		return &StateTransition{
			NextState: &AwaitingFinalCheckpointsState{},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&ValidateFinalizeReq{
						ArkPSBT: s.ArkPSBT,
						FinalCheckpointPSBTs: evt.
							FinalCheckpointPSBTs,
					},
				},
			}),
		}, nil

	case *SignFailedEvent:
		return &StateTransition{
			NextState: &FailedState{
				Reason: "sign failed after cosign",
			},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedEvent(s, s.String(), event, env), nil
	}
}

// ProcessEvent handles events for AwaitingFinalCheckpointsState.
func (s *AwaitingFinalCheckpointsState) ProcessEvent(ctx context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch event.(type) {
	case *FinalizeValidatedEvent:
		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{&FinalizeReq{}},
			}),
		}, nil

	case *FinalizeSucceededEvent:
		return &StateTransition{
			NextState: &FinalizedState{},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	case *FinalizeFailedEvent:
		return &StateTransition{
			NextState: &FailedState{
				Reason: "finalize failed",
			},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedEvent(s, s.String(), event, env), nil
	}
}

// ProcessEvent handles events for FinalizedState.
func (s *FinalizedState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	return unexpectedEvent(s, s.String(), event, env), nil
}

// ProcessEvent handles events for FailedState.
func (s *FailedState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	return unexpectedEvent(s, s.String(), event, env), nil
}

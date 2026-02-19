package oor

import (
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// unexpectedEvent returns a StateTransition that remains in the current state
// and emits no outbox work for an unexpected event.
//
// We prefer this to returning an error because unexpected events can be a
// normal consequence of retries, timeouts, or races at the actor boundary.
func unexpectedEvent(state State) *StateTransition {
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
			NextState: &AwaitingInputsLockState{
				Inputs:          inputs,
				ArkPSBT:         evt.ArkPSBT,
				CheckpointPSBTs: evt.CheckpointPSBTs,
				VTXOSigningDescriptors: evt.
					VTXOSigningDescriptors,
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
		return unexpectedEvent(s), nil
	}
}

// ProcessEvent handles events for AwaitingInputsLockState.
func (s *AwaitingInputsLockState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch evt := event.(type) {
	case *InputsLockSucceededEvent:
		validateReq := &ValidateSubmitReq{
			ArkPSBT:         s.ArkPSBT,
			CheckpointPSBTs: s.CheckpointPSBTs,
		}

		return &StateTransition{
			NextState: &AwaitingSubmitValidationState{
				Inputs:          s.Inputs,
				ArkPSBT:         s.ArkPSBT,
				CheckpointPSBTs: s.CheckpointPSBTs,
				VTXOSigningDescriptors: s.
					VTXOSigningDescriptors,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					validateReq,
				},
			}),
		}, nil

	case *InputsLockFailedEvent:
		return &StateTransition{
			NextState: &FailedState{
				Reason: evt.Reason,
			},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedEvent(s), nil
	}
}

// ProcessEvent handles events for AwaitingSubmitValidationState.
func (s *AwaitingSubmitValidationState) ProcessEvent(ctx context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch evt := event.(type) {
	case *SubmitValidatedEvent:

		return &StateTransition{
			NextState: &ValidatedState{
				Inputs:          s.Inputs,
				ArkPSBT:         s.ArkPSBT,
				CheckpointPSBTs: s.CheckpointPSBTs,
				VTXOSigningDescriptors: s.
					VTXOSigningDescriptors,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&CoSignReq{
						Inputs:  s.Inputs,
						ArkPSBT: s.ArkPSBT,
						CheckpointPSBTs: s.
							CheckpointPSBTs,
						VTXOSigningDescriptors: s.
							VTXOSigningDescriptors,
					},
				},
			}),
		}, nil

	case *SubmitFailedEvent:
		return &StateTransition{
			NextState: &FailedState{
				Reason: evt.Reason,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&UnlockInputsReq{
						Inputs: s.Inputs,
					},
				},
			}),
		}, nil

	default:
		return unexpectedEvent(s), nil
	}
}

// ProcessEvent handles events for ValidatedState.
func (s *ValidatedState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch evt := event.(type) {
	case *OperatorSignedEvent:
		return &StateTransition{
			NextState: &CoSignedState{
				Inputs:                  s.Inputs,
				ArkPSBT:                 s.ArkPSBT,
				CoSignedCheckpointPSBTs: s.CheckpointPSBTs,
			},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	case *SignFailedEvent:
		return &StateTransition{
			NextState: &FailedState{
				Reason: evt.Reason,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&UnlockInputsReq{
						Inputs: s.Inputs,
					},
				},
			}),
		}, nil

	default:
		return unexpectedEvent(s), nil
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

		finalCheckpoints := evt.FinalCheckpointPSBTs
		validateFinalizeReq := &ValidateFinalizeReq{
			ArkPSBT: s.ArkPSBT,
		}
		validateFinalizeReq.FinalCheckpointPSBTs = finalCheckpoints

		return &StateTransition{
			NextState: &AwaitingFinalizeValidationState{
				Inputs:  s.Inputs,
				ArkPSBT: s.ArkPSBT,
				CoSignedCheckpointPSBTs: s.
					CoSignedCheckpointPSBTs,
				FinalCheckpointPSBTs: finalCheckpoints,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					validateFinalizeReq,
				},
			}),
		}, nil

	case *SignFailedEvent:
		return &StateTransition{
			NextState: &FailedState{
				Reason: evt.Reason,
			},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedEvent(s), nil
	}
}

// ProcessEvent handles events for AwaitingFinalizeValidationState.
func (s *AwaitingFinalizeValidationState) ProcessEvent(ctx context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch evt := event.(type) {
	case *FinalizeValidatedEvent:
		finalizeReq := &FinalizeReq{
			ArkPSBT:              s.ArkPSBT,
			FinalCheckpointPSBTs: s.FinalCheckpointPSBTs,
			Inputs:               s.Inputs,
		}

		return &StateTransition{
			// Re-enter CoSignedState so FinalizeSucceededEvent and
			// FinalizeFailedEvent share one post-validation path.
			NextState: &CoSignedState{
				Inputs:  s.Inputs,
				ArkPSBT: s.ArkPSBT,
				CoSignedCheckpointPSBTs: s.
					CoSignedCheckpointPSBTs,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					finalizeReq,
				},
			}),
		}, nil

	case *FinalizeSucceededEvent:
		return &StateTransition{
			NextState: &FinalizedState{},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&NotifyRecipientsReq{
						ArkPSBT: s.ArkPSBT,
					},
				},
			}),
		}, nil

	case *FinalizeFailedEvent:
		return &StateTransition{
			NextState: &FailedState{
				Reason: evt.Reason,
			},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedEvent(s), nil
	}
}

// ProcessEvent handles events for FinalizedState.
func (s *FinalizedState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	return unexpectedEvent(s), nil
}

// ProcessEvent handles events for FailedState.
func (s *FailedState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	return unexpectedEvent(s), nil
}

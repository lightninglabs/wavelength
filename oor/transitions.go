package oor

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// unexpectedEvent returns a transition that stays in the current state and
// emits no outbox work for an unexpected event.
//
// This makes the FSM resilient to retries and late deliveries at the actor
// boundary.
func unexpectedEvent(state State, event Event) *StateTransition {
	_ = event

	return &StateTransition{
		NextState: state,
		NewEvents: fn.None[EmittedEvent](),
	}
}

// ProcessEvent handles events for Idle.
func (s *Idle) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch evt := event.(type) {
	case *StartTransferEvent:
		inputOutpoints := make([]wire.OutPoint, 0, len(evt.VTXOInputs))
		for i := range evt.VTXOInputs {
			inputOutpoints = append(
				inputOutpoints, evt.VTXOInputs[i].Outpoint,
			)
		}

		// Build a deterministic submit package:
		// - checkpoint txs convert VTXOs into checkpoints
		// - an Ark tx spends checkpoints and pays recipients
		//
		// The Ark txid becomes the stable session identifier.
		ark, checkpoints, err := buildSubmitPackage(
			evt.Policy,
			evt.VTXOInputs,
			evt.RecipientOutputs,
		)
		if err != nil {
			return nil, err
		}

		return &StateTransition{
			NextState: &AwaitingSubmitAccepted{
				InputOutpoints:  inputOutpoints,
				ArkPSBT:         ark,
				CheckpointPSBTs: checkpoints,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&SendSubmitPackageRequest{
						ArkPSBT:         ark,
						CheckpointPSBTs: checkpoints,
					},
				},
			}),
		}, nil

	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{Reason: evt.Reason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedEvent(s, event), nil
	}
}

// ProcessEvent handles events for AwaitingSubmitAccepted.
func (s *AwaitingSubmitAccepted) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch evt := event.(type) {
	case *SubmitAcceptedEvent:
		// Point-of-no-return: operator has co-signed checkpoints.
		//
		// After a crash, client must re-obtain co-signed bytes.
		if evt.ArkPSBT == nil || evt.ArkPSBT.UnsignedTx == nil {
			return nil, fmt.Errorf("ark psbt must be provided")
		}

		if s.ArkPSBT == nil || s.ArkPSBT.UnsignedTx == nil {
			return nil, fmt.Errorf("internal: missing ark psbt")
		}

		stateTxid := s.ArkPSBT.UnsignedTx.TxHash()
		evTxid := evt.ArkPSBT.UnsignedTx.TxHash()
		if stateTxid != evTxid {
			return nil, fmt.Errorf("ark txid mismatch")
		}

		if len(evt.CoSignedCheckpointPSBTs) == 0 {
			return nil, fmt.Errorf("co-signed checkpoints required")
		}

		checkpoints := evt.CoSignedCheckpointPSBTs

		// Signature material is produced outside the FSM.
		// The actor boundary uses a wallet to finalize checkpoints.
		return &StateTransition{
			NextState: &AwaitingCheckpointSignatures{
				SessionID:               evt.SessionID,
				InputOutpoints:          s.InputOutpoints,
				ArkPSBT:                 evt.ArkPSBT,
				CoSignedCheckpointPSBTs: checkpoints,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&RequestCheckpointSignatures{
						ArkPSBT: evt.ArkPSBT,
						CoSignedCheckpointPSBTs: evt.
							CoSignedCheckpointPSBTs,
					},
				},
			}),
		}, nil

	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{Reason: evt.Reason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedEvent(s, event), nil
	}
}

// ProcessEvent handles events for AwaitingCheckpointSignatures.
func (s *AwaitingCheckpointSignatures) ProcessEvent(ctx context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch evt := event.(type) {
	case *CheckpointsSignedEvent:
		if s.ArkPSBT == nil {
			return nil, fmt.Errorf("internal: missing ark psbt")
		}

		if len(evt.FinalCheckpointPSBTs) == 0 {
			return nil, fmt.Errorf("final checkpoints required")
		}

		// Finalize binds tap tree metadata onto checkpoints.
		err := oortx.ApplyFinalizeData(
			s.ArkPSBT, evt.FinalCheckpointPSBTs,
		)
		if err != nil {
			return nil, err
		}

		// Validate finalize package before emitting request.
		err = oortx.ValidateFinalizePackage(
			s.ArkPSBT, evt.FinalCheckpointPSBTs,
		)
		if err != nil {
			return nil, err
		}

		return &StateTransition{
			NextState: &AwaitingFinalizeAccepted{
				SessionID:            s.SessionID,
				InputOutpoints:       s.InputOutpoints,
				ArkPSBT:              s.ArkPSBT,
				FinalCheckpointPSBTs: evt.FinalCheckpointPSBTs,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&SendFinalizePackageRequest{
						ArkPSBT: s.ArkPSBT,
						FinalCheckpointPSBTs: evt.
							FinalCheckpointPSBTs,
					},
				},
			}),
		}, nil

	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{Reason: evt.Reason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedEvent(s, event), nil
	}
}

// ProcessEvent handles events for AwaitingFinalizeAccepted.
func (s *AwaitingFinalizeAccepted) ProcessEvent(ctx context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch evt := event.(type) {
	case *FinalizeAcceptedEvent:
		_ = evt

		return &StateTransition{
			NextState: &AwaitingLocalVTXOUpdate{
				SessionID:      s.SessionID,
				InputOutpoints: s.InputOutpoints,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&MarkInputsSpentRequest{
						Outpoints: s.InputOutpoints,
					},
				},
			}),
		}, nil

	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{Reason: evt.Reason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedEvent(s, event), nil
	}
}

// ProcessEvent handles events for AwaitingLocalVTXOUpdate.
func (s *AwaitingLocalVTXOUpdate) ProcessEvent(ctx context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch evt := event.(type) {
	case *InputsMarkedSpentEvent:
		_ = evt

		return &StateTransition{
			NextState: &Completed{},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{Reason: evt.Reason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedEvent(s, event), nil
	}
}

// ProcessEvent handles events for Completed.
func (s *Completed) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	return unexpectedEvent(s, event), nil
}

// ProcessEvent handles events for Failed.
func (s *Failed) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	return unexpectedEvent(s, event), nil
}

// buildSubmitPackage constructs a v0 OOR submit package using the shared
// darepo-client lib/tx/oor primitives.
func buildSubmitPackage(policy scripts.CheckpointPolicy,
	inputs []oortx.CheckpointInput,
	outputs []oortx.RecipientOutput) (*psbt.Packet, []*psbt.Packet, error) {

	if len(inputs) == 0 {
		return nil, nil, fmt.Errorf("checkpoint inputs required")
	}

	checkpoints := make([]*psbt.Packet, 0, len(inputs))
	checkpointOuts := make([]oortx.CheckpointOutput, 0, len(inputs))

	for i := range inputs {
		result, err := oortx.BuildCheckpointPSBT(policy, inputs[i])
		if err != nil {
			return nil, nil, err
		}

		checkpoints = append(checkpoints, result.PSBT)

		txid := result.PSBT.UnsignedTx.TxHash()
		cpOut := result.PSBT.UnsignedTx.TxOut[0]

		checkpointOuts = append(checkpointOuts,
			oortx.CheckpointOutput{
				Txid:           txid,
				Output:         cpOut,
				TapTreeEncoded: result.TapTreeEncoded,
			},
		)
	}

	ark, err := oortx.BuildArkPSBT(checkpointOuts, outputs)
	if err != nil {
		return nil, nil, err
	}

	return ark, checkpoints, nil
}

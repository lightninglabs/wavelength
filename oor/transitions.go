package oor

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// defaultRetryDelay is used when an outbox error is retryable but does not
// specify an explicit backoff.
const defaultRetryDelay = 1 * time.Second

// unexpectedEvent returns a transition that stays in the current state and
// emits no outbox work for an unexpected event.
//
// This makes the FSM resilient to retries and late deliveries at the actor
// boundary.
func unexpectedEvent(state State) *StateTransition {
	return &StateTransition{
		NextState: state,
		NewEvents: fn.None[EmittedEvent](),
	}
}

// ProcessEvent handles events for Idle.
func (s *Idle) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx

	switch evt := event.(type) {
	case *StartTransferEvent:
		// Build a deterministic submit package:
		// - checkpoint txs convert VTXOs into checkpoints
		// - an Ark tx spends checkpoints and pays recipients
		//
		// The Ark txid is the stable v0 session identifier.
		ark, checkpoints, err := buildSubmitPackage(
			evt.Policy,
			evt.VTXOInputs,
			evt.RecipientOutputs,
		)
		if err != nil {
			return nil, err
		}

		if ark == nil || ark.UnsignedTx == nil {
			return nil, fmt.Errorf("ark psbt must be provided")
		}

		if len(checkpoints) == 0 {
			return nil, fmt.Errorf("checkpoint psbts must be " +
				"provided")
		}

		inputOutpoints := make([]wire.OutPoint, 0, len(evt.VTXOInputs))
		for i := range evt.VTXOInputs {
			if evt.VTXOInputs[i].VTXO == nil {
				return nil, fmt.Errorf(
					"checkpoint input vtxo required",
				)
			}
			inputOutpoints = append(
				inputOutpoints, evt.VTXOInputs[i].VTXO.Outpoint,
			)
		}

		// If the FSM environment is already bound to a stable session
		// id (for example via `NewSessionFromSnapshot`), verify the
		// derived Ark txid matches.
		//
		// A mismatch here is a correctness bug. It implies either:
		// - non-deterministic tx building; or
		// - inconsistent input/recipient reconstruction from state.
		if env != nil && env.SessionID != (SessionID{}) {
			sessionID, err := sessionIDFromArk(ark)
			if err != nil {
				return nil, err
			}

			if sessionID != env.SessionID {
				return nil, fmt.Errorf("ark txid mismatch " +
					"with session id")
			}
		}

		signReq := &RequestArkSignatures{
			ArkPSBT:        ark,
			TransferInputs: evt.VTXOInputs,
		}

		return &StateTransition{
			NextState: &AwaitingArkSignatures{
				InputOutpoints:  inputOutpoints,
				ArkPSBT:         ark,
				CheckpointPSBTs: checkpoints,
				TransferInputs:  evt.VTXOInputs,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					signReq,
				},
			}),
		}, nil

	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{Reason: evt.Reason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedEvent(s), nil
	}
}

// ProcessEvent handles events for AwaitingArkSignatures.
func (s *AwaitingArkSignatures) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch evt := event.(type) {
	case *ArkSignedEvent:
		if evt.ArkPSBT == nil || evt.ArkPSBT.UnsignedTx == nil {
			return nil, fmt.Errorf(
				"signed ark psbt must be provided",
			)
		}

		if s.ArkPSBT == nil || s.ArkPSBT.UnsignedTx == nil {
			return nil, fmt.Errorf("internal: missing ark psbt")
		}

		originalTxid := s.ArkPSBT.UnsignedTx.TxHash()
		signedTxid := evt.ArkPSBT.UnsignedTx.TxHash()
		if signedTxid != originalTxid {
			return nil, fmt.Errorf("ark txid mismatch")
		}

		submitReq := &SendSubmitPackageRequest{
			ArkPSBT:         evt.ArkPSBT,
			CheckpointPSBTs: s.CheckpointPSBTs,
			TransferInputs:  s.TransferInputs,
		}

		return &StateTransition{
			NextState: &AwaitingSubmitAccepted{
				InputOutpoints:  s.InputOutpoints,
				ArkPSBT:         evt.ArkPSBT,
				CheckpointPSBTs: s.CheckpointPSBTs,
				TransferInputs:  s.TransferInputs,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					submitReq,
				},
			}),
		}, nil

	case *OutboxErrorEvent:
		return handleOutboxError(env, s, evt)

	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{Reason: evt.Reason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedEvent(s), nil
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
		if evt.SessionID != SessionID(stateTxid) {
			return nil, fmt.Errorf(
				"submit accepted session id mismatch",
			)
		}

		evTxid := evt.ArkPSBT.UnsignedTx.TxHash()
		if stateTxid != evTxid {
			return nil, fmt.Errorf("ark txid mismatch")
		}

		if len(evt.CoSignedCheckpointPSBTs) == 0 {
			return nil, fmt.Errorf("co-signed checkpoints required")
		}

		checkpoints := evt.CoSignedCheckpointPSBTs

		// Signature material is produced outside the FSM.
		// Ask the outbox boundary to attach client checkpoint
		// signatures.
		signReq := &RequestCheckpointSignatures{
			ArkPSBT:                 evt.ArkPSBT,
			CoSignedCheckpointPSBTs: evt.CoSignedCheckpointPSBTs,
			TransferInputs:          s.TransferInputs,
		}

		return &StateTransition{
			NextState: &AwaitingCheckpointSignatures{
				SessionID:               evt.SessionID,
				InputOutpoints:          s.InputOutpoints,
				ArkPSBT:                 evt.ArkPSBT,
				CoSignedCheckpointPSBTs: checkpoints,
				TransferInputs:          s.TransferInputs,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					signReq,
				},
			}),
		}, nil

	case *OutboxErrorEvent:
		return handleOutboxError(env, s, evt)

	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{Reason: evt.Reason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedEvent(s), nil
	}
}

// ProcessEvent handles events for AwaitingCheckpointSignatures.
func (s *AwaitingCheckpointSignatures) ProcessEvent(ctx context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch evt := event.(type) {
	case *CheckpointsSignedEvent:
		// The client has signed the checkpoint PSBTs (owner leaf).
		//
		// Next it binds finalize metadata to the Ark PSBT and submits
		// the finalize package.
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
					// Finalize is a pure "ack" boundary
					// in v0.
					//
					// Retry and resume after crash is safe
					// because the package is deterministic.
					//
					// The server should handle duplicates
					// idempotently.
					&SendFinalizePackageRequest{
						ArkPSBT: s.ArkPSBT,
						FinalCheckpointPSBTs: evt.
							FinalCheckpointPSBTs,
					},
				},
			}),
		}, nil

	case *OutboxErrorEvent:
		return handleOutboxError(env, s, evt)

	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{Reason: evt.Reason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedEvent(s), nil
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

	case *OutboxErrorEvent:
		return handleOutboxError(env, s, evt)

	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{Reason: evt.Reason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedEvent(s), nil
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

	case *OutboxErrorEvent:
		return handleOutboxError(env, s, evt)

	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{Reason: evt.Reason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedEvent(s), nil
	}
}

// ProcessEvent handles events for RetryBackoff.
func (s *RetryBackoff) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch evt := event.(type) {
	case *RetryDueEvent:
		_ = evt

		if s.ResumeSnapshot == nil {
			return nil, fmt.Errorf(
				"resume snapshot must be provided",
			)
		}

		nextState, err := OutgoingStateFromSnapshot(s.ResumeSnapshot)
		if err != nil {
			return nil, err
		}

		nextOutbox, err := OutboxForState(nextState)
		if err != nil {
			return nil, err
		}

		return &StateTransition{
			NextState: nextState,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: nextOutbox,
			}),
		}, nil
	case *FailEvent:
		return &StateTransition{
			NextState: &Failed{Reason: evt.Reason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedEvent(s), nil
	}
}

// ProcessEvent handles events for Completed.
func (s *Completed) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	return unexpectedEvent(s), nil
}

// ProcessEvent handles events for Failed.
func (s *Failed) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	return unexpectedEvent(s), nil
}

// handleOutboxError transitions the FSM into a RetryBackoff state if the error
// is retryable, otherwise returning a terminal failure.
func handleOutboxError(env *Environment, current State,
	evt *OutboxErrorEvent) (*StateTransition, error) {

	if evt == nil {
		return nil, fmt.Errorf("outbox error event must be provided")
	}

	if !evt.Retryable {
		return &StateTransition{
			NextState: &Failed{Reason: evt.ErrorReason},
			NewEvents: fn.None[EmittedEvent](),
		}, nil
	}

	if env == nil || env.SessionID == (SessionID{}) {
		return nil, fmt.Errorf("internal: missing session id")
	}

	snap, err := NewOutgoingSnapshot(env.SessionID, current)
	if err != nil {
		return nil, err
	}

	after := evt.RetryAfter
	if after == 0 {
		after = defaultRetryDelay
	}

	return &StateTransition{
		NextState: &RetryBackoff{
			ResumeSnapshot: snap,
			RetryAfter:     after,
			Reason:         evt.ErrorReason,
		},
		NewEvents: fn.Some(EmittedEvent{
			Outbox: []OutboxEvent{
				&ScheduleRetryRequest{
					After:  after,
					Reason: evt.ErrorReason,
				},
			},
		}),
	}, nil
}

// buildSubmitPackage constructs a v0 OOR submit package using the shared
// darepo-client lib/tx/oor primitives.
func buildSubmitPackage(policy scripts.CheckpointPolicy,
	inputs []TransferInput,
	outputs []oortx.RecipientOutput) (*psbt.Packet, []*psbt.Packet, error) {

	if len(inputs) == 0 {
		return nil, nil, fmt.Errorf("checkpoint inputs required")
	}

	checkpoints := make([]*psbt.Packet, 0, len(inputs))
	checkpointOuts := make([]oortx.CheckpointOutput, 0, len(inputs))

	for i := range inputs {
		checkpointInput, err := inputs[i].CheckpointInput()
		if err != nil {
			return nil, nil, err
		}

		result, err := oortx.BuildCheckpointPSBT(
			policy, checkpointInput,
		)
		if err != nil {
			return nil, nil, err
		}

		checkpoints = append(checkpoints, result.PSBT)

		checkpointOut, err := result.ToCheckpointOutput()
		if err != nil {
			return nil, nil, err
		}

		checkpointOuts = append(checkpointOuts, checkpointOut)
	}

	ark, err := oortx.BuildArkPSBT(checkpointOuts, outputs)
	if err != nil {
		return nil, nil, err
	}

	return ark, checkpoints, nil
}

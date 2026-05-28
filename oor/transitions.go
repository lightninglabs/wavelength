package oor

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
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

// failedState preserves the caller intent key when a running outgoing session
// enters a terminal failure state.
func failedState(reason string, current State) *Failed {
	return &Failed{
		Reason:         reason,
		IdempotencyKey: stateIdempotencyKey(current),
	}
}

func stateIdempotencyKey(state State) string {
	switch s := state.(type) {
	case *AwaitingArkSignatures:
		return s.IdempotencyKey

	case *AwaitingSubmitAccepted:
		return s.IdempotencyKey

	case *AwaitingCheckpointSignatures:
		return s.IdempotencyKey

	case *AwaitingFinalizeAccepted:
		return s.IdempotencyKey

	case *AwaitingLocalVTXOUpdate:
		return s.IdempotencyKey

	case *Completed:
		return s.IdempotencyKey

	case *Failed:
		return s.IdempotencyKey

	default:
		// Idle and incoming states carry no outgoing caller intent key.
		return ""
	}
}

// ProcessEvent handles events for Idle.
func (s *Idle) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx

	switch evt := event.(type) {
	case *StartTransferEvent:
		canonicalRecipients := oortx.CanonicalRecipientOutputs(
			evt.RecipientOutputs,
		)

		// Build a deterministic submit package:
		// - checkpoint txs convert VTXOs into checkpoints
		// - an Ark tx spends checkpoints and pays recipients
		//
		// The Ark txid is the stable v0 session identifier.
		ark, checkpoints, err := buildSubmitPackage(
			evt.Policy, evt.VTXOInputs, canonicalRecipients,
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

		for i := range evt.VTXOInputs {
			if evt.VTXOInputs[i].VTXO == nil {
				return nil, fmt.Errorf("checkpoint input " +
					"vtxo required")
			}
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
			ArkPSBT:         ark,
			CheckpointPSBTs: checkpoints,
			TransferInputs:  evt.VTXOInputs,
		}

		return &StateTransition{
			NextState: &AwaitingArkSignatures{
				ArkPSBT:          ark,
				CheckpointPSBTs:  checkpoints,
				TransferInputs:   evt.VTXOInputs,
				RecipientOutputs: canonicalRecipients,
				IdempotencyKey:   evt.IdempotencyKey,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					signReq,
				},
			}),
		}, nil

	case *FailEvent:
		return &StateTransition{
			NextState: failedState(evt.Reason, s),
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
			return nil, fmt.Errorf("signed ark psbt must be " +
				"provided")
		}

		if s.ArkPSBT == nil || s.ArkPSBT.UnsignedTx == nil {
			return nil, fmt.Errorf("internal: missing ark psbt")
		}

		originalTxid := s.ArkPSBT.UnsignedTx.TxHash()
		signedTxid := evt.ArkPSBT.UnsignedTx.TxHash()
		if signedTxid != originalTxid {
			return nil, fmt.Errorf("ark txid mismatch")
		}

		// Validate structural correctness only. Full script VM
		// validation is not possible here because the Ark tx only
		// carries the client's half of the 2-of-2 checkpoint collab
		// signature. The operator adds their half during submit.
		_, err := oortx.ValidateSubmitPackage(
			evt.ArkPSBT, s.CheckpointPSBTs,
		)
		if err != nil {
			return nil, err
		}

		submitReq := &SendSubmitPackageRequest{
			ArkPSBT:         evt.ArkPSBT,
			CheckpointPSBTs: s.CheckpointPSBTs,
			TransferInputs:  s.TransferInputs,
			Recipients:      s.RecipientOutputs,
		}

		return &StateTransition{
			NextState: &AwaitingSubmitAccepted{
				ArkPSBT:          evt.ArkPSBT,
				CheckpointPSBTs:  s.CheckpointPSBTs,
				TransferInputs:   s.TransferInputs,
				RecipientOutputs: s.RecipientOutputs,
				IdempotencyKey:   s.IdempotencyKey,
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
			NextState: failedState(evt.Reason, s),
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
			return nil, fmt.Errorf("submit accepted session id " +
				"mismatch")
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
				ArkPSBT:                 evt.ArkPSBT,
				CoSignedCheckpointPSBTs: checkpoints,
				TransferInputs:          s.TransferInputs,
				IdempotencyKey:          s.IdempotencyKey,
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
			NextState: failedState(evt.Reason, s),
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

		// Validate finalize package before emitting request.
		err := oortx.ValidateFinalizePackageSigned(
			s.ArkPSBT, evt.FinalCheckpointPSBTs,
		)
		if err != nil {
			return nil, err
		}

		return &StateTransition{
			NextState: &AwaitingFinalizeAccepted{
				SessionID:            s.SessionID,
				ArkPSBT:              s.ArkPSBT,
				FinalCheckpointPSBTs: evt.FinalCheckpointPSBTs,
				TransferInputs:       s.TransferInputs,
				IdempotencyKey:       s.IdempotencyKey,
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
			NextState: failedState(evt.Reason, s),
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
				TransferInputs: s.TransferInputs,
				IdempotencyKey: s.IdempotencyKey,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&MarkInputsSpentRequest{
						Outpoints: InputOutpoints(
							s.TransferInputs,
						),
					},
				},
			}),
		}, nil

	case *OutboxErrorEvent:
		return handleOutboxError(env, s, evt)

	case *FailEvent:
		return &StateTransition{
			NextState: failedState(evt.Reason, s),
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	default:
		return unexpectedEvent(s), nil
	}
}

// ProcessEvent handles events for AwaitingLocalVTXOUpdate.
func (s *AwaitingLocalVTXOUpdate) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	_ = ctx
	_ = env

	switch evt := event.(type) {
	case *InputsMarkedSpentEvent:
		_ = evt

		return &StateTransition{
			NextState: &Completed{
				IdempotencyKey: s.IdempotencyKey,
			},
			NewEvents: fn.None[EmittedEvent](),
		}, nil

	case *OutboxErrorEvent:
		return handleOutboxError(env, s, evt)

	case *FailEvent:
		return &StateTransition{
			NextState: failedState(evt.Reason, s),
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

// handleOutboxError emits retry scheduling for retryable errors while keeping
// the FSM in the current protocol state.
func handleOutboxError(env *Environment, current State,
	evt *OutboxErrorEvent) (*StateTransition, error) {

	if evt == nil {
		return nil, fmt.Errorf("outbox error event must be provided")
	}

	if !evt.Retryable {
		return &StateTransition{
			NextState: failedState(evt.ErrorReason, current),
			NewEvents: fn.None[EmittedEvent](),
		}, nil
	}

	after := evt.RetryAfter
	if after == 0 {
		after = defaultRetryDelay
	}

	return &StateTransition{
		NextState: current,
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
func buildSubmitPackage(policy arkscript.CheckpointPolicy,
	inputs []TransferInput, outputs []oortx.RecipientOutput) (*psbt.Packet,
	[]*psbt.Packet, error) {

	return BuildSubmitPackage(policy, inputs, outputs)
}

// BuildSubmitPackage constructs a v0 OOR submit package using the shared
// darepo-client lib/tx/oor primitives. It only builds deterministic PSBTs from
// caller-provided inputs and outputs; it does not acquire wallet locks, select
// wallet inputs, or persist OOR session state.
func BuildSubmitPackage(policy arkscript.CheckpointPolicy,
	inputs []TransferInput, outputs []oortx.RecipientOutput) (*psbt.Packet,
	[]*psbt.Packet, error) {

	if len(inputs) == 0 {
		return nil, nil, fmt.Errorf("checkpoint inputs required")
	}

	checkpoints := make([]*psbt.Packet, 0, len(inputs))
	checkpointOuts := make([]oortx.CheckpointOutput, 0, len(inputs))
	checkpointByTxid := make(
		map[chainhash.Hash]struct {
			tapTreeEncoded []byte
			ownerLeaf      []byte
			ownerPolicy    []byte
			sequence       uint32
			lockTime       uint32
		},
		len(inputs),
	)

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

		if inputs[i].CustomSpend != nil {
			applyCustomSpendTxContext(
				result.PSBT, inputs[i].CustomSpend,
			)
		}

		checkpoints = append(checkpoints, result.PSBT)

		checkpointOut, err := result.ToCheckpointOutput()
		if err != nil {
			return nil, nil, err
		}

		checkpointOuts = append(checkpointOuts, checkpointOut)
		checkpointByTxid[checkpointOut.Txid] = struct {
			tapTreeEncoded []byte
			ownerLeaf      []byte
			ownerPolicy    []byte
			sequence       uint32
			lockTime       uint32
		}{
			tapTreeEncoded: result.TapTreeEncoded,
			ownerLeaf:      result.OwnerLeafScript,
			ownerPolicy:    result.OwnerLeafPolicy,
			sequence: customSpendSequence(
				inputs[i].CustomSpend,
			),
		}

		if inputs[i].CustomSpend != nil {
			checkpointMeta := checkpointByTxid[checkpointOut.Txid]
			checkpointMeta.lockTime = inputs[i].CustomSpend.
				RequiredLockTime
			checkpointByTxid[checkpointOut.Txid] = checkpointMeta
		}
	}

	ark, err := oortx.BuildArkPSBT(checkpointOuts, outputs)
	if err != nil {
		return nil, nil, err
	}

	for i := range ark.UnsignedTx.TxIn {
		prevOut := ark.UnsignedTx.TxIn[i].PreviousOutPoint
		meta, ok := checkpointByTxid[prevOut.Hash]
		if !ok {
			return nil, nil, fmt.Errorf("missing checkpoint for "+
				"ark input %d", i)
		}

		// Custom spends can carry tx-context requirements such as
		// CLTV locktimes. The Ark transaction spends the checkpoint
		// output via the selected owner leaf, so the tx-level context
		// must follow the original custom spend path after checkpoint
		// sorting.
		if meta.lockTime > ark.UnsignedTx.LockTime {
			ark.UnsignedTx.LockTime = meta.lockTime
		}
		ark.UnsignedTx.TxIn[i].Sequence = meta.sequence

		if len(meta.ownerLeaf) == 0 && len(meta.ownerPolicy) > 0 {
			leaf, err := arkscript.DecodeLeafTemplate(
				meta.ownerPolicy,
			)
			if err != nil {
				return nil, nil, err
			}

			meta.ownerLeaf, err = leaf.Script()
			if err != nil {
				return nil, nil, err
			}
		}

		leaf, err := oortx.BuildTaprootTapLeafScript(
			meta.tapTreeEncoded, meta.ownerLeaf,
		)
		if err != nil {
			return nil, nil, err
		}

		addTaprootLeafScript(&ark.Inputs[i], leaf)
	}

	return ark, checkpoints, nil
}

// applyCustomSpendTxContext mirrors spend-path transaction constraints onto
// the transaction that spends the custom leaf.
func applyCustomSpendTxContext(pkt *psbt.Packet,
	spendPath *arkscript.SpendPath) {

	if pkt == nil || pkt.UnsignedTx == nil || spendPath == nil {
		return
	}

	if spendPath.RequiredLockTime != 0 {
		pkt.UnsignedTx.LockTime = spendPath.RequiredLockTime
	}

	for i := range pkt.UnsignedTx.TxIn {
		pkt.UnsignedTx.TxIn[i].Sequence = customSpendSequence(
			spendPath,
		)
	}
}

// customSpendSequence returns the tx input sequence needed for one custom
// spend path. CLTV paths require a non-final sequence even when the leaf does
// not also carry an explicit relative locktime.
func customSpendSequence(spendPath *arkscript.SpendPath) uint32 {
	switch {
	case spendPath == nil:
		return wire.MaxTxInSequenceNum

	case spendPath.RequiredSequence != 0:
		// Current Ark leaves use either CSV or CLTV transaction
		// context, not both. If a future leaf sets both fields, the
		// explicit sequence remains the consensus-critical value and
		// this branch is the point to revisit.
		return spendPath.RequiredSequence

	case spendPath.RequiredLockTime != 0:
		return wire.MaxTxInSequenceNum - 1

	default:
		return wire.MaxTxInSequenceNum
	}
}

// addTaprootLeafScript ensures the PSBT input carries the tapleaf script and
// control block used by the owner leaf path, avoiding duplicate inserts.
func addTaprootLeafScript(in *psbt.PInput, leaf *psbt.TaprootTapLeafScript) {
	if in == nil || leaf == nil {
		return
	}

	for i := range in.TaprootLeafScript {
		existing := in.TaprootLeafScript[i]
		if existing == nil {
			continue
		}

		if bytes.Equal(existing.ControlBlock, leaf.ControlBlock) &&
			bytes.Equal(existing.Script, leaf.Script) &&
			existing.LeafVersion == leaf.LeafVersion {
			return
		}
	}

	in.TaprootLeafScript = append(in.TaprootLeafScript, leaf)
}

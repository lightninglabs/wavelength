package oor

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// defaultRetryDelay is used when an outbox error is retryable but does not
// specify an explicit backoff.
const defaultRetryDelay = 1 * time.Second

const (
	// metadataRetryBaseDelay is the initial backoff before re-issuing the
	// authoritative incoming metadata query after a retryable failure.
	metadataRetryBaseDelay = 1 * time.Second

	// metadataRetryMaxDelay caps the exponential backoff between incoming
	// metadata retries. Without a cap a long-lived stuck session would
	// still re-query, just less often; the cap bounds the steady-state
	// rate.
	metadataRetryMaxDelay = 5 * time.Minute

	// maxMetadataRetries bounds how many times the incoming receive FSM
	// re-issues the metadata query before failing the session terminally.
	// A VTXO that never lands in the indexer must stop being re-queried so
	// one stuck session cannot spin the mailbox forever. With the base and
	// cap above this is on the order of an hour of attempts.
	maxMetadataRetries = 20

	// maxResolveRetries bounds how many times the incoming receive FSM
	// re-issues the phase-1 hint resolution query before failing the
	// session terminally. The phase-1 query has no failure response on
	// operator silence (only success or an explicit error advances the
	// state), so without this give-up a fabricated session id whose
	// resolution is never answered would pin a child in ReceiveResolving
	// forever, holding an r.incoming admission slot against the concurrency
	// cap. Reusing the metadata backoff schedule, this bounds an unanswered
	// resolve to roughly the same hour-scale window before the session
	// fails and frees its slot.
	maxResolveRetries = 20
)

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
		// Bind each standard checkpoint output's owner leaf to the
		// session operator key before building. A VTXO created under a
		// pre-rotation operator key would otherwise produce a
		// checkpoint output the rotated operator cannot co-sign and the
		// server rejects at submit. See normalizeCheckpointOwnerLeaves.
		err := normalizeCheckpointOwnerLeaves(
			evt.Policy, evt.VTXOInputs,
		)
		if err != nil {
			return nil, err
		}

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

		// Arm a re-drive timer alongside the submit request (see
		// submitOutbox): the cross-actor submit transport has no
		// failure path on operator silence (and must not give up: the
		// peer may just be offline), so without it a dead-lettered
		// submit would pin the session in AwaitingSubmitAccepted until
		// restart.
		next := &AwaitingSubmitAccepted{
			ArkPSBT:          evt.ArkPSBT,
			CheckpointPSBTs:  s.CheckpointPSBTs,
			TransferInputs:   s.TransferInputs,
			RecipientOutputs: s.RecipientOutputs,
			IdempotencyKey:   s.IdempotencyKey,
		}

		return &StateTransition{
			NextState: next,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: submitOutbox(next),
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
		return handleSubmitOutboxError(env, s, evt)

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

		// Finalize is a pure "ack" boundary in v0. Retry and resume
		// after crash is safe because the package is deterministic and
		// the server handles duplicates idempotently. Arm a re-drive
		// timer alongside the request (see finalizeOutbox) so a
		// dead-lettered finalize re-drives instead of wedging the
		// session until restart.
		next := &AwaitingFinalizeAccepted{
			SessionID:            s.SessionID,
			ArkPSBT:              s.ArkPSBT,
			FinalCheckpointPSBTs: evt.FinalCheckpointPSBTs,
			TransferInputs:       s.TransferInputs,
			IdempotencyKey:       s.IdempotencyKey,
		}

		return &StateTransition{
			NextState: next,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: finalizeOutbox(next),
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

// prePONRInputOutpoints returns the session's reserved input outpoints
// when the session has not yet crossed the point of no return (the
// server co-signing the checkpoints). Past that point the inputs must
// stay reserved: the server holds a co-signed spend of them, so
// releasing locally would invite double-spend attempts. Pre-PONR the
// server never locked anything, so a terminal failure can hand the
// inputs straight back to the spendable set.
func prePONRInputOutpoints(state State) []wire.OutPoint {
	var inputs []TransferInput
	switch s := state.(type) {
	case *AwaitingArkSignatures:
		inputs = s.TransferInputs

	case *AwaitingSubmitAccepted:
		inputs = s.TransferInputs

	default:
		return nil
	}

	outpoints := make([]wire.OutPoint, 0, len(inputs))
	for _, input := range inputs {
		if input.VTXO == nil {
			continue
		}

		outpoints = append(outpoints, input.VTXO.Outpoint)
	}

	return outpoints
}

// handleOutboxError emits retry scheduling for retryable errors while keeping
// the FSM in the current protocol state. Non-retryable errors drive the
// session to terminal Failed; when the failure lands before the point of
// no return, the transition also releases the reserved input VTXOs so a
// rejected submit (e.g. an output policy violation) does not strand
// spendable funds until a restart sweep.
func handleOutboxError(env *Environment, current State,
	evt *OutboxErrorEvent) (*StateTransition, error) {

	if evt == nil {
		return nil, fmt.Errorf("outbox error event must be provided")
	}

	if !evt.Retryable {
		newEvents := fn.None[EmittedEvent]()
		if outpoints := prePONRInputOutpoints(
			current,
		); len(outpoints) > 0 {

			newEvents = fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&ReleaseInputsRequest{
						Outpoints: outpoints,
					},
				},
			})
		}

		return &StateTransition{
			NextState: failedState(evt.ErrorReason, current),
			NewEvents: newEvents,
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

// handleSubmitOutboxError derives the retry-or-fail transition for an outbox
// error while the outgoing session is awaiting submit acceptance.
//
// Unlike the generic handleOutboxError, a retryable error here is bounded by a
// configurable cumulative retry window (env.MaxTransientSubmitRetry). The
// operator's transient submit rejections (INPUT_NOT_SPENDABLE and USER_BALANCE)
// are re-driven on the actor's fixed retry cadence, but only until the window
// since the first reject elapses. Once the budget is exhausted the session
// fails terminally via the same path handleOutboxError uses for non-retryable
// errors (which releases the reserved pre-point-of-no-return input VTXOs), so a
// genuinely-stuck input or a never-draining UserBalance recipient gives up
// instead of retrying forever. A zero budget keeps the legacy unbounded
// behavior, so a directly-constructed Environment without a configured cap
// still retries as before. Non-retryable errors keep handleOutboxError's
// immediate-terminal behavior.
//
// The transition never calls time.Now() directly: the current time comes from
// env.Now() so the FSM stays deterministic under an injected clock. The
// retry-window start is carried forward on the returned state (and persisted in
// the snapshot) so the bound survives restarts.
func handleSubmitOutboxError(env *Environment, current *AwaitingSubmitAccepted,
	evt *OutboxErrorEvent) (*StateTransition, error) {

	if evt == nil {
		return nil, fmt.Errorf("outbox error event must be provided")
	}

	// Non-retryable errors are terminal immediately. Reuse the shared
	// handler so the pre-point-of-no-return input release stays identical.
	if !evt.Retryable {
		return handleOutboxError(env, current, evt)
	}

	// env is non-nil on every protofsm ProcessEvent turn, but both reads
	// stay nil-safe (env.Now defaults to a real clock; the cap defaults to
	// 0 = unbounded) so directly-constructed test envs still work. Read the
	// configured cap first, then the clock, so the two agree on nil-safety.
	var maxRetry time.Duration
	if env != nil {
		maxRetry = env.MaxTransientSubmitRetry
	}

	now := env.Now()

	// If the retry window has already opened and its cumulative elapsed
	// time exceeds the configured budget, give up: fail terminally through
	// the same path a non-retryable error takes (releasing pre-PONR
	// inputs), noting the exhausted budget, the elapsed duration, and the
	// last reject reason. A zero budget disables this bound entirely.
	if maxRetry > 0 && current.FirstRejectUnixNanos != 0 {
		elapsed := now.Sub(time.Unix(0, current.FirstRejectUnixNanos))
		if elapsed > maxRetry {
			reason := fmt.Sprintf("submit retry budget of %s "+
				"exhausted after %s; last reject: %s", maxRetry,
				elapsed, evt.ErrorReason)

			return handleOutboxError(
				env, current, &OutboxErrorEvent{
					OutboxType:  evt.OutboxType,
					Retryable:   false,
					ErrorReason: reason,
				},
			)
		}
	}

	// Still within budget (or unbounded): open the retry window on the
	// first reject and carry it forward on subsequent rejects, then
	// schedule the retry exactly as the unbounded path does.
	next := *current
	if next.FirstRejectUnixNanos == 0 {
		next.FirstRejectUnixNanos = now.UnixNano()
	}

	after := evt.RetryAfter
	if after == 0 {
		after = defaultRetryDelay
	}

	return &StateTransition{
		NextState: &next,
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
// wavelength lib/tx/oor primitives.
func buildSubmitPackage(policy arkscript.CheckpointPolicy,
	inputs []TransferInput, outputs []oortx.RecipientOutput) (*psbt.Packet,
	[]*psbt.Packet, error) {

	return BuildSubmitPackage(policy, inputs, outputs)
}

// normalizeCheckpointOwnerLeaves rebuilds each standard input's checkpoint
// OUTPUT owner collaborative leaf so it commits to the session operator key
// rather than the spent input VTXO's operator key.
//
// The checkpoint output -- and the Ark tx that spends it cooperatively -- is
// governed by the session checkpoint policy, whose operator key is the
// operator's current key at session-creation time. The spent input VTXO may
// instead have been created under an older operator key, after the operator
// rotated. Deriving the output owner leaf from the input's operator key would
// make the server's submit-rebuild reject the checkpoint ("owner leaf policy
// does not contain operator key") and make the operator's Ark co-signature,
// produced with the session key, fail the leaf's key-membership check.
//
// The input SPEND path is untouched: spending the VTXO still uses its own
// tapscript, committed to the historical operator key (resolved server-side
// per input). Custom spends (e.g. vHTLC) carry their own owner leaf and are
// left untouched. Before any rotation the input and session keys are equal, so
// this is a no-op then.
func normalizeCheckpointOwnerLeaves(policy arkscript.CheckpointPolicy,
	inputs []TransferInput) error {

	if policy.OperatorKey == nil {
		return fmt.Errorf("checkpoint policy operator key required")
	}

	for i := range inputs {
		input := &inputs[i]

		if input.IsCustomSpend() {
			continue
		}

		if input.VTXO == nil || input.VTXO.ClientKey.PubKey == nil {
			continue
		}

		leaf, leafPolicy, err := defaultOwnerLeaf(
			input.VTXO.ClientKey.PubKey, policy.OperatorKey,
		)
		if err != nil {
			return err
		}

		// defaultOwnerLeaf only returns an empty leaf when one of its
		// key args is nil; both are non-nil here, so this is a
		// belt-and-suspenders guard rather than a reachable branch.
		if len(leaf) == 0 {
			continue
		}

		input.OwnerLeafScript = leaf
		input.OwnerLeafPolicy = leafPolicy
	}

	return nil
}

// BuildSubmitPackage constructs a v0 OOR submit package using the shared
// wavelength lib/tx/oor primitives. It only builds deterministic PSBTs from
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

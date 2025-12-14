package rounds

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/batch"
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
func (s *BatchBuildingState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	switch event.(type) {
	case *BuildBatchTxEvent:
		// Collect all boarding inputs, leave outputs, and VTXO
		// descriptors from client registrations.
		var (
			allBoardingInputs  []*BoardingInput
			allLeaveOutputs    []*wire.TxOut
			allVTXODescriptors []tree.VTXODescriptor
		)

		for _, reg := range s.ClientRegistrations {
			allBoardingInputs = append(
				allBoardingInputs, reg.BoardingInputs...,
			)
			allLeaveOutputs = append(
				allLeaveOutputs, reg.LeaveOutputs...,
			)

			// Collect all VTXO descriptors from the map.
			for _, desc := range reg.VTXODescriptors {
				allVTXODescriptors = append(
					allVTXODescriptors, *desc,
				)
			}
		}

		// Build the commitment transaction PSBT.
		psbtPacket, changeIdx, vtxoTrees, err := buildCommitmentTx(
			ctx, env, allBoardingInputs, allLeaveOutputs,
			allVTXODescriptors,
		)
		if err != nil {
			// Batch building failed - transition to FailedState.
			reason := fmt.Sprintf("build commitment tx: %v", err)

			return buildFailureTransition(
				env, s.ClientRegistrations, reason,
			), nil
		}

		// Transition to BatchBuiltState with the funded PSBT.
		return &StateTransition{
			NextState: &BatchBuiltState{
				ClientRegistrations: s.ClientRegistrations,
				PSBT:                psbtPacket,
				ChangeOutputIndex:   changeIdx,
				VTXOTrees:           vtxoTrees,
			},
			NewEvents: fn.Some(EmittedEvent{
				// Emit the internal event to prepare client
				// notifications. This event is handled in
				// BatchBuiltState.
				InternalEvent: []Event{
					&PrepareClientNotificationsEvent{},
				},
			}),
		}, nil

	default:
		return unexpectedEvent(s, "batch-building", event, env), nil
	}
}

// ProcessEvent handles the events from the BatchBuiltState state.
func (s *BatchBuiltState) ProcessEvent(_ context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	switch event.(type) {
	case *PrepareClientNotificationsEvent:
		// For each client, create a message with their personalized
		// data.
		var outboxMsgs []OutboxEvent
		for clientID, reg := range s.ClientRegistrations {
			// Extract VTXO tree paths for this client if they have
			// VTXO requests.
			var vtxoTreePaths map[int]*tree.Tree
			hasVTXOs := len(reg.VTXODescriptors) > 0
			if hasVTXOs && len(s.VTXOTrees) > 0 {
				// For now, give the client the full trees if
				// they have VTXOs.
				//
				// TODO(elle): Send the client only their paths.
				//  This will be done in a follow up commit.
				vtxoTreePaths = s.VTXOTrees
			}

			outboxMsgs = append(outboxMsgs, &ClientBatchInfo{
				Client:        clientID,
				BatchPSBT:     s.PSBT,
				VTXOTreePaths: vtxoTreePaths,
			})
		}

		// Add timeout for boarding signature collection.
		outboxMsgs = append(outboxMsgs, &StartTimeoutReq{
			RoundID:  env.RoundID,
			Phase:    TimeoutPhaseBoardingSigs,
			Duration: env.Terms.SignatureCollectionTimeout,
		})

		// Transition to AwaitingBoardingSigsState.
		// TODO(elle): If VTXOs exist, transition to
		// AwaitingVTXONoncesState instead for VTXO signing first.
		return &StateTransition{
			NextState: &AwaitingBoardingSigsState{
				ClientRegistrations: s.ClientRegistrations,
				PSBT:                s.PSBT,
				ChangeOutputIndex:   s.ChangeOutputIndex,
				VTXOTrees:           s.VTXOTrees,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: outboxMsgs,
			}),
		}, nil

	default:
		return unexpectedEvent(s, "batch-built", event, env), nil
	}
}

// ProcessEvent handles the events from the FailedState state.
// FailedState is a terminal state, so it ignores all events.
func (s *FailedState) ProcessEvent(_ context.Context, _ Event,
	_ *Environment) (*StateTransition, error) {

	// Terminal state - remain in FailedState and emit no events.
	return &StateTransition{
		NextState: s,
	}, nil
}

// buildFailureTransition creates a state transition to FailedState with all
// the necessary outbox events to notify clients, unlock boarding inputs, and
// inform the actor of the failure.
func buildFailureTransition(env *Environment,
	clientRegs map[clientconn.ClientID]*ClientRegistration,
	reason string) *StateTransition {

	var outboxMsgs []OutboxEvent

	// Collect all boarding input outpoints for unlocking.
	var allOutpoints []*wire.OutPoint
	for clientID, reg := range clientRegs {
		// Notify each client that the round has failed.
		outboxMsgs = append(outboxMsgs, &ClientRoundFailedResp{
			Client:  clientID,
			RoundID: env.RoundID,
			Reason:  reason,
		})

		// Collect outpoints from this client's boarding inputs.
		for _, bi := range reg.BoardingInputs {
			allOutpoints = append(allOutpoints, bi.Outpoint)
		}
	}

	// Request unlocking of all boarding inputs.
	if len(allOutpoints) > 0 {
		outboxMsgs = append(outboxMsgs, &UnlockBoardingInputsReq{
			RoundID:   env.RoundID,
			Outpoints: allOutpoints,
		})
	}

	// Notify the actor that the round has failed.
	outboxMsgs = append(outboxMsgs, &RoundFailedReq{
		FailedRoundID: env.RoundID,
		Reason:        reason,
	})

	return &StateTransition{
		NextState: &FailedState{
			Reason: reason,
		},
		NewEvents: fn.Some(EmittedEvent{
			Outbox: outboxMsgs,
		}),
	}
}

// ProcessEvent handles events in the AwaitingBoardingSigsState. This
// state waits for clients to submit their boarding input signatures.
//
// TODO(elle): Implement ClientBoardingSignaturesEvent handling:
//   - Validate signatures against expected boarding inputs
//   - Track which clients have submitted signatures
//   - When all signatures collected, transition to ServerSigningState
func (s *AwaitingBoardingSigsState) ProcessEvent(_ context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	switch event.(type) {
	case *BoardingSignaturesTimeoutEvent:
		// Timeout expired - fail the round.
		reason := "boarding signature collection timeout"

		return buildFailureTransition(
			env, s.ClientRegistrations, reason,
		), nil

	case *RegistrationTimeoutEvent:
		// Ignore stale timeout from registration phase.
		return &StateTransition{
			NextState: s,
		}, nil

	default:
		return nil, fmt.Errorf(
			"awaiting-boarding-sigs: unexpected event: %T", event,
		)
	}
}

// buildCommitmentTx constructs the commitment transaction PSBT with boarding
// inputs, required outputs (leaves), and VTXO tree outputs. It funds the
// transaction using the wallet and builds VTXO trees if needed.
//
// TODO(elle): Add connector outputs (forfeit trees) when implemented.
func buildCommitmentTx(ctx context.Context, env *Environment,
	boardingInputs []*BoardingInput, requiredOutputs []*wire.TxOut,
	vtxoDescriptors []tree.VTXODescriptor) (*psbt.Packet, int32,
	map[int]*tree.Tree, error) {

	// Step 1: Create unsigned transaction with boarding inputs and
	// required outputs.
	tx := wire.NewMsgTx(2)

	// Add boarding inputs.
	for _, bi := range boardingInputs {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: *bi.Outpoint,
			Sequence:         wire.MaxTxInSequenceNum,
		})
	}

	// Add required outputs (leave requests).
	for _, output := range requiredOutputs {
		tx.AddTxOut(output)
	}

	// Add batch outputs (VTXO tree roots). We'll record their indices
	// after FundPsbt reorders the transaction.
	var vtxoTreeCtx *batch.TreeContext
	if len(vtxoDescriptors) > 0 {
		var err error
		vtxoTreeCtx, err = batch.BuildTreeContext(
			env.Terms, vtxoDescriptors,
		)
		if err != nil {
			return nil, -1, nil, fmt.Errorf("build batch "+
				"outputs: %w", err)
		}

		for _, output := range vtxoTreeCtx.Outputs() {
			tx.AddTxOut(output)
		}
	}

	// Step 2: Convert to PSBT.
	packet, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, -1, nil, fmt.Errorf("create psbt: %w", err)
	}

	// Step 3: Add UTXO information for boarding inputs so FundPsbt can
	// calculate fees correctly.
	for i, bi := range boardingInputs {
		packet.Inputs[i].WitnessUtxo = &wire.TxOut{
			Value:    int64(bi.Value),
			PkScript: bi.PkScript,
		}

		// Note: Boarding inputs use collaborative tapscript leaf path,
		// not key spend. However, for fee estimation purposes, we set
		// SigHashDefault.
		packet.Inputs[i].SighashType = txscript.SigHashDefault
	}

	// Step 4: Get fee rate from estimator.
	feeRate, err := env.FeeEstimator.EstimateFeePerKW(env.ConfTarget)
	if err != nil {
		return nil, -1, nil, fmt.Errorf("estimate fee: %w", err)
	}

	// Step 5: Call FundPsbt to add wallet inputs and change.
	//
	// Note: FundPsbt reorders inputs and outputs, so any indices recorded
	// before this call will be invalid.
	changeIdx, err := env.WalletController.FundPsbt(
		ctx, packet, env.MinConfs, feeRate, env.WalletAccount,
	)
	if err != nil {
		return nil, -1, nil, fmt.Errorf("fund psbt: %w", err)
	}

	// Step 6: Build VTXO trees if VTXOs exist.
	var vtxoTrees map[int]*tree.Tree
	if vtxoTreeCtx != nil {
		// After FundPsbt reordering, find the VTXO tree root outputs
		// by matching their PkScripts.
		//
		// TODO(elle): write a test that covers this reordering once
		// we add tests covering this code-path.
		batchOutputs := vtxoTreeCtx.Outputs()
		batchOutputIndices, err := findOutputIndices(
			batchOutputs, packet.UnsignedTx,
		)
		if err != nil {
			return nil, -1, nil, fmt.Errorf("find batch "+
				"outputs: %w", err)
		}

		// Build VTXO trees using the post-FundPsbt batch output
		// indices.
		vtxoTrees, err = vtxoTreeCtx.BuildVTXOTreesForCommitmentTx(
			packet.UnsignedTx, batchOutputIndices,
		)
		if err != nil {
			return nil, -1, nil, fmt.Errorf("build VTXO trees: %w",
				err)
		}
	}

	return packet, changeIdx, vtxoTrees, nil
}

// findOutputIndices finds the indices of the given outputs in the transaction
// by matching their PkScripts. This is used after FundPsbt reorders the
// transaction to locate specific outputs by their script.
func findOutputIndices(expectedOutputs []*wire.TxOut,
	tx *wire.MsgTx) ([]int, error) {

	indices := make([]int, len(expectedOutputs))

	for i, expectedOut := range expectedOutputs {
		found := false
		for j, txOut := range tx.TxOut {
			if !bytes.Equal(expectedOut.PkScript, txOut.PkScript) {
				continue
			}

			indices[i] = j
			found = true

			break
		}

		if !found {
			return nil, fmt.Errorf("output %d not found in tx", i)
		}
	}

	return indices, nil
}

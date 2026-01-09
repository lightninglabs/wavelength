package rounds

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
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
				newClientErrorResp(clientID, errMsg),
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

// unlockBoardingInputsList unlocks a list of boarding inputs. This is called
// when a client registration fails partway through (e.g., forfeit VTXO lock
// failure) and we need to clean up boarding inputs that were successfully
// locked. Errors are logged but don't stop the unlocking process.
func unlockBoardingInputsList(ctx context.Context, env *Environment,
	inputs []*BoardingInput) {

	for _, input := range inputs {
		err := env.BoardingInputLocker.Unlock(
			ctx, input.Outpoint, env.RoundID,
		)
		if err != nil {
			env.Log.ErrorS(ctx, "Failed to unlock boarding "+
				"input", err,
				"outpoint", input.Outpoint.String())
		}
	}
}

// unlockBoardingInputs unlocks all boarding inputs for the given client
// registrations. This is called when a round fails to release all locked
// inputs. Errors are logged but don't stop the unlocking process, ensuring
// we attempt to unlock all inputs even if some fail.
func unlockBoardingInputs(ctx context.Context, env *Environment,
	clientRegs map[clientconn.ClientID]*ClientRegistration) {

	for _, reg := range clientRegs {
		for _, input := range reg.BoardingInputs {
			err := env.BoardingInputLocker.Unlock(
				ctx, input.Outpoint, env.RoundID,
			)
			if err != nil {
				// Log the error but continue unlocking other
				// inputs. We don't want one failure to prevent
				// releasing other locked inputs.
				env.Log.ErrorS(ctx, "Failed to unlock boarding "+
					"input", err,
					"outpoint", input.Outpoint.String())
			}
		}
	}
}

// lockForfeitVTXOs attempts to lock all forfeit VTXOs for a client in the
// VTXOStore. If any lock fails, it returns an error. If all locks succeed,
// it returns nil.
func lockForfeitVTXOs(ctx context.Context, env *Environment,
	inputs []*ForfeitInput) error {

	if len(inputs) == 0 {
		return nil
	}

	outpoints := make([]wire.OutPoint, 0, len(inputs))
	for _, input := range inputs {
		outpoints = append(outpoints, *input.Outpoint)
	}

	err := env.VTXOStore.LockVTXO(ctx, env.RoundID, outpoints...)
	if err != nil {
		return fmt.Errorf("failed to lock forfeit VTXOs: %w", err)
	}

	return nil
}

// unlockForfeitVTXOs unlocks all forfeit VTXOs for the given client
// registrations. This is called when a round fails to release all locked
// VTXOs. Errors are logged but don't stop the unlocking process, ensuring
// we attempt to unlock all VTXOs even if some fail.
func unlockForfeitVTXOs(ctx context.Context, env *Environment,
	clientRegs map[clientconn.ClientID]*ClientRegistration) {

	for _, reg := range clientRegs {
		if len(reg.ForfeitInputs) == 0 {
			continue
		}

		outpoints := make(
			[]wire.OutPoint, 0, len(reg.ForfeitInputs),
		)
		for _, input := range reg.ForfeitInputs {
			outpoints = append(outpoints, *input.Outpoint)
		}

		err := env.VTXOStore.UnlockVTXO(
			ctx, env.RoundID, outpoints...,
		)
		if err != nil {
			// Log the error but continue unlocking other
			// VTXOs. We don't want one failure to prevent
			// releasing other locked VTXOs.
			env.Log.ErrorS(ctx, "Failed to unlock forfeit "+
				"VTXOs", err,
				"count", len(outpoints))
		}
	}
}

// newClientRegistration creates a ClientRegistration from a validated join
// request result.
func newClientRegistration(clientID ClientID,
	result *JoinRequestResult) *ClientRegistration {

	return &ClientRegistration{
		ClientID:        clientID,
		BoardingInputs:  result.BoardingInputs,
		ForfeitInputs:   result.ForfeitInputs,
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

		// Attempt to lock all forfeit VTXOs for this client.
		err = lockForfeitVTXOs(ctx, env, result.ForfeitInputs)
		if err != nil {
			// Unlock the boarding inputs since we can't proceed.
			unlockBoardingInputsList(
				ctx, env, result.BoardingInputs,
			)

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

		// Attempt to lock all forfeit VTXOs for this client.
		err = lockForfeitVTXOs(ctx, env, result.ForfeitInputs)
		if err != nil {
			// Unlock the boarding inputs since we can't proceed.
			unlockBoardingInputsList(
				ctx, env, result.BoardingInputs,
			)

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
			allForfeitInputs   []*ForfeitInput
			allLeaveOutputs    []*wire.TxOut
			allVTXODescriptors []tree.VTXODescriptor
		)

		for _, reg := range s.ClientRegistrations {
			allBoardingInputs = append(
				allBoardingInputs, reg.BoardingInputs...,
			)
			allForfeitInputs = append(
				allForfeitInputs, reg.ForfeitInputs...,
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
		psbtPacket, _, vtxoTrees, connectorTrees,
			connectorAssignments, err := buildCommitmentTx(
			ctx, env, allBoardingInputs, allForfeitInputs,
			allLeaveOutputs, allVTXODescriptors,
		)
		if err != nil {
			// Batch building failed - transition to FailedState.
			reason := fmt.Sprintf("build commitment tx: %v", err)

			return buildFailureTransition(
				ctx, env, s.ClientRegistrations, reason,
			), nil
		}

		// Transition to BatchBuiltState with the funded PSBT.
		return &StateTransition{
			NextState: &BatchBuiltState{
				ClientRegistrations:  s.ClientRegistrations,
				PSBT:                 psbtPacket,
				VTXOTrees:            vtxoTrees,
				ConnectorTrees:       connectorTrees,
				ConnectorAssignments: connectorAssignments,
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
func (s *BatchBuiltState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	switch event.(type) {
	case *PrepareClientNotificationsEvent:
		return s.handlePrepareClientNotifications(ctx, env)

	default:
		return unexpectedEvent(s, "batch-built", event, env), nil
	}
}

// handlePrepareClientNotifications prepares client notifications with batch
// data and transitions to either AwaitingVTXONoncesState (if VTXOs exist) or
// AwaitingInputSigsState (if no VTXOs).
func (s *BatchBuiltState) handlePrepareClientNotifications(
	ctx context.Context, env *Environment) (*StateTransition, error) {

	// For each client, create a message with their personalized data.
	// The PSBT contains WitnessUtxo for inputs, providing the prevout
	// info clients need to compute sighashes.
	var outboxMsgs []OutboxEvent
	for clientID, reg := range s.ClientRegistrations {
		// Extract VTXO tree paths for this client if they have
		// VTXO requests.
		var vtxoTreePaths map[int]*tree.Tree
		if len(reg.VTXODescriptors) > 0 && len(s.VTXOTrees) > 0 {
			// Collect all cosigner keys from the client's VTXO
			// descriptors.
			clientKeys := make(
				[]*btcec.PublicKey, 0, len(reg.VTXODescriptors),
			)
			for _, desc := range reg.VTXODescriptors {
				clientKeys = append(
					clientKeys, desc.CoSignerKey,
				)
			}

			// Extract the VTXO paths relevant to this client.
			var err error
			vtxoTreePaths, err = batch.ExtractClientVTXOPaths(
				s.VTXOTrees, clientKeys,
			)
			if err != nil {
				return buildFailureTransition(
					ctx, env, s.ClientRegistrations,
					fmt.Sprintf("extract VTXO paths for "+
						"client %s: %v", clientID, err),
				), nil
			}
		}

		// Extract connector leaf assignments for this client if they
		// have forfeits.
		var connectorLeafMap map[wire.OutPoint]*types.ConnectorLeafInfo
		if len(reg.ForfeitInputs) > 0 {
			connectorLeafMap = make(
				map[wire.OutPoint]*types.ConnectorLeafInfo,
				len(reg.ForfeitInputs),
			)
			for _, input := range reg.ForfeitInputs {
				outpoint := *input.Outpoint
				assignment, ok :=
					s.ConnectorAssignments[outpoint]
				if !ok {
					return buildFailureTransition(
						ctx, env, s.ClientRegistrations,
						fmt.Sprintf("missing "+
							"connector assignment "+
							"for client %s",
							clientID),
					), nil
				}

				leafInfo := &types.ConnectorLeafInfo{
					LeafOutpoint: assignment.LeafOutpoint,
					LeafOutput:   assignment.LeafOutput,
				}
				connectorLeafMap[outpoint] = leafInfo
			}
		}

		outboxMsgs = append(outboxMsgs, &ClientBatchInfo{
			Client:           clientID,
			BatchPSBT:        s.PSBT,
			VTXOTreePaths:    vtxoTreePaths,
			ConnectorLeafMap: connectorLeafMap,
		})
	}

	// Check if there are any VTXOs in the batch.
	hasVTXOs := len(s.VTXOTrees) > 0
	if hasVTXOs {
		return s.transitionToVTXONonces(ctx, env, outboxMsgs)
	}

	// No VTXOs - go directly to boarding signatures.
	return s.transitionToInputSigs(ctx, env, outboxMsgs)
}

// transitionToVTXONonces creates TreeSignCoordinators for each VTXO tree and
// transitions to AwaitingVTXONoncesState.
func (s *BatchBuiltState) transitionToVTXONonces(ctx context.Context,
	env *Environment, outboxMsgs []OutboxEvent) (*StateTransition, error) {

	// Create TreeSignCoordinators for each VTXO tree.
	treeCoordinators := make(map[int]*batch.TreeSignCoordinator)
	for idx, vtxoTree := range s.VTXOTrees {
		coordinator, err := batch.NewTreeSignCoordinator(
			env.WalletController, &env.Terms.OperatorKey, vtxoTree,
		)
		if err != nil {
			return buildFailureTransition(
				ctx, env, s.ClientRegistrations,
				fmt.Sprintf("create tree coordinator for "+
					"output %d: %v", idx, err),
			), nil
		}

		treeCoordinators[idx] = coordinator
	}

	// Add timeout for VTXO nonce collection.
	outboxMsgs = append(outboxMsgs, &StartTimeoutReq{
		RoundID:  env.RoundID,
		Phase:    TimeoutPhaseVTXONonces,
		Duration: env.Terms.SignatureCollectionTimeout,
	})

	return &StateTransition{
		NextState: &AwaitingVTXONoncesState{
			ClientRegistrations:  s.ClientRegistrations,
			PSBT:                 s.PSBT,
			VTXOTrees:            s.VTXOTrees,
			ConnectorTrees:       s.ConnectorTrees,
			ConnectorAssignments: s.ConnectorAssignments,
			TreeSignCoordinators: treeCoordinators,
			ClientsWithNonces: make(
				map[clientconn.ClientID]struct{},
			),
		},
		NewEvents: fn.Some(EmittedEvent{
			Outbox: outboxMsgs,
		}),
	}, nil
}

// transitionToInputSigs transitions directly to AwaitingInputSigsState
// when there are no VTXOs in the batch.
func (s *BatchBuiltState) transitionToInputSigs(ctx context.Context,
	env *Environment, outboxMsgs []OutboxEvent) (*StateTransition, error) {

	// Notify clients with boarding inputs that we're ready for their
	// signatures. This is separate from ClientBatchInfo because there may
	// be VTXO signing phases between batch construction and boarding
	// signature collection.
	for clientID, reg := range s.ClientRegistrations {
		if len(reg.BoardingInputs) == 0 {
			continue
		}

		outboxMsgs = append(outboxMsgs, &ClientAwaitingInputSigsResp{
			Client: clientID,
		})
	}

	// Add timeout for input signature collection.
	outboxMsgs = append(outboxMsgs, &StartTimeoutReq{
		RoundID:  env.RoundID,
		Phase:    TimeoutPhaseInputSigs,
		Duration: env.Terms.SignatureCollectionTimeout,
	})

	return &StateTransition{
		NextState: &AwaitingInputSigsState{
			ClientRegistrations:  s.ClientRegistrations,
			PSBT:                 s.PSBT,
			VTXOTrees:            s.VTXOTrees,
			ConnectorTrees:       s.ConnectorTrees,
			ConnectorAssignments: s.ConnectorAssignments,
			ClientsSubmitted: make(
				map[clientconn.ClientID]struct{},
			),
			CollectedSignatures: make(InputSigsMap),
		},
		NewEvents: fn.Some(EmittedEvent{
			Outbox: outboxMsgs,
		}),
	}, nil
}

// buildFailureTransition creates a state transition to FailedState with all
// the necessary outbox events to notify clients and inform the actor of the
// failure. It also directly unlocks all boarding inputs.
func buildFailureTransition(ctx context.Context, env *Environment,
	clientRegs map[clientconn.ClientID]*ClientRegistration,
	reason string) *StateTransition {

	var outboxMsgs []OutboxEvent

	// Notify each client that the round has failed.
	for clientID := range clientRegs {
		outboxMsgs = append(outboxMsgs, &ClientRoundFailedResp{
			Client:  clientID,
			RoundID: env.RoundID,
			Reason:  reason,
		})
	}

	// Unlock all boarding inputs directly in the FSM.
	unlockBoardingInputs(ctx, env, clientRegs)

	// Unlock all forfeit VTXOs directly in the FSM.
	unlockForfeitVTXOs(ctx, env, clientRegs)

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

// ProcessEvent handles events in the AwaitingInputSigsState. This
// state waits for clients to submit their boarding input signatures.
func (s *AwaitingInputSigsState) ProcessEvent(ctx context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	switch evt := event.(type) {
	case *ClientBoardingSignaturesEvent:
		return s.handleBoardingSignatures(evt, env)

	case *InputSignaturesTimeoutEvent:
		// Timeout expired - fail the round.
		reason := "input signature collection timeout"

		return buildFailureTransition(
			ctx, env, s.ClientRegistrations, reason,
		), nil

	default:
		return unexpectedEvent(s, "awaiting-input-sigs", event, env),
			nil
	}
}

// handleBoardingSignatures processes a client's boarding signature submission.
// It validates the signatures cryptographically against the tapscript
// collaborative spend path, stores them for later use, and tracks the client
// as having submitted. When all clients have submitted, it transitions to
// ServerSigningState.
func (s *AwaitingInputSigsState) handleBoardingSignatures(
	evt *ClientBoardingSignaturesEvent,
	env *Environment) (*StateTransition, error) {

	clientID := evt.ClientID

	// Check if client is registered in this round.
	reg, exists := s.ClientRegistrations[clientID]
	if !exists {
		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					newClientErrorResp(
						clientID, "not registered",
					),
				},
			}),
		}, nil
	}

	// Check if client already submitted.
	if s.hasClientSubmitted(clientID) {
		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					newClientErrorResp(
						clientID, "already submitted",
					),
				},
			}),
		}, nil
	}

	// Verify signature count matches the number of boarding inputs.
	if len(evt.Signatures) != len(reg.BoardingInputs) {
		errMsg := fmt.Sprintf(
			"expected %d signatures, got %d",
			len(reg.BoardingInputs), len(evt.Signatures),
		)

		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					newClientErrorResp(
						clientID, errMsg,
					),
				},
			}),
		}, nil
	}

	// Build a map from outpoints to boarding inputs for quick lookup.
	outpointToInput := make(map[wire.OutPoint]*BoardingInput)
	for _, bi := range reg.BoardingInputs {
		outpointToInput[*bi.Outpoint] = bi
	}

	// Build a prevout fetcher from the PSBT's WitnessUtxo fields.
	tx := s.PSBT.UnsignedTx
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(nil)
	for i, pIn := range s.PSBT.Inputs {
		if pIn.WitnessUtxo != nil {
			prevOutFetcher.AddPrevOut(
				tx.TxIn[i].PreviousOutPoint, pIn.WitnessUtxo,
			)
		}
	}

	// Validate each signature cryptographically.
	for _, sig := range evt.Signatures {
		// Look up the boarding input for this signature.
		boardingInput, found := outpointToInput[sig.Outpoint]
		if !found {
			errMsg := fmt.Sprintf(
				"unknown outpoint: %v", sig.Outpoint,
			)

			return &StateTransition{
				NextState: s,
				NewEvents: fn.Some(EmittedEvent{
					Outbox: []OutboxEvent{
						newClientErrorResp(
							clientID,
							errMsg,
						),
					},
				}),
			}, nil
		}

		// Verify the input index is valid.
		if sig.InputIndex < 0 || sig.InputIndex >= len(s.PSBT.Inputs) {
			errMsg := fmt.Sprintf(
				"invalid input index: %d", sig.InputIndex,
			)

			return &StateTransition{
				NextState: s,
				NewEvents: fn.Some(EmittedEvent{
					Outbox: []OutboxEvent{
						newClientErrorResp(
							clientID,
							errMsg,
						),
					},
				}),
			}, nil
		}

		// Verify the schnorr signature against the sighash.
		err := ValidateBoardingSignature(
			boardingInput, sig, tx, prevOutFetcher,
		)
		if err != nil {
			return &StateTransition{
				NextState: s,
				NewEvents: fn.Some(EmittedEvent{
					Outbox: []OutboxEvent{
						newClientErrorResp(
							clientID, err.Error(),
						),
					},
				}),
			}, nil
		}
	}

	// Mark client as having submitted and store their signatures.
	newClientsSubmitted := make(map[clientconn.ClientID]struct{})
	for id := range s.ClientsSubmitted {
		newClientsSubmitted[id] = struct{}{}
	}
	newClientsSubmitted[clientID] = struct{}{}

	// Copy collected signatures and add the new client's signatures.
	newCollectedSigs := make(InputSigsMap)
	for id, sigs := range s.CollectedSignatures {
		newCollectedSigs[id] = sigs
	}
	newCollectedSigs[clientID] = evt.Signatures

	// Create new state with updated tracking.
	newState := &AwaitingInputSigsState{
		ClientRegistrations:  s.ClientRegistrations,
		PSBT:                 s.PSBT,
		VTXOTrees:            s.VTXOTrees,
		ConnectorTrees:       s.ConnectorTrees,
		ConnectorAssignments: s.ConnectorAssignments,
		ClientsSubmitted:     newClientsSubmitted,
		CollectedSignatures:  newCollectedSigs,
	}

	// Check if all clients have submitted.
	if newState.allClientsSubmitted() {
		// Cancel the input signatures timeout and transition to
		// ServerSigningState. Emit ServerSignInputsEvent to trigger
		// server signing.
		//
		return &StateTransition{
			NextState: &ServerSigningState{
				ClientRegistrations: s.ClientRegistrations,
				PSBT:                s.PSBT,
				VTXOTrees:           s.VTXOTrees,
				CollectedSignatures: newCollectedSigs,
			},
			NewEvents: fn.Some(EmittedEvent{
				InternalEvent: []Event{
					&ServerSignInputsEvent{},
				},
				Outbox: []OutboxEvent{
					&CancelTimeoutReq{
						RoundID: env.RoundID,
						Phase:   TimeoutPhaseInputSigs,
					},
				},
			}),
		}, nil
	}

	// Not all clients have submitted yet - remain in current state.
	return &StateTransition{
		NextState: newState,
	}, nil
}

// buildCommitmentTx constructs the commitment transaction PSBT with boarding
// inputs, forfeit inputs, required outputs (leaves), VTXO tree outputs, and
// connector outputs for forfeits. It funds the transaction using the wallet
// and builds both VTXO and connector trees if needed.
//
// Outputs in the transaction include:
//  1. Leave outputs (client withdrawals)
//  2. VTXO tree outputs (batch outputs)
//  3. Connector outputs (forfeit trees)
//  4. Change output (if needed, added by FundPsbt)
func buildCommitmentTx(ctx context.Context, env *Environment,
	boardingInputs []*BoardingInput, forfeitInputs []*ForfeitInput,
	requiredOutputs []*wire.TxOut,
	vtxoDescriptors []tree.VTXODescriptor) (*psbt.Packet, int32,
	map[int]*tree.Tree, map[int]*tree.Tree,
	map[wire.OutPoint]*ConnectorLeafAssignment, error) {

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
			return nil, -1, nil, nil, nil, fmt.Errorf("build "+
				"batch outputs: %w", err)
		}

		for _, output := range vtxoTreeCtx.Outputs() {
			tx.AddTxOut(output)
		}
	}

	// Add connector outputs (for forfeit trees). We'll record their
	// indices after FundPsbt reorders the transaction.
	numForfeits := len(forfeitInputs)
	var connectorOutputs []*wire.TxOut
	if numForfeits > 0 {
		maxPerTree := int(env.Terms.MaxConnectorsPerTree)
		if maxPerTree <= 0 {
			return nil, -1, nil, nil, nil, fmt.Errorf(
				"max connectors per tree must be > 0",
			)
		}

		for i := 0; i < numForfeits; i += maxPerTree {
			numInOutput := maxPerTree
			if i+numInOutput > numForfeits {
				numInOutput = numForfeits - i
			}

			connectorOutput, err := tree.BuildConnectorOutput(
				numInOutput, env.Terms.ConnectorDustAmount,
				env.Terms.ConnectorAddress,
			)
			if err != nil {
				return nil, -1, nil, nil, nil, fmt.Errorf(
					"build connector output: %w", err,
				)
			}

			connectorOutputs = append(
				connectorOutputs, connectorOutput,
			)
			tx.AddTxOut(connectorOutput)
		}
	}

	// Step 2: Convert to PSBT.
	packet, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, -1, nil, nil, nil, fmt.Errorf("create psbt: %w",
			err)
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
		return nil, -1, nil, nil, nil, fmt.Errorf("estimate fee: %w",
			err)
	}

	// Step 5: Call FundPsbt to add wallet inputs and change.
	//
	// Note: FundPsbt reorders inputs and outputs, so any indices recorded
	// before this call will be invalid.
	changeIdx, err := env.WalletController.FundPsbt(
		ctx, packet, env.MinConfs, feeRate, env.WalletAccount,
	)
	if err != nil {
		return nil, -1, nil, nil, nil, fmt.Errorf("fund psbt: %w",
			err)
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
			return nil, -1, nil, nil, nil, fmt.Errorf("find "+
				"batch outputs: %w", err)
		}

		// Build VTXO trees using the post-FundPsbt batch output
		// indices.
		vtxoTrees, err = vtxoTreeCtx.BuildVTXOTreesForCommitmentTx(
			packet.UnsignedTx, batchOutputIndices,
		)
		if err != nil {
			return nil, -1, nil, nil, nil, fmt.Errorf("build "+
				"VTXO trees: %w", err)
		}
	}

	// Step 7: Build connector trees and assignments if forfeits exist.
	var (
		connectorTrees       map[int]*tree.Tree
		connectorAssignments map[wire.OutPoint]*ConnectorLeafAssignment
	)
	if numForfeits > 0 {
		connectorOutputIndices, err := findOutputIndices(
			connectorOutputs, packet.UnsignedTx,
		)
		if err != nil {
			return nil, -1, nil, nil, nil, fmt.Errorf(
				"find connector outputs: %w", err,
			)
		}

		connectorTrees, connectorAssignments, err =
			buildConnectorTreesAndAssignments(
				env, packet.UnsignedTx, forfeitInputs,
				connectorOutputIndices,
			)
		if err != nil {
			return nil, -1, nil, nil, nil, fmt.Errorf(
				"build connector trees: %w", err,
			)
		}
	}

	return packet, changeIdx, vtxoTrees, connectorTrees,
		connectorAssignments, nil
}

// findOutputIndices finds the indices of the given outputs in the transaction
// by matching their PkScripts and values. This is used after FundPsbt reorders
// the transaction to locate specific outputs by their script and amount.
func findOutputIndices(expectedOutputs []*wire.TxOut,
	tx *wire.MsgTx) ([]int, error) {

	indices := make([]int, len(expectedOutputs))
	used := make([]bool, len(tx.TxOut))

	for i, expectedOut := range expectedOutputs {
		found := false
		for j, txOut := range tx.TxOut {
			if used[j] {
				continue
			}

			if expectedOut.Value != txOut.Value {
				continue
			}

			if !bytes.Equal(expectedOut.PkScript, txOut.PkScript) {
				continue
			}

			indices[i] = j
			used[j] = true
			found = true

			break
		}

		if !found {
			return nil, fmt.Errorf("output %d not found in tx", i)
		}
	}

	return indices, nil
}

// buildConnectorTreesAndAssignments builds connector trees and assigns each
// forfeit input to a connector leaf.
func buildConnectorTreesAndAssignments(env *Environment, tx *wire.MsgTx,
	forfeitInputs []*ForfeitInput, connectorOutputIndices []int) (
	map[int]*tree.Tree, map[wire.OutPoint]*ConnectorLeafAssignment, error) {

	numForfeits := len(forfeitInputs)
	if numForfeits == 0 {
		return nil, nil, nil
	}

	sortedForfeits := make([]*ForfeitInput, 0, numForfeits)
	for _, input := range forfeitInputs {
		if input == nil || input.Outpoint == nil {
			return nil, nil, fmt.Errorf(
				"forfeit input outpoint is nil",
			)
		}

		sortedForfeits = append(sortedForfeits, input)
	}
	sort.Slice(sortedForfeits, func(i, j int) bool {
		return sortedForfeits[i].Outpoint.String() <
			sortedForfeits[j].Outpoint.String()
	})

	maxPerTree := int(env.Terms.MaxConnectorsPerTree)
	if maxPerTree <= 0 {
		return nil, nil, fmt.Errorf(
			"max connectors per tree must be > 0",
		)
	}

	if env.Terms.ConnectorDustAmount <= 0 {
		return nil, nil, fmt.Errorf(
			"connector dust amount must be > 0",
		)
	}

	if env.Terms.ConnectorAddress == nil {
		return nil, nil, fmt.Errorf("connector address cannot be nil")
	}

	if env.Terms.OperatorKey.PubKey == nil {
		return nil, nil, fmt.Errorf("operator key cannot be nil")
	}

	radix := int(env.Terms.TreeRadix)
	if radix < 2 {
		return nil, nil, fmt.Errorf("tree radix must be at least 2")
	}

	expectedOutputs := (numForfeits + maxPerTree - 1) / maxPerTree
	if len(connectorOutputIndices) != expectedOutputs {
		return nil, nil, fmt.Errorf(
			"connector output count mismatch: %d != %d",
			len(connectorOutputIndices), expectedOutputs,
		)
	}

	connectorTrees := make(map[int]*tree.Tree)
	connectorAssignments := make(
		map[wire.OutPoint]*ConnectorLeafAssignment, numForfeits,
	)
	txid := tx.TxHash()

	offset := 0
	for i, outputIdx := range connectorOutputIndices {
		numInOutput := maxPerTree
		if offset+numInOutput > numForfeits {
			numInOutput = numForfeits - offset
		}

		connectorOutput := tx.TxOut[outputIdx]
		connectorOutpoint := wire.OutPoint{
			Hash:  txid,
			Index: uint32(outputIdx),
		}
		connectorDesc := tree.ConnectorDescriptor{
			PkScript:  connectorOutput.PkScript,
			NumLeaves: numInOutput,
			Amount:    env.Terms.ConnectorDustAmount,
		}

		connectorTree, err := tree.BuildConnectorTree(
			connectorOutpoint, connectorOutput, connectorDesc,
			env.Terms.OperatorKey.PubKey, radix,
		)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"build connector tree %d: %w", i, err,
			)
		}

		leaves := connectorTree.Root.GetLeafNodes()
		if len(leaves) != numInOutput {
			return nil, nil, fmt.Errorf(
				"connector tree %d leaf count mismatch: "+
					"%d != %d", i, len(leaves),
				numInOutput,
			)
		}

		connectorTrees[outputIdx] = connectorTree

		for leafIdx := 0; leafIdx < numInOutput; leafIdx++ {
			forfeitInput := sortedForfeits[offset+leafIdx]
			leaf := leaves[leafIdx]
			leafOutpoint, err := leaf.GetNonAnchorOutpoint()
			if err != nil {
				return nil, nil, fmt.Errorf(
					"connector leaf outpoint: %w", err,
				)
			}

			leafOutput, err := leafNonAnchorOutput(leaf)
			if err != nil {
				return nil, nil, fmt.Errorf(
					"connector leaf output: %w", err,
				)
			}

			connectorAssignments[*forfeitInput.Outpoint] =
				&ConnectorLeafAssignment{
					ForfeitOutpoint: *forfeitInput.Outpoint,
					LeafOutpoint:    *leafOutpoint,
					LeafOutput:      leafOutput,
				}
		}

		offset += numInOutput
	}

	return connectorTrees, connectorAssignments, nil
}

// leafNonAnchorOutput returns the non-anchor output for a leaf node.
func leafNonAnchorOutput(leaf *tree.Node) (*wire.TxOut, error) {
	if leaf == nil {
		return nil, fmt.Errorf("leaf cannot be nil")
	}

	anchorScript := scripts.AnchorOutput().PkScript
	for _, output := range leaf.Outputs {
		if !bytes.Equal(output.PkScript, anchorScript) {
			return output, nil
		}
	}

	return nil, fmt.Errorf("no non-anchor output found")
}

// ProcessEvent handles events in the AwaitingVTXONoncesState. This state waits
// for clients with VTXOs to submit their MuSig2 nonces.
func (s *AwaitingVTXONoncesState) ProcessEvent(ctx context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	switch evt := event.(type) {
	case *ClientVTXONoncesEvent:
		return s.handleClientNonces(ctx, env, evt)

	case *VTXONoncesTimeoutEvent:
		// The timeout was reached before all nonces were collected.
		return buildFailureTransition(
			ctx, env, s.ClientRegistrations,
			"VTXO nonce collection timeout",
		), nil

	default:
		return unexpectedEvent(s, "awaiting-vtxo-nonces", event, env),
			nil
	}
}

// handleClientNonces processes nonces submitted by a client, adding them to
// the tree coordinators. If all clients have submitted nonces, it transitions
// to the next state AwaitingVTXOSignaturesState.
func (s *AwaitingVTXONoncesState) handleClientNonces(
	ctx context.Context, env *Environment,
	evt *ClientVTXONoncesEvent) (*StateTransition, error) {

	clientID := evt.ClientID

	// Check if client is registered in this round.
	reg, exists := s.ClientRegistrations[clientID]
	if !exists {
		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					newClientErrorResp(
						clientID,
						"not registered in this round",
					),
				},
			}),
		}, nil
	}

	// Check if client has VTXOs (should only accept nonces from VTXO
	// clients).
	if len(reg.VTXODescriptors) == 0 {
		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					newClientErrorResp(
						clientID, "client has no VTXOs",
					),
				},
			}),
		}, nil
	}

	if s.hasClientSubmittedNonces(clientID) {
		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					newClientErrorResp(
						clientID,
						"already submitted nonces",
					),
				},
			}),
		}, nil
	}

	if len(evt.Nonces) == 0 {
		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					newClientErrorResp(
						clientID, "no nonces provided",
					),
				},
			}),
		}, nil
	}

	// Verify client submitted nonces for all their signing keys.
	for keyHex := range reg.VTXODescriptors {
		if _, ok := evt.Nonces[keyHex]; !ok {
			errMsg := fmt.Sprintf(
				"missing nonces for signing key %x",
				keyHex[:],
			)

			return &StateTransition{
				NextState: s,
				NewEvents: fn.Some(EmittedEvent{
					Outbox: []OutboxEvent{
						newClientErrorResp(
							clientID,
							errMsg,
						),
					},
				}),
			}, nil
		}
	}

	totalAccepted := 0

	for signingKeyHex, nonces := range evt.Nonces {
		if len(nonces) == 0 {
			errMsg := fmt.Sprintf(
				"no nonces for signing key %x",
				signingKeyHex[:],
			)

			return &StateTransition{
				NextState: s,
				NewEvents: fn.Some(EmittedEvent{
					Outbox: []OutboxEvent{
						newClientErrorResp(
							clientID,
							errMsg,
						),
					},
				}),
			}, nil
		}

		desc := reg.VTXODescriptors[signingKeyHex]
		if desc == nil || desc.CoSignerKey == nil ||
			nonces == nil {

			errMsg := fmt.Sprintf(
				"unknown signing key %x", signingKeyHex[:],
			)

			return &StateTransition{
				NextState: s,
				NewEvents: fn.Some(EmittedEvent{
					Outbox: []OutboxEvent{
						newClientErrorResp(
							clientID,
							errMsg,
						),
					},
				}),
			}, nil
		}

		for idx, coordinator := range s.TreeSignCoordinators {
			accepted, err := coordinator.AddNonces(
				desc.CoSignerKey, nonces,
			)
			if err != nil {
				errMsg := fmt.Sprintf(
					"failed to add nonces for tree %d: %v",
					idx, err,
				)

				return &StateTransition{
					NextState: s,
					NewEvents: fn.Some(EmittedEvent{
						Outbox: []OutboxEvent{
							newClientErrorResp(
								clientID,
								errMsg,
							),
						},
					}),
				}, nil
			}

			totalAccepted += accepted
		}
	}

	if totalAccepted == 0 {
		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					newClientErrorResp(clientID,
						"no valid nonces provided"),
				},
			}),
		}, nil
	}

	// Track that this client has submitted nonces.
	newClientsWithNonces := make(map[clientconn.ClientID]struct{})
	for cid := range s.ClientsWithNonces {
		newClientsWithNonces[cid] = struct{}{}
	}
	newClientsWithNonces[clientID] = struct{}{}

	// Create new state with updated tracking.
	newState := &AwaitingVTXONoncesState{
		ClientRegistrations:  s.ClientRegistrations,
		PSBT:                 s.PSBT,
		VTXOTrees:            s.VTXOTrees,
		ConnectorTrees:       s.ConnectorTrees,
		ConnectorAssignments: s.ConnectorAssignments,
		TreeSignCoordinators: s.TreeSignCoordinators,
		ClientsWithNonces:    newClientsWithNonces,
	}

	// Check if all clients have submitted nonces.
	if newState.allClientsSubmittedNonces() {
		return newState.transitionToVTXOSignatures(ctx, env)
	}

	// Not all clients have submitted yet - remain in current state.
	return &StateTransition{
		NextState: newState,
	}, nil
}

// transitionToVTXOSignatures handles the transition from
// AwaitingVTXONoncesState to AwaitingVTXOSignaturesState. It generates the
// operator's partial signatures, aggregates nonces, and sends aggregated
// nonces to each client.
func (s *AwaitingVTXONoncesState) transitionToVTXOSignatures(
	ctx context.Context, env *Environment) (*StateTransition, error) {

	// Generate operator's partial signatures for all trees. This must be
	// done after all client nonces are collected.
	for idx, coordinator := range s.TreeSignCoordinators {
		err := coordinator.Sign()
		if err != nil {
			return buildFailureTransition(
				ctx, env, s.ClientRegistrations,
				fmt.Sprintf("operator sign for tree %d: %v",
					idx, err),
			), nil
		}
	}

	// Prepare outbox messages with aggregated nonces for each client.
	var outboxMsgs []OutboxEvent

	// Cancel the nonces timeout.
	outboxMsgs = append(outboxMsgs, &CancelTimeoutReq{
		RoundID: env.RoundID,
		Phase:   TimeoutPhaseVTXONonces,
	})

	// Send aggregated nonces to each client with VTXOs.
	for clientID, reg := range s.ClientRegistrations {
		if len(reg.VTXODescriptors) == 0 {
			continue
		}

		// Collect signing keys for this client.
		clientKeys := make(
			[]*btcec.PublicKey, 0, len(reg.VTXODescriptors),
		)
		for _, desc := range reg.VTXODescriptors {
			clientKeys = append(clientKeys, desc.CoSignerKey)
		}

		// Aggregate nonces from all coordinators for this client.
		aggNonces := make(map[tree.TxID]tree.Musig2PubNonce)
		for _, coordinator := range s.TreeSignCoordinators {
			clientAggNonces, err := coordinator.
				GetAggNoncesForSigners(clientKeys)
			if err != nil {
				return buildFailureTransition(
					ctx, env, s.ClientRegistrations,
					fmt.Sprintf("get agg nonces for %s: %v",
						clientID, err),
				), nil
			}

			// Merge nonces from this coordinator into the
			// aggregated map.
			for txid, nonce := range clientAggNonces {
				aggNonces[txid] = nonce
			}
		}

		outboxMsgs = append(outboxMsgs, &ClientVTXOAggNonces{
			Client:    clientID,
			AggNonces: aggNonces,
		})
	}

	// Start timeout for VTXO signature collection.
	outboxMsgs = append(outboxMsgs, &StartTimeoutReq{
		RoundID:  env.RoundID,
		Phase:    TimeoutPhaseVTXOSignatures,
		Duration: env.Terms.SignatureCollectionTimeout,
	})

	return &StateTransition{
		NextState: &AwaitingVTXOSignaturesState{
			ClientRegistrations:  s.ClientRegistrations,
			PSBT:                 s.PSBT,
			VTXOTrees:            s.VTXOTrees,
			ConnectorTrees:       s.ConnectorTrees,
			ConnectorAssignments: s.ConnectorAssignments,
			TreeSignCoordinators: s.TreeSignCoordinators,
			ClientsWithSignatures: make(
				map[clientconn.ClientID]struct{},
			),
		},
		NewEvents: fn.Some(EmittedEvent{
			Outbox: outboxMsgs,
		}),
	}, nil
}

// ProcessEvent handles events in the AwaitingVTXOSignaturesState. This state
// waits for clients with VTXOs to submit their MuSig2 partial signatures.
func (s *AwaitingVTXOSignaturesState) ProcessEvent(ctx context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	switch evt := event.(type) {
	case *ClientVTXOPartialSigsEvent:
		return s.handleClientPartialSigs(ctx, env, evt)

	case *VTXOSignaturesTimeoutEvent:
		// Timeout expired before all partial sigs were collected.
		return buildFailureTransition(
			ctx, env, s.ClientRegistrations,
			"VTXO signature collection timeout",
		), nil

	default:
		return unexpectedEvent(
			s, "awaiting-vtxo-signatures", event, env,
		), nil
	}
}

// handleClientPartialSigs processes partial signatures submitted by a client,
// adding them to the tree coordinators. If all clients have submitted
// signatures, it aggregates the final signatures and transitions to
// AwaitingInputSigsState.
func (s *AwaitingVTXOSignaturesState) handleClientPartialSigs(
	ctx context.Context, env *Environment,
	evt *ClientVTXOPartialSigsEvent) (*StateTransition, error) {

	clientID := evt.ClientID

	// Check if client is registered in this round.
	reg, exists := s.ClientRegistrations[clientID]
	if !exists {
		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					newClientErrorResp(
						clientID,
						"not registered in this round",
					),
				},
			}),
		}, nil
	}

	// Check if client has VTXOs (should only accept sigs from VTXO
	// clients).
	if len(reg.VTXODescriptors) == 0 {
		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					newClientErrorResp(
						clientID, "client has no VTXOs",
					),
				},
			}),
		}, nil
	}

	if len(evt.Signatures) == 0 {
		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					newClientErrorResp(
						clientID,
						"no signatures provided",
					),
				},
			}),
		}, nil
	}

	if s.hasClientSubmittedSignatures(clientID) {
		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					newClientErrorResp(
						clientID,
						"already submitted signatures",
					),
				},
			}),
		}, nil
	}

	// Verify client submitted signatures for all their signing keys.
	for keyHex := range reg.VTXODescriptors {
		if _, ok := evt.Signatures[keyHex]; !ok {
			errMsg := fmt.Sprintf(
				"missing signatures for signing key %x",
				keyHex[:],
			)

			return &StateTransition{
				NextState: s,
				NewEvents: fn.Some(EmittedEvent{
					Outbox: []OutboxEvent{
						newClientErrorResp(
							clientID,
							errMsg,
						),
					},
				}),
			}, nil
		}
	}

	totalAccepted := 0

	for signingKeyHex, sigs := range evt.Signatures {
		if len(sigs) == 0 {
			errMsg := fmt.Sprintf(
				"no signatures for signing key %x",
				signingKeyHex[:],
			)

			return &StateTransition{
				NextState: s,
				NewEvents: fn.Some(EmittedEvent{
					Outbox: []OutboxEvent{
						newClientErrorResp(
							clientID,
							errMsg,
						),
					},
				}),
			}, nil
		}

		desc := reg.VTXODescriptors[signingKeyHex]
		if desc == nil || desc.CoSignerKey == nil ||
			sigs == nil {

			errMsg := fmt.Sprintf(
				"unknown signing key %x", signingKeyHex[:],
			)

			return &StateTransition{
				NextState: s,
				NewEvents: fn.Some(EmittedEvent{
					Outbox: []OutboxEvent{
						newClientErrorResp(
							clientID,
							errMsg,
						),
					},
				}),
			}, nil
		}

		for idx, coordinator := range s.TreeSignCoordinators {
			accepted, err := coordinator.AddPartialSignatures(
				desc.CoSignerKey, sigs,
			)
			if err != nil {
				errMsg := fmt.Sprintf(
					"failed to add sigs for tree %d: %v",
					idx, err,
				)

				return &StateTransition{
					NextState: s,
					NewEvents: fn.Some(EmittedEvent{
						Outbox: []OutboxEvent{
							newClientErrorResp(
								clientID,
								errMsg,
							),
						},
					}),
				}, nil
			}

			totalAccepted += accepted
		}
	}

	if totalAccepted == 0 {
		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					newClientErrorResp(
						clientID,
						"no valid signatures provided",
					),
				},
			}),
		}, nil
	}

	// Track that this client has submitted signatures.
	newClientsWithSignatures := make(map[clientconn.ClientID]struct{})
	for cid := range s.ClientsWithSignatures {
		newClientsWithSignatures[cid] = struct{}{}
	}
	newClientsWithSignatures[clientID] = struct{}{}

	// Create new state with updated tracking.
	newState := &AwaitingVTXOSignaturesState{
		ClientRegistrations:   s.ClientRegistrations,
		PSBT:                  s.PSBT,
		VTXOTrees:             s.VTXOTrees,
		ConnectorTrees:        s.ConnectorTrees,
		ConnectorAssignments:  s.ConnectorAssignments,
		TreeSignCoordinators:  s.TreeSignCoordinators,
		ClientsWithSignatures: newClientsWithSignatures,
	}

	// Check if all clients have submitted signatures.
	if newState.allClientsSubmittedSignatures() {
		return newState.transitionToInputSigs(ctx, env)
	}

	// Not all clients have submitted yet - remain in current state.
	return &StateTransition{
		NextState: newState,
	}, nil
}

// transitionToInputSigs handles the transition from
// AwaitingVTXOSignaturesState to AwaitingInputSigsState. It aggregates final
// signatures and sends them to each client with VTXOs.
func (s *AwaitingVTXOSignaturesState) transitionToInputSigs(
	ctx context.Context, env *Environment) (*StateTransition, error) {

	var outboxMsgs []OutboxEvent

	// Cancel the signatures timeout.
	outboxMsgs = append(outboxMsgs, &CancelTimeoutReq{
		RoundID: env.RoundID,
		Phase:   TimeoutPhaseVTXOSignatures,
	})

	// Send aggregated final signatures to each client with VTXOs.
	for clientID, reg := range s.ClientRegistrations {
		if len(reg.VTXODescriptors) == 0 {
			continue
		}

		// Collect signing keys for this client.
		clientKeys := make(
			[]*btcec.PublicKey, 0, len(reg.VTXODescriptors),
		)
		for _, desc := range reg.VTXODescriptors {
			clientKeys = append(clientKeys, desc.CoSignerKey)
		}

		// Aggregate final signatures from all coordinators for this
		// client.
		aggSigs := make(map[tree.TxID]*schnorr.Signature)
		for _, coordinator := range s.TreeSignCoordinators {
			clientSigs, err := coordinator.GetFinalSigsForSigners(
				clientKeys,
			)
			if err != nil {
				errMsg := fmt.Sprintf(
					"get final sigs for client %s: %v",
					clientID, err,
				)

				return buildFailureTransition(
					ctx, env, s.ClientRegistrations, errMsg,
				), nil
			}

			// Merge signatures from this coordinator into the
			// aggregated map.
			for txid, sig := range clientSigs {
				aggSigs[txid] = sig
			}
		}

		outboxMsgs = append(outboxMsgs, &ClientVTXOAggSigs{
			Client:  clientID,
			AggSigs: aggSigs,
		})
	}

	// Notify clients with boarding inputs that we're ready for their
	// signatures.
	for clientID, reg := range s.ClientRegistrations {
		if len(reg.BoardingInputs) > 0 {
			outboxMsgs = append(
				outboxMsgs,
				&ClientAwaitingInputSigsResp{
					Client: clientID,
				},
			)
		}
	}

	// Start timeout for input signature collection.
	outboxMsgs = append(outboxMsgs, &StartTimeoutReq{
		RoundID:  env.RoundID,
		Phase:    TimeoutPhaseInputSigs,
		Duration: env.Terms.SignatureCollectionTimeout,
	})

	return &StateTransition{
		NextState: &AwaitingInputSigsState{
			ClientRegistrations:  s.ClientRegistrations,
			PSBT:                 s.PSBT,
			VTXOTrees:            s.VTXOTrees,
			ConnectorTrees:       s.ConnectorTrees,
			ConnectorAssignments: s.ConnectorAssignments,
			ClientsSubmitted: make(
				map[clientconn.ClientID]struct{},
			),
			CollectedSignatures: make(InputSigsMap),
		},
		NewEvents: fn.Some(EmittedEvent{
			Outbox: outboxMsgs,
		}),
	}, nil
}

// ProcessEvent handles events in the ServerSigningState. This state signs the
// server's wallet inputs on the commitment transaction.
func (s *ServerSigningState) ProcessEvent(ctx context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	switch event.(type) {
	case *ServerSignInputsEvent:
		return s.handleServerSigning(ctx, env)

	default:
		return unexpectedEvent(s, "server-signing", event, env), nil
	}
}

// handleServerSigning performs server-side signing of all inputs in the PSBT.
// For boarding inputs, it adds the operator's signature to complete the
// collaborative spend path. For wallet inputs, it calls FinalizePsbt.
func (s *ServerSigningState) handleServerSigning(ctx context.Context,
	env *Environment) (*StateTransition, error) {

	// First, sign and finalize all boarding inputs with the collected
	// client signatures and the operator's signatures.
	err := s.signBoardingInputs(env)
	if err != nil {
		return buildFailureTransition(
			ctx, env, s.ClientRegistrations,
			fmt.Sprintf("failed to sign boarding inputs: %v", err),
		), nil
	}

	// Now finalize the PSBT which signs all wallet-controlled inputs.
	finalTx, err := env.WalletController.FinalizePsbt(ctx, s.PSBT)
	if err != nil {
		return buildFailureTransition(
			ctx, env, s.ClientRegistrations,
			fmt.Sprintf("failed to finalize PSBT: %v", err),
		), nil
	}

	// Persist the round to storage.
	round := &Round{
		RoundID:             env.RoundID,
		FinalTx:             finalTx,
		VTXOTrees:           s.VTXOTrees,
		ClientRegistrations: s.ClientRegistrations,
	}

	err = env.RoundStore.PersistRound(ctx, round)
	if err != nil {
		return buildFailureTransition(
			ctx, env, s.ClientRegistrations,
			fmt.Sprintf("failed to persist round: %v", err),
		), nil
	}

	// Persist VTXOs in unconfirmed state before broadcast.
	if len(s.VTXOTrees) > 0 {
		vtxos, err := collectVTXOs(
			env.RoundID, s.VTXOTrees, s.ClientRegistrations,
		)
		if err != nil {
			return buildFailureTransition(
				ctx, env, s.ClientRegistrations,
				fmt.Sprintf("collect VTXOs: %v", err),
			), nil
		}

		err = env.VTXOStore.PersistVTXOs(ctx, vtxos)
		if err != nil {
			return buildFailureTransition(
				ctx, env, s.ClientRegistrations,
				fmt.Sprintf("persist VTXOs: %v", err),
			), nil
		}
	}

	env.Log.InfoS(ctx, "Persisted round", "round_id", env.RoundID)

	return &StateTransition{
		NextState: &FinalizedState{
			ClientRegistrations: s.ClientRegistrations,
			FinalTx:             finalTx,
			VTXOTrees:           s.VTXOTrees,
		},
		NewEvents: fn.Some(EmittedEvent{
			Outbox: []OutboxEvent{
				&BroadcastRoundReq{
					RoundID:  env.RoundID,
					SignedTx: finalTx,
				},
			},
		}),
	}, nil
}

// signBoardingInputs signs all boarding inputs with both the client's
// signature (from CollectedSignatures) and the operator's signature.
func (s *ServerSigningState) signBoardingInputs(env *Environment) error {
	tx := s.PSBT.UnsignedTx

	// Build a prevout fetcher from the PSBT's WitnessUtxo fields.
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(nil)
	for i, pIn := range s.PSBT.Inputs {
		if pIn.WitnessUtxo == nil {
			return fmt.Errorf("missing WitnessUtxo for input %d", i)
		}

		prevOutFetcher.AddPrevOut(
			tx.TxIn[i].PreviousOutPoint, pIn.WitnessUtxo,
		)
	}

	// Create signature hashes for the transaction.
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)

	// Process each client's boarding inputs.
	for clientID, clientSigs := range s.CollectedSignatures {
		reg, exists := s.ClientRegistrations[clientID]
		if !exists {
			return fmt.Errorf("client %s not found in "+
				"registrations", clientID)
		}

		// Sign each boarding input for this client.
		for _, clientSig := range clientSigs {
			err := s.signSingleBoardingInput(
				env, reg, clientSig, tx, sigHashes,
				prevOutFetcher,
			)
			if err != nil {
				return fmt.Errorf("failed to sign input %d: %w",
					clientSig.InputIndex, err)
			}
		}
	}

	return nil
}

// signSingleBoardingInput signs a single boarding input with both the client's
// and operator's signatures, then sets the final script witness on the PSBT.
func (s *ServerSigningState) signSingleBoardingInput(env *Environment,
	reg *ClientRegistration, clientSig *types.BoardingInputSignature,
	tx *wire.MsgTx, sigHashes *txscript.TxSigHashes,
	prevOutFetcher txscript.PrevOutputFetcher) error {

	// Find the boarding input that matches this signature's outpoint.
	var boardingInput *BoardingInput
	for _, bi := range reg.BoardingInputs {
		if *bi.Outpoint == clientSig.Outpoint {
			boardingInput = bi

			break
		}
	}

	if boardingInput == nil {
		return fmt.Errorf("boarding input not found for outpoint %v",
			clientSig.Outpoint)
	}

	// Get the spend info for the collaborative path.
	spendInfo, err := scripts.NewVTXOSpendInfo(
		boardingInput.Tapscript, scripts.VTXOCollabPathLeaf,
	)
	if err != nil {
		return fmt.Errorf("failed to get spend info: %w", err)
	}

	inputIdx := clientSig.InputIndex

	if inputIdx < 0 || inputIdx >= len(s.PSBT.Inputs) {
		return fmt.Errorf("invalid input index: %d", inputIdx)
	}

	input := s.PSBT.Inputs[inputIdx]

	// Get the prevout for this input.
	prevOut := input.WitnessUtxo
	if prevOut == nil {
		return fmt.Errorf("missing WitnessUtxo for input %d", inputIdx)
	}

	// Sign with the operator's key.
	operatorSig, err := scripts.SignVTXOCollabInput(
		env.WalletController, tx, inputIdx, spendInfo,
		boardingInput.OperatorKeyDesc, prevOut, sigHashes,
		prevOutFetcher,
	)
	if err != nil {
		return fmt.Errorf("operator signing failed: %w", err)
	}

	// Build the witness stack with both signatures.
	witness, err := scripts.VTXOCollabSpendWitness(
		clientSig.ClientSignature, operatorSig, spendInfo,
	)
	if err != nil {
		return fmt.Errorf("failed to build witness: %w", err)
	}

	// Set the final script witness on the PSBT input.
	input.FinalScriptWitness, err = serializeWitness(witness)
	if err != nil {
		return fmt.Errorf("failed to serialize witness: %w", err)
	}

	return nil
}

// serializeWitness serializes a witness stack to the wire format expected by
// FinalScriptWitness.
func serializeWitness(witness wire.TxWitness) ([]byte, error) {
	var buf bytes.Buffer

	err := psbt.WriteTxWitness(&buf, witness)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// collectVTXOs builds a slice of VTXOs from the constructed VTXO trees for
// persistence. Each leaf in the tree corresponds to a VTXO.
func collectVTXOs(roundID RoundID, vtxoTrees map[int]*tree.Tree,
	clientRegs map[clientconn.ClientID]*ClientRegistration) ([]*VTXO,
	error) {

	const leafMissingMsg = "leaf missing outputs or cosigners"

	// Build an index of descriptors keyed by PkScript for fast lookup when
	// traversing leaves. Each VTXO descriptor has a unique script derived
	// from its signing keys.
	descriptorIndex := make(map[string]*tree.VTXODescriptor)
	for _, reg := range clientRegs {
		for _, desc := range reg.VTXODescriptors {
			key := hex.EncodeToString(desc.PkScript)
			descriptorIndex[key] = desc
		}
	}

	var vtxos []*VTXO

	for outputIdx, vtxoTree := range vtxoTrees {
		err := vtxoTree.Root.ForEachLeaf(
			func(node *tree.Node) error {
				if len(node.Outputs) == 0 ||
					len(node.CoSigners) == 0 {

					return fmt.Errorf(leafMissingMsg)
				}

				pkScript := node.Outputs[0].PkScript
				key := hex.EncodeToString(pkScript)
				desc, ok := descriptorIndex[key]
				if !ok {
					return fmt.Errorf(
						"no descriptor for leaf %x",
						pkScript,
					)
				}

				vtxos = append(vtxos, &VTXO{
					RoundID:          roundID,
					BatchOutputIndex: outputIdx,
					Descriptor:       desc,
					Status:           VTXOStatusUnconfirmed,
				})

				return nil
			},
		)
		if err != nil {
			return nil, err
		}
	}

	return vtxos, nil
}

// ProcessEvent handles events in the FinalizedState. This state holds the
// fully signed transaction ready for broadcast.
//
// TODO(elle): handle re-broadcast logic.
func (s *FinalizedState) ProcessEvent(ctx context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	switch e := event.(type) {
	case *TransactionConfirmedEvent:
		// Mark VTXOs live upon confirmation.
		if len(s.VTXOTrees) > 0 {
			err := env.VTXOStore.MarkVTXOsLive(ctx, env.RoundID)
			if err != nil {
				return buildFailureTransition(
					ctx, env, s.ClientRegistrations,
					fmt.Sprintf("mark VTXOs live: %v", err),
				), nil
			}
		}

		// Persist the round as confirmed for bookkeeping.
		err := env.RoundStore.MarkRoundConfirmed(
			ctx, env.RoundID, e.BlockHeight, e.BlockHash,
		)
		if err != nil {
			return buildFailureTransition(
				ctx, env, s.ClientRegistrations,
				fmt.Sprintf("mark round confirmed: %v", err),
			), nil
		}

		return &StateTransition{
			NextState: &ConfirmedState{
				ClientRegistrations: s.ClientRegistrations,
				FinalTx:             s.FinalTx,
				VTXOTrees:           s.VTXOTrees,
				BlockHeight:         e.BlockHeight,
				BlockHash:           e.BlockHash,
			},
		}, nil

	default:
		return unexpectedEvent(s, "finalised", event, env), nil
	}
}

// ProcessEvent handles the events from the FailedState state.
// FailedState is a terminal state, so it ignores all events.
func (s *FailedState) ProcessEvent(_ context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	return unexpectedEvent(s, "failed", event, env), nil
}

// ProcessEvent handles events in the ConfirmedState. This is a terminal state,
// so all events are ignored.
func (s *ConfirmedState) ProcessEvent(_ context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	return unexpectedEvent(s, "confirmed", event, env), nil
}

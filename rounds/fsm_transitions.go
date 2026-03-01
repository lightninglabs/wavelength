package rounds

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	// ErrJoinRequestInvalid is returned when a client's join request fails
	// validation.
	ErrJoinRequestInvalid = fmt.Errorf("join request invalid")

	// ErrJoinAuthHeightUnavailable is returned when join-auth validation
	// is enabled but no usable chain height is available.
	ErrJoinAuthHeightUnavailable = fmt.Errorf(
		"join auth validation height unavailable",
	)
)

// unexpectedEvent returns a StateTransition that remains in the current state
// and logs a warning. This is used instead of returning an error to avoid
// crashing the FSM on unexpected events.
func unexpectedEvent(state State, stateName string, event Event,
	env *Environment) *StateTransition {

	env.Log.WarnS(context.Background(), "Ignoring unexpected event", nil,
		slog.String("state", stateName),
		slog.String("event_type", fmt.Sprintf("%T", event)),
	)

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

	env.Log.DebugS(ctx, "Locking boarding inputs",
		LogInputCount(len(inputs)))

	for _, input := range inputs {
		err := env.BoardingInputLocker.Lock(
			ctx, input.Outpoint, env.RoundID,
		)
		if err != nil {
			env.Log.WarnS(ctx, "Failed to lock boarding input", err,
				LogOutpoint(input.Outpoint))

			// If we fail to lock the boarding input, return an
			// error to the client but remain in the current state.
			return fmt.Errorf("failed to lock boarding "+
				"input %v: %v", input.Outpoint, err)
		}
	}

	env.Log.DebugS(ctx, "Boarding inputs locked successfully",
		LogInputCount(len(inputs)))

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

// lockForfeitVTXOs attempts to lock all forfeit VTXOs for a client in the
// shared VTXO locker. If any lock fails, it returns an error. If all locks
// succeed,
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

	if env.VTXOLocker == nil {
		// Preserve compatibility for tests and configurations that do
		// not wire VTXOLocker yet.
		err := env.VTXOStore.LockVTXO(ctx, env.RoundID, outpoints...)
		if err != nil {
			return fmt.Errorf(
				"failed to lock forfeit VTXOs: %w", err,
			)
		}

		return nil
	}

	owner := vtxo.RoundLockOwner(env.RoundID.String())
	err := env.VTXOLocker.LockMany(ctx, outpoints, owner)
	if err != nil {
		return fmt.Errorf("failed to lock forfeit VTXOs: %w", err)
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
		ForfeitInputs:   result.ForfeitInputs,
		LeaveOutputs:    result.RequiredOutputs,
		VTXODescriptors: result.VTXODescriptors,
	}
}

// extractBoardingOutpoints extracts the outpoints from a slice of
// BoardingInputs. Returns nil if inputs is nil or empty.
func extractBoardingOutpoints(inputs []*BoardingInput) []wire.OutPoint {
	if len(inputs) == 0 {
		return nil
	}

	outpoints := make([]wire.OutPoint, 0, len(inputs))
	for _, input := range inputs {
		if input.Outpoint != nil {
			outpoints = append(outpoints, *input.Outpoint)
		}
	}

	return outpoints
}

// extractVTXOOutpoints extracts the outpoints from a slice of ForfeitInputs.
// Returns nil if inputs is nil or empty.
func extractVTXOOutpoints(inputs []*ForfeitInput) []wire.OutPoint {
	if len(inputs) == 0 {
		return nil
	}

	outpoints := make([]wire.OutPoint, 0, len(inputs))
	for _, input := range inputs {
		if input.Outpoint != nil {
			outpoints = append(outpoints, *input.Outpoint)
		}
	}

	return outpoints
}

// validateJoinRequestForAdmission validates a join request using the best
// available block height for auth freshness checks.
func validateJoinRequestForAdmission(ctx context.Context, env *Environment,
	req *types.JoinRoundRequest,
	currentBlockHeight uint32) (*JoinRequestResult, error) {

	validationHeight := currentBlockHeight
	if validationHeight == 0 {
		validationHeight = env.StartHeight
	}

	if !env.DisableJoinRequestAuth && validationHeight == 0 {
		return nil, ErrJoinAuthHeightUnavailable
	}

	return ValidateJoinRequestAtHeight(
		ctx, env, req, validationHeight,
	)
}

// ProcessEvent handles the events from the CreatedState state.
//
// Event handling:
//
//   - ClientJoinRequestEvent: Emits a ValidateAndLockJoinReq outbox event
//     and transitions to AwaitingJoinValidationState. The OutboxHandler
//     validates and locks the inputs, then feeds back a success or failure
//     event.
func (s *CreatedState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Processing event",
		LogState("Created"),
		LogEvent(event))

	switch evt := event.(type) {
	case *ClientJoinRequestEvent:
		env.Log.DebugS(ctx, "First client joining round",
			LogClientID(evt.ClientID),
			LogVTXOCount(len(evt.Request.VTXOReqs)),
			LogBoardingCount(len(evt.Request.BoardingReqs)),
			LogLeaveCount(len(evt.Request.LeaveReqs)))

		// Emit validation + locking to the OutboxHandler.
		regs := make(
			map[clientconn.ClientID]*ClientRegistration,
		)
		req := &ValidateAndLockJoinReq{
			RoundID:  env.RoundID,
			ClientID: evt.ClientID,
			Request:  evt.Request,

			CurrentBlockHeight:     evt.CurrentBlockHeight,
			StartHeight:            env.StartHeight,
			DisableJoinRequestAuth: env.DisableJoinRequestAuth,
		}

		return &StateTransition{
			NextState: &AwaitingJoinValidationState{
				ExistingRegistrations: regs,
				PendingClientID:       evt.ClientID,
				IsFirstClient:         true,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{req},
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
//   - ClientJoinRequestEvent: If the client is already registered, sends
//     ClientErrorResp. Otherwise emits a ValidateAndLockJoinReq outbox
//     event and transitions to AwaitingJoinValidationState.
//
//   - RegistrationTimeoutEvent: Registration phase timed out. Emits
//     RoundSealedReq to notify actor, then internal SealEvent to seal.
//
//   - SealEvent: Transitions to BatchBuildingState with all accumulated
//     registrations, emits BuildBatchTxEvent to start batch construction.
func (s *RegistrationState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Processing event",
		LogState("Registration"),
		LogEvent(event),
		LogClientCount(len(s.ClientRegistrations)))

	switch evt := event.(type) {
	case *ClientJoinRequestEvent:
		env.Log.DebugS(ctx, "Client requesting to join",
			LogClientID(evt.ClientID),
			LogVTXOCount(len(evt.Request.VTXOReqs)),
			LogBoardingCount(len(evt.Request.BoardingReqs)),
			LogLeaveCount(len(evt.Request.LeaveReqs)))

		// Check if client is already registered in this round.
		if s.isClientRegistered(evt.ClientID) {
			env.Log.WarnS(ctx, "Client already registered", nil,
				LogClientID(evt.ClientID))

			return clientErrorTransition(
				s, evt.ClientID, "client already registered",
			), nil
		}

		// Emit validation + locking to the OutboxHandler.
		req := &ValidateAndLockJoinReq{
			RoundID:  env.RoundID,
			ClientID: evt.ClientID,
			Request:  evt.Request,

			CurrentBlockHeight:     evt.CurrentBlockHeight,
			StartHeight:            env.StartHeight,
			DisableJoinRequestAuth: env.DisableJoinRequestAuth,
		}

		return &StateTransition{
			NextState: &AwaitingJoinValidationState{
				ExistingRegistrations: s.ClientRegistrations,
				PendingClientID:       evt.ClientID,
				IsFirstClient:         false,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{req},
			}),
		}, nil

	case *RegistrationTimeoutEvent:
		env.Log.InfoS(ctx, "Registration timeout, sealing round",
			LogClientCount(len(s.ClientRegistrations)))

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
		env.Log.InfoS(ctx, "Registration sealed, building batch",
			LogClientCount(len(s.ClientRegistrations)))

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

// ProcessEvent handles the events from the AwaitingJoinValidationState state.
//
// Event handling:
//
//   - ValidateAndLockSucceededEvent: Validation and locking succeeded.
//     Transitions to RegistrationState with the new client added. Emits a
//     ClientSuccessResp and, if this is the first client, a StartTimeoutReq
//     for the registration phase.
//
//   - ValidateAndLockFailedEvent: Validation or locking failed. Returns to
//     CreatedState (first client) or RegistrationState (subsequent client)
//     with a ClientErrorResp.
func (s *AwaitingJoinValidationState) ProcessEvent(ctx context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Processing event",
		LogState("AwaitingJoinValidation"),
		LogEvent(event))

	switch evt := event.(type) {
	case *ValidateAndLockSucceededEvent:
		env.Log.InfoS(ctx, "Join validation succeeded",
			LogClientID(evt.ClientID))

		// Build the new registrations map with the validated client.
		newRegs := make(
			map[clientconn.ClientID]*ClientRegistration,
		)
		for id, reg := range s.ExistingRegistrations {
			newRegs[id] = reg
		}
		newRegs[evt.ClientID] = newClientRegistration(
			evt.ClientID, evt.Result,
		)

		// Build the success response for the client.
		successResp := &ClientSuccessResp{
			Client:  evt.ClientID,
			RoundID: env.RoundID,
			AcceptedBoardingOutpoints: extractBoardingOutpoints(
				evt.Result.BoardingInputs,
			),
			AcceptedVTXOOutpoints: extractVTXOOutpoints(
				evt.Result.ForfeitInputs,
			),
		}

		outbox := []OutboxEvent{successResp}

		// First client triggers the registration timeout.
		if s.IsFirstClient {
			outbox = append(
				outbox,
				newStartTimeoutReq(
					env, TimeoutPhaseRegistration,
				),
			)
		}

		return &StateTransition{
			NextState: newRegistrationState(newRegs),
			NewEvents: fn.Some(EmittedEvent{
				Outbox: outbox,
			}),
		}, nil

	case *ValidateAndLockFailedEvent:
		env.Log.WarnS(ctx, "Join validation failed", nil,
			LogClientID(evt.ClientID),
			slog.String("reason", evt.Reason))

		// Return to the previous state with a client error.
		var prevState State
		if s.IsFirstClient {
			prevState = &CreatedState{}
		} else {
			prevState = newRegistrationState(
				s.ExistingRegistrations,
			)
		}

		return clientErrorTransition(
			prevState, evt.ClientID, evt.Reason,
		), nil

	default:
		return unexpectedEvent(
			s, "awaiting-join-validation", event, env,
		), nil
	}
}

// ProcessEvent handles the events from the BatchBuildingState state.
func (s *BatchBuildingState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Processing event",
		LogState("BatchBuilding"),
		LogEvent(event))

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

		env.Log.DebugS(ctx, "Building commitment transaction",
			LogBoardingCount(len(allBoardingInputs)),
			LogLeaveCount(len(allLeaveOutputs)),
			LogVTXOCount(len(allVTXODescriptors)))

		// Emit a BuildBatchReq for the OutboxHandler to perform
		// fee estimation, wallet funding, and tree construction.
		buildReq := &BuildBatchReq{
			RoundID:         env.RoundID,
			BoardingInputs:  allBoardingInputs,
			ForfeitInputs:   allForfeitInputs,
			RequiredOutputs: allLeaveOutputs,
			VTXODescriptors: allVTXODescriptors,
			Terms:           env.Terms,
			ForfeitScript:   env.ForfeitScript,
		}

		return &StateTransition{
			NextState: &AwaitingBatchBuildState{
				ClientRegistrations: s.ClientRegistrations,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{buildReq},
			}),
		}, nil

	default:
		return unexpectedEvent(s, "batch-building", event, env), nil
	}
}

// ProcessEvent handles the events from the AwaitingBatchBuildState state.
// It waits for the OutboxHandler to finish building the commitment transaction.
func (s *AwaitingBatchBuildState) ProcessEvent(ctx context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Processing event",
		LogState("AwaitingBatchBuild"),
		LogEvent(event))

	switch msg := event.(type) {
	case *BuildBatchSucceededEvent:
		env.Log.InfoS(ctx,
			"Commitment transaction built successfully",
			slog.Int("tree_count", len(msg.VTXOTrees)),
			slog.Int("input_count",
				len(msg.PSBT.Inputs)),
			slog.Int("output_count",
				len(msg.PSBT.Outputs)))

		// Transition to BatchBuiltState with the funded PSBT.
		return &StateTransition{
			NextState: &BatchBuiltState{
				ClientRegistrations:  s.ClientRegistrations,
				PSBT:                 msg.PSBT,
				VTXOTrees:            msg.VTXOTrees,
				ConnectorTrees:       msg.ConnectorTrees,
				ConnectorAssignments: msg.ConnectorAssignments,
				ConnectorDescriptors: msg.ConnectorDescriptors,
			},
			NewEvents: fn.Some(EmittedEvent{
				// Emit the internal event to prepare
				// client notifications.
				InternalEvent: []Event{
					&PrepareClientNotificationsEvent{},
				},
			}),
		}, nil

	case *BuildBatchFailedEvent:
		env.Log.WarnS(ctx, "Batch build failed", nil,
			slog.String("reason", msg.Reason))

		return buildFailureTransition(
			ctx, env, s.ClientRegistrations, msg.Reason,
		), nil

	default:
		return unexpectedEvent(
			s, "awaiting-batch-build", event, env,
		), nil
	}
}

// ProcessEvent handles the events from the BatchBuiltState state.
func (s *BatchBuiltState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Processing event",
		LogState("BatchBuilt"),
		LogEvent(event))

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
func (s *BatchBuiltState) handlePrepareClientNotifications(ctx context.Context,
	env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Preparing client notifications",
		LogClientCount(len(s.ClientRegistrations)),
		slog.Int("tree_count", len(s.VTXOTrees)))

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
				env.Log.WarnS(ctx, "Failed to extract VTXO paths", err,
					LogClientID(clientID))

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
			RoundID:          env.RoundID,
			BatchPSBT:        s.PSBT,
			VTXOTreePaths:    vtxoTreePaths,
			ConnectorLeafMap: connectorLeafMap,
		})
	}

	// Check if there are any VTXOs in the batch.
	hasVTXOs := len(s.VTXOTrees) > 0
	if hasVTXOs {
		env.Log.InfoS(ctx, "Transitioning to VTXO nonce collection",
			slog.Int("tree_count", len(s.VTXOTrees)))

		return s.transitionToVTXONonces(ctx, env, outboxMsgs)
	}

	env.Log.InfoS(ctx, "No VTXOs, transitioning to input signature collection")

	// No VTXOs - go directly to boarding signatures.
	return s.transitionToInputSigs(ctx, env, outboxMsgs)
}

// transitionToVTXONonces creates TreeSignCoordinators for each VTXO tree and
// transitions to AwaitingVTXONoncesState.
func (s *BatchBuiltState) transitionToVTXONonces(ctx context.Context,
	env *Environment, outboxMsgs []OutboxEvent) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Creating tree sign coordinators",
		slog.Int("tree_count", len(s.VTXOTrees)))

	// Create TreeSignCoordinators for each VTXO tree.
	treeCoordinators := make(map[int]*batch.TreeSignCoordinator)
	for idx, vtxoTree := range s.VTXOTrees {
		coordinator, err := batch.NewTreeSignCoordinator(
			env.WalletController, &env.Terms.OperatorKey, vtxoTree,
		)
		if err != nil {
			env.Log.WarnS(ctx, "Failed to create tree coordinator", err,
				LogOutputIndex(idx))

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

	// Count clients with boarding inputs for logging.
	clientsWithBoarding := 0

	// Notify clients with boarding inputs that we're ready for their
	// signatures. This is separate from ClientBatchInfo because there may
	// be VTXO signing phases between batch construction and boarding
	// signature collection.
	for clientID, reg := range s.ClientRegistrations {
		if len(reg.BoardingInputs) == 0 {
			continue
		}

		clientsWithBoarding++
		outboxMsgs = append(outboxMsgs, &ClientAwaitingInputSigsResp{
			Client:  clientID,
			RoundID: env.RoundID,
		})
	}

	env.Log.DebugS(ctx, "Awaiting input signatures",
		slog.Int("clients_with_boarding", clientsWithBoarding),
		LogClientCount(len(s.ClientRegistrations)))

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
			ConnectorDescriptors: s.ConnectorDescriptors,
			ClientsSubmitted: make(
				map[clientconn.ClientID]struct{},
			),
			CollectedSignatures: make(InputSigsMap),
			CollectedForfeitTxs: make(ForfeitTxsMap),
		},
		NewEvents: fn.Some(EmittedEvent{
			Outbox: outboxMsgs,
		}),
	}, nil
}

// buildFailureTransition creates a state transition to FailedState with all
// the necessary outbox events to notify clients, unlock locked inputs, and
// inform the actor of the failure. Unlocking is emitted as outbox events
// so the FSM transition itself performs no I/O.
func buildFailureTransition(_ context.Context, env *Environment,
	clientRegs map[clientconn.ClientID]*ClientRegistration,
	reason string) *StateTransition {

	env.Log.WarnS(context.Background(), "Round entering failed state", nil,
		LogReason(reason),
		LogClientCount(len(clientRegs)))

	var outboxMsgs []OutboxEvent

	// Notify each client that the round has failed.
	for clientID := range clientRegs {
		outboxMsgs = append(outboxMsgs, &ClientRoundFailedResp{
			Client:  clientID,
			RoundID: env.RoundID,
			Reason:  reason,
		})
	}

	// Emit unlock outbox events. The handler will execute the actual
	// unlock I/O — this keeps buildFailureTransition pure with respect
	// to locking side effects.
	outboxMsgs = append(outboxMsgs, &UnlockBoardingInputsReq{
		RoundID:             env.RoundID,
		ClientRegistrations: clientRegs,
	})
	outboxMsgs = append(outboxMsgs, &UnlockForfeitVTXOsReq{
		RoundID:             env.RoundID,
		ClientRegistrations: clientRegs,
	})

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

	env.Log.DebugS(ctx, "Processing event",
		LogState("AwaitingInputSigs"),
		LogEvent(event),
		LogSubmitted(len(s.ClientsSubmitted)),
		LogExpected(len(s.ClientRegistrations)))

	switch evt := event.(type) {
	case *ClientInputSignaturesEvent:
		return s.handleInputSignatures(ctx, evt, env)

	case *InputSignaturesTimeoutEvent:
		env.Log.WarnS(ctx, "Input signature collection timeout", nil,
			LogSubmitted(len(s.ClientsSubmitted)),
			LogExpected(len(s.ClientRegistrations)))

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

// handleInputSignatures processes a client's input signature submission. It
// validates boarding signatures, validates forfeit transactions, stores the
// signatures for later use, and tracks the client as having submitted. When
// all clients have submitted, it transitions to ServerSigningState.
func (s *AwaitingInputSigsState) handleInputSignatures(ctx context.Context,
	evt *ClientInputSignaturesEvent, env *Environment) (*StateTransition,
	error) {

	clientID := evt.ClientID

	env.Log.DebugS(ctx, "Received boarding signatures",
		LogClientID(clientID),
		LogSigCount(len(evt.Signatures)))

	// Check if client is registered in this round.
	reg, exists := s.ClientRegistrations[clientID]
	if !exists {
		return clientErrorTransition(s, clientID, "not registered"), nil
	}

	// Check if client already submitted.
	if s.hasClientSubmitted(clientID) {
		return clientErrorTransition(
			s, clientID, "already submitted",
		), nil
	}

	// Verify signature count matches the number of boarding inputs.
	if len(evt.Signatures) != len(reg.BoardingInputs) {
		env.Log.WarnS(ctx, "Signature count mismatch", nil,
			LogClientID(clientID),
			slog.Int("expected", len(reg.BoardingInputs)),
			slog.Int("got", len(evt.Signatures)))

		errMsg := fmt.Sprintf(
			"expected %d signatures, got %d",
			len(reg.BoardingInputs), len(evt.Signatures),
		)

		return clientErrorTransition(s, clientID, errMsg), nil
	}

	// Verify forfeit tx count matches the number of forfeit inputs.
	if len(evt.ForfeitTxs) != len(reg.ForfeitInputs) {
		errMsg := fmt.Sprintf(
			"expected %d forfeit txs, got %d",
			len(reg.ForfeitInputs), len(evt.ForfeitTxs),
		)

		return clientErrorTransition(s, clientID, errMsg), nil
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

			return clientErrorTransition(s, clientID, errMsg), nil
		}

		// Verify the input index is valid.
		if sig.InputIndex < 0 || sig.InputIndex >= len(s.PSBT.Inputs) {
			errMsg := fmt.Sprintf(
				"invalid input index: %d", sig.InputIndex,
			)

			return clientErrorTransition(s, clientID, errMsg), nil
		}

		// Verify the schnorr signature against the sighash.
		err := ValidateBoardingSignature(
			boardingInput, sig, tx, prevOutFetcher,
		)
		if err != nil {
			return clientErrorTransition(s, clientID, err.Error()),
				nil
		}
	}

	// Validate forfeit transactions if the client has forfeits.
	if len(reg.ForfeitInputs) > 0 {
		err := validateForfeitTxs(
			evt.ForfeitTxs, reg, s.ConnectorAssignments,
			env.ForfeitScript, env.Terms.OperatorKey.PubKey,
		)
		if err != nil {
			return clientErrorTransition(
				s, clientID, err.Error(),
			), nil
		}
	}

	env.Log.DebugS(ctx, "Signatures validated successfully",
		LogClientID(clientID),
		LogSigCount(len(evt.Signatures)))

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

	// Copy collected forfeit txs and add the new client's submissions.
	newCollectedForfeitTxs := make(ForfeitTxsMap)
	for id, txs := range s.CollectedForfeitTxs {
		newCollectedForfeitTxs[id] = txs
	}
	newCollectedForfeitTxs[clientID] = evt.ForfeitTxs

	env.Log.InfoS(ctx, "Client signatures accepted",
		LogClientID(clientID),
		LogSubmitted(len(newClientsSubmitted)),
		LogExpected(len(s.ClientRegistrations)))

	// Create new state with updated tracking.
	newState := &AwaitingInputSigsState{
		ClientRegistrations:  s.ClientRegistrations,
		PSBT:                 s.PSBT,
		VTXOTrees:            s.VTXOTrees,
		ConnectorTrees:       s.ConnectorTrees,
		ConnectorAssignments: s.ConnectorAssignments,
		ConnectorDescriptors: s.ConnectorDescriptors,
		ClientsSubmitted:     newClientsSubmitted,
		CollectedSignatures:  newCollectedSigs,
		CollectedForfeitTxs:  newCollectedForfeitTxs,
	}

	// Check if all clients have submitted.
	if newState.allClientsSubmitted() {
		env.Log.InfoS(ctx, "All signatures collected, transitioning to server signing",
			LogClientCount(len(s.ClientRegistrations)))

		// Cancel the input signatures timeout and transition to
		// ServerSigningState. Emit ServerSignInputsEvent to trigger
		// server signing.
		return &StateTransition{
			NextState: &ServerSigningState{
				ClientRegistrations:  s.ClientRegistrations,
				PSBT:                 s.PSBT,
				VTXOTrees:            s.VTXOTrees,
				ConnectorAssignments: s.ConnectorAssignments,
				ConnectorDescriptors: s.ConnectorDescriptors,
				CollectedSignatures:  newCollectedSigs,
				CollectedForfeitTxs:  newCollectedForfeitTxs,
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
// LND's FundPsbt cannot estimate witness weight for taproot script path spends,
// so we use a two-phase approach:
// 1. Create PSBT with just outputs (no external inputs)
//   - leave outputs (client withdrawals)
//   - VTXO tree outputs (batch outputs)
//   - Connector outputs (forfeit trees)
//   - Change output (if needed (added by FundPsbt)).
//
// 2. Fund with LND (it only sees wallet inputs)
// 3. Add boarding inputs after funding
// 4. Adjust change output to account for boarding input contribution.
//
//nolint:funlen
func buildCommitmentTx(ctx context.Context,
	terms *batch.Terms,
	feeEstimator chainfee.Estimator, confTarget uint32,
	walletCtrl WalletController, minConfs int32, walletAccount string,
	boardingInputs []*BoardingInput, forfeitInputs []*ForfeitInput,
	requiredOutputs []*wire.TxOut,
	vtxoDescriptors []tree.VTXODescriptor) (*psbt.Packet, int32,
	map[int]*tree.Tree, map[int]*tree.Tree,
	map[wire.OutPoint]*ConnectorLeafAssignment, error) {

	// Calculate boarding input totals for later adjustment.
	var totalBoardingValue btcutil.Amount
	for _, bi := range boardingInputs {
		totalBoardingValue += bi.Value
	}

	feeRate, err := feeEstimator.EstimateFeePerKW(confTarget)
	if err != nil {
		return nil, -1, nil, nil, nil,
			fmt.Errorf("estimate fee: %w", err)
	}

	// Calculate fee for boarding inputs using LND's weight estimator. We
	// calculate this ourselves since LND's FundPsbt cannot estimate
	// witness weight for taproot script path spends.
	//
	// The witness for a collaborative tapscript spend consists of:
	// - 2 schnorr signatures: 64 * 2 = 128 bytes
	// - Script: ~70 bytes (2-of-2 multisig script)
	// - Control block: ~33 bytes (1 byte header + 32 byte internal key)
	// - Encoding overhead: ~4 bytes
	// Total: ~235 witness bytes = 235 weight units.
	const boardingWitnessWeight = 235
	var weightEstimator input.TxWeightEstimator
	for range boardingInputs {
		weightEstimator.AddWitnessInput(boardingWitnessWeight)
	}
	boardingFee := feeRate.FeeForWeight(weightEstimator.Weight())

	// Next, we'll create outputs-only transaction for funding. We don't
	// include boarding inputs here because LND can't estimate their
	// witness weight. Instead, we'll add them after funding and adjust the
	// change output.
	tx := wire.NewMsgTx(2)

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
			terms, vtxoDescriptors,
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
		maxPerTree := int(terms.MaxConnectorsPerTree)
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
				numInOutput, terms.ConnectorDustAmount,
				terms.ConnectorAddress,
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

	packet, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, -1, nil, nil, nil, fmt.Errorf("create psbt: %w",
			err)
	}

	// Now we'll call FundPsbt to add wallet inputs and change.
	//
	// Note: FundPsbt reorders inputs and outputs, so any indices recorded
	// before this call will be invalid.
	changeIdx, err := walletCtrl.FundPsbt(
		ctx, packet, minConfs, feeRate, walletAccount,
	)
	if err != nil {
		return nil, -1, nil, nil, nil, fmt.Errorf("fund psbt: %w",
			err)
	}

	// Now we'll add the boarding inputs to the funded PSBT. Since LND
	// cannot estimate witness weight for taproot script path spends, we
	// add boarding inputs after funding.
	for _, bi := range boardingInputs {
		// Add input to the transaction.
		packet.UnsignedTx.TxIn = append(packet.UnsignedTx.TxIn,
			&wire.TxIn{
				PreviousOutPoint: *bi.Outpoint,
				Sequence:         wire.MaxTxInSequenceNum,
			},
		)

		// Add PSBT input metadata.
		collabLeaf := bi.Tapscript.Leaves[0]
		ctrlBlockBytes, err := bi.Tapscript.ControlBlock.ToBytes()
		if err != nil {
			return nil, -1, nil, nil, nil,
				fmt.Errorf("serialize control block: %w", err)
		}

		leafHash := txscript.NewTapLeaf(
			collabLeaf.LeafVersion, collabLeaf.Script,
		).TapHash()
		leafHashBytes := leafHash[:]

		// Build the BIP32 derivation path for the operator key.
		keyFamily := uint32(bi.OperatorKeyDesc.Family)
		bip32Path := []uint32{keyFamily, bi.OperatorKeyDesc.Index}

		packet.Inputs = append(packet.Inputs, psbt.PInput{
			WitnessUtxo: &wire.TxOut{
				Value:    int64(bi.Value),
				PkScript: bi.PkScript,
			},
			SighashType: txscript.SigHashDefault,
			TaprootLeafScript: []*psbt.TaprootTapLeafScript{
				{
					ControlBlock: ctrlBlockBytes,
					Script:       collabLeaf.Script,
					LeafVersion:  collabLeaf.LeafVersion,
				},
			},
			TaprootMerkleRoot: bi.Tapscript.ControlBlock.RootHash(
				collabLeaf.Script,
			),
			TaprootInternalKey: schnorr.SerializePubKey(
				bi.Tapscript.ControlBlock.InternalKey,
			),
			TaprootBip32Derivation: []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						bi.OperatorKeyDesc.PubKey,
					),
					LeafHashes: [][]byte{
						leafHashBytes,
					},
					MasterKeyFingerprint: 0,
					Bip32Path:            bip32Path,
				},
			},
		})
	}

	// Adjust change output to account for boarding input value. Boarding
	// inputs contribute: value - fee. This extra value goes to the change
	// output (or reduces what the wallet needed to provide).
	if changeIdx >= 0 && len(boardingInputs) > 0 {
		boardingContribution := totalBoardingValue - boardingFee
		packet.UnsignedTx.TxOut[changeIdx].Value += int64(
			boardingContribution,
		)
	}

	// Next, we'll build VTXO trees if VTXOs exist.
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
				terms, packet.UnsignedTx, forfeitInputs,
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

// buildConnectorDescriptors constructs connector tree descriptors from
// connector assignments.
func buildConnectorDescriptors(
	connectorAssignments map[wire.OutPoint]*ConnectorLeafAssignment,
	forfeitScript []byte) ([]*ConnectorTreeDescriptor, error) {

	if len(connectorAssignments) == 0 {
		return nil, nil
	}

	counts := make(map[int]int)
	for _, assignment := range connectorAssignments {
		if assignment == nil {
			return nil, fmt.Errorf(
				"connector assignment cannot be nil",
			)
		}

		if assignment.ConnectorOutputIndex < 0 {
			return nil, fmt.Errorf(
				"connector output index must be non-negative",
			)
		}

		counts[assignment.ConnectorOutputIndex]++
	}

	outputIndices := make([]int, 0, len(counts))
	for idx := range counts {
		outputIndices = append(outputIndices, idx)
	}
	sort.Ints(outputIndices)

	descriptors := make([]*ConnectorTreeDescriptor, 0, len(outputIndices))
	for _, idx := range outputIndices {
		descriptors = append(descriptors, &ConnectorTreeDescriptor{
			OutputIndex:   idx,
			NumLeaves:     counts[idx],
			ForfeitScript: forfeitScript,
		})
	}

	return descriptors, nil
}

// buildConnectorTreesAndAssignments builds connector trees and assigns each
// forfeit input to a connector leaf.
func buildConnectorTreesAndAssignments(terms *batch.Terms,
	tx *wire.MsgTx, forfeitInputs []*ForfeitInput,
	connectorOutputIndices []int) (map[int]*tree.Tree,
	map[wire.OutPoint]*ConnectorLeafAssignment, error) {

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

	maxPerTree := int(terms.MaxConnectorsPerTree)
	if maxPerTree <= 0 {
		return nil, nil, fmt.Errorf(
			"max connectors per tree must be > 0",
		)
	}

	if terms.ConnectorDustAmount <= 0 {
		return nil, nil, fmt.Errorf(
			"connector dust amount must be > 0",
		)
	}

	if terms.ConnectorAddress == nil {
		return nil, nil, fmt.Errorf("connector address cannot be nil")
	}

	if terms.OperatorKey.PubKey == nil {
		return nil, nil, fmt.Errorf("operator key cannot be nil")
	}

	radix := int(terms.TreeRadix)
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
			Amount:    terms.ConnectorDustAmount,
		}

		connectorTree, err := tree.BuildConnectorTree(
			connectorOutpoint, connectorOutput, connectorDesc,
			terms.OperatorKey.PubKey, radix,
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

			outpoint := *forfeitInput.Outpoint
			connectorAssignments[outpoint] =
				&ConnectorLeafAssignment{
					ConnectorOutputIndex: outputIdx,
					LeafIndex:            leafIdx,
					LeafOutpoint:         *leafOutpoint,
					LeafOutput:           leafOutput,
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

	env.Log.DebugS(ctx, "Processing event",
		LogState("AwaitingVTXONonces"),
		LogEvent(event),
		LogSubmitted(len(s.ClientsWithNonces)),
		slog.Int("tree_count", len(s.VTXOTrees)))

	switch evt := event.(type) {
	case *ClientVTXONoncesEvent:
		return s.handleClientNonces(ctx, env, evt)

	case *VTXONoncesTimeoutEvent:
		env.Log.WarnS(ctx, "VTXO nonce collection timeout", nil,
			LogSubmitted(len(s.ClientsWithNonces)))

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
func (s *AwaitingVTXONoncesState) handleClientNonces(ctx context.Context,
	env *Environment, evt *ClientVTXONoncesEvent) (*StateTransition,
	error) {

	clientID := evt.ClientID

	env.Log.DebugS(ctx, "Received VTXO nonces",
		LogClientID(clientID),
		LogKeyCount(len(evt.Nonces)))

	// Check if client is registered in this round.
	reg, exists := s.ClientRegistrations[clientID]
	if !exists {
		env.Log.WarnS(ctx, "Client not registered", nil,
			LogClientID(clientID))

		return clientErrorTransition(
			s, clientID, "not registered in this round",
		), nil
	}

	// Only accept nonces from clients with VTXOs.
	if len(reg.VTXODescriptors) == 0 {
		env.Log.WarnS(ctx, "Client has no VTXOs", nil,
			LogClientID(clientID))

		return clientErrorTransition(
			s, clientID, "client has no VTXOs",
		), nil
	}

	if s.hasClientSubmittedNonces(clientID) {
		env.Log.WarnS(ctx, "Client already submitted nonces", nil,
			LogClientID(clientID))

		return clientErrorTransition(
			s, clientID, "already submitted nonces",
		), nil
	}

	if len(evt.Nonces) == 0 {
		env.Log.WarnS(ctx, "No nonces provided", nil,
			LogClientID(clientID))

		return clientErrorTransition(
			s, clientID, "no nonces provided",
		), nil
	}

	// Verify client submitted nonces for all their signing keys.
	for keyHex := range reg.VTXODescriptors {
		if _, ok := evt.Nonces[keyHex]; !ok {
			errMsg := fmt.Sprintf(
				"missing nonces for signing key %x", keyHex[:],
			)

			return clientErrorTransition(s, clientID, errMsg), nil
		}
	}

	totalAccepted := 0

	for signingKeyHex, nonces := range evt.Nonces {
		if len(nonces) == 0 {
			errMsg := fmt.Sprintf(
				"no nonces for signing key %x",
				signingKeyHex[:],
			)

			return clientErrorTransition(s, clientID, errMsg), nil
		}

		desc := reg.VTXODescriptors[signingKeyHex]
		if desc == nil || desc.CoSignerKey == nil || nonces == nil {
			errMsg := fmt.Sprintf(
				"unknown signing key %x", signingKeyHex[:],
			)

			return clientErrorTransition(s, clientID, errMsg), nil
		}

		for idx, coordinator := range s.TreeSignCoordinators {
			accepted, err := coordinator.AddNonces(
				desc.CoSignerKey, nonces,
			)
			if err != nil {
				errMsg := fmt.Sprintf(
					"add nonces for tree %d: %v", idx, err,
				)

				return clientErrorTransition(
					s, clientID, errMsg,
				), nil
			}

			totalAccepted += accepted
		}
	}

	if totalAccepted == 0 {
		env.Log.WarnS(ctx, "No valid nonces provided", nil,
			LogClientID(clientID))

		return clientErrorTransition(
			s, clientID, "no valid nonces provided",
		), nil
	}

	env.Log.DebugS(ctx, "Nonces validated successfully",
		LogClientID(clientID),
		slog.Int("accepted_count", totalAccepted))

	// Track that this client has submitted nonces.
	newClientsWithNonces := make(map[clientconn.ClientID]struct{})
	for cid := range s.ClientsWithNonces {
		newClientsWithNonces[cid] = struct{}{}
	}
	newClientsWithNonces[clientID] = struct{}{}

	env.Log.InfoS(ctx, "Client nonces accepted",
		LogClientID(clientID),
		LogSubmitted(len(newClientsWithNonces)))

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
		env.Log.InfoS(ctx, "All nonces collected, transitioning "+
			"to VTXO signatures")

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

	env.Log.DebugS(ctx, "Generating operator partial signatures",
		slog.Int("tree_count", len(s.TreeSignCoordinators)))

	// Generate operator's partial signatures for all trees. This must be
	// done after all client nonces are collected.
	for idx, coordinator := range s.TreeSignCoordinators {
		err := coordinator.Sign()
		if err != nil {
			env.Log.WarnS(ctx, "Operator signing failed", err,
				LogOutputIndex(idx))

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
			RoundID:   env.RoundID,
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

	env.Log.DebugS(ctx, "Processing event",
		LogState("AwaitingVTXOSignatures"),
		LogEvent(event),
		LogSubmitted(len(s.ClientsWithSignatures)))

	switch evt := event.(type) {
	case *ClientVTXOPartialSigsEvent:
		return s.handleClientPartialSigs(ctx, env, evt)

	case *VTXOSignaturesTimeoutEvent:
		env.Log.WarnS(ctx, "VTXO signature collection timeout", nil,
			LogSubmitted(len(s.ClientsWithSignatures)))

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

	env.Log.DebugS(ctx, "Received VTXO partial signatures",
		LogClientID(clientID),
		LogKeyCount(len(evt.Signatures)))

	// Check if client is registered in this round.
	reg, exists := s.ClientRegistrations[clientID]
	if !exists {
		env.Log.WarnS(ctx, "Client not registered", nil,
			LogClientID(clientID))

		return clientErrorTransition(
			s, clientID, "not registered in this round",
		), nil
	}

	// Only accept signatures from clients with VTXOs.
	if len(reg.VTXODescriptors) == 0 {
		env.Log.WarnS(ctx, "Client has no VTXOs", nil,
			LogClientID(clientID))

		return clientErrorTransition(
			s, clientID, "client has no VTXOs",
		), nil
	}

	if len(evt.Signatures) == 0 {
		env.Log.WarnS(ctx, "No signatures provided", nil,
			LogClientID(clientID))

		return clientErrorTransition(
			s, clientID, "no signatures provided",
		), nil
	}

	if s.hasClientSubmittedSignatures(clientID) {
		env.Log.WarnS(ctx, "Client already submitted signatures", nil,
			LogClientID(clientID))

		return clientErrorTransition(
			s, clientID, "already submitted signatures",
		), nil
	}

	// Verify client submitted signatures for all their signing keys.
	for keyHex := range reg.VTXODescriptors {
		if _, ok := evt.Signatures[keyHex]; !ok {
			errMsg := fmt.Sprintf(
				"missing signatures for signing key %x",
				keyHex[:],
			)

			return clientErrorTransition(s, clientID, errMsg), nil
		}
	}

	totalAccepted := 0

	for signingKeyHex, sigs := range evt.Signatures {
		if len(sigs) == 0 {
			errMsg := fmt.Sprintf(
				"no signatures for signing key %x",
				signingKeyHex[:],
			)

			return clientErrorTransition(s, clientID, errMsg), nil
		}

		desc := reg.VTXODescriptors[signingKeyHex]
		if desc == nil || desc.CoSignerKey == nil || sigs == nil {
			errMsg := fmt.Sprintf(
				"unknown signing key %x", signingKeyHex[:],
			)

			return clientErrorTransition(s, clientID, errMsg), nil
		}

		for idx, coordinator := range s.TreeSignCoordinators {
			accepted, err := coordinator.AddPartialSignatures(
				desc.CoSignerKey, sigs,
			)
			if err != nil {
				errMsg := fmt.Sprintf(
					"add sigs for tree %d: %v", idx, err,
				)

				return clientErrorTransition(
					s, clientID, errMsg,
				), nil
			}

			totalAccepted += accepted
		}
	}

	if totalAccepted == 0 {
		return clientErrorTransition(
			s, clientID, "no valid signatures provided",
		), nil
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
			RoundID: env.RoundID,
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
					Client:  clientID,
					RoundID: env.RoundID,
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

	connectorDescriptors, err := buildConnectorDescriptors(
		s.ConnectorAssignments, env.ForfeitScript,
	)
	if err != nil {
		return buildFailureTransition(
			ctx, env, s.ClientRegistrations,
			fmt.Sprintf("build connector descriptors: %v", err),
		), nil
	}

	return &StateTransition{
		NextState: &AwaitingInputSigsState{
			ClientRegistrations:  s.ClientRegistrations,
			PSBT:                 s.PSBT,
			VTXOTrees:            s.VTXOTrees,
			ConnectorTrees:       s.ConnectorTrees,
			ConnectorAssignments: s.ConnectorAssignments,
			ConnectorDescriptors: connectorDescriptors,
			ClientsSubmitted: make(
				map[clientconn.ClientID]struct{},
			),
			CollectedSignatures: make(InputSigsMap),
			CollectedForfeitTxs: make(ForfeitTxsMap),
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

	env.Log.DebugS(ctx, "Processing event",
		LogState("ServerSigning"),
		LogEvent(event))

	switch event.(type) {
	case *ServerSignInputsEvent:
		return s.handleServerSigning(ctx, env)

	default:
		return unexpectedEvent(s, "server-signing", event, env), nil
	}
}

// handleServerSigning emits a SignAndFinalizeRoundReq outbox event for
// the handler to perform signing I/O (SignOutputRaw, FinalizePsbt) and
// transitions to AwaitingSignAndFinalizeState. This keeps the FSM pure.
func (s *ServerSigningState) handleServerSigning(ctx context.Context,
	env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Emitting sign-and-finalize request",
		slog.Int("input_count", len(s.PSBT.Inputs)),
		LogClientCount(len(s.CollectedSignatures)))

	signReq := &SignAndFinalizeRoundReq{
		RoundID:              env.RoundID,
		PSBT:                 s.PSBT,
		CollectedSignatures:  s.CollectedSignatures,
		CollectedForfeitTxs:  s.CollectedForfeitTxs,
		ClientRegistrations:  s.ClientRegistrations,
		ConnectorAssignments: s.ConnectorAssignments,
		OperatorKey:          env.Terms.OperatorKey,
		VTXOExitDelay:        env.Terms.VTXOExitDelay,
	}

	return &StateTransition{
		NextState: &AwaitingSignAndFinalizeState{
			ClientRegistrations:  s.ClientRegistrations,
			VTXOTrees:            s.VTXOTrees,
			ConnectorDescriptors: s.ConnectorDescriptors,
			SweepKey:             env.Terms.SweepKey.PubKey,
			CSVDelay:             env.Terms.SweepDelay,
			StartHeight:          env.StartHeight,
		},
		NewEvents: fn.Some(EmittedEvent{
			Outbox: []OutboxEvent{signReq},
		}),
	}, nil
}

// signBoardingInputs signs all boarding inputs with both the client's
// signature (from CollectedSignatures) and the operator's signature.
// This is a free function so it can be called from both FSM transitions
// and the OutboxHandler.
func signBoardingInputs(psbtPacket *psbt.Packet,
	collectedSigs InputSigsMap,
	clientRegs map[clientconn.ClientID]*ClientRegistration,
	walletCtrl WalletController) error {

	tx := psbtPacket.UnsignedTx

	// Build a prevout fetcher from the PSBT's WitnessUtxo fields.
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(nil)
	for i, pIn := range psbtPacket.Inputs {
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
	for clientID, clientSigs := range collectedSigs {
		reg, exists := clientRegs[clientID]
		if !exists {
			return fmt.Errorf("client %s not found in "+
				"registrations", clientID)
		}

		// Sign each boarding input for this client.
		for _, clientSig := range clientSigs {
			err := signSingleBoardingInput(
				psbtPacket, reg, clientSig, tx,
				sigHashes, prevOutFetcher,
				walletCtrl,
			)
			if err != nil {
				return fmt.Errorf("failed to sign input %d: %w",
					clientSig.InputIndex, err)
			}
		}
	}

	return nil
}

// signSingleBoardingInput signs a single boarding input with both the
// client's and operator's signatures, then sets the final script witness
// on the PSBT. This is a free function so it can be called from both FSM
// transitions and the OutboxHandler.
func signSingleBoardingInput(psbtPacket *psbt.Packet,
	reg *ClientRegistration, clientSig *types.BoardingInputSignature,
	tx *wire.MsgTx, sigHashes *txscript.TxSigHashes,
	prevOutFetcher txscript.PrevOutputFetcher,
	walletCtrl WalletController) error {

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

	if inputIdx < 0 || inputIdx >= len(psbtPacket.Inputs) {
		return fmt.Errorf("invalid input index: %d", inputIdx)
	}

	// Use a pointer to modify the actual PSBT input, not a copy.
	input := &psbtPacket.Inputs[inputIdx]

	// Get the prevout for this input.
	prevOut := input.WitnessUtxo
	if prevOut == nil {
		return fmt.Errorf("missing WitnessUtxo for input %d", inputIdx)
	}

	// Sign with the operator's key.
	operatorSig, err := scripts.SignVTXOCollabInput(
		walletCtrl, tx, inputIdx, spendInfo,
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

				// Compute the outpoint for this VTXO leaf.
				outpoint, err := node.GetNonAnchorOutpoint()
				if err != nil {
					return fmt.Errorf(
						"get VTXO outpoint: %w", err,
					)
				}

				vtxos = append(vtxos, &VTXO{
					Outpoint:         *outpoint,
					RoundID:          roundID,
					BatchOutputIndex: outputIdx,
					Descriptor:       desc,
					Status:           VTXOStatusPending,
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

// ProcessEvent handles events in AwaitingSignAndFinalizeState. This state
// waits for the OutboxHandler to complete signing and PSBT finalization.
//
// On success the FSM emits a PersistServerSigningReq and transitions to
// AwaitingServerSignPersistState. On failure it transitions to FailedState
// via buildFailureTransition.
func (s *AwaitingSignAndFinalizeState) ProcessEvent(ctx context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Processing event",
		LogState("AwaitingSignAndFinalize"),
		LogEvent(event))

	switch e := event.(type) {
	case *SignAndFinalizeSucceededEvent:
		env.Log.InfoS(ctx, "Signing and finalization complete",
			LogTxID(e.FinalTx.TxHash().String()))

		// Construct the persistence request from the state's
		// carried-forward data and the event's signing results.
		persistReq := &PersistServerSigningReq{
			RoundID:              env.RoundID,
			FinalTx:              e.FinalTx,
			VTXOTrees:            s.VTXOTrees,
			ForfeitInfos:         e.ForfeitInfos,
			ClientRegistrations:  s.ClientRegistrations,
			ConnectorDescriptors: s.ConnectorDescriptors,
			SweepKey:             s.SweepKey,
			CSVDelay:             s.CSVDelay,
		}

		return &StateTransition{
			NextState: &AwaitingServerSignPersistState{
				ClientRegistrations: s.ClientRegistrations,
				FinalTx:             e.FinalTx,
				VTXOTrees:           s.VTXOTrees,
				ForfeitInfos:        e.ForfeitInfos,
				StartHeight:         s.StartHeight,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{persistReq},
			}),
		}, nil

	case *SignAndFinalizeFailedEvent:
		return buildFailureTransition(
			ctx, env, s.ClientRegistrations, e.Reason,
		), nil

	default:
		return unexpectedEvent(
			s, "awaiting_sign_and_finalize", event, env,
		), nil
	}
}

// ProcessEvent handles events in AwaitingServerSignPersistState. This state
// waits for the OutboxHandler to complete persistence of the round and VTXOs.
//
// On success the FSM transitions to FinalizedState and emits a
// BroadcastRoundReq. On failure it transitions to FailedState via
// buildFailureTransition.
func (s *AwaitingServerSignPersistState) ProcessEvent(ctx context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Processing event",
		LogState("AwaitingServerSignPersist"),
		LogEvent(event))

	switch e := event.(type) {
	case *PersistServerSigningSucceededEvent:
		env.Log.InfoS(ctx, "Persisted round",
			"round_id", env.RoundID)

		return &StateTransition{
			NextState: &FinalizedState{
				ClientRegistrations: s.ClientRegistrations,
				FinalTx:             s.FinalTx,
				VTXOTrees:           s.VTXOTrees,
				ForfeitInfos:        s.ForfeitInfos,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&BroadcastRoundReq{
						RoundID:     env.RoundID,
						SignedTx:    s.FinalTx,
						StartHeight: s.StartHeight,
					},
				},
			}),
		}, nil

	case *PersistServerSigningFailedEvent:
		return buildFailureTransition(
			ctx, env, s.ClientRegistrations, e.Reason,
		), nil

	default:
		return unexpectedEvent(
			s, "awaiting_server_sign_persist", event, env,
		), nil
	}
}

// ProcessEvent handles events in the FinalizedState. This state holds the
// fully signed transaction ready for broadcast.
//
// On TransactionConfirmedEvent the FSM emits a ConfirmRoundReq outbox event
// and transitions to AwaitingConfirmPersistState. The OutboxHandler performs
// the actual DB persistence and feeds back a success or failure event.
//
// TODO(elle): handle re-broadcast logic.
func (s *FinalizedState) ProcessEvent(ctx context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Processing event",
		LogState("Finalized"),
		LogEvent(event))

	switch e := event.(type) {
	case *TransactionConfirmedEvent:
		env.Log.InfoS(ctx, "Transaction confirmed",
			slog.Int("block_height", int(e.BlockHeight)),
			slog.Int("vtxo_trees", len(s.VTXOTrees)))

		// Emit an outbox event requesting the handler to persist
		// confirmation data. The handler will mark VTXOs live,
		// record forfeits, and mark the round confirmed.
		outbox := []OutboxEvent{
			&ConfirmRoundReq{
				RoundID:      env.RoundID,
				VTXOTrees:    s.VTXOTrees,
				ForfeitInfos: s.ForfeitInfos,
				BlockHeight:  e.BlockHeight,
				BlockHash:    e.BlockHash,
			},
		}

		return &StateTransition{
			NextState: &AwaitingConfirmPersistState{
				ClientRegistrations: s.ClientRegistrations,
				FinalTx:             s.FinalTx,
				VTXOTrees:           s.VTXOTrees,
				BlockHeight:         e.BlockHeight,
				BlockHash:           e.BlockHash,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: outbox,
			}),
		}, nil

	default:
		return unexpectedEvent(s, "finalised", event, env), nil
	}
}

// ProcessEvent handles events in the AwaitingConfirmPersistState. This state
// waits for the OutboxHandler to complete persistence of round confirmation
// data.
func (s *AwaitingConfirmPersistState) ProcessEvent(ctx context.Context,
	event Event, env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Processing event",
		LogState("AwaitingConfirmPersist"),
		LogEvent(event))

	switch e := event.(type) {
	case *ConfirmRoundSucceededEvent:
		env.Log.InfoS(ctx, "Round confirmed and complete",
			slog.Int("block_height", int(s.BlockHeight)))

		return &StateTransition{
			NextState: &ConfirmedState{
				ClientRegistrations: s.ClientRegistrations,
				FinalTx:             s.FinalTx,
				VTXOTrees:           s.VTXOTrees,
				BlockHeight:         s.BlockHeight,
				BlockHash:           s.BlockHash,
			},
		}, nil

	case *ConfirmRoundFailedEvent:
		// The transaction IS confirmed on-chain so unlocking
		// inputs is nonsensical — only notify clients and the
		// actor of the persistence failure.
		var outboxMsgs []OutboxEvent
		for clientID := range s.ClientRegistrations {
			outboxMsgs = append(
				outboxMsgs, &ClientRoundFailedResp{
					Client:  clientID,
					RoundID: env.RoundID,
					Reason:  e.Reason,
				},
			)
		}
		outboxMsgs = append(outboxMsgs, &RoundFailedReq{
			FailedRoundID: env.RoundID,
			Reason:        e.Reason,
		})

		return &StateTransition{
			NextState: &FailedState{
				Reason: e.Reason,
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: outboxMsgs,
			}),
		}, nil

	default:
		return unexpectedEvent(
			s, "awaiting_confirm_persist", event, env,
		), nil
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

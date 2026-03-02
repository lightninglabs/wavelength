package round

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/tx"
	"github.com/lightninglabs/darepo-client/lib/types"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// buildBoardingRequest constructs a types.BoardingRequest from a
// BoardingIntent.
func buildBoardingRequest(intent BoardingIntent) types.BoardingRequest {
	return intent.Request
}

// failWithNotification creates a state transition to ClientFailedState and
// emits a RoundFailedNotification. This is the standard pattern for handling
// internal errors without returning an error to the FSM (which would halt it).
func failWithNotification(reason string, err error, recoverable bool,
	roundID fn.Option[RoundID]) *ClientStateTransition {

	return &ClientStateTransition{
		NextState: &ClientFailedState{
			Reason:      reason,
			Error:       err,
			Recoverable: recoverable,
		},
		NewEvents: fn.Some(ClientEmittedEvent{
			Outbox: []ClientOutMsg{
				&RoundFailedNotification{
					RoundID:       roundID,
					Reason:        reason,
					Recoverable:   recoverable,
					OriginalError: err,
				},
			},
		}),
	}
}

// selfLoop creates a self-loop transition that stays in the current state
// without emitting any events. Used for unknown events in non-terminal states
// to avoid halting the FSM.
func selfLoop(state ClientState) *ClientStateTransition {
	return &ClientStateTransition{
		NextState: state,
	}
}

// signBoardingInputs signs all boarding inputs for a commitment transaction.
// This builds the PrevOutputFetcher, sigHashes, and generates Schnorr
// signatures for each boarding intent's input.
func signBoardingInputs(wallet ClientWallet, commitmentTx *psbt.Packet,
	intents Intents, boardingInputIndices map[wire.OutPoint]int,
) ([]*types.BoardingInputSignature, error) {

	tx := commitmentTx.UnsignedTx

	// Build a PrevOutputFetcher from ALL PSBT inputs. Taproot sighash
	// (BIP341) requires prevout info for all inputs.
	prevOuts := make(map[wire.OutPoint]*wire.TxOut)
	for i, pIn := range commitmentTx.Inputs {
		if pIn.WitnessUtxo == nil {
			return nil, fmt.Errorf("PSBT input %d missing "+
				"WitnessUtxo", i)
		}
		outpoint := tx.TxIn[i].PreviousOutPoint
		prevOuts[outpoint] = pIn.WitnessUtxo
	}
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)

	// Build structured boarding input signatures for each intent.
	var boardingInputSigs []*types.BoardingInputSignature
	for _, boardingIntent := range intents.Boarding {
		outpoint := boardingIntent.Request.Outpoint
		inputIdx, found := boardingInputIndices[*outpoint]
		if !found {
			return nil, fmt.Errorf("no input index "+
				"found for boarding outpoint %s",
				outpoint)
		}

		spendInfo, err := scripts.NewVTXOSpendInfo(
			boardingIntent.Address.Tapscript,
			scripts.VTXOCollabPathLeaf,
		)
		if err != nil {
			return nil, err
		}

		chainInfo := boardingIntent.ChainInfo
		addr := boardingIntent.Address.Address
		amt := chainInfo.Amount

		// Use PayToAddrScript to get the full pkScript with OP_1
		// OP_PUSHBYTES_32 prefix for P2TR addresses. ScriptAddress()
		// only returns the 32-byte witness program.
		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, fmt.Errorf("pay to addr script: %w", err)
		}

		output := &wire.TxOut{
			Value:    int64(amt),
			PkScript: pkScript,
		}

		signature, err := scripts.SignVTXOCollabInput(
			wallet, tx, inputIdx, spendInfo,
			&boardingIntent.Address.KeyDesc, output,
			sigHashes, prevOutFetcher,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to sign "+
				"boarding input %d: %w", inputIdx, err)
		}

		schnorrSig, ok := signature.(*schnorr.Signature)
		if !ok {
			return nil, fmt.Errorf("signature is not a " +
				"schnorr signature")
		}

		inputSig := &types.BoardingInputSignature{
			InputIndex:      inputIdx,
			Outpoint:        *outpoint,
			ClientSignature: schnorrSig,
		}
		boardingInputSigs = append(boardingInputSigs, inputSig)
	}

	return boardingInputSigs, nil
}

// ProcessEvent handles events in the Idle state. The only pool-addition
// event is IntentPackage — the actor layer converts all raw inputs
// (boarding confirmations, VTXO requests, refresh/leave) into
// IntentPackage before sending to the FSM.
func (s *Idle) ProcessEvent(ctx context.Context, event ClientEvent,
	env *ClientEnvironment) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *IntentPackage:
		if evt.isEmpty() {
			return selfLoop(s), nil
		}

		env.Log.InfoS(ctx, "Starting round assembly from "+
			"intent package", evt.logAttributes())

		return &ClientStateTransition{
			NextState: &PendingRoundAssembly{
				Boarding: slices.Clone(evt.Boarding),
				Forfeits: slices.Clone(evt.Forfeits),
				VTXOs:    slices.Clone(evt.VTXOs),
				Leaves:   slices.Clone(evt.Leaves),
			},
		}, nil

	default:
		// Self-loop on unknown events - do not halt the FSM.
		return selfLoop(s), nil
	}
}

// ProcessEvent for PendingRoundAssembly tracks confirmed boarding intents and
// transitions to registration once all are ready.
//
//nolint:funlen
func (s *PendingRoundAssembly) ProcessEvent(ctx context.Context,
	event ClientEvent, env *ClientEnvironment) (*ClientStateTransition,
	error) {

	switch evt := event.(type) {
	case *IntentPackage:
		// An atomic bundle of intents. Unpack into our pools,
		// deduplicating boarding intents by outpoint.
		if evt.isEmpty() {
			return selfLoop(s), nil
		}

		// Deduplicate boarding intents by outpoint.
		updatedBoarding := slices.Clone(s.Boarding)
		for _, newIntent := range evt.Boarding {
			dup := slices.ContainsFunc(
				updatedBoarding,
				func(b BoardingIntent) bool {
					return b.Outpoint ==
						newIntent.Outpoint
				},
			)
			if !dup {
				updatedBoarding = append(
					updatedBoarding, newIntent,
				)
			}
		}

		// Deduplicate forfeit requests by VTXO outpoint. A VTXO
		// can only be forfeited once per round.
		updatedForfeits := slices.Clone(s.Forfeits)
		for _, newForfeit := range evt.Forfeits {
			dup := slices.ContainsFunc(
				updatedForfeits,
				func(f types.ForfeitRequest) bool {
					if f.VTXOOutpoint == nil ||
						newForfeit.VTXOOutpoint == nil {

						return false
					}

					return *f.VTXOOutpoint ==
						*newForfeit.VTXOOutpoint
				},
			)
			if !dup {
				updatedForfeits = append(
					updatedForfeits, newForfeit,
				)
			}
		}

		updatedVTXOs := slices.Clone(s.VTXOs)
		updatedVTXOs = append(updatedVTXOs, evt.VTXOs...)

		updatedLeaves := slices.Clone(s.Leaves)
		updatedLeaves = append(updatedLeaves, evt.Leaves...)

		return &ClientStateTransition{
			NextState: &PendingRoundAssembly{
				Boarding: updatedBoarding,
				VTXOs:    updatedVTXOs,
				Forfeits: updatedForfeits,
				Leaves:   updatedLeaves,
			},
		}, nil

	// It's time to register our confirmed boarding UTXOs for the next
	// round. We'll send a message to the server using our outbox, then
	// transition to the next phase.
	case *RegistrationRequested:
		env.Log.InfoS(ctx, "Registration requested, preparing to join round",
			slog.Int("boarding_intent_count", len(s.Boarding)),
			slog.Int("vtxo_intent_count", len(s.VTXOs)))

		// Calculate total input amount from all boarding intents.
		var totalInput btcutil.Amount
		for _, boarding := range s.Boarding {
			totalInput += boarding.ChainInfo.Amount
		}

		// Calculate total output amount from all VTXO requests.
		var totalOutput btcutil.Amount
		for _, vtxo := range s.VTXOs {
			totalOutput += vtxo.Amount
		}

		// Include all forfeited VTXO amounts as inputs.
		forfeitAmt, err := computeTotalForfeitAmount(
			ctx, env.VTXOStore, s.Forfeits,
		)
		if err != nil {
			return failWithNotification(
				"failed to compute forfeit amount",
				err, true, fn.None[RoundID](),
			), nil
		}
		totalInput += forfeitAmt

		// Include leave amounts as requested on-chain outputs.
		for i, req := range s.Leaves {
			if req.Output == nil {
				return failWithNotification(
					"leave request has nil output",
					fmt.Errorf("leave request %d "+
						"has nil output", i),
					true, fn.None[RoundID](),
				), nil
			}

			totalOutput += btcutil.Amount(req.Output.Value)
		}

		// Validate that we have outputs to create.
		if totalOutput == 0 {
			return failWithNotification(
				"no VTXO output amount",
				fmt.Errorf("total VTXO output is zero"),
				true, fn.None[RoundID](),
			), nil
		}

		// Validate that outputs don't exceed inputs.
		if totalOutput > totalInput {
			return failWithNotification(
				"outputs exceed inputs",
				fmt.Errorf(
					"total output (%d) exceeds total "+
						"input (%d)",
					totalOutput, totalInput,
				),
				true, fn.None[RoundID](),
			), nil
		}

		// Calculate the implicit operator fee (inputs - outputs).
		operatorFee := totalInput - totalOutput

		// Validate that the operator fee is within acceptable limits.
		if operatorFee > env.MaxOperatorFee {
			return failWithNotification(
				"operator fee exceeds limit",
				fmt.Errorf(
					"operator fee (%d) exceeds max "+
						"allowed (%d)",
					operatorFee, env.MaxOperatorFee,
				),
				true, fn.None[RoundID](),
			), nil
		}

		env.Log.InfoS(ctx, "Amount validation passed",
			btclog.Fmt("total_input", "%v", totalInput),
			btclog.Fmt("total_output", "%v", totalOutput),
			btclog.Fmt("operator_fee", "%v", operatorFee))

		// Extract the set of values from the intent map, as we don't
		// need to track them by outpoint any longer.
		boardingReqs := fn.Map(s.Boarding, buildBoardingRequest)
		vtxoReqs := slices.Clone(s.VTXOs)

		// Build forfeit requests from the decoupled forfeit pool.
		forfeitReqs, err := sortedForfeitRequests(s.Forfeits)
		if err != nil {
			return failWithNotification(
				"invalid forfeit requests",
				err, true, fn.None[RoundID](),
			), nil
		}

		// Leave requests are already in append order.
		leaveReqs := slices.Clone(s.Leaves)

		env.Log.InfoS(ctx, "Sending JoinRoundRequest to server",
			slog.Int("boarding_requests", len(boardingReqs)),
			slog.Int("vtxo_requests", len(vtxoReqs)),
			slog.Int("forfeit_requests", len(forfeitReqs)),
			slog.Int("leave_requests", len(leaveReqs)))

		// Build Intents with all pools for downstream validation.
		intent := Intents{
			Boarding: slices.Clone(s.Boarding),
			VTXOs:    vtxoReqs,
			Leaves:   leaveReqs,
			Forfeits: slices.Clone(s.Forfeits),
		}

		// Derive a fresh identifier key for the join-request
		// authorization challenge.
		identifierKeyDesc, err := deriveJoinAuthIdentifierKey(
			ctx, env.Wallet,
		)
		if err != nil {
			return failWithNotification(
				"failed to derive join auth identifier",
				err, true, fn.None[RoundID](),
			), nil
		}

		idPub := identifierKeyDesc.PubKey

		// When auth is enabled, produce a BIP-322 proof that
		// binds the request contents to the identifier key.
		var joinAuth *types.JoinRoundAuth
		if !env.DisableJoinRequestAuth {
			auth, err := buildJoinRoundAuth(
				ctx, env, identifierKeyDesc, intent, vtxoReqs,
				forfeitReqs, leaveReqs,
			)
			if err != nil {
				return failWithNotification(
					"failed to build round auth",
					fmt.Errorf(
						"join auth: %w", err,
					),
					true, fn.None[RoundID](),
				), nil
			}

			joinAuth = auth
		}

		// With all this extracted, we'll now send the
		// JoinRoundRequest to kick off the signing process.
		return &ClientStateTransition{
			NextState: &RegistrationSentState{
				Intents: intent,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: []ClientOutMsg{
					&JoinRoundRequest{
						BoardingRequests: boardingReqs,
						VTXORequests:     vtxoReqs,
						ForfeitRequests:  forfeitReqs,
						LeaveRequests:    leaveReqs,
						Identifier:       idPub,
						Auth:             joinAuth,
					},
				},
			}),
		}, nil

	case *BoardingFailed:
		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
			},
		}, nil

	default:
		// Self-loop on unknown events - do not halt the FSM.
		return selfLoop(s), nil
	}
}

// ProcessEvent for RegistrationSentState.
func (s *RegistrationSentState) ProcessEvent(
	ctx context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *RoundJoined:
		env.Log.InfoS(ctx, "Successfully joined round",
			slog.String("round_id", evt.RoundID.String()),
			slog.Int("boarding_intent_count", len(s.Intents.Boarding)),
			slog.Int("vtxo_intent_count", len(s.Intents.VTXOs)))

		return &ClientStateTransition{
			NextState: &RoundJoinedState{
				RoundID: evt.RoundID,
				Intents: s.Intents.Clone(),
			},
		}, nil

	case *BoardingFailed:
		// Server rejected the registration or the request timed out.
		// Transition to failure state.
		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
			},
		}, nil

	default:
		// Self-loop on unknown events - do not halt the FSM.
		return selfLoop(s), nil
	}
}

// ProcessEvent for RoundJoinedState.
func (s *RoundJoinedState) ProcessEvent(
	ctx context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *CommitmentTxBuilt:
		txid := evt.Tx.UnsignedTx.TxHash()
		env.Log.InfoS(ctx, "Received commitment transaction from server",
			slog.String("round_id", evt.RoundID.String()),
			slog.String("commitment_txid", txid.String()),
			slog.Int("vtxo_tree_count", len(evt.VTXOTreePaths)))

		return &ClientStateTransition{
			NextState: &CommitmentTxReceivedState{
				RoundID:       evt.RoundID,
				CommitmentTx:  evt.Tx,
				TxID:          txid,
				VTXOTreePaths: evt.VTXOTreePaths,
				Intents:       s.Intents.Clone(),
				ClientTrees:   make(map[SignerKey]*tree.Tree),
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				InternalEvent: []ClientEvent{evt},
			}),
		}, nil

	case *BoardingFailed:
		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
			},
		}, nil

	default:
		// Self-loop on unknown events - do not halt the FSM.
		return selfLoop(s), nil
	}
}

// validateBoardingInputs checks that all boarding UTXOs are present in the
// commitment transaction and returns a map of outpoint to input index.
func validateBoardingInputs(commitmentTx *wire.MsgTx,
	intents []BoardingIntent) (map[wire.OutPoint]int, error) {

	if commitmentTx == nil {
		return nil, fmt.Errorf("commitment tx is nil")
	}
	if len(intents) == 0 {
		return nil, fmt.Errorf("no boarding intents to validate")
	}

	// Build map of outpoint to input index.
	outpointToIdx := make(map[wire.OutPoint]int)
	for i, txIn := range commitmentTx.TxIn {
		outpointToIdx[txIn.PreviousOutPoint] = i
	}

	// Validate all intent outpoints are present in the commitment tx.
	for _, intent := range intents {
		outpoint := intent.Request.Outpoint
		if _, found := outpointToIdx[*outpoint]; !found {
			return nil, fmt.Errorf("boarding UTXO %s not found "+
				"in commitment tx", outpoint)
		}
	}

	return outpointToIdx, nil
}

// validateLeaveOutputs verifies that all leave outputs are present in the
// commitment transaction with matching values and scripts. This ensures the
// server has properly included the requested on-chain exit outputs.
func validateLeaveOutputs(
	commitmentTx *wire.MsgTx, leaves []*types.LeaveRequest,
) error {

	if commitmentTx == nil {
		return fmt.Errorf("commitment tx is nil")
	}

	// If there are no leave requests, nothing to validate.
	if len(leaves) == 0 {
		return nil
	}

	// Build a set of expected leave outputs for matching. We use string for
	// pkScript since slices cannot be map keys.
	type leaveOutput struct {
		value    int64
		pkScript string
	}
	expectedOutputs := make(map[leaveOutput]int)
	for _, leave := range leaves {
		key := leaveOutput{
			value:    leave.Output.Value,
			pkScript: string(leave.Output.PkScript),
		}
		expectedOutputs[key]++
	}

	// Search through commitment tx outputs for matching leave outputs.
	for _, txOut := range commitmentTx.TxOut {
		key := leaveOutput{
			value:    txOut.Value,
			pkScript: string(txOut.PkScript),
		}
		if count, found := expectedOutputs[key]; found && count > 0 {
			expectedOutputs[key]--
		}
	}

	// Check if all expected outputs were found.
	for key, count := range expectedOutputs {
		if count > 0 {
			return fmt.Errorf(
				"leave output not found in commitment tx: "+
					"value=%d, remaining=%d",
				key.value, count,
			)
		}
	}

	return nil
}

// ProcessEvent for CommitmentTxReceivedState.
//
//nolint:ll
func (s *CommitmentTxReceivedState) ProcessEvent(
	ctx context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *CommitmentTxBuilt:
		env.Log.InfoS(ctx, "Validating commitment transaction",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("boarding_intent_count", len(s.Intents.Boarding)),
			slog.Int("vtxo_intent_count", len(s.Intents.VTXOs)),
			slog.Int("leave_intent_count", len(s.Intents.Leaves)))

		// Validate boarding inputs if we have any boarding intents.
		// Refresh-only rounds have no boarding inputs to validate.
		var boardingInputIndices map[wire.OutPoint]int
		if len(s.Intents.Boarding) > 0 {
			var err error
			boardingInputIndices, err = validateBoardingInputs(
				s.CommitmentTx.UnsignedTx, s.Intents.Boarding,
			)
			if err != nil {
				env.Log.WarnS(ctx, "Commitment tx validation failed", err,
					slog.String("round_id", s.RoundID.String()))

				return &ClientStateTransition{
					NextState: &ClientFailedState{
						Reason: "commitment tx " +
							"validation failed",
						Error:       err,
						Recoverable: true,
					},
				}, nil
			}
		} else {
			boardingInputIndices = make(map[wire.OutPoint]int)
		}

		env.Log.DebugS(ctx, "Validated boarding inputs in commitment tx",
			slog.Int("boarding_input_count", len(boardingInputIndices)))

		// Validate leave outputs if we have any leave requests. Each
		// leave output must be present in the commitment tx with the
		// correct value and script.
		if len(s.Intents.Leaves) > 0 {
			if err := validateLeaveOutputs(
				s.CommitmentTx.UnsignedTx, s.Intents.Leaves,
			); err != nil {
				env.Log.WarnS(
					ctx, "Leave output validation failed",
					err,
					slog.String("round_id", s.RoundID.String()),
				)

				return &ClientStateTransition{
					NextState: &ClientFailedState{
						Reason: "leave output " +
							"validation failed",
						Error:       err,
						Recoverable: true,
					},
				}, nil
			}

			env.Log.DebugS(
				ctx, "Validated leave outputs in commitment tx",
				slog.Int("leave_output_count", len(s.Intents.Leaves)),
			)
		}

		clientTrees := make(map[SignerKey]*tree.Tree)

		// Next, we'll make sure that each of the VTXO requests that we
		// originally requested are actually present in the VTXT trees
		// that the server sent us.
		for i, vtxoReq := range s.Intents.VTXOs {
			// Convert VTXORequest to LeafDescriptor for validation.
			expectedLeaf := tree.LeafDescriptor{
				Amount:      vtxoReq.Amount,
				PkScript:    vtxoReq.PkScript,
				CoSignerKey: vtxoReq.SigningKey.PubKey,
			}

			// Search through all VTXO trees to find the one
			// containing this VTXO request.
			var clientTree *tree.Tree
			var validateErr error
			for _, vtxoTree := range s.VTXOTreePaths {
				clientTree, validateErr = vtxoTree.ValidatePath(
					vtxoReq.SigningKey.PubKey, expectedLeaf,
					env.OperatorTerms.PubKey,
				)
				if validateErr == nil {
					// Found the VTXO in this tree.
					break
				}
			}
			if validateErr != nil {
				return &ClientStateTransition{
					NextState: &ClientFailedState{
						Reason: fmt.Sprintf(
							"VTXT validation "+
								"failed for VTXO "+
								"request %d", i,
						),
						Error:       validateErr,
						Recoverable: false,
					},
				}, nil
			}

			// Ensure we actually found a client tree. This handles the
			// edge case where VTXOTreePaths is empty.
			if clientTree == nil {
				return &ClientStateTransition{
					NextState: &ClientFailedState{
						Reason: fmt.Sprintf(
							"no client tree found "+
								"for VTXO request %d", i,
						),
						Error: fmt.Errorf(
							"VTXO tree not found",
						),
						Recoverable: false,
					},
				}, nil
			}

			// Now that we know this VTXO request was properly
			// included in the tree, we'll store the client-tree
			// (traversal path from the root to this vtxo leaf).
			signerKey := NewSignerKey(vtxoReq.SigningKey.PubKey)
			clientTrees[signerKey] = clientTree
		}

		// Make sure all anchor outputs are valid in each tree. If they
		// aren't we may not be able to go on chain.
		for outputIdx, vtxoTree := range s.VTXOTreePaths {
			if err := vtxoTree.ValidateAnchors(); err != nil {
				return &ClientStateTransition{
					NextState: &ClientFailedState{
						Reason: fmt.Sprintf(
							"anchor output validation "+
								"failed for output %d",
							outputIdx,
						),
						Error:       err,
						Recoverable: false,
					},
				}, nil
			}
		}

		env.Log.InfoS(ctx, "Commitment transaction validated successfully",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("client_trees", len(clientTrees)),
			slog.Int("vtxo_tree_count", len(s.VTXOTreePaths)))

		// Proceed to nonce generation. Forfeit mappings (if any) are
		// carried forward through the MuSig2 signing states. Forfeit
		// signatures are collected AFTER VTXO tree signing is complete,
		// ensuring clients only forfeit old VTXOs after verifying new
		// VTXOs are properly signed.
		return &ClientStateTransition{
			NextState: &CommitmentTxValidatedState{
				RoundID:              s.RoundID,
				CommitmentTx:         s.CommitmentTx,
				VTXOTreePaths:        s.VTXOTreePaths,
				Intents:              s.Intents.Clone(),
				ClientTrees:          clientTrees,
				BoardingInputIndices: boardingInputIndices,
				ForfeitMappings:      evt.ForfeitMappings,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				InternalEvent: []ClientEvent{&GenerateNonces{}},
			}),
		}, nil

	case *BoardingFailed:
		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
			},
		}, nil

	default:
		// Self-loop on unknown events - do not halt the FSM.
		return selfLoop(s), nil
	}
}

// ProcessEvent for CommitmentTxValidatedState.
func (s *CommitmentTxValidatedState) ProcessEvent(
	ctx context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch event.(type) {
	case *GenerateNonces:
		env.Log.InfoS(ctx, "Generating MuSig2 nonces for VTXO tree signing",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("boarding_intent_count", len(s.Intents.Boarding)),
			slog.Int("vtxo_intent_count", len(s.Intents.VTXOs)))

		// For leave-only rounds (no new VTXOs), skip nonce generation
		// and go directly to forfeit collection. The server doesn't
		// need nonces when there are no VTXOs to sign. We check
		// Intents.Leaves explicitly rather than ForfeitMappings,
		// since a batch tx could have boarding inputs and leave
		// outputs without any refresh forfeits.
		if len(s.Intents.VTXOs) == 0 && len(s.Intents.Leaves) > 0 {
			env.Log.InfoS(ctx, "Leave-only round, skipping "+
				"to forfeit collection",
				slog.String("round_id", s.RoundID.String()),
				slog.Int("leave_count", len(s.Intents.Leaves)))

			// Build forfeit request messages for each VTXO being
			// forfeited.
			var outbox []ClientOutMsg
			for vtxoOutpoint, info := range s.ForfeitMappings {
				connOut := info.ConnectorOutpoint
				connScript := info.ConnectorPkScript
				connAmt := info.ConnectorAmount
				forfeitScript := env.OperatorTerms.ForfeitScript
				roundIDStr := s.RoundID.String()

				msg := &ForfeitRequestToVTXO{
					VTXOOutpoint:          vtxoOutpoint,
					RoundID:               roundIDStr,
					ConnectorOutpoint:     connOut,
					ConnectorPkScript:     connScript,
					ConnectorAmount:       connAmt,
					ServerForfeitPkScript: forfeitScript,
				}
				outbox = append(outbox, msg)
			}

			// Transition directly to forfeit collection.
			collectedForfeits := make(
				map[wire.OutPoint]*ForfeitSignatureResponse,
			)

			return &ClientStateTransition{
				NextState: &ForfeitSignaturesCollectingState{
					RoundID:           s.RoundID,
					CommitmentTx:      s.CommitmentTx,
					VTXOTreePaths:     s.VTXOTreePaths,
					Intents:           s.Intents.Clone(),
					ClientTrees:       s.ClientTrees,
					ExpectedForfeits:  s.ForfeitMappings,
					CollectedForfeits: collectedForfeits,
					BoardingInputIndices: s.
						BoardingInputIndices,
				},
				NewEvents: fn.Some(ClientEmittedEvent{
					Outbox: outbox,
				}),
			}, nil
		}

		// At this point, all the basic validation checks have passed.
		// So now we'll generate a musig2 session to create nonces to
		// sign the VTXO tree. Each VTXO that we created will
		// effectively be a new musig session.
		musig2Sessions := make(map[SignerKey]*tree.SignerSession)
		for _, vtxoReq := range s.Intents.VTXOs {
			signerKey := NewSignerKey(
				vtxoReq.SigningKey.PubKey,
			)

			// Get the client tree for this signer key.
			// The sweep tweak and batch output are
			// properties of the tree that were set when
			// the operator built it.
			clientTree := s.ClientTrees[signerKey]
			if clientTree == nil {
				return nil, fmt.Errorf(
					"no client tree for signer "+
						"key %x", signerKey[:],
				)
			}

			sweepTweak := clientTree.SweepTapscriptRoot
			batchOut := clientTree.BatchOutput
			root := clientTree.Root
			prevOutFetcher, err := root.PrevOutputFetcher(batchOut)
			if err != nil {
				return nil, fmt.Errorf("failed to "+
					"create prev output fetcher "+
					"for signer %x: %w",
					signerKey[:], err)
			}

			// TODO(roasbeef): actually use the interface
			// in front of this?
			session, err := tree.NewSignerSession(
				env.Wallet, &vtxoReq.SigningKey,
				sweepTweak, prevOutFetcher,
				clientTree.Root,
			)
			if err != nil {
				return nil, fmt.Errorf("failed to "+
					"create signing session for "+
					"client %x: %w",
					signerKey[:], err)
			}

			musig2Sessions[signerKey] = session
		}

		// Now that we have all our sessions created, we'll have each
		// of them generate nonces to use in tree signing. The server
		// expects nonces grouped by signer key first, then by txid.
		allNonces := make(
			map[SignerKey]map[tree.TxID]tree.Musig2PubNonce,
		)
		for signerKey, session := range musig2Sessions {
			nonces := session.GetNonces()
			allNonces[signerKey] = nonces
		}

		env.Log.InfoS(ctx, "Generated MuSig2 nonces, sending to server",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("session_count", len(musig2Sessions)),
			slog.Int("signer_key_count", len(allNonces)))

		// MuSig2 nonces have been generated locally. Send them to the
		// server to participate in the aggregated nonce computation.
		nonceMsg := &SubmitNoncesRequest{
			RoundID: s.RoundID,
			Nonces:  allNonces,
		}

		return &ClientStateTransition{
			NextState: &NoncesSentState{
				RoundID:              s.RoundID,
				CommitmentTx:         s.CommitmentTx,
				VTXOTreePaths:        s.VTXOTreePaths,
				Intents:              s.Intents.Clone(),
				ClientTrees:          s.ClientTrees,
				Musig2Sessions:       musig2Sessions,
				BoardingInputIndices: s.BoardingInputIndices,
				ForfeitMappings:      s.ForfeitMappings,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: []ClientOutMsg{nonceMsg},
			}),
		}, nil

	default:
		// Self-loop on unknown events - do not halt the FSM.
		return selfLoop(s), nil
	}
}

// ProcessEvent for ForfeitSignaturesCollectingState. This state handles the
// collection of forfeit signatures from VTXO actors after VTXO tree signing
// is complete. Each VTXO actor signs its forfeit transaction and sends a
// ForfeitSignatureResponse. Once all expected signatures are collected, we
// sign boarding inputs, submit all signatures to the server, and transition
// to InputSigSentState.
func (s *ForfeitSignaturesCollectingState) ProcessEvent(
	ctx context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *ForfeitSignatureResponse:
		// Validate this is a response we're expecting.
		connectorInfo, expected := s.ExpectedForfeits[evt.VTXOOutpoint]
		if !expected {
			return nil, fmt.Errorf("unexpected forfeit signature "+
				"for VTXO %s", evt.VTXOOutpoint)
		}

		// Validate the forfeit transaction structure using lib/tx. The
		// VTXOAmount check ensures the penalty output equals the
		// forfeited VTXO value, preventing value theft.
		params := tx.ForfeitTxParams{
			VTXOOutpoint:        evt.VTXOOutpoint,
			ConnectorOutpoint:   connectorInfo.ConnectorOutpoint,
			ServerForfeitScript: env.OperatorTerms.ForfeitScript,
			ExpectedAmount:      connectorInfo.VTXOAmount,
		}
		err := tx.ValidateForfeitTx(evt.ForfeitTx, params)
		if err != nil {
			return nil, fmt.Errorf("invalid forfeit tx for VTXO "+
				"%s: %w", evt.VTXOOutpoint, err)
		}

		// Check for duplicate response.
		_, already := s.CollectedForfeits[evt.VTXOOutpoint]
		if already {
			// Already have this signature, ignore duplicate.
			return &ClientStateTransition{NextState: s}, nil
		}

		// Add to collected signatures in an immutable way. FSM states
		// should be treated as immutable to prevent side effects.
		updatedForfeits := maps.Clone(s.CollectedForfeits)
		updatedForfeits[evt.VTXOOutpoint] = evt

		// Check if all forfeit signatures have been collected.
		if len(updatedForfeits) < len(s.ExpectedForfeits) {
			// Still waiting for more signatures - return new state
			// with explicit struct to ensure immutability.
			return &ClientStateTransition{
				//nolint:ll
				NextState: &ForfeitSignaturesCollectingState{
					RoundID:              s.RoundID,
					CommitmentTx:         s.CommitmentTx,
					VTXOTreePaths:        s.VTXOTreePaths,
					Intents:              s.Intents.Clone(),
					ClientTrees:          s.ClientTrees,
					BoardingInputIndices: s.BoardingInputIndices,
					ExpectedForfeits:     s.ExpectedForfeits,
					CollectedForfeits:    updatedForfeits,
				},
			}, nil
		}

		// All forfeit signatures collected! Build the submission.
		forfeitSigs := make(map[wire.OutPoint]*schnorr.Signature)
		forfeitTxs := make(map[wire.OutPoint]*wire.MsgTx)
		forfeitedVTXOs := make([]wire.OutPoint, 0, len(updatedForfeits))
		for outpoint, resp := range updatedForfeits {
			forfeitSigs[outpoint] = resp.Signature
			forfeitTxs[outpoint] = resp.ForfeitTx
			forfeitedVTXOs = append(forfeitedVTXOs, outpoint)
		}

		env.Log.InfoS(ctx, "All forfeit signatures collected, signing boarding inputs",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("forfeit_count", len(forfeitedVTXOs)),
			slog.Int("boarding_intent_count", len(s.Intents.Boarding)))

		// Sign all boarding inputs using the shared helper.
		boardingInputSigs, err := signBoardingInputs(
			env.Wallet, s.CommitmentTx, s.Intents,
			s.BoardingInputIndices,
		)
		if err != nil {
			return nil, fmt.Errorf("sign boarding inputs: %w", err)
		}

		// Build outbox messages.
		txid := s.CommitmentTx.UnsignedTx.TxHash()
		callerID := fmt.Sprintf("commitment-%s", txid.String())

		var pkScript []byte
		if len(s.CommitmentTx.UnsignedTx.TxOut) > 0 {
			pkScript = s.CommitmentTx.UnsignedTx.TxOut[0].PkScript
		}

		outboxMsgs := []ClientOutMsg{
			&SubmitVTXOForfeitSigsToServer{
				RoundID:     s.RoundID,
				ForfeitSigs: forfeitSigs,
				ForfeitTxs:  forfeitTxs,
			},
			&SubmitForfeitSigRequest{
				RoundID:    s.RoundID,
				Signatures: boardingInputSigs,
			},
			&RegisterConfirmationRequest{
				CallerID:    callerID,
				Txid:        &txid,
				PkScript:    pkScript,
				TargetConfs: env.OperatorTerms.MinConfirmations,
				HeightHint:  env.StartHeight,
			},
		}

		// Checkpoint round state.
		intents := s.Intents.Clone()
		for i := range intents.Boarding {
			intents.Boarding[i].Status = BoardingStatusAdopted
		}
		round := &Round{
			RoundID:       s.RoundID,
			StartHeight:   env.StartHeight,
			CommitmentTx:  fn.Some(s.CommitmentTx),
			VTXOTreePaths: fn.Some(s.VTXOTreePaths),
			Intents:       intents,
		}

		nextState := &InputSigSentState{
			RoundID:        s.RoundID,
			CommitmentTx:   s.CommitmentTx,
			VTXOTreePaths:  s.VTXOTreePaths,
			Intents:        s.Intents.Clone(),
			ClientTrees:    s.ClientTrees,
			InputSigs:      boardingInputSigs,
			ForfeitedVTXOs: forfeitedVTXOs,
		}

		err = env.RoundStore.CommitState(ctx, round, nextState)
		if err != nil {
			return nil, fmt.Errorf("failed to commit round "+
				"state: %w", err)
		}

		env.Log.InfoS(ctx, "Round state checkpointed with forfeit signatures",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("boarding_sig_count", len(boardingInputSigs)),
			slog.Int("forfeit_sig_count", len(forfeitSigs)))

		checkpointNotify := &RoundCheckpointedNotification{
			RoundID: s.RoundID,
		}

		return &ClientStateTransition{
			NextState: nextState,
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: append(outboxMsgs, checkpointNotify),
			}),
		}, nil

	case *BoardingFailed:
		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
			},
		}, nil

	default:
		// Self-loop on unknown events - do not halt the FSM.
		return selfLoop(s), nil
	}
}

// ProcessEvent for NoncesSentState.
func (s *NoncesSentState) ProcessEvent(
	ctx context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *NoncesAggregated:
		env.Log.InfoS(ctx, "Received aggregated nonces from server",
			slog.String("round_id", evt.RoundID.String()),
			slog.Int("agg_nonce_count", len(evt.AggNonces)))

		// Received aggregated nonces from the server. Now register
		// them with our signing session and generate partial
		// signatures.
		//
		// The server sends ONE combined/aggregated nonce per
		// transaction (not individual nonces from each participant).
		// The server has already aggregated all participants' nonces
		// using MuSig2 nonce aggregation.
		//
		// The event now contains properly typed nonces directly.
		aggNoncesMap := evt.AggNonces

		// With the nonces grouped, we need to register the nonces with
		// each client session.
		for _, musig2Session := range s.Musig2Sessions {
			// Register the combined nonces with our signing
			// session.
			err := musig2Session.RegisterAggNonces(aggNoncesMap)
			if err != nil {
				return nil, fmt.Errorf("failed to register "+
					"combined nonces: %w", err)
			}
		}

		env.Log.DebugS(ctx, "Registered aggregated nonces with signing sessions",
			slog.Int("session_count", len(s.Musig2Sessions)))

		return &ClientStateTransition{
			NextState: &NoncesAggregatedState{
				RoundID:              s.RoundID,
				CommitmentTx:         s.CommitmentTx,
				VTXOTreePaths:        s.VTXOTreePaths,
				Intents:              s.Intents.Clone(),
				ClientTrees:          s.ClientTrees,
				Musig2Sessions:       s.Musig2Sessions,
				AggNonces:            evt.AggNonces,
				BoardingInputIndices: s.BoardingInputIndices,
				ForfeitMappings:      s.ForfeitMappings,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				InternalEvent: []ClientEvent{
					&GeneratePartialSigs{},
				},
			}),
		}, nil

	case *BoardingFailed:
		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
			},
		}, nil

	default:
		// Self-loop on unknown events - do not halt the FSM.
		return selfLoop(s), nil
	}
}

// ProcessEvent for NoncesAggregatedState.
func (s *NoncesAggregatedState) ProcessEvent(
	ctx context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch event.(type) {
	case *GeneratePartialSigs:
		env.Log.InfoS(ctx, "Generating partial signatures for VTXO tree",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("session_count", len(s.Musig2Sessions)))

		// At this stage, the nonces have been aggregated for each
		// client, so now we'll generate and send our partial
		// signatures. The server expects signatures grouped by signer
		// key first, then by transaction ID.
		allSignatures := make(
			map[SignerKey]map[tree.TxID]*musig2.PartialSignature,
		)

		for signerKey, musig2Session := range s.Musig2Sessions {
			// Generate partial signatures for all transactions in
			// our path. The map is keyed by transaction ID.
			partialSigs, err := musig2Session.Signatures(true)
			if err != nil {
				return nil, fmt.Errorf("failed to generate "+
					"partial signatures: %w", err)
			}

			allSignatures[signerKey] = partialSigs
		}

		// Create a single message with all signatures grouped by signer
		// key.
		submitPartialSigsMsg := &SubmitPartialSigRequest{
			RoundID:    s.RoundID,
			Signatures: allSignatures,
		}

		env.Log.InfoS(ctx, "Sending partial signatures to server",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("signer_key_count", len(allSignatures)))

		// Partial MuSig2 signatures have been generated using the
		// aggregated nonces. Send them to the server for signature
		// aggregation.
		return &ClientStateTransition{
			NextState: &PartialSigsSentState{
				RoundID:              s.RoundID,
				CommitmentTx:         s.CommitmentTx,
				VTXOTreePaths:        s.VTXOTreePaths,
				Intents:              s.Intents.Clone(),
				ClientTrees:          s.ClientTrees,
				Musig2Sessions:       s.Musig2Sessions,
				BoardingInputIndices: s.BoardingInputIndices,
				ForfeitMappings:      s.ForfeitMappings,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: []ClientOutMsg{submitPartialSigsMsg},
			}),
		}, nil

	default:
		// Self-loop on unknown events - do not halt the FSM.
		return selfLoop(s), nil
	}
}

// ProcessEvent for PartialSigsSentState.
func (s *PartialSigsSentState) ProcessEvent(
	ctx context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *OperatorSigned:
		env.Log.InfoS(ctx, "Received aggregated signatures from operator",
			slog.String("round_id", evt.RoundID.String()),
			slog.Int("agg_sig_count", len(evt.AggSigs)))

		// At this point, Received complete VTXT signatures from the
		// server after the operator aggregated all partial signatures.
		//
		// Now, we'll validate that the aggregated signatures are valid
		// for each VTXT before proceeding. This prevents the operator
		// from providing invalid signatures that would make our VTXOs
		// unspendable.
		//
		// Convert the typed signatures to raw bytes for validation.
		sigBytes := make(map[tree.TxID][]byte, len(evt.AggSigs))
		for txid, sig := range evt.AggSigs {
			sigBytes[txid] = sig.Serialize()
		}
		for outputIdx, vtxoTree := range s.VTXOTreePaths {
			err := vtxoTree.ValidateAndSubmitSignatures(sigBytes)
			if err != nil {
				return &ClientStateTransition{
					NextState: &ClientFailedState{
						Reason: fmt.Sprintf(
							"VTXT signature "+
								"validation "+
								"failed: %d",
							outputIdx,
						),
						Error:       err,
						Recoverable: false,
					},
				}, nil
			}
		}

		env.Log.InfoS(ctx, "Validated aggregated signatures",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("forfeit_mapping_count", len(s.ForfeitMappings)))

		// VTXO tree signatures validated. Now check if this round
		// includes refresh requests. If so, we need to collect forfeit
		// signatures from VTXO actors before signing boarding inputs.
		// This ensures clients only forfeit old VTXOs after verifying
		// their new VTXOs are properly signed.
		if len(s.ForfeitMappings) > 0 {
			return s.transitionToForfeitCollection(ctx, env)
		}

		// No refresh requests - proceed to sign boarding inputs.
		env.Log.InfoS(ctx, "Signing boarding inputs",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("boarding_intent_count", len(s.Intents.Boarding)))

		// Sign all boarding inputs using the shared helper.
		boardingInputSigs, err := signBoardingInputs(
			env.Wallet, s.CommitmentTx, s.Intents,
			s.BoardingInputIndices,
		)
		if err != nil {
			return nil, fmt.Errorf("sign boarding inputs: %w", err)
		}

		// Create a single forfeit signature request with all
		// signatures.
		forfeitSigReq := &SubmitForfeitSigRequest{
			RoundID:    s.RoundID,
			Signatures: boardingInputSigs,
		}

		txid := s.CommitmentTx.UnsignedTx.TxHash()
		callerID := fmt.Sprintf("commitment-%s", txid.String())

		// Get pkScript from the first output for LND confirmation
		// tracking.
		commitTx := s.CommitmentTx.UnsignedTx
		var pkScript []byte
		if len(commitTx.TxOut) > 0 {
			pkScript = commitTx.TxOut[0].PkScript
		}

		env.Log.InfoS(ctx, "Building RegisterConfirmationRequest",
			slog.String("round_id", s.RoundID.String()),
			slog.String("txid", txid.String()),
			slog.Int("num_outputs", len(commitTx.TxOut)),
			slog.Int("pkscript_len", len(pkScript)),
			slog.Int("target_confs", int(env.OperatorTerms.MinConfirmations)))

		outboxMsgs := []ClientOutMsg{
			forfeitSigReq,
			&RegisterConfirmationRequest{
				CallerID:    callerID,
				Txid:        &txid,
				PkScript:    pkScript,
				TargetConfs: env.OperatorTerms.MinConfirmations,
				HeightHint:  env.StartHeight,
			},
		}

		// Checkpoint the round state at the "point of no return".
		// After sending boarding input signatures, the server may
		// broadcast the commitment transaction. We must persist all
		// round data to enable recovery if the client restarts.
		//
		// Mark all intents as Adopted (frozen in this round) and then
		// save them alongside the round.
		intents := s.Intents.Clone()
		for i := range intents.Boarding {
			intents.Boarding[i].Status = BoardingStatusAdopted
		}
		round := &Round{
			RoundID:       s.RoundID,
			StartHeight:   env.StartHeight,
			CommitmentTx:  fn.Some(s.CommitmentTx),
			VTXOTreePaths: fn.Some(s.VTXOTreePaths),
			Intents:       intents,
		}

		env.Log.InfoS(ctx, "Signed boarding inputs, checkpointing round state",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("boarding_sig_count", len(boardingInputSigs)))

		// Checkpoint round data + FSM state atomically at the "point
		// of no return". The next state is persisted so restart can
		// recover to InputSigSentState. For boarding-only rounds,
		// ForfeitedVTXOs is nil.
		nextState := &InputSigSentState{
			RoundID:       s.RoundID,
			CommitmentTx:  s.CommitmentTx,
			VTXOTreePaths: s.VTXOTreePaths,
			Intents:       s.Intents.Clone(),
			ClientTrees:   s.ClientTrees,
			InputSigs:     boardingInputSigs,
		}
		err = env.RoundStore.CommitState(ctx, round, nextState)
		if err != nil {
			return nil, fmt.Errorf("failed to commit round "+
				"state: %w", err)
		}

		env.Log.InfoS(ctx, "Round state checkpointed at point of no return",
			slog.String("round_id", s.RoundID.String()),
			slog.String("commitment_txid", txid.String()))

		checkpointNotify := &RoundCheckpointedNotification{
			RoundID: s.RoundID,
		}

		return &ClientStateTransition{
			NextState: nextState,
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: append(outboxMsgs, checkpointNotify),
			}),
		}, nil

	case *BoardingFailed:
		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
			},
		}, nil
	default:
		// Self-loop on unknown events - do not halt the FSM.
		return selfLoop(s), nil
	}
}

// transitionToForfeitCollection builds the transition to
// ForfeitSignaturesCollectingState for refresh rounds. This helper is extracted
// to keep ProcessEvent under the line length limit.
func (s *PartialSigsSentState) transitionToForfeitCollection(
	ctx context.Context, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	// Build forfeit request messages for each VTXO being refreshed. The
	// forfeit script is a static operator property from OperatorTerms.
	var outbox []ClientOutMsg
	for vtxoOutpoint, info := range s.ForfeitMappings {
		msg := &ForfeitRequestToVTXO{
			VTXOOutpoint:          vtxoOutpoint,
			RoundID:               s.RoundID.String(),
			ConnectorOutpoint:     info.ConnectorOutpoint,
			ConnectorPkScript:     info.ConnectorPkScript,
			ConnectorAmount:       info.ConnectorAmount,
			ServerForfeitPkScript: env.OperatorTerms.ForfeitScript,
		}
		outbox = append(outbox, msg)
	}

	env.Log.InfoS(ctx, "Transitioning to forfeit collection",
		slog.String("round_id", s.RoundID.String()),
		slog.Int("forfeit_count", len(s.ForfeitMappings)))

	// Transition to forfeit collection state. After collecting all forfeit
	// signatures, that state will sign boarding inputs and transition to
	// InputSigSent.
	collectedForfeits := make(map[wire.OutPoint]*ForfeitSignatureResponse)

	return &ClientStateTransition{
		NextState: &ForfeitSignaturesCollectingState{
			RoundID:              s.RoundID,
			CommitmentTx:         s.CommitmentTx,
			VTXOTreePaths:        s.VTXOTreePaths,
			Intents:              s.Intents.Clone(),
			ClientTrees:          s.ClientTrees,
			ExpectedForfeits:     s.ForfeitMappings,
			CollectedForfeits:    collectedForfeits,
			BoardingInputIndices: s.BoardingInputIndices,
		},
		NewEvents: fn.Some(ClientEmittedEvent{
			Outbox: outbox,
		}),
	}, nil
}

// buildClientVTXOs constructs ClientVTXO instances from the intents and client
// trees.
func buildClientVTXOs(intents Intents, trees map[SignerKey]*tree.Tree,
	roundID RoundID) ([]*ClientVTXO, error) {

	vtxos := make([]*ClientVTXO, 0)
	for _, req := range intents.VTXOs {
		signerKey := NewSignerKey(req.SigningKey.PubKey)
		clientTree := trees[signerKey]
		if clientTree == nil {
			return nil, fmt.Errorf("missing client tree " +
				"for signing key")
		}

		leaves := clientTree.Root.GetLeafNodes()

		for _, leaf := range leaves {
			outpoint, err := leaf.GetNonAnchorOutpoint()
			if err != nil {
				return nil, fmt.Errorf("failed to "+
					"derive VTXO outpoint: %w", err)
			}

			vtxos = append(vtxos, &ClientVTXO{
				Outpoint:    *outpoint,
				Amount:      req.Amount,
				PkScript:    req.PkScript,
				Expiry:      req.Expiry,
				ClientKey:   req.SigningKey,
				OperatorKey: req.OperatorKey,
				TreePath:    clientTree,
				RoundID:     fn.Some(roundID),
			})
		}
	}

	return vtxos, nil
}

// ProcessEvent for InputSigSentState.
func (s *InputSigSentState) ProcessEvent(
	ctx context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *BoardingFailed:
		env.Log.WarnS(ctx, "Boarding failed while awaiting confirmation", nil,
			slog.String("round_id", s.RoundID.String()),
			slog.String("reason", evt.Reason))

		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
			},
		}, nil

	case *BoardingConfirmed:
		env.Log.InfoS(ctx, "Commitment transaction confirmed",
			slog.String("round_id", s.RoundID.String()),
			slog.String("txid", evt.TxID.String()),
			slog.Int("block_height", int(evt.BlockHeight)),
			slog.Int("confirmations", int(evt.Confirmations)))

		vtxos, err := buildClientVTXOs(
			s.Intents, s.ClientTrees, s.RoundID,
		)
		if err != nil {
			return &ClientStateTransition{
				NextState: &ClientFailedState{
					Reason: "failed to build client " +
						"VTXOs",
					Error:       err,
					Recoverable: false,
				},
			}, nil
		}

		env.Log.InfoS(ctx, "Built client VTXOs, requesting persistence",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("vtxo_count", len(vtxos)))

		// Emit SaveVTXOsReq for the outbox handler to persist
		// the VTXOs. The FSM transitions to an awaiting state
		// until the handler responds with success or failure.
		return &ClientStateTransition{
			NextState: &AwaitingSaveVTXOsState{
				RoundID:        s.RoundID,
				VTXOs:          vtxos,
				ConfEvent:      evt,
				ForfeitedVTXOs: s.ForfeitedVTXOs,
				Intents:        s.Intents.Clone(),
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: []ClientOutMsg{
					&SaveVTXOsReq{VTXOs: vtxos},
				},
			}),
		}, nil

	default:
		// Self-loop on unknown events - do not halt the FSM.
		return selfLoop(s), nil
	}
}

// ProcessEvent for AwaitingSaveVTXOsState. Handles the outbox handler
// response after VTXO persistence is attempted.
func (s *AwaitingSaveVTXOsState) ProcessEvent(
	ctx context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *SaveVTXOsSucceeded:
		env.Log.InfoS(ctx, "VTXOs saved, round complete",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("vtxo_count", len(s.VTXOs)))

		confEvt := s.ConfEvent
		confInfo := ConfInfo{
			Height:    confEvt.BlockHeight,
			BlockHash: confEvt.BlockHash,
		}

		// Compute batch expiry as absolute block height.
		sweepDelay := int32(env.OperatorTerms.SweepDelay)
		batchExpiry := confEvt.BlockHeight + sweepDelay

		// Build outbox messages starting with standard
		// notifications.
		outbox := []ClientOutMsg{
			&VTXOCreatedNotification{
				VTXOs:          s.VTXOs,
				RoundID:        s.RoundID.String(),
				CommitmentTxID: confEvt.TxID,
				BatchExpiry:    batchExpiry,
				CreatedHeight:  confEvt.BlockHeight,
			},
			&RoundCompletedNotification{
				RoundID:  s.RoundID,
				TxID:     confEvt.TxID,
				ConfInfo: confInfo,
			},
		}

		// If this round included refresh requests, notify old
		// VTXO actors that their forfeit is confirmed.
		for _, vtxoOutpoint := range s.ForfeitedVTXOs {
			outbox = append(outbox,
				&ForfeitConfirmedToVTXO{
					VTXOOutpoint:   vtxoOutpoint,
					CommitmentTxID: confEvt.TxID,
					BlockHeight:    confEvt.BlockHeight,
				})
		}

		return &ClientStateTransition{
			NextState: &ConfirmedState{
				TxID:          confEvt.TxID,
				BlockHeight:   confEvt.BlockHeight,
				BlockHash:     confEvt.BlockHash,
				Confirmations: confEvt.Confirmations,
				VTXOs:         s.VTXOs,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: outbox,
			}),
		}, nil

	case *SaveVTXOsFailed:
		return failWithNotification(
			"failed to save VTXOs", evt.Error,
			false, fn.Some(s.RoundID),
		), nil

	default:
		return selfLoop(s), nil
	}
}

// ProcessEvent for ConfirmedState. After boarding completes successfully, we
// automatically transition back to Idle to allow processing new boarding
// addresses and intents.
func (s *ConfirmedState) ProcessEvent(_ context.Context, event ClientEvent,
	env *ClientEnvironment) (*ClientStateTransition, error) {

	switch event.(type) {
	case *RoundComplete:
		// Boarding is complete for this round. Transition back to Idle
		// to process new confirmations for existing boarding addresses
		// or start new rounds.
		return &ClientStateTransition{
			NextState: &Idle{},
		}, nil

	default:
		// Stay in confirmed state for unexpected events.
		return &ClientStateTransition{
			NextState: s,
		}, nil
	}
}

// ProcessEvent for ClientFailedState. This state is recoverable and
// accepts IntentPackage events to restart the boarding process after a
// failure. Instead of duplicating the Idle logic, we transition to Idle
// and forward the event as an internal event for Idle to process.
func (s *ClientFailedState) ProcessEvent(
	ctx context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *RecoveryInitiated:
		env.Log.InfoS(ctx, "Initiating CSV timeout recovery",
			btclog.Fmt("outpoint", "%v", evt.Outpoint),
			slog.String("sweep_txid", evt.SweepTxID.String()),
			slog.String("reason", evt.Reason))

		// Initiate CSV timeout recovery to sweep the boarding UTXO
		// back to the client's wallet after the relative timelock
		// expires.
		return &ClientStateTransition{
			NextState: &RecoveryInitiatedState{
				Outpoint:  evt.Outpoint,
				SweepTxID: evt.SweepTxID,
				Reason:    evt.Reason,
			},
		}, nil

	case *IntentPackage:
		if evt.isEmpty() {
			return selfLoop(s), nil
		}

		env.Log.InfoS(ctx, "Recovering from failed state "+
			"with intent package",
			evt.logAttributes())

		return &ClientStateTransition{
			NextState: &Idle{},
			NewEvents: fn.Some(ClientEmittedEvent{
				InternalEvent: []ClientEvent{evt},
			}),
		}, nil

	default:
		// Self-loop on unknown events - do not halt the FSM.
		return selfLoop(s), nil
	}
}

// ProcessEvent for RecoveryInitiatedState (semi-terminal state).
func (s *RecoveryInitiatedState) ProcessEvent(
	_ context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	// Semi-terminal state - self-loop on all events since the recovery
	// sweep transaction has been broadcast and we're waiting for
	// confirmation.
	return &ClientStateTransition{
		NextState: s,
	}, nil
}

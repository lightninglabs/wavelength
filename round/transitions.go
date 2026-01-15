package round

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tree"
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

// ProcessEvent handles the events from the Idle state. In this state, we'll
// receive boarding UTXO confirmations or resume existing boarding flows.
func (s *Idle) ProcessEvent(ctx context.Context, event ClientEvent,
	env *ClientEnvironment) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *ResumeBoardingIntents:
		// If for some reason, there aren't any new intents, then we'll
		// stay in the idle state.
		if evt.isEmpty() {
			env.Log.DebugS(ctx, "ResumeBoardingIntents received "+
				"with no intents")

			return &ClientStateTransition{
				NextState: s,
			}, nil
		}

		env.Log.InfoS(ctx, "Resuming boarding intents",
			evt.logAttributes())

		// Otherwise, we'll start to assemble a round with the resumed
		// intents.
		return &ClientStateTransition{
			NextState: &PendingRoundAssembly{
				Boarding: slices.Clone(evt.Boarding),
				VTXOs:    slices.Clone(evt.VTXOs),
			},
		}, nil

	case *BoardingUTXOConfirmed:
		env.Log.InfoS(ctx, "Processing boarding UTXO confirmation in Idle state",
			btclog.Fmt("outpoint", "%v", evt.Outpoint),
			slog.Int("block_height", int(evt.BlockHeight)))

		// A boarding UTXO was confirmed. The wallet has already
		// persisted the intent; we just need to create an internal
		// BoardingIntent and transition to PendingRoundAssembly.
		if evt.Tx == nil {
			return failWithNotification(
				"confirmation event missing transaction",
				fmt.Errorf("BoardingUTXOConfirmed.Tx is nil"),
				true, fn.None[RoundID](),
			), nil
		}

		// Extract the confirmed output value.
		if int(evt.Outpoint.Index) >= len(evt.Tx.TxOut) {
			return failWithNotification(
				fmt.Sprintf(
					"invalid outpoint index %d for tx %s",
					evt.Outpoint.Index, evt.Outpoint.Hash,
				),
				fmt.Errorf("outpoint index out of range"),
				true, fn.None[RoundID](),
			), nil
		}
		confirmedOutput := evt.Tx.TxOut[evt.Outpoint.Index]

		// Create the chain info from the confirmation event, including
		// the TxProof for SPV verification.
		chainInfo := BoardingChainInfo{
			ConfHeight: evt.BlockHeight,
			ConfHash:   evt.BlockHash,
			ConfTx:     evt.Tx,
			OutPoint:   evt.Outpoint,
			Amount:     btcutil.Amount(confirmedOutput.Value),
			TxProof:    evt.TxProof,
		}

		// Create a wallet intent with the full address from the event.
		walletIntent := WalletBoardingIntent{
			Address:   evt.Address,
			Outpoint:  evt.Outpoint,
			ChainInfo: chainInfo,
			Status:    BoardingStatusConfirmed,
		}

		// Build the boarding request from the address.
		boardingRequest := types.BoardingRequest{
			Outpoint:    &evt.Outpoint,
			ClientKey:   evt.Address.KeyDesc.PubKey,
			OperatorKey: evt.Address.OperatorKey,
		}

		// Build the VTXO template from the boarding address info. The
		// client wants a single VTXO with the full confirmed amount.
		//
		// TODO(elle): support fan-out and fan-in by separating this
		// direct link between boarding UTXO and VTXO request.
		// Also: vtxo info should not be directly derived from boarding
		// info.
		vtxoIntent := types.VTXORequest{
			Amount: btcutil.Amount(
				confirmedOutput.Value,
			),
			PkScript:    confirmedOutput.PkScript,
			ClientKey:   evt.Address.KeyDesc.PubKey,
			OperatorKey: evt.Address.OperatorKey,
			Expiry:      evt.Address.ExitDelay,
			SigningKey:  evt.Address.KeyDesc,
		}

		// Create a BoardingIntent for the next round.
		boardingIntent := BoardingIntent{
			BoardingIntent: walletIntent,
			Request:        boardingRequest,
		}

		env.Log.InfoS(ctx, "Transitioning to PendingRoundAssembly",
			slog.Int("amount", int(chainInfo.Amount)),
			btclog.Fmt("outpoint", "%v", evt.Outpoint))

		return &ClientStateTransition{
			NextState: &PendingRoundAssembly{
				Boarding: []BoardingIntent{boardingIntent},
				VTXOs:    []types.VTXORequest{vtxoIntent},
			},
		}, nil

	default:
		// Self-loop on unknown events - do not halt the FSM.
		return selfLoop(s), nil
	}
}

// ProcessEvent for PendingRoundAssembly tracks confirmed boarding intents and
// transitions to registration once all are ready.
func (s *PendingRoundAssembly) ProcessEvent(ctx context.Context,
	event ClientEvent, env *ClientEnvironment) (*ClientStateTransition,
	error) {

	switch evt := event.(type) {
	// A new boarding UTXO was confirmed. The wallet handles persistence;
	// we just add to our internal map.
	case *BoardingUTXOConfirmed:
		env.Log.InfoS(ctx, "Additional boarding UTXO confirmed during assembly",
			btclog.Fmt("outpoint", "%v", evt.Outpoint),
			slog.Int("current_boarding_intent_count", len(s.Boarding)),
			slog.Int("current_vtxo_intent_count", len(s.VTXOs)))

		if evt.Tx == nil {
			return failWithNotification(
				"confirmation event missing transaction",
				fmt.Errorf("BoardingUTXOConfirmed.Tx is nil"),
				true, fn.None[RoundID](),
			), nil
		}

		// Extract the confirmed output value.
		if int(evt.Outpoint.Index) >= len(evt.Tx.TxOut) {
			return failWithNotification(
				fmt.Sprintf(
					"invalid outpoint index %d for tx %s",
					evt.Outpoint.Index, evt.Outpoint.Hash,
				),
				fmt.Errorf("outpoint index out of range"),
				true, fn.None[RoundID](),
			), nil
		}
		confirmedOutput := evt.Tx.TxOut[evt.Outpoint.Index]

		for _, intent := range s.Boarding {
			if intent.Outpoint != evt.Outpoint {
				continue
			}

			env.Log.InfoS(ctx, "Boarding UTXO already present in intents map",
				btclog.Fmt("outpoint", "%v", evt.Outpoint))

			// Self-loop without adding duplicate intent.
			return selfLoop(s), nil
		}

		// Create the chain info from the confirmation event, including
		// the TxProof for SPV verification.
		chainInfo := BoardingChainInfo{
			ConfHeight: evt.BlockHeight,
			ConfHash:   evt.BlockHash,
			ConfTx:     evt.Tx,
			OutPoint:   evt.Outpoint,
			Amount:     btcutil.Amount(confirmedOutput.Value),
			TxProof:    evt.TxProof,
		}

		// Create a wallet intent with the full address from the event.
		walletIntent := WalletBoardingIntent{
			Address:   evt.Address,
			Outpoint:  evt.Outpoint,
			ChainInfo: chainInfo,
			Status:    BoardingStatusConfirmed,
		}

		// Build the boarding request from the address.
		boardingRequest := types.BoardingRequest{
			Outpoint:    &evt.Outpoint,
			ClientKey:   evt.Address.KeyDesc.PubKey,
			OperatorKey: evt.Address.OperatorKey,
		}

		// Build the VTXO template from the boarding address info. The
		// client wants a single VTXO with the full confirmed amount.
		vtxoIntent := types.VTXORequest{
			Amount: btcutil.Amount(
				confirmedOutput.Value,
			),
			PkScript:    confirmedOutput.PkScript,
			ClientKey:   evt.Address.KeyDesc.PubKey,
			OperatorKey: evt.Address.OperatorKey,
			Expiry:      evt.Address.ExitDelay,
			SigningKey:  evt.Address.KeyDesc,
		}

		intent := BoardingIntent{
			BoardingIntent: walletIntent,
			Request:        boardingRequest,
		}

		// Add the new intent to our existing set.
		updatedBoardingIntents := slices.Clone(s.Boarding)
		updatedBoardingIntents = append(updatedBoardingIntents, intent)

		// Also add the new VTXO intent.
		updatedVTXOIntents := slices.Clone(s.VTXOs)
		updatedVTXOIntents = append(updatedVTXOIntents, vtxoIntent)

		return &ClientStateTransition{
			NextState: &PendingRoundAssembly{
				Boarding: updatedBoardingIntents,
				VTXOs:    updatedVTXOIntents,
			},
		}, nil

	// It's time to register our confirmed boarding UTXOs for the next
	// round. We'll send a message to the server using our outbox, then
	// transition to the next phase.
	case *RegistrationRequested:
		env.Log.InfoS(ctx, "Registration requested, preparing to join round",
			slog.Int("boarding_intent_count", len(s.Boarding)),
			slog.Int("vtxo_intent_count", len(s.VTXOs)))

		intent := Intents{
			Boarding: slices.Clone(s.Boarding),
			VTXOs:    slices.Clone(s.VTXOs),
		}

		// TODO(elle): should be able to remove the checks below and
		// only need to check that total input and output amounts are
		// sane.

		// Extract the set of values from the intent map, as we don't
		// need to track them by outpoint any longer.
		boardingReqs := fn.Map(s.Boarding, buildBoardingRequest)
		if len(boardingReqs) == 0 {
			return failWithNotification(
				"no boarding requests to register",
				fmt.Errorf("empty boarding requests"),
				true, fn.None[RoundID](),
			), nil
		}

		// Next, we'll extract all the VTXO requests.
		vtxoReqs := slices.Clone(s.VTXOs)
		if len(vtxoReqs) == 0 {
			return failWithNotification(
				"no VTXO requests to register",
				fmt.Errorf("empty VTXO requests"),
				true, fn.None[RoundID](),
			), nil
		}

		env.Log.InfoS(ctx, "Sending JoinRoundRequest to server",
			slog.Int("boarding_requests", len(boardingReqs)),
			slog.Int("vtxo_requests", len(vtxoReqs)))

		// With all this extracted, we'll now send the JoinRoundRequest
		// to kick off the signing process.
		return &ClientStateTransition{
			NextState: &RegistrationSentState{
				Intents: intent,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: []ClientOutMsg{
					&JoinRoundRequest{
						BoardingRequests: boardingReqs,
						VTXORequests:     vtxoReqs,
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
			slog.Int("vtxo_intent_count", len(s.Intents.VTXOs)))

		// First, well make sure that all boarding UTXOs are present
		// in the round transaction and build the outpoint-to-index map.
		boardingInputIndices, err := validateBoardingInputs(
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

		env.Log.DebugS(ctx, "Validated boarding inputs in commitment tx",
			slog.Int("boarding_input_count", len(boardingInputIndices)))

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

		// TODO(roasbeef): for refresh and off boarding, need extra
		// validation for:
		//   * connector tree
		//   * outputs on commit, etc

		env.Log.InfoS(ctx, "Commitment transaction validated successfully",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("client_trees", len(clientTrees)),
			slog.Int("vtxo_tree_count", len(s.VTXOTreePaths)))

		// We'll now transition to the CommitmentTxValidatedState, and
		// emit an internal generate nonces event so we can propagate
		// the state.
		return &ClientStateTransition{
			NextState: &CommitmentTxValidatedState{
				RoundID:              s.RoundID,
				CommitmentTx:         s.CommitmentTx,
				VTXOTreePaths:        s.VTXOTreePaths,
				Intents:              s.Intents.Clone(),
				ClientTrees:          clientTrees,
				BoardingInputIndices: boardingInputIndices,
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
//
//nolint:funlen
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

		env.Log.InfoS(ctx, "Validated aggregated signatures, signing boarding inputs",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("boarding_intent_count", len(s.Intents.Boarding)))

		// Now that we know all the signatures are valid, we'll sign
		// off on each of our boarding inputs sent to the server.
		//
		// Build a PrevOutputFetcher from ALL PSBT inputs. Taproot
		// sighash (BIP341) requires prevout info for all inputs.
		tx := s.CommitmentTx.UnsignedTx
		prevOuts := make(map[wire.OutPoint]*wire.TxOut)
		for i, pIn := range s.CommitmentTx.Inputs {
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
		for _, boardingIntent := range s.Intents.Boarding {
			outpoint := boardingIntent.Request.Outpoint
			inputIdx, found := s.BoardingInputIndices[*outpoint]
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

			// Access chain info directly from the embedded wallet
			// intent.
			chainInfo := boardingIntent.ChainInfo
			addr := boardingIntent.Address.Address
			amt := chainInfo.Amount

			// Use PayToAddrScript to get the full pkScript with
			// OP_1 OP_PUSHBYTES_32 prefix for P2TR addresses.
			// ScriptAddress() only returns the 32-byte witness.
			pkScript, err := txscript.PayToAddrScript(addr)
			if err != nil {
				return nil, fmt.Errorf("pay to addr script: %w",
					err)
			}

			// Create the TxOut for the boarding output.
			output := &wire.TxOut{
				Value:    int64(amt),
				PkScript: pkScript,
			}

			signature, err := scripts.SignVTXOCollabInput(
				env.Wallet, tx, inputIdx, spendInfo,
				&boardingIntent.Address.KeyDesc, output,
				sigHashes, prevOutFetcher,
			)
			if err != nil {
				return nil, fmt.Errorf("failed to sign "+
					"boarding input %d: %w", inputIdx, err)
			}

			// Convert input.Signature to *schnorr.Signature.
			schnorrSig, ok := signature.(*schnorr.Signature)
			if !ok {
				return nil, fmt.Errorf("signature is not a " +
					"schnorr signature")
			}

			// Build the structured boarding input signature.
			inputSig := &types.BoardingInputSignature{
				InputIndex:      inputIdx,
				Outpoint:        *outpoint,
				ClientSignature: schnorrSig,
			}
			boardingInputSigs = append(
				boardingInputSigs, inputSig,
			)
		}

		numSigs := len(boardingInputSigs)
		numIntents := len(s.Intents.Boarding)
		if numSigs != numIntents {
			return nil, fmt.Errorf("signature count %d != "+
				"intent count %d", numSigs, numIntents)
		}

		// Create a single forfeit signature request with all
		// signatures.
		forfeitSigReq := &SubmitForfeitSigRequest{
			RoundID:    s.RoundID,
			Signatures: boardingInputSigs,
		}

		txid := tx.TxHash()
		callerID := fmt.Sprintf("commitment-%s", txid.String())

		// Get pkScript from the first output for LND confirmation
		// tracking.
		var pkScript []byte
		if len(tx.TxOut) > 0 {
			pkScript = tx.TxOut[0].PkScript
		}

		env.Log.InfoS(ctx, "Building RegisterConfirmationRequest",
			slog.String("round_id", s.RoundID.String()),
			slog.String("txid", txid.String()),
			slog.Int("num_outputs", len(tx.TxOut)),
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
		// recover to InputSigSentState.
		nextState := &InputSigSentState{
			RoundID:       s.RoundID,
			CommitmentTx:  s.CommitmentTx,
			VTXOTreePaths: s.VTXOTreePaths,
			Intents:       s.Intents.Clone(),
			ClientTrees:   s.ClientTrees,
			InputSigs:     boardingInputSigs,
		}
		err := env.RoundStore.CommitState(ctx, round, nextState)
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

		env.Log.InfoS(ctx, "Built client VTXOs from confirmed transaction",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("vtxo_count", len(vtxos)))

		// Persist VTXOs with their extracted tree paths for future
		// spending.
		if err := env.VTXOStore.SaveVTXOs(ctx, vtxos); err != nil {
			return nil, fmt.Errorf("failed to save VTXOs: %w", err)
		}

		env.Log.InfoS(ctx, "Saved VTXOs to store, round complete",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("vtxo_count", len(vtxos)))

		confInfo := ConfInfo{
			Height:    evt.BlockHeight,
			BlockHash: evt.BlockHash,
		}

		return &ClientStateTransition{
			NextState: &ConfirmedState{
				TxID:          evt.TxID,
				BlockHeight:   evt.BlockHeight,
				BlockHash:     evt.BlockHash,
				Confirmations: evt.Confirmations,
				VTXOs:         vtxos,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: []ClientOutMsg{
					&VTXOCreatedNotification{VTXOs: vtxos},
					&RoundCompletedNotification{
						RoundID:  s.RoundID,
						TxID:     evt.TxID,
						ConfInfo: confInfo,
					},
				},
			}),
		}, nil

	default:
		// Self-loop on unknown events - do not halt the FSM.
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

// ProcessEvent for ClientFailedState. This state is now recoverable and
// accepts the same events as Idle (BoardingUTXOConfirmed,
// ResumeBoardingIntents) to allow the FSM to restart the boarding process
// after a failure. Instead of duplicating the Idle logic, we transition to
// Idle and forward the event as an internal event for Idle to process.
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

	case *ResumeBoardingIntents:
		// Recovery path: transition to Idle and forward the event.
		// If no intents are provided, stay in the current state.
		if evt.isEmpty() {
			return selfLoop(s), nil
		}

		env.Log.InfoS(ctx, "Recovering from failed state with resumed intents",
			evt.logAttributes())

		return &ClientStateTransition{
			NextState: &Idle{},
			NewEvents: fn.Some(ClientEmittedEvent{
				InternalEvent: []ClientEvent{evt},
			}),
		}, nil

	case *BoardingUTXOConfirmed:
		env.Log.InfoS(ctx, "Recovering from failed state with new boarding confirmation",
			btclog.Fmt("outpoint", "%v", evt.Outpoint))

		// Recovery path: transition to Idle and forward the event
		// to be processed there. This avoids duplicating the
		// BoardingUTXOConfirmed handling logic.
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

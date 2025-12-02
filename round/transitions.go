package round

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// buildBoardingRequest constructs a types.BoardingRequest from a
// BoardingIntent. This pulls together the outpoint, keys, and exit delay from
// the embedded wallet intent and address, along with the TxProof from
// ChainInfo if present.
func buildBoardingRequest(intent BoardingIntent) types.BoardingRequest {
	addr := intent.Address

	return types.BoardingRequest{
		Outpoint:    &intent.Outpoint,
		ClientKey:   addr.KeyDesc.PubKey,
		OperatorKey: addr.OperatorKey,
		ExitDelay:   addr.ExitDelay,
		TxProof:     intent.ChainInfo.TxProof,
	}
}

// ProcessEvent handles the events from the Idle state. In this state, we'll
// receive boarding UTXO confirmations or resume existing boarding flows.
func (s *Idle) ProcessEvent(_ context.Context, event ClientEvent,
	env *ClientEnvironment) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *ResumeBoardingIntents:
		// If for some reason, there aren't any new intents, then we'll
		// stay in the idle state.
		if len(evt.Intents) == 0 {
			return &ClientStateTransition{
				NextState: s,
			}, nil
		}

		// Otherwise, we'll start to assemble a round with the resumed
		// intents.
		return &ClientStateTransition{
			NextState: &PendingRoundAssembly{
				Intents: evt.Intents,
			},
		}, nil

	case *BoardingUTXOConfirmed:
		// A boarding UTXO was confirmed. The wallet has already
		// persisted the intent; we just need to create an internal
		// BoardingIntent and transition to PendingRoundAssembly.
		if evt.Tx == nil {
			return nil, fmt.Errorf("confirmation event " +
				"missing transaction")
		}

		// Extract the confirmed output value.
		if int(evt.Outpoint.Index) >= len(evt.Tx.TxOut) {
			return nil, fmt.Errorf("invalid outpoint index %d for "+
				"tx %s", evt.Outpoint.Index, evt.Outpoint.Hash)
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
		vtxoTemplate := []types.VTXORequest{
			{
				Amount: btcutil.Amount(
					confirmedOutput.Value,
				),
				PkScript:    confirmedOutput.PkScript,
				ClientKey:   evt.Address.KeyDesc.PubKey,
				OperatorKey: evt.Address.OperatorKey,
				Expiry:      evt.Address.ExitDelay,
				SigningKey:  evt.Address.KeyDesc,
			},
		}

		// Create the round's BoardingIntent.
		intent := BoardingIntent{
			BoardingIntent:  walletIntent,
			BoardingRequest: boardingRequest,
			VtxoTemplate:    vtxoTemplate,
			RoundID:         fn.None[string](),
		}

		intentMap := make(map[wire.OutPoint]BoardingIntent)
		intentMap[evt.Outpoint] = intent

		return &ClientStateTransition{
			NextState: &PendingRoundAssembly{
				Intents: intentMap,
			},
		}, nil

	default:
		return nil, fmt.Errorf("idle: unexpected event: %T", event)
	}
}

// ProcessEvent for PendingRoundAssembly tracks confirmed boarding intents and
// transitions to registration once all are ready.
func (s *PendingRoundAssembly) ProcessEvent(
	_ context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	// A new boarding UTXO was confirmed. The wallet handles persistence;
	// we just add to our internal map.
	case *BoardingUTXOConfirmed:
		if evt.Tx == nil {
			return nil, fmt.Errorf("confirmation event missing " +
				"transaction")
		}

		// Extract the confirmed output value.
		if int(evt.Outpoint.Index) >= len(evt.Tx.TxOut) {
			return nil, fmt.Errorf("invalid outpoint index %d for "+
				"tx %s", evt.Outpoint.Index, evt.Outpoint.Hash)
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
		vtxoTemplate := []types.VTXORequest{
			{
				Amount: btcutil.Amount(
					confirmedOutput.Value,
				),
				PkScript:    confirmedOutput.PkScript,
				ClientKey:   evt.Address.KeyDesc.PubKey,
				OperatorKey: evt.Address.OperatorKey,
				Expiry:      evt.Address.ExitDelay,
				SigningKey:  evt.Address.KeyDesc,
			},
		}

		intent := BoardingIntent{
			BoardingIntent:  walletIntent,
			BoardingRequest: boardingRequest,
			VtxoTemplate:    vtxoTemplate,
			RoundID:         fn.None[string](),
		}

		// Add the newly confirmed intent to our map.
		updatedIntents := maps.Clone(s.Intents)
		updatedIntents[evt.Outpoint] = intent

		return &ClientStateTransition{
			NextState: &PendingRoundAssembly{
				Intents: updatedIntents,
			},
		}, nil

	// It's time to register our confirmed boarding UTXOs for the next
	// round. We'll send a message to the server using our outbox, then
	// transition to the next phase.
	case *RegistrationRequested:
		// Extract the set of values from the intent map, as we don't
		// need to track them by outpoint any longer.
		//
		intentSlice := slices.Collect(maps.Values(s.Intents))
		boardingReqs := fn.Map(intentSlice, buildBoardingRequest)
		if len(boardingReqs) == 0 {
			return nil, fmt.Errorf("no boarding requests " +
				"to register")
		}

		// Next, we'll extract all the VTXO templates from the set of
		// nested intents.
		vtxoReqLists := fn.Map(
			intentSlice,
			func(intent BoardingIntent) []types.VTXORequest {
				return intent.VtxoTemplate
			},
		)
		vtxoReqs := fn.Flatten(vtxoReqLists)
		if len(vtxoReqs) == 0 {
			return nil, fmt.Errorf("no VTXO requests to register")
		}

		// With all this extract, we'll now send the JoinRoundRequest
		// to kick off the singing process.
		return &ClientStateTransition{
			NextState: &RegistrationSentState{
				Intents: intentSlice,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: []ClientEvent{
					&JoinRoundRequest{
						BoardingRequests: boardingReqs,
						VTXORequests:     vtxoReqs,
						RoundID:          evt.RoundID,
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
		return nil, fmt.Errorf("pending_round_assembly: "+
			"unexpected event: %T", event)
	}
}

// ProcessEvent for RegistrationSentState.
func (s *RegistrationSentState) ProcessEvent(
	_ context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *RoundJoined:
		return &ClientStateTransition{
			NextState: &RoundJoinedState{
				RoundID: evt.RoundID,
				Intents: slices.Clone(s.Intents),
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
		return nil, fmt.Errorf("registration_sent: unexpected "+
			"event: %T", event)
	}
}

// ProcessEvent for RoundJoinedState.
func (s *RoundJoinedState) ProcessEvent(
	_ context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *CommitmentTxBuilt:
		return &ClientStateTransition{
			NextState: &CommitmentTxReceivedState{
				RoundID:      evt.RoundID,
				CommitmentTx: evt.Tx,
				TxID:         evt.TxID,
				VTXTTree:     evt.VTXTTree,
				Intents:      slices.Clone(s.Intents),
				ClientTrees:  make(map[string]*tree.Tree),
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				InternalEvent: []ClientEvent{
					&CommitmentTxBuilt{
						CommitmentTxBuiltEvent: evt.
							CommitmentTxBuiltEvent,
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
		return nil, fmt.Errorf("round_joined: unexpected event: "+
			"%T", event)
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
		outpoint := intent.BoardingRequest.Outpoint
		if _, found := outpointToIdx[*outpoint]; !found {
			return nil, fmt.Errorf("boarding UTXO %s not found "+
				"in commitment tx", outpoint)
		}
	}

	return outpointToIdx, nil
}

func signerKeyID(pk *btcec.PublicKey) string {
	if pk == nil {
		return ""
	}

	return hex.EncodeToString(pk.SerializeCompressed())
}

// ProcessEvent for CommitmentTxReceivedState.
//
//nolint:ll
func (s *CommitmentTxReceivedState) ProcessEvent(
	_ context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *CommitmentTxBuilt:
		// First, well make sure that all boarding UTXOs are present
		// in the round transaction and build the outpoint-to-index map.
		boardingInputIndices, err := validateBoardingInputs(
			s.CommitmentTx, s.Intents,
		)
		if err != nil {
			return &ClientStateTransition{
				NextState: &ClientFailedState{
					Reason: "commitment tx " +
						"validation failed",
					Error:       err,
					Recoverable: true,
				},
			}, nil
		}

		clientTrees := make(map[string]*tree.Tree)

		// Next, we'll make sure that each of the VTXO requests that we
		// originally requested are actually present in the VTXT tree
		// that the server sent us.
		vtxoRequests := fn.Map(
			s.Intents,
			func(intent BoardingIntent) []types.VTXORequest {
				return intent.VtxoTemplate
			},
		)
		for i, vtxoReq := range fn.Flatten(vtxoRequests) {
			clientTree, err := ValidateVTXTPath(
				s.VTXTTree, vtxoReq.SigningKey.PubKey,
				[]types.VTXORequest{vtxoReq},
				env.OperatorTerms.PubKey,
			)
			if err != nil {
				return &ClientStateTransition{
					NextState: &ClientFailedState{
						Reason: fmt.Sprintf(
							"VTXT validation "+
								"failed for VTXO "+
								"request %d", i,
						),
						Error:       err,
						Recoverable: false,
					},
				}, nil
			}

			// Now that we know this VTXO request was properly
			// included in the tree, we'll store the client-tree
			// (travesal path from the root to this vtox leaf).
			clientID := signerKeyID(vtxoReq.SigningKey.PubKey)
			clientTrees[clientID] = clientTree
		}

		// Make sure all anchor outputs are valid in the tree, if they
		// aren't we may not be able to go on chain.
		if err := ValidateAnchorOutputs(s.VTXTTree); err != nil {
			return &ClientStateTransition{
				NextState: &ClientFailedState{
					Reason: "anchor output " +
						"validation failed",
					Error:       err,
					Recoverable: false,
				},
			}, nil
		}

		// TODO(roasbeef): for refresh and off boarding, need extra
		// validation for:
		//   * connector tree
		//   * outputs on commit, etc

		// We'll now transition to the CommitmentTxValidatedState, and
		// emit an internal generate nonces event so we can propagate
		// the state.
		return &ClientStateTransition{
			NextState: &CommitmentTxValidatedState{
				RoundID:              s.RoundID,
				CommitmentTx:         s.CommitmentTx,
				VTXTTree:             s.VTXTTree,
				Intents:              slices.Clone(s.Intents),
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
		return nil, fmt.Errorf("commitment_tx_received: unexpected "+
			"event: %T", event)
	}
}

// ProcessEvent for CommitmentTxValidatedState.
func (s *CommitmentTxValidatedState) ProcessEvent(
	_ context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch event.(type) {
	case *GenerateNonces:
		// Get sweep tapscript root from the validated tree. This was
		// set when the operator built the tree.
		sweepTweak := s.VTXTTree.SweepTapscriptRoot

		// Build the prev output fetcher for signing. The batch output
		// is needed so the root transaction can look up the output it
		// spends from the commitment transaction.
		prevOutFetcher, err := s.VTXTTree.Root.PrevOutputFetcher(
			s.VTXTTree.BatchOutput,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create prev output "+
				"fetcher: %w", err)
		}

		// At this point, all the basic validation checks have passed.
		// So now we'll generate a musig2 session to create nonces to
		// sign the VTXO tree. Each VTXO that we created will
		// effectively be a new musig session.
		musig2Sessions := make(map[string]*tree.SignerSession)
		for _, boardingIntent := range s.Intents {
			for _, vtxoReq := range boardingIntent.VtxoTemplate {
				clientID := signerKeyID(
					vtxoReq.SigningKey.PubKey,
				)

				// TODO(roasbeef): actually use the interface
				// in front of this?
				session, err := tree.NewSignerSession(
					env.Wallet, &vtxoReq.SigningKey,
					sweepTweak, prevOutFetcher,
					s.VTXTTree.Root,
				)
				if err != nil {
					return nil, fmt.Errorf("failed to "+
						"create signing session for "+
						"client %s: %w", clientID, err)
				}

				musig2Sessions[clientID] = session
			}
		}

		// Now that we have all our sessions created, we'll have each
		// of them generate nonces to use in tree signing.
		submitTreeNonces := make([]ClientEvent, 0, len(musig2Sessions))
		for cid, session := range musig2Sessions {
			nonces, err := session.GetNonces()
			if err != nil {
				return nil, fmt.Errorf("failed to "+
					"generate nonces for client %s: %w",
					cid, err)
			}
			nonceBytes := make([][]byte, 0, len(nonces))
			for _, nonce := range nonces {
				nonceBytes = append(nonceBytes, nonce[:])
			}
			nonceMsg := &SubmitNoncesRequest{
				RoundID:        s.RoundID,
				Nonces:         nonceBytes,
				ParticipantKey: session.PubKey(),
			}

			submitTreeNonces = append(submitTreeNonces, nonceMsg)
		}

		// MuSig2 nonces have been generated locally. Send them to the
		// server to participate in the aggregated nonce computation.
		return &ClientStateTransition{
			NextState: &NoncesSentState{
				RoundID:              s.RoundID,
				CommitmentTx:         s.CommitmentTx,
				VTXTTree:             s.VTXTTree,
				Intents:              slices.Clone(s.Intents),
				ClientTrees:          s.ClientTrees,
				Musig2Sessions:       musig2Sessions,
				BoardingInputIndices: s.BoardingInputIndices,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: submitTreeNonces,
			}),
		}, nil

	default:
		return nil, fmt.Errorf("commitment_tx_validated: unexpected "+
			"event: %T", event)
	}
}

// ProcessEvent for NoncesSentState.
func (s *NoncesSentState) ProcessEvent(
	_ context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *NoncesAggregated:
		// Received aggregated nonces from the server. Now register
		// them with our signing session and generate partial
		// signatures.
		//
		// The server sends ONE combined/aggregated nonce per
		// transaction (not individual nonces from each participant).
		// The server has already aggregated all participants' nonces
		// using MuSig2 nonce aggregation.
		//
		// Build txid-to-nonces map by traversing the tree. The server
		// sends nonces in tree traversal order, so we map them to
		// txids using the tree structure.
		aggNoncesMap := make(map[string][]Musig2PubNonce)
		nonceIndex := 0

		err := s.VTXTTree.Root.ForEach(func(node *tree.Node) error {
			if nonceIndex >= len(evt.AggregatedNonces) {
				return fmt.Errorf("not enough aggregated " +
					"nonces")
			}

			txid, err := node.TXID()
			if err != nil {
				return fmt.Errorf("failed to get txid: %w", err)
			}

			// Each transaction gets one combined nonce from the
			// server. We wrap it in a slice to match
			// RegisterNonces signature.
			var nonce Musig2PubNonce
			copy(nonce[:], evt.AggregatedNonces[nonceIndex])
			aggNoncesMap[txid.String()] = []Musig2PubNonce{nonce}
			nonceIndex++

			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("failed to build nonce "+
				"map: %w", err)
		}

		// With the nonces grouped, we need to register the nonces with
		// each client session.
		for _, musig2Session := range s.Musig2Sessions {
			// Register the combined nonces with our signing
			// session.
			err = musig2Session.RegisterNonces(aggNoncesMap)
			if err != nil {
				return nil, fmt.Errorf("failed to register "+
					"combined nonces: %w", err)
			}
		}

		return &ClientStateTransition{
			NextState: &NoncesAggregatedState{
				RoundID:              s.RoundID,
				CommitmentTx:         s.CommitmentTx,
				VTXTTree:             s.VTXTTree,
				Intents:              slices.Clone(s.Intents),
				ClientTrees:          s.ClientTrees,
				Musig2Sessions:       s.Musig2Sessions,
				AggregatedNonces:     evt.AggregatedNonces,
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
		return nil, fmt.Errorf("nonces_sent: unexpected event: %T",
			event)
	}
}

// ProcessEvent for NoncesAggregatedState.
func (s *NoncesAggregatedState) ProcessEvent(
	_ context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch event.(type) {
	case *GeneratePartialSigs:
		// At this stage, the nonces have been aggregated for each
		// client, so now we'll generate and send our partial
		// signatures.
		//
		// TODO(roasbeef); how should the partial sigs actually be
		// assembled?
		var (
			submitPartialSigs []ClientEvent
		)
		for _, musig2Session := range s.Musig2Sessions {
			// Generate partial signatures for all transactions in
			// our path.
			partialSigs, err := musig2Session.Signatures(true)
			if err != nil {
				return nil, fmt.Errorf("failed to generate "+
					"partial signatures: %w", err)
			}

			var partialSigBytes [][]byte
			for _, sig := range partialSigs {
				var buf bytes.Buffer
				err := sig.Encode(&buf)
				if err != nil {
					return nil, fmt.Errorf("failed to "+
						"encode partial "+
						"signature: %w", err)
				}
				partialSigBytes = append(
					partialSigBytes, buf.Bytes(),
				)
			}

			submitPartialSigs = append(
				submitPartialSigs, &SubmitPartialSigRequest{
					RoundID:        s.RoundID,
					ParticipantKey: musig2Session.PubKey(),
					PartialSigs:    partialSigBytes,
				},
			)
		}

		// Partial MuSig2 signatures have been generated using the
		// aggregated nonces. Send them to the server for signature
		// aggregation.
		return &ClientStateTransition{
			NextState: &PartialSigsSentState{
				RoundID:              s.RoundID,
				CommitmentTx:         s.CommitmentTx,
				VTXTTree:             s.VTXTTree,
				Intents:              slices.Clone(s.Intents),
				ClientTrees:          s.ClientTrees,
				Musig2Sessions:       s.Musig2Sessions,
				BoardingInputIndices: s.BoardingInputIndices,
			},
			// TODO(roasbeef): group into a single message?
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: submitPartialSigs,
			}),
		}, nil

	default:
		return nil, fmt.Errorf("nonces_aggregated: unexpected "+
			"event: %T", event)
	}
}

// ProcessEvent for PartialSigsSentState.
func (s *PartialSigsSentState) ProcessEvent(
	ctx context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *OperatorSigned:
		// At this point, Received complete VTXT signatures from the
		// server after the operator aggregated all partial signatures.
		//
		// Now, we'll validate that the aggregated signatures are valid
		// for the VTXT before proceeding. This prevents the operator
		// from providing invalid signatures that would make our VTXOs
		// unspendable.
		err := ValidateVTXTSignatures(s.VTXTTree, evt.Signatures)
		if err != nil {
			return &ClientStateTransition{
				NextState: &ClientFailedState{
					Reason: "VTXT signature " +
						"validation failed",
					Error:       err,
					Recoverable: false,
				},
			}, nil
		}

		// Now that we know all the signatures are valid, we'll sign
		// off on each of our boarding inputs sent to the server.
		var boardingSigs []*schnorr.Signature
		for _, boardingIntent := range s.Intents {
			outpoint := boardingIntent.BoardingRequest.Outpoint
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
			pkScript := addr.ScriptAddress()
			amt := chainInfo.Amount

			// Create the TxOut for the boarding output.
			output := &wire.TxOut{
				Value:    int64(amt),
				PkScript: pkScript,
			}

			prevOutFetcher := txscript.NewCannedPrevOutputFetcher(
				pkScript, int64(amt),
			)

			tx := s.CommitmentTx

			sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)

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
			boardingSigs = append(boardingSigs, schnorrSig)
		}

		sigBytes := make([][]byte, len(boardingSigs))
		for i, sig := range boardingSigs {
			sigBytes[i] = sig.Serialize()
		}
		if len(sigBytes) != len(s.Intents) {
			return nil, fmt.Errorf("signature count %d != intent "+
				"count %d", len(sigBytes), len(s.Intents))
		}

		outboxMsgs := make([]ClientEvent, 0, len(sigBytes))
		for i, intent := range s.Intents {
			if intent.Address.Address == nil {
				return nil, fmt.Errorf("intent %d missing "+
					"boarding address", i)
			}
			forfeitSig := &SubmitForfeitSigRequest{
				RoundID:        s.RoundID,
				ParticipantKey: intent.Address.KeyDesc.PubKey,
				ForfeitSigs:    [][]byte{sigBytes[i]},
			}
			outboxMsgs = append(outboxMsgs, forfeitSig)
		}

		txid := s.CommitmentTx.TxHash()
		callerID := fmt.Sprintf("commitment-%s", txid.String())
		outboxMsgs = append(outboxMsgs, &RegisterConfirmationRequest{
			CallerID:    callerID,
			Txid:        &txid,
			TargetConfs: env.OperatorTerms.MinConfirmations,
		})

		// Checkpoint the round state at the "point of no return".
		// After sending boarding input signatures, the server may
		// broadcast the commitment transaction. We must persist all
		// round data to enable recovery if the client restarts.
		//
		// Mark all intents as Adopted (frozen in this round) and set
		// their RoundID, then save them alongside the round.
		adoptedIntents := make([]BoardingIntent, len(s.Intents))
		for i, intent := range s.Intents {
			intent.Status = BoardingStatusAdopted
			intent.RoundID = fn.Some(s.RoundID)
			adoptedIntents[i] = intent
		}
		round := &Round{
			RoundID:      s.RoundID,
			CommitmentTx: fn.Some(s.CommitmentTx),
			VTXTTree:     fn.Some(s.VTXTTree),
			BoardingGroup: &BoardingGroup{
				RoundID: s.RoundID,
				Intents: adoptedIntents,
			},
		}

		// Checkpoint round data + FSM state atomically at the "point
		// of no return". The next state is persisted so restart can
		// recover to InputSigSentState.
		nextState := &InputSigSentState{
			RoundID:      s.RoundID,
			CommitmentTx: s.CommitmentTx,
			VTXTTree:     s.VTXTTree,
			Intents:      slices.Clone(s.Intents),
			ClientTrees:  s.ClientTrees,
			InputSigs:    sigBytes,
		}
		err = env.RoundStore.CommitState(ctx, round, nextState)
		if err != nil {
			return nil, fmt.Errorf("failed to commit round "+
				"state: %w", err)
		}

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
		return nil, fmt.Errorf("partial_sigs_sent: unexpected "+
			"event: %T", event)
	}
}

// buildClientVTXOs constructs ClientVTXO instances from the boarding intents
// and client trees.
func buildClientVTXOs(intents []BoardingIntent,
	trees map[string]*tree.Tree, roundID string) ([]*ClientVTXO, error) {

	vtxos := make([]*ClientVTXO, 0)
	for _, intent := range intents {
		// Each intent has a VTXO template with one or more requests.
		for _, req := range intent.VtxoTemplate {
			tree := trees[signerKeyID(req.SigningKey.PubKey)]
			if tree == nil {
				return nil, fmt.Errorf("missing client tree " +
					"for signing key")
			}

			// Use the key descriptor from the intent's boarding
			// address.
			clientKeyDesc := intent.Address.KeyDesc

			leaves := tree.Root.GetLeafNodes()

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
					ClientKey:   clientKeyDesc,
					OperatorKey: req.OperatorKey,
					TreePath:    tree,
					RoundID:     fn.Some(roundID),
				})
			}
		}
	}

	return vtxos, nil
}

// ProcessEvent for InputSigSentState.
func (s *InputSigSentState) ProcessEvent(
	_ context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *BoardingFailed:
		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
			},
		}, nil

	case *BoardingConfirmed:
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

		// Persist VTXOs with their extracted tree paths for future
		// spending.
		if err := env.VTXOStore.SaveVTXOs(vtxos); err != nil {
			return nil, fmt.Errorf("failed to save VTXOs: %w", err)
		}

		return &ClientStateTransition{
			NextState: &ConfirmedState{
				TxID:          evt.TxID,
				BlockHeight:   evt.BlockHeight,
				Confirmations: evt.Confirmations,
				VTXOs:         vtxos,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: []ClientEvent{
					&VTXOCreatedNotification{VTXOs: vtxos},
					&RoundCompletedNotification{
						RoundID: s.RoundID,
						TxID:    evt.TxID,
					},
				},
			}),
		}, nil

	default:
		return nil, fmt.Errorf("input_sig_sent: unexpected event: %T",
			event)
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

// ProcessEvent for ClientFailedState (terminal state).
func (s *ClientFailedState) ProcessEvent(
	_ context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *RecoveryInitiated:
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

	default:
		// Stay in failed state for other events since no other
		// transitions are valid from this terminal state.
		return &ClientStateTransition{
			NextState: s,
		}, nil
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

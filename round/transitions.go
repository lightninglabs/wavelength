package round

import (
	"bytes"
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
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/tx"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
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

		spendInfo, err := arkscript.NewVTXOSpendInfoFromPolicy(
			boardingIntent.Address.KeyDesc.PubKey,
			boardingIntent.Address.OperatorKey,
			boardingIntent.Address.ExitDelay,
			0,
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

		signature, err := arkscript.SignVTXOCollabInput(
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

		// Build a set of existing boarding outpoints for O(1)
		// dedup lookups.
		boardingSeen := fn.NewSet[wire.OutPoint]()
		for _, b := range s.Boarding {
			boardingSeen.Add(b.Outpoint)
		}

		updatedBoarding := slices.Clone(s.Boarding)
		for _, newIntent := range evt.Boarding {
			if boardingSeen.Contains(newIntent.Outpoint) {
				continue
			}

			boardingSeen.Add(newIntent.Outpoint)
			updatedBoarding = append(
				updatedBoarding, newIntent,
			)
		}

		// Deduplicate forfeit requests by VTXO outpoint. A VTXO
		// can only be forfeited once per round.
		forfeitSeen := fn.NewSet[wire.OutPoint]()
		for _, f := range s.Forfeits {
			if f.VTXOOutpoint != nil {
				forfeitSeen.Add(*f.VTXOOutpoint)
			}
		}

		updatedForfeits := slices.Clone(s.Forfeits)
		for _, newForfeit := range evt.Forfeits {
			if newForfeit.VTXOOutpoint == nil {
				continue
			}

			if forfeitSeen.Contains(*newForfeit.VTXOOutpoint) {
				continue
			}

			forfeitSeen.Add(*newForfeit.VTXOOutpoint)
			updatedForfeits = append(
				updatedForfeits, newForfeit,
			)
		}

		// Deduplicate VTXO requests by effective pkScript. Two refresh
		// paths (wallet-driven and auto-expiry) could race to
		// create an output for the same VTXO. Duplicate outputs
		// would inflate totalOutput and cause the balance check
		// to fail, locking the VTXO in PendingForfeitState.
		vtxoScriptSeen := fn.NewSet[string]()
		for _, v := range s.VTXOs {
			pkScript, err := v.EffectivePkScript()
			if err != nil {
				return failWithNotification(
					"invalid VTXO policy",
					fmt.Errorf(
						"derive VTXO pkScript: %w", err,
					),
					true, fn.None[RoundID](),
				), nil
			}

			vtxoScriptSeen.Add(string(pkScript))
		}

		updatedVTXOs := slices.Clone(s.VTXOs)
		for _, newVTXO := range evt.VTXOs {
			pkScript, err := newVTXO.EffectivePkScript()
			if err != nil {
				return failWithNotification(
					"invalid VTXO policy",
					fmt.Errorf(
						"derive new VTXO pkScript: %w",
						err,
					),
					true, fn.None[RoundID](),
				), nil
			}

			key := string(pkScript)
			if vtxoScriptSeen.Contains(key) {
				continue
			}

			vtxoScriptSeen.Add(key)
			updatedVTXOs = append(updatedVTXOs, newVTXO)
		}

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
	case *IntentRequested:
		env.Log.InfoS(ctx, "Registration requested, preparing to join round",
			slog.Int("boarding_intent_count", len(s.Boarding)),
			slog.Int("vtxo_intent_count", len(s.VTXOs)))

		// Registration may outlive the triggering actor request, so use
		// a detached context for local store and wallet operations. We
		// still use the original actor context later when emitting the
		// outbox.
		opCtx := context.WithoutCancel(ctx)

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
			opCtx, env.VTXOStore, s.Forfeits,
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

		// Under the #270 seal-time fee handshake the client no
		// longer validates an implicit operator fee at intent
		// composition time: the binding fee arrives later via the
		// JoinRoundQuote message and is checked in
		// QuoteReceivedState against env.MaxOperatorFee. The
		// balance invariant (outputs <= inputs) stays here as a
		// sanity guard.
		operatorFee := totalInput - totalOutput

		env.Log.InfoS(ctx, "Intent balance check passed",
			btclog.Fmt("total_input", "%v", totalInput),
			btclog.Fmt("total_output", "%v", totalOutput),
			btclog.Fmt("estimated_operator_fee", "%v", operatorFee))

		// Extract the set of values from the intent map, as we don't
		// need to track them by outpoint any longer.
		boardingReqs := fn.Map(s.Boarding, buildBoardingRequest)
		vtxoReqs := slices.Clone(s.VTXOs)

		vtxoReqs, err = ensureVTXOSigningKeys(
			opCtx, env.Wallet, vtxoReqs,
		)
		if err != nil {
			return failWithNotification(
				"failed to derive vtxo signing keys",
				err, true, fn.None[RoundID](),
			), nil
		}

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

		// Stamp the single change marker required by the #270
		// seal-time fee handshake. Source paths leave IsChange
		// unset so individual entry points (auto-refresh, manual
		// refresh / leave RPCs, multi-VTXO refresh batches)
		// cannot accidentally produce 2+ markers when their
		// outputs are accumulated into the same assembling
		// round. Boarding and directed-send self-change paths
		// stamp markers explicitly; designateChangeMarker
		// respects those and only adds a marker when none is
		// present.
		designateChangeMarker(vtxoReqs, leaveReqs)

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
			opCtx, env.Wallet,
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
				opCtx, env, identifierKeyDesc, intent, vtxoReqs,
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
			NextState: &IntentSentState{
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

// ProcessEvent for IntentSentState.
func (s *IntentSentState) ProcessEvent(
	ctx context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *RoundJoined:
		// Under the #270 seal-time handshake the server's admission
		// ack (carried as RoundJoined) no longer marks the client as
		// committed to the round — it's a watermark only. The actor
		// layer uses this event to re-key the FSM from the ephemeral
		// temp key to the server-assigned RoundID (see
		// handleRoundJoined) but the state machine must stay parked
		// in IntentSentState so the subsequent JoinRoundQuote can be
		// handled. Transitioning to RoundJoinedState here would
		// consume IntentSentState before the quote arrives, leaving
		// the quote with no handler and stalling the round.
		env.Log.InfoS(ctx, "Intent admitted; awaiting seal-time quote",
			slog.String("round_id", evt.RoundID.String()),
			slog.Int("boarding_intent_count", len(s.Intents.Boarding)),
			slog.Int("vtxo_intent_count", len(s.Intents.VTXOs)))

		return selfLoop(s), nil

	case *JoinRoundQuoteReceived:
		// Under the #270 seal-time handshake the round will not
		// advance into batch-building until we explicitly accept
		// (or reject) the quote. Park in QuoteReceivedState so
		// the next event (QuoteAccepted/QuoteRejected, emitted
		// internally) drives the decision.
		env.Log.InfoS(ctx, "Received seal-time quote",
			slog.String("round_id", evt.RoundID.String()),
			slog.Int64("operator_fee_sat", evt.Quote.OperatorFeeSat),
			slog.Uint64("seal_pass", uint64(evt.Quote.SealPass)))

		nextState := &QuoteReceivedState{
			RoundID: evt.RoundID,
			Quote:   evt.Quote,
			Intents: s.Intents.Clone(),
		}

		decision := evaluateQuote(
			env, evt.RoundID, s.Intents, evt.Quote,
		)

		return &ClientStateTransition{
			NextState: nextState,
			NewEvents: fn.Some(ClientEmittedEvent{
				InternalEvent: []ClientEvent{decision},
			}),
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

// evaluateQuote applies the client's acceptance policy to a
// server-issued quote. Returns QuoteAccepted when:
//
//   - the server did not reject the intent (RejectReason == 0);
//   - OperatorFeeSat is non-negative and within env.MaxOperatorFee;
//   - the quote's VTXOQuotes / LeaveQuotes slices have the same
//     length as the intent's VTXORequests / LeaveRequests;
//   - every per-entry echo (pkScript, recipient key) matches the
//     corresponding intent entry;
//   - every non-change output amount equals the intent target;
//     only the designated IsChange=true output may deviate.
//
// Otherwise returns QuoteRejected with a diagnostic reason string.
// Kept separate so unit tests can drive the decision logic without
// spinning the full FSM. The intents argument is the client's own
// composed intent — comparing the server's echo against it is the
// client's only line of defense against a server that silently
// re-shapes the outputs (e.g. shifting fee burden from the change
// output onto a recipient while keeping total fee under cap).

// designateChangeMarker normalizes IsChange across the composed
// intent under the #270 seal-time fee handshake. The proto contract
// requires exactly one IsChange=true marker for multi-output intents
// (the slot the server stamps the residual into); single-output
// intents need no marker because the server treats the lone output
// as the implicit change.
//
// Rules applied (in order):
//
//  1. If any VTXO or leave already carries IsChange=true, leave it
//     alone. This preserves explicit wallet decisions: boarding
//     change in handleBoard / handleTriggerBoard, and directed-send
//     self-change in handleSendVTXOs.
//
//  2. If two or more outputs carry IsChange=true (which can only
//     happen when an entry-point path accidentally double-stamps,
//     e.g., mixing boarding-change + directed-send self-change),
//     keep the FIRST marker and clear the rest. Defensive — the
//     proto invariant is "exactly one", so silently submitting two
//     would let the server reject the round.
//
//  3. If no marker is set and the total output count is greater
//     than one, stamp the first VTXO. When the intent has only
//     leaves (no VTXOs — cooperative leave-only batches), stamp
//     the first leave. Single-output intents get no marker.
//
// Mutates the slices in place.
func designateChangeMarker(
	vtxoReqs []types.VTXORequest, leaveReqs []*types.LeaveRequest,
) {

	// First pass: count and locate existing markers.
	var (
		firstVTXOIdx  = -1
		firstLeaveIdx = -1
		markerCount   int
	)
	for i, req := range vtxoReqs {
		if !req.IsChange {
			continue
		}
		if firstVTXOIdx == -1 {
			firstVTXOIdx = i
		}
		markerCount++
	}
	for i, leave := range leaveReqs {
		if leave == nil || !leave.IsChange {
			continue
		}
		if firstLeaveIdx == -1 {
			firstLeaveIdx = i
		}
		markerCount++
	}

	// Defensive: if multiple markers are present, keep the first
	// (preferring VTXO over leave when both have a marker).
	if markerCount > 1 {
		keepVTXO := firstVTXOIdx != -1
		for i := range vtxoReqs {
			if !vtxoReqs[i].IsChange {
				continue
			}
			if keepVTXO && i == firstVTXOIdx {
				continue
			}
			vtxoReqs[i].IsChange = false
		}
		for i := range leaveReqs {
			if leaveReqs[i] == nil || !leaveReqs[i].IsChange {
				continue
			}
			// If we are keeping a VTXO marker, every leave
			// marker must be cleared. Otherwise the first
			// leave marker is kept.
			if !keepVTXO && i == firstLeaveIdx {
				continue
			}
			leaveReqs[i].IsChange = false
		}

		return
	}

	// Exactly one marker already set: nothing to do.
	if markerCount == 1 {
		return
	}

	// No marker yet. Stamp one only when the composed intent has
	// more than a single output — single-output intents need no
	// marker by proto contract.
	totalOutputs := len(vtxoReqs) + len(leaveReqs)
	if totalOutputs <= 1 {
		return
	}
	if len(vtxoReqs) > 0 {
		vtxoReqs[0].IsChange = true
		return
	}
	if len(leaveReqs) > 0 && leaveReqs[0] != nil {
		leaveReqs[0].IsChange = true
	}
}

func evaluateQuote(env *ClientEnvironment, roundID RoundID,
	intents Intents, quote *ClientQuote) ClientEvent {

	if quote == nil {
		return &QuoteRejected{
			RoundID: roundID,
			Reason:  "nil quote",
		}
	}

	// Server-side refusal surfaces as a non-OK RejectReason.
	// Propagate it into ClientFailedState so the client's caller
	// sees the server's classification rather than a local error.
	// When the server rejected the intent, vtxo_quotes /
	// leave_quotes are empty by proto contract, so skip the echo
	// validation below and emit the rejection directly. Decoder-
	// side validation in FromProto guarantees the enum name is
	// known, so the typed `String()` rendering is safe to surface
	// directly to operators / logs.
	if quote.RejectReason != roundpb.QuoteReason_QUOTE_OK {
		return &QuoteRejected{
			RoundID: roundID,
			QuoteID: quote.QuoteID,
			Reason: fmt.Sprintf(
				"server rejected intent: %s",
				quote.RejectReason,
			),
		}
	}

	if quote.OperatorFeeSat < 0 {
		return &QuoteRejected{
			RoundID: roundID,
			QuoteID: quote.QuoteID,
			Reason: fmt.Sprintf(
				"operator fee is negative: %d",
				quote.OperatorFeeSat,
			),
		}
	}

	// The spec (#270 "Quote expiry races") calls out that a slow
	// client may receive a quote past its commit window and have
	// its accept land after the server already resealed. Reject
	// stale quotes locally so the FSM doesn't sign against a
	// quote_id the server has moved past. A zero expiry is treated
	// as "no explicit deadline" so pre-#270 harnesses keep working.
	if quote.QuoteExpiresAt > 0 &&
		env.now().Unix() >= quote.QuoteExpiresAt {

		return &QuoteRejected{
			RoundID: roundID,
			QuoteID: quote.QuoteID,
			Reason: fmt.Sprintf(
				"quote expired at %d (now=%d)",
				quote.QuoteExpiresAt, env.now().Unix(),
			),
		}
	}

	// Fail closed on an unset MaxOperatorFee: the zero value is
	// the Go default for an unconfigured `btcutil.Amount`, so
	// treating it as "no cap" would silently accept any server-
	// quoted fee whenever an integrator forgets to plumb the
	// field through. Callers that want an uncapped environment
	// can supply a sentinel (e.g. math.MaxInt64) deliberately.
	feeCap := int64(env.MaxOperatorFee)
	if feeCap <= 0 {
		return &QuoteRejected{
			RoundID: roundID,
			QuoteID: quote.QuoteID,
			Reason: "operator fee cap is unset: " +
				"refusing to sign",
		}
	}
	if quote.OperatorFeeSat > feeCap {
		return &QuoteRejected{
			RoundID: roundID,
			QuoteID: quote.QuoteID,
			Reason: fmt.Sprintf(
				"operator fee %d exceeds cap %d",
				quote.OperatorFeeSat, feeCap,
			),
		}
	}

	if reason, ok := validateQuoteEchoes(intents, quote); !ok {
		return &QuoteRejected{
			RoundID: roundID,
			QuoteID: quote.QuoteID,
			Reason:  reason,
		}
	}

	return &QuoteAccepted{
		RoundID: roundID,
		QuoteID: quote.QuoteID,
	}
}

// validateQuoteEchoes cross-checks that the server's per-output
// quote entries preserve the intent's fixed-output layout. It
// enforces positional length parity, pkScript / recipient-key echo
// equality, and non-change amount equality; deviation is permitted
// only on the single IsChange=true output across both slices.
// Returns a diagnostic reason and ok=false on first mismatch.
func validateQuoteEchoes(intents Intents,
	quote *ClientQuote) (string, bool) {

	if len(quote.VTXOQuotes) != len(intents.VTXOs) {
		return fmt.Sprintf(
			"quote vtxo entries %d != intent vtxos %d",
			len(quote.VTXOQuotes), len(intents.VTXOs),
		), false
	}
	if len(quote.LeaveQuotes) != len(intents.Leaves) {
		return fmt.Sprintf(
			"quote leave entries %d != intent leaves %d",
			len(quote.LeaveQuotes), len(intents.Leaves),
		), false
	}

	for i := range intents.VTXOs {
		vtxoReq := intents.VTXOs[i]
		entry := quote.VTXOQuotes[i]

		intentScript, err := vtxoReq.EffectivePkScript()
		if err != nil {
			return fmt.Sprintf(
				"vtxo[%d] pkScript derivation: %v",
				i, err,
			), false
		}
		if !bytes.Equal(entry.PkScript, intentScript) {
			return fmt.Sprintf(
				"vtxo[%d] pkScript echo mismatch", i,
			), false
		}

		var intentKey []byte
		if vtxoReq.SigningKey.PubKey != nil {
			intentKey = vtxoReq.SigningKey.PubKey.
				SerializeCompressed()
		}
		if !bytes.Equal(entry.RecipientKey, intentKey) {
			return fmt.Sprintf(
				"vtxo[%d] recipient key echo mismatch", i,
			), false
		}

		if !vtxoReq.IsChange &&
			entry.AmountSat != int64(vtxoReq.Amount) {

			return fmt.Sprintf(
				"vtxo[%d] non-change amount %d != "+
					"intent target %d",
				i, entry.AmountSat, int64(vtxoReq.Amount),
			), false
		}
	}

	for i := range intents.Leaves {
		leaveReq := intents.Leaves[i]
		entry := quote.LeaveQuotes[i]

		if leaveReq == nil || leaveReq.Output == nil {
			return fmt.Sprintf(
				"leave[%d] intent missing output", i,
			), false
		}
		if !bytes.Equal(entry.PkScript, leaveReq.Output.PkScript) {
			return fmt.Sprintf(
				"leave[%d] pkScript echo mismatch", i,
			), false
		}

		if !leaveReq.IsChange &&
			entry.AmountSat != leaveReq.Output.Value {

			return fmt.Sprintf(
				"leave[%d] non-change amount %d != "+
					"intent target %d",
				i, entry.AmountSat, leaveReq.Output.Value,
			), false
		}
	}

	return "", true
}

// ProcessEvent for QuoteReceivedState.
func (s *QuoteReceivedState) ProcessEvent(
	ctx context.Context, event ClientEvent, env *ClientEnvironment,
) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *JoinRoundQuoteReceived:
		// Server reseal: a later pass arrives while we still
		// hold the prior pass's quote. The spec (#270) allows
		// reseals with a higher seal_pass_number; replace the
		// in-state quote and re-evaluate. A lower / equal
		// SealPass is a stale redelivery we drop silently, same
		// as the existing default-branch behavior.
		currentPass := uint32(0)
		if s.Quote != nil {
			currentPass = s.Quote.SealPass
		}
		if evt.Quote == nil || evt.Quote.SealPass <= currentPass {
			return selfLoop(s), nil
		}

		env.Log.InfoS(ctx, "Received reseal quote",
			slog.String("round_id", evt.RoundID.String()),
			slog.Int64("operator_fee_sat",
				evt.Quote.OperatorFeeSat),
			slog.Uint64("prev_seal_pass",
				uint64(currentPass)),
			slog.Uint64("seal_pass",
				uint64(evt.Quote.SealPass)))

		nextState := &QuoteReceivedState{
			RoundID: evt.RoundID,
			Quote:   evt.Quote,
			Intents: s.Intents.Clone(),
		}

		decision := evaluateQuote(
			env, evt.RoundID, s.Intents, evt.Quote,
		)

		return &ClientStateTransition{
			NextState: nextState,
			NewEvents: fn.Some(ClientEmittedEvent{
				InternalEvent: []ClientEvent{decision},
			}),
		}, nil

	case *QuoteAccepted:
		env.Log.InfoS(ctx, "Accepting seal-time quote",
			slog.String("round_id", evt.RoundID.String()),
			slog.Int64("operator_fee_sat", s.Quote.OperatorFeeSat))

		accept := &JoinRoundAcceptOutbox{
			RoundID: evt.RoundID,
			QuoteID: evt.QuoteID,
		}

		// Capture the server-authoritative leave amounts onto
		// the intents so confirmation-time accounting
		// (computeClientOperatorFee → VTXOCreatedNotification.
		// OperatorFeeSat) uses the quoted residual rather than
		// the pre-fee intent target. Without this the emitted
		// fee on mixed refresh+leave rounds would understate
		// the operator's take by the leave-side residual.
		intents := s.Intents.Clone()
		if s.Quote != nil && len(s.Quote.LeaveQuotes) > 0 {
			amounts := make([]int64, len(s.Quote.LeaveQuotes))
			for i := range s.Quote.LeaveQuotes {
				amounts[i] = s.Quote.LeaveQuotes[i].AmountSat
			}
			intents.QuotedLeaveAmounts = amounts
		}

		return &ClientStateTransition{
			NextState: &RoundJoinedState{
				RoundID: evt.RoundID,
				Intents: intents,
				Quote:   s.Quote,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: []ClientOutMsg{accept},
			}),
		}, nil

	case *QuoteRejected:
		env.Log.WarnS(ctx, "Rejecting seal-time quote", nil,
			slog.String("round_id", evt.RoundID.String()),
			slog.String("reason", evt.Reason))

		reject := &JoinRoundRejectOutbox{
			RoundID: evt.RoundID,
			QuoteID: evt.QuoteID,
			Reason:  evt.Reason,
		}

		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      evt.Reason,
				Recoverable: false,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: []ClientOutMsg{reject},
			}),
		}, nil

	default:
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
				Quote:         s.Quote,
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
// server has properly included the requested on-chain exit outputs. The
// expectedAmounts slice is positional against leaves; when non-nil it
// supplies the per-leave expected on-chain value instead of the intent's
// target (the server is the amount authority under seal-time fees).
func validateLeaveOutputs(
	commitmentTx *wire.MsgTx, leaves []*types.LeaveRequest,
	expectedAmounts []int64,
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
	for i, leave := range leaves {
		value := leave.Output.Value
		if expectedAmounts != nil && i < len(expectedAmounts) {
			value = expectedAmounts[i]
		}

		key := leaveOutput{
			value:    value,
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

// quoteVTXOAmount returns the expected VTXO leaf amount for the
// intent at position i. When the state carries a server-issued
// quote, evaluateQuote has already validated that len(VTXOQuotes)
// == len(Intents.VTXOs), so the positional lookup is safe; the
// intent target is returned only for harness paths that bypass the
// seal-time handshake (quote == nil).
func quoteVTXOAmount(quote *ClientQuote, i int,
	vtxoReq types.VTXORequest) btcutil.Amount {

	if quote == nil {
		return vtxoReq.Amount
	}

	return btcutil.Amount(quote.VTXOQuotes[i].AmountSat)
}

// quoteLeaveAmounts returns a positional slice of expected leave
// output values. Nil indicates "use intent targets" (pre-quote
// harness paths); otherwise entry i is the quote's per-leave value.
// evaluateQuote enforces len(LeaveQuotes) == len(leaves) upstream,
// so the positional lookup is safe.
func quoteLeaveAmounts(quote *ClientQuote,
	leaves []*types.LeaveRequest) []int64 {

	if quote == nil {
		return nil
	}

	out := make([]int64, len(leaves))
	for i := range leaves {
		out[i] = quote.LeaveQuotes[i].AmountSat
	}

	return out
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
		// correct value and script. When the client accepted a
		// seal-time quote, the server is the amount authority — the
		// per-leave expected value comes from Quote.LeaveAmounts
		// (positional) rather than the intent's target amount, which
		// was only a hint at seal time.
		if len(s.Intents.Leaves) > 0 {
			leaveAmounts := quoteLeaveAmounts(s.Quote, s.Intents.Leaves)
			if err := validateLeaveOutputs(
				s.CommitmentTx.UnsignedTx, s.Intents.Leaves,
				leaveAmounts,
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
			pkScript, err := vtxoReq.EffectivePkScript()
			if err != nil {
				return &ClientStateTransition{
					NextState: &ClientFailedState{
						Reason: "VTXT validation failed",
						Error: fmt.Errorf("derive pkScript for "+
							"VTXO request %d: %w", i, err),
						Recoverable: true,
					},
				}, nil
			}

			// The quote (when present) is the authoritative source
			// for the amount each VTXO leaf carries — the client's
			// intent target is a hint rather than a commitment
			// under seal-time fees. Fall back to the intent target
			// for harness paths that bypass the quote handshake.
			expectedAmount := quoteVTXOAmount(s.Quote, i, vtxoReq)

			// Convert VTXORequest to LeafDescriptor for validation.
			expectedLeaf := tree.LeafDescriptor{
				Amount:      expectedAmount,
				PkScript:    pkScript,
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
				// The error is carried into the failed state;
				// the FSM does not raise it as a Go error.
				return &ClientStateTransition{ //nolint:nilerr
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
				// Error carried into failed state.
				return &ClientStateTransition{ //nolint:nilerr
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
			forfeitReqs := forfeitRequestMap(
				s.Intents.Forfeits,
			)
			var outbox []ClientOutMsg
			for vtxoOutpoint, info := range s.ForfeitMappings {
				connOut := info.ConnectorOutpoint
				connScript := info.ConnectorPkScript
				connAmt := info.ConnectorAmount
				forfeitScript := env.OperatorTerms.ForfeitScript
				roundIDStr := s.RoundID.String()
				req := forfeitReqs[vtxoOutpoint]

				msg := &ForfeitRequestToVTXO{
					VTXOOutpoint:          vtxoOutpoint,
					RoundID:               roundIDStr,
					ConnectorOutpoint:     connOut,
					ConnectorPkScript:     connScript,
					ConnectorAmount:       connAmt,
					ServerForfeitPkScript: forfeitScript,
					ForfeitSpend:          req.ForfeitSpend,
				}
				outbox = append(outbox, msg)
			}

			outbox = append(outbox, &StartTimeoutReq{
				RoundID:  s.RoundID,
				Phase:    TimeoutPhaseForfeitCollection,
				Duration: env.ForfeitCollectionTimeout,
			})

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

		forfeitReqs := forfeitRequestMap(s.Intents.Forfeits)
		req := forfeitReqs[evt.VTXOOutpoint]

		// Validate the forfeit transaction structure using lib/tx. The
		// VTXOAmount check ensures the penalty output equals the
		// forfeited VTXO value, preventing value theft.
		params := tx.ForfeitTxParams{
			VTXOOutpoint:        evt.VTXOOutpoint,
			ConnectorOutpoint:   connectorInfo.ConnectorOutpoint,
			ServerForfeitScript: env.OperatorTerms.ForfeitScript,
			ExpectedAmount:      connectorInfo.VTXOAmount,
			ExpectedSequence:    expectedForfeitSequence(req),
			ExpectedLockTime:    expectedForfeitLockTime(req),
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
			return s.waitForMoreForfeitSignatures(
				updatedForfeits,
			), nil
		}

		// All forfeit signatures collected! Build the submission.
		return s.finishForfeitCollection(ctx, env, updatedForfeits)

	case *BoardingFailed:
		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: cancelForfeitTimeout(
					s.RoundID,
				),
			}),
		}, nil

	case *ForfeitCollectionTimedOut:
		// Ignore stale timeout events for other rounds. Timeouts are
		// routed by RoundID, but this guard preserves FSM safety.
		if evt.RoundID != s.RoundID {
			return selfLoop(s), nil
		}

		collectedCount := len(s.CollectedForfeits)
		expectedCount := len(s.ExpectedForfeits)
		reason := fmt.Sprintf("forfeit signature collection timeout: "+
			"collected %d/%d", collectedCount, expectedCount)

		env.Log.WarnS(ctx, "Forfeit signature collection timed out", nil,
			slog.String("round_id", s.RoundID.String()),
			slog.Int("collected_forfeits", collectedCount),
			slog.Int("expected_forfeits", expectedCount))

		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      reason,
				Error:       fmt.Errorf("%s", reason),
				Recoverable: true,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: cancelForfeitTimeout(
					s.RoundID,
				),
			}),
		}, nil

	default:
		// Self-loop on unknown events - do not halt the FSM.
		return selfLoop(s), nil
	}
}

func (s *ForfeitSignaturesCollectingState) waitForMoreForfeitSignatures(
	collectedForfeits map[wire.OutPoint]*ForfeitSignatureResponse,
) *ClientStateTransition {

	return &ClientStateTransition{
		NextState: &ForfeitSignaturesCollectingState{
			RoundID:              s.RoundID,
			CommitmentTx:         s.CommitmentTx,
			VTXOTreePaths:        s.VTXOTreePaths,
			Intents:              s.Intents.Clone(),
			ClientTrees:          s.ClientTrees,
			BoardingInputIndices: s.BoardingInputIndices,
			ExpectedForfeits:     s.ExpectedForfeits,
			CollectedForfeits:    collectedForfeits,
		},
	}
}

func (s *ForfeitSignaturesCollectingState) finishForfeitCollection(
	ctx context.Context, env *ClientEnvironment,
	collectedForfeits map[wire.OutPoint]*ForfeitSignatureResponse,
) (*ClientStateTransition, error) {

	forfeitTxs := make(map[wire.OutPoint]*types.ForfeitTxSig)
	forfeitedVTXOs := make([]wire.OutPoint, 0, len(collectedForfeits))
	for outpoint, resp := range collectedForfeits {
		forfeitTxs[outpoint] = &types.ForfeitTxSig{
			UnsignedTx:    resp.ForfeitTx,
			ClientVTXOSig: resp.Signature,
			SpendPath:     resp.SpendPath,
		}
		forfeitedVTXOs = append(forfeitedVTXOs, outpoint)
	}

	env.Log.InfoS(ctx, "All forfeit signatures collected, signing boarding inputs",
		slog.String("round_id", s.RoundID.String()),
		slog.Int("forfeit_count", len(forfeitedVTXOs)),
		slog.Int("boarding_intent_count", len(s.Intents.Boarding)))

	boardingInputSigs, err := signBoardingInputs(
		env.Wallet, s.CommitmentTx, s.Intents, s.BoardingInputIndices,
	)
	if err != nil {
		return nil, fmt.Errorf("sign boarding inputs: %w", err)
	}

	outboxMsgs := s.forfeitCollectionOutbox(
		env, forfeitTxs, boardingInputSigs,
	)
	round := s.checkpointRound(env.StartHeight)
	nextState := s.inputSigSentState(boardingInputSigs, forfeitedVTXOs)

	// Checkpointing may outlive the triggering actor request, so
	// use a detached context for the local store write.
	opCtx := context.WithoutCancel(ctx)
	err = env.RoundStore.CommitState(opCtx, round, nextState)
	if err != nil {
		return nil, fmt.Errorf("failed to commit round state: %w", err)
	}

	env.Log.InfoS(ctx, "Round state checkpointed with forfeit signatures",
		slog.String("round_id", s.RoundID.String()),
		slog.Int("boarding_sig_count", len(boardingInputSigs)),
		slog.Int("forfeit_sig_count", len(forfeitTxs)))

	checkpointNotify := &RoundCheckpointedNotification{
		RoundID: s.RoundID,
	}

	return &ClientStateTransition{
		NextState: nextState,
		NewEvents: fn.Some(ClientEmittedEvent{
			Outbox: append(outboxMsgs, checkpointNotify),
		}),
	}, nil
}

func (s *ForfeitSignaturesCollectingState) forfeitCollectionOutbox(
	env *ClientEnvironment,
	forfeitTxs map[wire.OutPoint]*types.ForfeitTxSig,
	boardingInputSigs []*types.BoardingInputSignature,
) []ClientOutMsg {

	txid := s.CommitmentTx.UnsignedTx.TxHash()
	callerID := fmt.Sprintf("commitment-%s", txid.String())

	var pkScript []byte
	if len(s.CommitmentTx.UnsignedTx.TxOut) > 0 {
		pkScript = s.CommitmentTx.UnsignedTx.TxOut[0].PkScript
	}

	outboxMsgs := []ClientOutMsg{
		&CancelTimeoutReq{
			RoundID: s.RoundID,
			Phase:   TimeoutPhaseForfeitCollection,
		},
		&SubmitVTXOForfeitSigsToServer{
			RoundID:    s.RoundID,
			ForfeitTxs: forfeitTxs,
		},
		&RegisterConfirmationRequest{
			CallerID:    callerID,
			Txid:        &txid,
			PkScript:    pkScript,
			TargetConfs: env.OperatorTerms.MinConfirmations,
			HeightHint:  env.StartHeight,
		},
	}

	if len(boardingInputSigs) == 0 {
		return outboxMsgs
	}

	return append(
		outboxMsgs[:2],
		append([]ClientOutMsg{
			&SubmitForfeitSigRequest{
				RoundID:    s.RoundID,
				Signatures: boardingInputSigs,
			},
		}, outboxMsgs[2:]...)...,
	)
}

func (s *ForfeitSignaturesCollectingState) checkpointRound(
	startHeight uint32,
) *Round {

	intents := s.Intents.Clone()
	for i := range intents.Boarding {
		intents.Boarding[i].Status = BoardingStatusAdopted
	}

	return &Round{
		RoundID:       s.RoundID,
		StartHeight:   startHeight,
		CommitmentTx:  fn.Some(s.CommitmentTx),
		VTXOTreePaths: fn.Some(s.VTXOTreePaths),
		Intents:       intents,
	}
}

func (s *ForfeitSignaturesCollectingState) inputSigSentState(
	boardingInputSigs []*types.BoardingInputSignature,
	forfeitedVTXOs []wire.OutPoint,
) *InputSigSentState {

	return &InputSigSentState{
		RoundID:        s.RoundID,
		CommitmentTx:   s.CommitmentTx,
		VTXOTreePaths:  s.VTXOTreePaths,
		Intents:        s.Intents.Clone(),
		ClientTrees:    s.ClientTrees,
		InputSigs:      boardingInputSigs,
		ForfeitedVTXOs: forfeitedVTXOs,
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
				// Error carried into failed state.
				return &ClientStateTransition{ //nolint:nilerr
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

		// Propagate validated signatures to client sub-trees.
		// ClientTrees were extracted before signing (in
		// CommitmentTxReceivedState) via ExtractPathForCoSigners
		// which creates new node objects. Those copies don't have
		// the aggregated signatures that were just submitted to
		// VTXOTreePaths. We submit the same signatures to ensure
		// persisted client trees contain valid signatures for
		// unilateral exit (unrolling).
		for _, clientTree := range s.ClientTrees {
			if err := clientTree.SubmitTreeSigs(
				evt.AggSigs,
			); err != nil {
				return failWithNotification(
					"failed to propagate sigs "+
						"to client tree",
					err, false,
					fn.Some(s.RoundID),
				), nil
			}

			if err := clientTree.VerifySigned(); err != nil {
				return failWithNotification(
					"client tree sig "+
						"verification failed",
					err, false,
					fn.Some(s.RoundID),
				), nil
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
		// Checkpointing may outlive the triggering actor request, so
		// use a detached context for the local store write.
		opCtx := context.WithoutCancel(ctx)

		err = env.RoundStore.CommitState(opCtx, round, nextState)
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
	forfeitReqs := forfeitRequestMap(s.Intents.Forfeits)
	var outbox []ClientOutMsg
	for vtxoOutpoint, info := range s.ForfeitMappings {
		req := forfeitReqs[vtxoOutpoint]
		msg := &ForfeitRequestToVTXO{
			VTXOOutpoint:          vtxoOutpoint,
			RoundID:               s.RoundID.String(),
			ConnectorOutpoint:     info.ConnectorOutpoint,
			ConnectorPkScript:     info.ConnectorPkScript,
			ConnectorAmount:       info.ConnectorAmount,
			ServerForfeitPkScript: env.OperatorTerms.ForfeitScript,
			ForfeitSpend:          req.ForfeitSpend,
		}
		outbox = append(outbox, msg)
	}

	outbox = append(outbox, &StartTimeoutReq{
		RoundID:  s.RoundID,
		Phase:    TimeoutPhaseForfeitCollection,
		Duration: env.ForfeitCollectionTimeout,
	})

	env.Log.InfoS(ctx, "Transitioning to forfeit collection",
		slog.String("round_id", s.RoundID.String()),
		slog.Int("forfeit_count", len(s.ForfeitMappings)),
		slog.Duration("forfeit_timeout", env.ForfeitCollectionTimeout))

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

// forfeitRequestMap indexes local forfeit requests by outpoint so custom local
// spend metadata can be recovered when building actor messages later.
func forfeitRequestMap(
	requests []types.ForfeitRequest,
) map[wire.OutPoint]types.ForfeitRequest {

	indexed := make(map[wire.OutPoint]types.ForfeitRequest, len(requests))
	for i := 0; i < len(requests); i++ {
		if requests[i].VTXOOutpoint == nil {
			continue
		}

		indexed[*requests[i].VTXOOutpoint] = requests[i]
	}

	return indexed
}

// expectedForfeitSequence returns the tx sequence expected for a forfeit input.
func expectedForfeitSequence(req types.ForfeitRequest) uint32 {
	if req.ForfeitSpend == nil || req.ForfeitSpend.SpendInfo == nil {
		return 0
	}

	return req.ForfeitSpend.RequiredSequence
}

// expectedForfeitLockTime returns the tx locktime expected for a forfeit.
func expectedForfeitLockTime(req types.ForfeitRequest) uint32 {
	if req.ForfeitSpend == nil || req.ForfeitSpend.SpendInfo == nil {
		return 0
	}

	return req.ForfeitSpend.RequiredLockTime
}

// ensureVTXOSigningKeys fills missing VTXO signing keys by deriving fresh
// keys from the wallet. Existing signing keys are preserved.
func ensureVTXOSigningKeys(ctx context.Context, wallet ClientWallet,
	vtxoReqs []types.VTXORequest) ([]types.VTXORequest, error) {

	updated := slices.Clone(vtxoReqs)
	for i := range updated {
		if updated[i].SigningKey.PubKey != nil {
			continue
		}

		keyDesc, err := wallet.DeriveNextKey(
			ctx, keychain.KeyFamilyMultiSig,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"derive signing key for vtxo %d: %w",
				i, err,
			)
		}

		updated[i].SigningKey = *keyDesc
	}

	return updated, nil
}

// computeClientOperatorFee derives the per-client operator fee
// contributed to this round. Under the Ark round model the
// client's fee equals the difference between its contributed
// input value (boarding inputs + forfeited VTXOs) and its
// received output value (locally owned round VTXOs + cooperative
// leave outputs). Every other source of imbalance on the
// commitment transaction belongs to counterparties, not to this
// client, so they do not enter the fee math.
//
// Returns zero if the difference is not strictly positive. A
// zero or negative result means the client did not pay the
// operator in this round (pure receive round, or fully
// remote-funded directed-send recipient slot), and the caller
// suppresses FeePaidMsg emission accordingly.
//
// The fee number is used by the round actor to emit one
// FeePaidMsg per round so the ledger's total_fees_paid_sat
// stays consistent with the actual operator revenue the client
// paid. Boarding fees are deliberately not split out yet -- the
// emission path labels every fee as FeeTypeRefresh under task
// B's scope, and a future PR classifies by round composition.
func computeClientOperatorFee(intents Intents,
	ownedVTXOs []*ClientVTXO) int64 {

	var inputsSat int64

	for i := range intents.Boarding {
		// ChainInfo.Amount is always populated since a
		// BoardingIntent is only constructed after its UTXO
		// confirms; the zero guard is defensive only.
		amt := int64(intents.Boarding[i].ChainInfo.Amount)
		if amt > 0 {
			inputsSat += amt
		}
	}

	for i := range intents.Forfeits {
		// ForfeitRequest.Amount is the canonical local value
		// hint the wallet populates at intent-build time. The
		// comment on types.ForfeitRequest notes that the
		// wire-authoritative source is the VTXOStore lookup,
		// but at confirmation time the pool already reflects
		// the consumed values, so the hint is sufficient for
		// the fee math without a second store roundtrip.
		amt := int64(intents.Forfeits[i].Amount)
		if amt > 0 {
			inputsSat += amt
		}
	}

	var outputsSat int64

	for _, v := range ownedVTXOs {
		if v != nil {
			outputsSat += int64(v.Amount)
		}
	}

	for i := range intents.Leaves {
		amt := intents.LeaveAmount(i)
		if amt > 0 {
			outputsSat += amt
		}
	}

	fee := inputsSat - outputsSat
	if fee <= 0 {
		return 0
	}

	return fee
}

// leafNonAnchorAmount returns the value of the non-anchor output of
// a leaf Node. The leaf's tx has exactly two outputs: the VTXO (or
// connector) output and the P2A anchor. Returns the VTXO/connector
// amount, which under the #270 handshake reflects the server's
// seal-time quote residual — authoritative for local persistence.
func leafNonAnchorAmount(leaf *tree.Node) (btcutil.Amount, error) {
	if leaf == nil {
		return 0, fmt.Errorf("nil leaf node")
	}

	anchorScript := arkscript.AnchorOutput().PkScript
	for _, out := range leaf.Outputs {
		if !bytes.Equal(out.PkScript, anchorScript) {
			return btcutil.Amount(out.Value), nil
		}
	}

	return 0, fmt.Errorf("no non-anchor output found in leaf node")
}

// buildClientVTXOs constructs locally owned ClientVTXO instances from the
// intents and client trees. Requests that do not resolve to locally owned
// pkScripts are skipped: the client still co-signs their tree path, but it
// must not persist them as spendable local balance.
func buildClientVTXOs(ctx context.Context, checker OwnedScriptChecker,
	intents Intents, trees map[SignerKey]*tree.Tree,
	roundID RoundID) ([]*ClientVTXO, error) {

	vtxos := make([]*ClientVTXO, 0)
	for _, req := range intents.VTXOs {
		if !req.HasLocalOwner() {
			continue
		}

		pkScript, err := req.EffectivePkScript()
		if err != nil {
			return nil, fmt.Errorf("derive VTXO pkScript: %w", err)
		}

		if checker != nil {
			owned, err := checker.IsOwnedScript(
				ctx, pkScript,
			).Unpack()
			if err != nil {
				return nil, fmt.Errorf("check owned "+
					"script: %w", err)
			}

			if !owned {
				continue
			}
		}

		params, err := req.DecodeStandardPolicyTemplate()
		isStandard := err == nil

		signerKey := NewSignerKey(req.SigningKey.PubKey)
		clientTree := trees[signerKey]
		if clientTree == nil {
			return nil, fmt.Errorf("missing client tree " +
				"for signing key")
		}

		leaves := clientTree.Root.GetLeafNodes()
		if len(leaves) != 1 {
			return nil, fmt.Errorf("expected exactly "+
				"1 leaf for signing key, got %d",
				len(leaves))
		}
		leaf := leaves[0]

		outpoint, err := leaf.GetNonAnchorOutpoint()
		if err != nil {
			return nil, fmt.Errorf("failed to "+
				"derive VTXO outpoint: %w", err)
		}

		// The on-chain tx output is the source of truth for the
		// VTXO amount — under #270 the server stamps the seal-time
		// residual onto the VTXODescriptor before building the
		// tree, so the leaf's non-anchor output carries the quoted
		// value rather than the intent target (which is zero for
		// change outputs). Reading req.Amount here would persist
		// stale data.
		leafAmount, err := leafNonAnchorAmount(leaf)
		if err != nil {
			return nil, fmt.Errorf("derive leaf amount: %w", err)
		}

		policyTemplate, _ := req.EffectivePolicyTemplate()

		vtxo := &ClientVTXO{
			Outpoint:       *outpoint,
			Amount:         leafAmount,
			PolicyTemplate: policyTemplate,
			PkScript:       pkScript,
			OwnerKey:       req.OwnerKey,
			TreePath:       clientTree,
			RoundID:        fn.Some(roundID),
			Origin:         req.Origin,
		}
		if isStandard {
			vtxo.Expiry = params.ExitDelay
			vtxo.OperatorKey = params.OperatorKey
		}

		vtxos = append(vtxos, vtxo)
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
			ctx, env.OwnedScriptChecker,
			s.Intents, s.ClientTrees, s.RoundID,
		)
		if err != nil {
			// Error carried into failed state.
			return &ClientStateTransition{ //nolint:nilerr
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

		// Compute batch expiry as absolute block height.
		sweepDelay := int32(env.OperatorTerms.SweepDelay)
		batchExpiry := evt.BlockHeight + sweepDelay

		// Fill in round metadata so VTXOs are complete from the
		// first write. This avoids a race where callers read the
		// VTXO before the VTXO manager's second upsert populates
		// these fields.
		for _, cv := range vtxos {
			cv.CommitmentTxID = evt.TxID
			cv.BatchExpiry = batchExpiry
			cv.CreatedHeight = evt.BlockHeight
		}

		if len(vtxos) > 0 {
			// Persist VTXOs with their extracted tree paths for
			// future spending. Confirmation handling may outlive
			// the triggering actor request, so use a detached
			// context for the store write.
			opCtx := context.WithoutCancel(ctx)

			if err := env.VTXOStore.SaveVTXOs(
				opCtx, vtxos,
			); err != nil {
				return nil, fmt.Errorf(
					"failed to save VTXOs: %w", err,
				)
			}

			env.Log.InfoS(ctx,
				"Saved owned VTXOs to store, round complete",
				slog.String("round_id", s.RoundID.String()),
				slog.Int("vtxo_count", len(vtxos)))
		}

		confInfo := ConfInfo{
			Height:    evt.BlockHeight,
			BlockHash: evt.BlockHash,
		}

		// Build outbox messages starting with standard notifications.
		outbox := make([]ClientOutMsg, 0, 2)
		if len(vtxos) > 0 {
			operatorFee := computeClientOperatorFee(
				s.Intents, vtxos,
			)
			outbox = append(outbox, &VTXOCreatedNotification{
				VTXOs:          vtxos,
				RoundID:        s.RoundID.String(),
				CommitmentTxID: evt.TxID,
				BatchExpiry:    batchExpiry,
				CreatedHeight:  evt.BlockHeight,
				OperatorFeeSat: operatorFee,
			})
		}
		outbox = append(outbox, &RoundCompletedNotification{
			RoundID:  s.RoundID,
			TxID:     evt.TxID,
			ConfInfo: confInfo,
		})

		// If this round included refresh requests, notify the old VTXO
		// actors that their forfeit is now confirmed. This allows them
		// to transition to the terminal Forfeited state.
		for _, vtxoOutpoint := range s.ForfeitedVTXOs {
			outbox = append(outbox, &ForfeitConfirmedToVTXO{
				VTXOOutpoint:   vtxoOutpoint,
				CommitmentTxID: evt.TxID,
				BlockHeight:    evt.BlockHeight,
			})
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
				Outbox: outbox,
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

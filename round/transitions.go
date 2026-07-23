package round

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/batchcanon"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/tx"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// buildBoardingRequest constructs a types.BoardingRequest from a
// BoardingIntent.
func buildBoardingRequest(intent BoardingIntent) types.BoardingRequest {
	return intent.Request
}

// cleanupSignerSessions releases all transaction-level MuSig2 sessions. It
// attempts every signer path so one cleanup failure cannot strand the rest.
func cleanupSignerSessions(sessions map[SignerKey]*tree.SignerSession) error {
	cleanupErrors := make([]error, 0)
	for signerKey, session := range sessions {
		if session == nil {
			continue
		}

		err := session.Cleanup()
		if err != nil {
			cleanupErrors = append(
				cleanupErrors, fmt.Errorf("clean up signer "+
					"%x: %w", signerKey[:], err),
			)
		}
	}

	return errors.Join(cleanupErrors...)
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

// releaseForfeitsOnFailure prepends rollback messages to a transition that
// lands in ClientFailedState, returning standard forfeit-reserved VTXOs to
// LiveState and dropping temporary custom forfeit actors so they are not
// stranded after a failed round. It is a no-op for any non-failure transition,
// for rounds that reserved no forfeits, and when a rollback was already
// emitted (so the IntentSentState admission-timeout path, which releases
// explicitly, is not double-counted).
//
// When the wrapped handler returns a raw (nil, err) — a pre-signing validation
// failure such as a forfeit-mapping or forfeit-tx construction error — there is
// no transition for the FSM to fail on, so the engine would tear the state
// machine down without ever emitting a ClientFailedState. That strands the
// forfeit-reserved inputs in pending-forfeit. To avoid this, an inner error is
// converted into a synthetic failure transition (mirroring
// failWithNotification) so the round fails cleanly, the failure stays
// observable, and the release still fires. The caller drops the now-handled
// error.
//
// Rolling back is unconditionally safe BEFORE the client submits its VTXO
// forfeit signatures to the server (SubmitVTXOForfeitSigsToServer, emitted on
// the ForfeitSignaturesCollectingState -> InputSigSentState transition): until
// that point the server holds no forfeit signature and cannot broadcast a
// forfeit, so returning the inputs to LiveState cannot double-spend. The
// pre-signing states (PendingRoundAssembly through
// ForfeitSignaturesCollectingState) therefore wire this into every failure.
// Past that point the wrapper has exactly one caller: InputSigSentState's
// dead-answer path (wavelength#844), where a RoundStatusReported carrying the
// operator's authoritative dead verdict proves the round never finalized, its
// commitment can never confirm, and the forfeit signatures the operator may
// hold are unspendable, restoring the same cannot-double-spend invariant. No
// post-signing failure releases without that verdict.
//
// Rollback messages are prepended (not appended) so they are the first items
// processOutbox dispatches. The local vtxo-manager rollbacks are handled
// best-effort (log-and-continue) and never return an error, whereas the
// server/timeout Tells that may already be in the outbox
// (JoinRoundRejectOutbox, CancelTimeoutReq) can fail mid-flight (TCP RST,
// mailbox reconnect) and short-circuit the rest of the outbox. Dispatching
// local rollback first guarantees the VTXOs are returned to a clean local state
// regardless of whether those Tells succeed.
func releaseForfeitsOnFailure(transition *ClientStateTransition, err error,
	roundID fn.Option[RoundID],
	forfeits []types.ForfeitRequest) (*ClientStateTransition, error) {

	rollback := rollbackOutbox(forfeits)

	// A raw inner error has no transition for the FSM to land on;
	// synthesize a failure transition so the release below has somewhere to
	// attach, and drop the now-handled error so the engine does not tear
	// the FSM down. We only do this when there are forfeits to release:
	// with none, letting the error propagate is the established behavior.
	if transition == nil {
		if err == nil || len(rollback) == 0 {
			return transition, err
		}

		transition = failWithNotification(
			err.Error(), err, false, roundID,
		)
		err = nil
	}

	// Only a transition into the terminal failure state can strand forfeit
	// reservations; leave every other transition untouched.
	failedState, failed := transition.NextState.(*ClientFailedState)
	if !failed {
		return transition, err
	}

	if len(rollback) == 0 {
		return transition, err
	}

	emitted := transition.NewEvents.UnwrapOr(ClientEmittedEvent{})

	// Some handlers (the admission timeout in IntentSentState, or any state
	// that fails through failureOutbox) already queued the rollback
	// themselves, and a few could in principle already carry the terminal
	// notification. Scan once for both so we neither release the same
	// inputs twice nor duplicate the drop, while keeping the two concerns
	// independent: the release returns the VTXOs to LiveState, the
	// notification retires the originating job, and a handler doing the
	// former must not suppress the latter.
	var alreadyReleased, alreadyNotified bool
	for _, msg := range emitted.Outbox {
		switch msg.(type) {
		case *ReleaseForfeitReservation, *DropCustomForfeitReservation:
			alreadyReleased = true

		case *TerminalJobFailedNotification:
			alreadyNotified = true
		}
	}

	if !alreadyReleased {
		emitted.Outbox = append(rollback, emitted.Outbox...)
	}

	// A terminal-for-job failure (e.g. the operator cannot fund the
	// commitment tx) must not sit in recoverable replay: the round is dead
	// and re-submitting these same inputs next start just hits the same
	// wall. Alongside the forfeit release (which returns the VTXOs to the
	// live set), emit a notification carrying the forfeited outpoints,
	// which are exactly the originating job's pending-intent anchors, so
	// the actor drops the persisted pending intent and surfaces the job as
	// failed. This is orthogonal to the release above: a handler that
	// already rolled back still needs the job retired, so we key only on
	// whether the drop is already present, not on whether we performed the
	// release. The release itself is safe in every state this wrapper
	// guards: unconditionally pre-signing, and in InputSigSentState only
	// behind the operator's dead verdict (see the function comment).
	if failedState.FailureCode.IsTerminalForJob() && !alreadyNotified {
		emitted.Outbox = append(emitted.Outbox,
			&TerminalJobFailedNotification{
				RoundID:          roundID,
				ForfeitOutpoints: forfeitOutpoints(forfeits),
				FailureCode:      failedState.FailureCode,
				Reason:           failedState.Reason,
			},
		)
	}

	transition.NewEvents = fn.Some(emitted)

	return transition, err
}

// forfeitOutpoints extracts the VTXO outpoints from a set of forfeit requests,
// skipping any with a nil outpoint. These outpoints are the pending-intent
// anchors of the job that reserved them, so the actor uses them to drop the
// job's persisted intent on a terminal failure.
func forfeitOutpoints(forfeits []types.ForfeitRequest) []wire.OutPoint {
	outpoints := make([]wire.OutPoint, 0, len(forfeits))
	for _, forfeit := range forfeits {
		if forfeit.VTXOOutpoint == nil {
			continue
		}

		outpoints = append(outpoints, *forfeit.VTXOOutpoint)
	}

	return outpoints
}

// rollbackOutbox builds local rollback messages for a round that failed before
// connector-bound forfeit signatures were sent.
func rollbackOutbox(forfeits []types.ForfeitRequest) []ClientOutMsg {
	standard, custom := forfeitRollbackOutpoints(forfeits)

	outbox := make([]ClientOutMsg, 0, 2)
	if len(standard) > 0 {
		outbox = append(outbox, &ReleaseForfeitReservation{
			Outpoints: standard,
		})
	}
	if len(custom) > 0 {
		outbox = append(outbox, &DropCustomForfeitReservation{
			Outpoints: custom,
		})
	}

	return outbox
}

// withFailureCode stamps a typed RoundFailureCode onto a transition's
// ClientFailedState when that is where it lands. BoardingFailed handlers that
// build their failed state through a helper (which defaults the code to
// RoundFailureUnknown) use this to carry evt.FailureCode onto the state
// without threading the code through every failure helper's signature. It is a
// no-op for a nil transition, an Unknown code, or a non-failure transition.
func withFailureCode(t *ClientStateTransition,
	code RoundFailureCode) *ClientStateTransition {

	if t == nil || code == RoundFailureUnknown {
		return t
	}

	if fs, ok := t.NextState.(*ClientFailedState); ok {
		fs.FailureCode = code
	}

	return t
}

// failureOutbox builds the common failure notification and rollback messages
// for a round that failed before forfeit signatures were sent.
func failureOutbox(reason string, err error, recoverable bool,
	roundID fn.Option[RoundID],
	forfeits []types.ForfeitRequest) []ClientOutMsg {

	outbox := []ClientOutMsg{
		&RoundFailedNotification{
			RoundID:       roundID,
			Reason:        reason,
			Recoverable:   recoverable,
			OriginalError: err,
		},
	}
	outbox = append(outbox, rollbackOutbox(forfeits)...)

	return outbox
}

// failBeforeForfeitSigning fails the round and emits rollback messages for
// every forfeit input that was reserved before connector-bound forfeit
// signatures were requested.
func failBeforeForfeitSigning(reason string, err error, recoverable bool,
	roundID RoundID,
	forfeits []types.ForfeitRequest) *ClientStateTransition {

	return &ClientStateTransition{
		NextState: &ClientFailedState{
			Reason:      reason,
			Error:       err,
			Recoverable: recoverable,
		},
		NewEvents: fn.Some(ClientEmittedEvent{
			Outbox: failureOutbox(
				reason, err, recoverable, fn.Some(roundID),
				forfeits,
			),
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
	intents Intents,
	boardingInputIndices map[wire.OutPoint]int) (
	[]*types.BoardingInputSignature, error) {

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
			return nil, fmt.Errorf("no input index found for "+
				"boarding outpoint %s", outpoint)
		}

		spendInfo, err := arkscript.NewVTXOSpendInfoFromPolicy(
			boardingIntent.Address.KeyDesc.PubKey,
			boardingIntent.Address.OperatorKey,
			boardingIntent.Address.ExitDelay, 0,
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
			&boardingIntent.Address.KeyDesc, output, sigHashes,
			prevOutFetcher,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to sign boarding input "+
				"%d: %w", inputIdx, err)
		}

		schnorrSig, ok := signature.(*schnorr.Signature)
		if !ok {
			return nil, fmt.Errorf("signature is not a schnorr " +
				"signature")
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
			"intent package", evt.logAttributes()...)

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
func (s *PendingRoundAssembly) ProcessEvent(ctx context.Context,
	event ClientEvent, env *ClientEnvironment) (*ClientStateTransition,
	error) {

	// Any transition into ClientFailedState from this pre-signing state
	// returns forfeit-reserved inputs to LiveState; see
	// releaseForfeitsOnFailure.
	transition, err := s.processEvent(ctx, event, env)

	return releaseForfeitsOnFailure(
		transition, err, fn.None[RoundID](), s.Forfeits,
	)
}

// processEvent runs the PendingRoundAssembly event handling; ProcessEvent wraps
// it to return forfeit-reserved inputs to LiveState on failure.
//
//nolint:funlen
func (s *PendingRoundAssembly) processEvent(ctx context.Context,
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
					fmt.Errorf("derive VTXO pkScript: %w",
						err),
					true,
					fn.None[RoundID](),
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
					fmt.Errorf("derive new VTXO "+
						"pkScript: %w", err),
					true,
					fn.None[RoundID](),
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
		env.Log.InfoS(
			ctx,
			"Registration requested, preparing to join round",
			slog.Int("boarding_intent_count", len(s.Boarding)),
			slog.Int("vtxo_intent_count", len(s.VTXOs)),
		)

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
				"failed to compute forfeit amount", err, true,
				fn.None[RoundID](),
			), nil
		}
		totalInput += forfeitAmt

		// Include leave amounts as requested on-chain outputs.
		for i, req := range s.Leaves {
			if req.Output == nil {
				return failWithNotification(
					"leave request has nil output",
					fmt.Errorf("leave request %d has "+
						"nil output", i),
					true,
					fn.None[RoundID](),
				), nil
			}

			totalOutput += btcutil.Amount(req.Output.Value)
		}

		// Validate that we have outputs to create.
		if totalOutput == 0 {
			return failWithNotification(
				"no VTXO output amount",
				fmt.Errorf("total VTXO output is zero"), true,
				fn.None[RoundID](),
			), nil
		}

		// Validate that outputs don't exceed inputs.
		if totalOutput > totalInput {
			return failWithNotification(
				"outputs exceed inputs",
				fmt.Errorf("total output (%d) exceeds total "+
					"input (%d)", totalOutput, totalInput),
				true,
				fn.None[RoundID](),
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
			btclog.Fmt("estimated_operator_fee", "%v", operatorFee),
		)

		// Extract the set of values from the intent map, as we don't
		// need to track them by outpoint any longer.
		boardingReqs := fn.Map(s.Boarding, buildBoardingRequest)
		vtxoReqs := slices.Clone(s.VTXOs)

		vtxoReqs, err = ensureVTXOSigningKeys(
			opCtx, env.Wallet, vtxoReqs,
		)
		if err != nil {
			return failWithNotification(
				"failed to derive vtxo signing keys", err, true,
				fn.None[RoundID](),
			), nil
		}

		// Build forfeit requests from the decoupled forfeit pool.
		forfeitReqs, err := sortedForfeitRequests(s.Forfeits)
		if err != nil {
			return failWithNotification(
				"invalid forfeit requests", err, true,
				fn.None[RoundID](),
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
			slog.Int("leave_requests", len(leaveReqs)),
		)

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
				"failed to derive join auth identifier", err,
				true, fn.None[RoundID](),
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
					fmt.Errorf("join auth: %w", err), true,
					fn.None[RoundID](),
				), nil
			}

			joinAuth = auth
		}

		// With all this extracted, we'll now send the
		// JoinRoundRequest to kick off the signing process.
		outbox := []ClientOutMsg{
			&JoinRoundRequest{
				BoardingRequests: boardingReqs,
				VTXORequests:     vtxoReqs,
				ForfeitRequests:  forfeitReqs,
				LeaveRequests:    leaveReqs,
				Identifier:       idPub,
				Auth:             joinAuth,
			},
		}

		// Arm the registration (admission) timeout so a server that
		// never returns a RoundJoined watermark cannot park us in
		// IntentSentState forever. On expiry the FSM fails the round
		// and releases any forfeit-reserved inputs (wavelength#653).
		// A non-positive timeout disables the safety net.
		if env.RegistrationTimeout > 0 {
			outbox = append(outbox, &StartTimeoutReq{
				RoundKey: env.RoundKey,
				Phase:    TimeoutPhaseRegistration,
				Duration: env.RegistrationTimeout,
			})
		}

		return &ClientStateTransition{
			NextState: &IntentSentState{
				Intents: intent,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: outbox,
			}),
		}, nil

	case *BoardingFailed:
		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
				FailureCode: evt.FailureCode,
			},
		}, nil

	default:
		// Self-loop on unknown events - do not halt the FSM.
		return selfLoop(s), nil
	}
}

// ProcessEvent for IntentSentState.
func (s *IntentSentState) ProcessEvent(ctx context.Context, event ClientEvent,
	env *ClientEnvironment) (*ClientStateTransition, error) {

	// Any transition into ClientFailedState from this pre-signing state
	// returns forfeit-reserved inputs to LiveState; see
	// releaseForfeitsOnFailure. Idempotent with the admission-timeout path,
	// which already releases explicitly.
	transition, err := s.processEvent(ctx, event, env)

	return releaseForfeitsOnFailure(
		transition, err, fn.Some(s.AdmittedRoundID), s.Intents.Forfeits,
	)
}

// processEvent runs the IntentSentState event handling; ProcessEvent wraps it
// to return forfeit-reserved inputs to LiveState on failure.
func (s *IntentSentState) processEvent(ctx context.Context, event ClientEvent,
	env *ClientEnvironment) (*ClientStateTransition, error) {

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
		//
		// Persist the admitted RoundID onto the next IntentSentState
		// so the quote and commitment handlers can cross-check the
		// server's claimed round identity (defense-in-depth against
		// future actor-routing regressions).
		env.Log.InfoS(ctx, "Intent admitted; awaiting seal-time quote",
			slog.String("round_id", evt.RoundID.String()),
			slog.Int(
				"boarding_intent_count",
				len(s.Intents.Boarding),
			),
			slog.Int("vtxo_intent_count", len(s.Intents.VTXOs)),
		)

		// Admission arrived, so cancel the registration timeout. The
		// post-admission wait for the seal-time quote is governed by
		// the quote's own expiry, not this watermark timer.
		cancelReg := []ClientOutMsg{
			&CancelTimeoutReq{
				RoundKey: env.RoundKey,
				Phase:    TimeoutPhaseRegistration,
			},
		}

		return &ClientStateTransition{
			NextState: &IntentSentState{
				Intents:         s.Intents.Clone(),
				AdmittedRoundID: evt.RoundID,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: cancelReg,
			}),
		}, nil

	case *RegistrationTimedOut:
		// Ignore a stale timeout once the round has been admitted.
		// Admission (RoundJoined) cancels the timer and re-keys the
		// round away from its temp key, so the actor layer already
		// drops a late TimeoutMsg whose composite id carries the temp
		// key. This guard is belt-and-suspenders for any in-flight
		// RegistrationTimedOut that raced past the cancel, and keeps
		// the invariant explicit at the FSM if the arming/re-key timing
		// ever changes: never abort a round we know the server
		// admitted.
		if s.AdmittedRoundID != (RoundID{}) {
			env.Log.DebugS(ctx, "Ignoring stale registration timeout; "+
				"round already admitted",
				slog.String(
					"round_id", s.AdmittedRoundID.String(),
				),
			)

			return selfLoop(s), nil
		}

		// The server never acknowledged our JoinRoundRequest within the
		// admission window. Fail the round as recoverable (the client
		// may retry) and release any forfeit-reserved inputs back to
		// LiveState so they are not stranded in pending-forfeit
		// (wavelength#653). Releasing is safe here: at this phase no
		// forfeit signatures have been produced or sent to the server,
		// so there is nothing to double-spend.
		const reason = "round admission timed out"
		timeoutErr := fmt.Errorf("server did not acknowledge join " +
			"request before registration timeout")

		env.Log.WarnS(ctx, "Round admission timed out; failing round "+
			"and releasing forfeit reservation", timeoutErr,
			slog.Int("forfeit_count", len(s.Intents.Forfeits)),
		)

		outbox := failureOutbox(
			reason, timeoutErr, true, fn.None[RoundID](),
			s.Intents.Forfeits,
		)

		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      reason,
				Error:       timeoutErr,
				Recoverable: true,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: outbox,
			}),
		}, nil

	case *JoinRoundQuoteReceived:
		// Under the #270 seal-time handshake the round will not
		// advance into batch-building until we explicitly accept
		// (or reject) the quote. Park in QuoteReceivedState so
		// the next event (QuoteAccepted/QuoteRejected, emitted
		// internally) drives the decision.
		//
		// Cross-check the quote's RoundID against the admitted
		// RoundID from the prior RoundJoined ack. A mismatch means
		// either the server sent a quote for a round we did not
		// admit to or the actor's routing landed the message on
		// the wrong FSM; in either case signing against the quote
		// would attribute the client's intent to the wrong round.
		// When AdmittedRoundID is unset (RoundJoined has not yet
		// arrived) the actor layer is responsible for buffering
		// the quote until re-key; reaching the FSM in that state
		// is itself unexpected and we fail loudly.
		if s.AdmittedRoundID == (RoundID{}) {
			env.Log.WarnS(ctx, "Quote arrived before admission",
				nil,
				slog.String("round_id",
					evt.RoundID.String()))

			return failWithNotification(
				"quote arrived before admission",
				fmt.Errorf("quote round_id=%s but no "+
					"admitted RoundID on FSM", evt.RoundID),
				false,
				fn.Some(evt.RoundID),
			), nil
		}
		if evt.RoundID != s.AdmittedRoundID {
			env.Log.WarnS(ctx, "Quote round_id mismatch",
				nil,
				slog.String(
					"quote_round_id", evt.RoundID.String(),
				),
				slog.String(
					"admitted_round_id",
					s.AdmittedRoundID.String(),
				))

			return failWithNotification(
				"quote round_id mismatch",
				fmt.Errorf("quote round_id=%s does not match "+
					"admitted round_id=%s", evt.RoundID,
					s.AdmittedRoundID),
				false,
				fn.Some(evt.RoundID),
			), nil
		}

		env.Log.InfoS(ctx, "Received seal-time quote",
			slog.String("round_id", evt.RoundID.String()),
			slog.Int64("operator_fee_sat",
				evt.Quote.OperatorFeeSat),
			slog.Uint64("seal_pass", uint64(evt.Quote.SealPass)),
		)

		nextState := &QuoteReceivedState{
			RoundID: evt.RoundID,
			Quote:   evt.Quote,
			Intents: s.Intents.Clone(),
		}

		decision := evaluateQuote(
			ctx, env, evt.RoundID, s.Intents, evt.Quote,
		)

		return &ClientStateTransition{
			NextState: nextState,
			NewEvents: fn.Some(ClientEmittedEvent{
				InternalEvent: []ClientEvent{decision},
			}),
		}, nil

	case *BoardingFailed:
		// Server rejected the registration or the request timed out.
		// Roll back any reserved forfeits because no signatures have
		// been produced at this phase.
		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
				FailureCode: evt.FailureCode,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: failureOutbox(
					evt.Reason, evt.Error, evt.Recoverable,
					fn.None[RoundID](), s.Intents.Forfeits,
				),
			}),
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
//  1. If any non-fixed VTXO or leave already carries IsChange=true,
//     leave it alone. This preserves explicit wallet decisions:
//     boarding change in handleBoard / handleTriggerBoard, and
//     directed-send self-change in handleSendVTXOs. A fixed VTXO
//     marked IsChange remains malformed and is rejected by quote
//     validation/server admission instead of being silently repaired.
//
//  2. If two or more outputs carry IsChange=true (which can only
//     happen when an entry-point path accidentally double-stamps,
//     e.g., mixing boarding-change + directed-send self-change),
//     keep the FIRST non-fixed marker and clear the rest. Defensive —
//     the proto invariant is "exactly one", so silently submitting
//     two would let the server reject the round.
//
//  3. If no marker is set and the total output count is greater
//     than one, stamp the first non-fixed VTXO. When there is no
//     non-fixed VTXO, stamp the first leave. Single-output intents
//     get no marker.
//
// Mutates the slices in place.
func designateChangeMarker(vtxoReqs []types.VTXORequest,
	leaveReqs []*types.LeaveRequest) {

	// First pass: count and locate existing markers.
	var (
		firstVTXOIdx        = -1
		firstFixedVTXOIdx   = -1
		firstLeaveIdx       = -1
		nonFixedMarkerCount int
		markerCount         int
	)
	for i, req := range vtxoReqs {
		if !req.IsChange {
			continue
		}
		if req.FixedAmount {
			if firstFixedVTXOIdx == -1 {
				firstFixedVTXOIdx = i
			}
			markerCount++

			continue
		}
		if firstVTXOIdx == -1 {
			firstVTXOIdx = i
		}
		nonFixedMarkerCount++
		markerCount++
	}
	for i, leave := range leaveReqs {
		if leave == nil || !leave.IsChange {
			continue
		}
		if firstLeaveIdx == -1 {
			firstLeaveIdx = i
		}
		nonFixedMarkerCount++
		markerCount++
	}

	// Defensive: if multiple markers are present, keep the first non-fixed
	// marker (preferring VTXO over leave when both have one). If every
	// marker is fixed, keep the first fixed marker so admission rejects the
	// malformed request instead of silently changing the caller's intent.
	if markerCount > 1 && nonFixedMarkerCount > 0 {
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
	if markerCount > 1 && firstFixedVTXOIdx != -1 {
		for i := range vtxoReqs {
			if i == firstFixedVTXOIdx {
				continue
			}
			vtxoReqs[i].IsChange = false
		}
		for i := range leaveReqs {
			if leaveReqs[i] != nil {
				leaveReqs[i].IsChange = false
			}
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
	for i := range vtxoReqs {
		if vtxoReqs[i].FixedAmount {
			continue
		}

		vtxoReqs[i].IsChange = true

		return
	}
	if len(leaveReqs) > 0 && leaveReqs[0] != nil {
		leaveReqs[0].IsChange = true
	}
}

func evaluateQuote(ctx context.Context, env *ClientEnvironment, roundID RoundID,
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
			Reason:  quoteRejectReason(quote.RejectReason),
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

	// Belt-and-braces cap check on the operator-declared fee.
	// The realised-fee check below is the authoritative defense,
	// but rejecting a self-incriminating declaration early keeps
	// the diagnostic message close to the field the operator
	// chose. A malicious operator can lie about this number, so
	// we never rely on it alone (see realisedQuoteFee).
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

	// Realised-fee cap enforcement (#379). The
	// quote.OperatorFeeSat field is an operator-supplied claim;
	// the actual economic fee the client will pay is
	// Σ(inputs) − Σ(quoted outputs). A malicious operator can
	// quote a small OperatorFeeSat while reducing the change
	// output (or any quote-decided amount) by a much larger
	// delta, so cap enforcement against the declared field alone
	// is bypassable. Recompute the realised fee from the
	// authoritative input amounts (boarding ChainInfo, VTXOStore
	// forfeit values) and the quote's positional output amounts;
	// reject if it exceeds the cap or if the quote's declared
	// fee disagrees with the realised value (operator dishonesty).
	// Quote evaluation may outlive the actor request that triggered local
	// registration, so detach local store reads from caller cancellation in
	// the same way IntentRequested does for registration-time accounting.
	opCtx := context.WithoutCancel(ctx)
	realised, err := realisedQuoteFee(opCtx, env, intents, quote)
	if err != nil {
		return &QuoteRejected{
			RoundID: roundID,
			QuoteID: quote.QuoteID,
			Reason: fmt.Sprintf(
				"realised fee computation failed: %v", err,
			),
		}
	}
	if realised < 0 {
		return &QuoteRejected{
			RoundID: roundID,
			QuoteID: quote.QuoteID,
			Reason: fmt.Sprintf(
				"realised fee is negative: outputs exceed "+
					"inputs by %d sat", -realised,
			),
		}
	}
	if realised > feeCap {
		return &QuoteRejected{
			RoundID: roundID,
			QuoteID: quote.QuoteID,
			Reason: fmt.Sprintf(
				"realised operator fee %d exceeds cap %d "+
					"(quoted operator_fee_sat=%d)",
				realised, feeCap, quote.OperatorFeeSat,
			),
		}
	}
	if realised != quote.OperatorFeeSat {
		return &QuoteRejected{
			RoundID: roundID,
			QuoteID: quote.QuoteID,
			Reason: fmt.Sprintf(
				"quoted operator_fee_sat=%d disagrees with "+
					"realised fee=%d (Σinputs−Σoutputs)",
				quote.OperatorFeeSat, realised,
			),
		}
	}

	return &QuoteAccepted{
		RoundID: roundID,
		QuoteID: quote.QuoteID,
	}
}

// realisedQuoteFee returns the economic operator fee the client
// will pay if it signs the supplied quote, computed as
// Σ(authoritative inputs) − Σ(quoted outputs). Inputs are sourced
// from the client's own intent composition (boarding ChainInfo
// amounts and forfeit values looked up from the VTXOStore) so a
// malicious operator cannot inflate them; outputs are sourced from
// the quote's positional VTXOQuotes / LeaveQuotes amounts because
// those are the values the server will actually stamp into the
// commitment tx and VTXO tree. This is the authoritative cap-
// enforcement signal: every other field on the quote (notably
// OperatorFeeSat) is operator-attested and may be a lie.
//
// Returns an error only when the VTXOStore lookup for a forfeited
// VTXO fails; an unset store falls back to the embedded forfeit
// Amount hint so harness paths without persistence keep working.
func realisedQuoteFee(ctx context.Context, env *ClientEnvironment,
	intents Intents, quote *ClientQuote) (int64, error) {

	// Sum inputs and outputs without filtering zero, so the
	// realised fee equals Σinputs−Σoutputs exactly. Filtering
	// zero is a no-op arithmetically but invites the reader to
	// believe negative values are also benignly absorbed; they
	// are not — a negative amount on either side would silently
	// shift the realised fee in a direction that lets a hostile
	// operator pass the cap. Reject any negative value explicitly
	// instead so callers see a diagnostic rather than a quietly
	// wrong realised fee.
	var inputsSat int64
	for i := range intents.Boarding {
		amt := int64(intents.Boarding[i].ChainInfo.Amount)
		if amt < 0 {
			return 0, fmt.Errorf("negative boarding input "+
				"amount: %d sat", amt)
		}
		inputsSat += amt
	}

	// Forfeit values must come from the VTXOStore (or, when the
	// store is nil, the embedded Amount hint). Trusting any
	// operator-supplied number here would re-open the same
	// inflation hole the cap is meant to close.
	forfeitAmt, err := computeTotalForfeitAmount(
		ctx, env.VTXOStore, intents.Forfeits,
	)
	if err != nil {
		return 0, fmt.Errorf("forfeit amount lookup: %w", err)
	}
	inputsSat += int64(forfeitAmt)

	var outputsSat int64
	for i := range quote.VTXOQuotes {
		amt := quote.VTXOQuotes[i].AmountSat
		if amt < 0 {
			return 0, fmt.Errorf("negative vtxo output amount at "+
				"index %d: %d sat", i, amt)
		}
		outputsSat += amt
	}
	for i := range quote.LeaveQuotes {
		amt := quote.LeaveQuotes[i].AmountSat
		if amt < 0 {
			return 0, fmt.Errorf("negative leave output amount at "+
				"index %d: %d sat", i, amt)
		}
		outputsSat += amt
	}

	return inputsSat - outputsSat, nil
}

// quoteRejectReason formats server-side quote rejections for operator-facing
// logs and operation status output.
func quoteRejectReason(reason roundpb.QuoteReason) string {
	switch reason {
	case roundpb.QuoteReason_INSUFFICIENT_RESIDUAL:
		return fmt.Sprintf("server rejected intent: %s (not enough "+
			"value remains for the change output after seal-time "+
			"operator fees; use a larger input or reduce fixed "+
			"outputs)", reason)

	case roundpb.QuoteReason_INVALID_CHANGE_DESIGNATION:
		return fmt.Sprintf("server rejected intent: %s (the intent "+
			"must have exactly one change output when it has "+
			"multiple outputs; this is unexpected when using the "+
			"standard client and should be reported)", reason)

	default:
		return fmt.Sprintf("server rejected intent: %s", reason)
	}
}

// validateQuoteEchoes cross-checks that the server's per-output
// quote entries preserve the intent's fixed-output layout. It
// enforces positional length parity, pkScript / recipient-key echo
// equality, and non-change amount equality; deviation is permitted
// only on the single IsChange=true output across both slices.
// Returns a diagnostic reason and ok=false on first mismatch.
func validateQuoteEchoes(intents Intents, quote *ClientQuote) (string, bool) {
	if len(quote.VTXOQuotes) != len(intents.VTXOs) {
		return fmt.Sprintf("quote vtxo entries %d != intent "+
			"vtxos %d", len(quote.VTXOQuotes),
			len(intents.VTXOs)), false
	}
	if len(quote.LeaveQuotes) != len(intents.Leaves) {
		return fmt.Sprintf("quote leave entries %d != intent "+
			"leaves %d", len(quote.LeaveQuotes),
			len(intents.Leaves)), false
	}

	// When the intent carries exactly one non-fixed output across the
	// combined VTXORequests + LeaveRequests, the server treats that sole
	// output as implicit change and stamps the residual on it without
	// requiring IsChange=true on the wire (#270, see the server's
	// resolveChangeDesignation). Its intent Amount is only a target or
	// lower-bound hint in boarding / leave flows; the honest quote can
	// therefore be above or below that target depending on the realised
	// seal-time fee and input value. Do not enforce amount equality here:
	// realisedQuoteFee below is the authoritative security check, because
	// it verifies the actual signed outputs imply the quoted, capped fee.
	// FixedAmount disables this exception for contract outputs.
	totalOutputs := len(intents.VTXOs) + len(intents.Leaves)
	implicitChange := totalOutputs == 1 && !singleOutputFixed(intents)

	for i := range intents.VTXOs {
		vtxoReq := intents.VTXOs[i]
		entry := quote.VTXOQuotes[i]

		intentScript, err := vtxoReq.EffectivePkScript()
		if err != nil {
			return fmt.Sprintf("vtxo[%d] pkScript "+
				"derivation: %v", i, err), false
		}
		if !bytes.Equal(entry.PkScript, intentScript) {
			return fmt.Sprintf("vtxo[%d] pkScript echo "+
				"mismatch", i), false
		}

		var intentKey []byte
		if vtxoReq.SigningKey.PubKey != nil {
			intentKey = vtxoReq.SigningKey.PubKey.
				SerializeCompressed()
		}
		if !bytes.Equal(entry.RecipientKey, intentKey) {
			return fmt.Sprintf("vtxo[%d] recipient key "+
				"echo mismatch", i), false
		}

		if vtxoReq.FixedAmount && vtxoReq.IsChange {
			return fmt.Sprintf("vtxo[%d] fixed amount "+
				"cannot be change", i), false
		}

		if !implicitChange && !vtxoReq.IsChange {
			// Multi-output intent: only the explicit
			// IsChange=true slot may deviate from its
			// intent target.
			if entry.AmountSat != int64(vtxoReq.Amount) {
				return fmt.Sprintf("vtxo[%d] "+
					"non-change amount %d != "+
					"intent target %d", i,
					entry.AmountSat,
					int64(vtxoReq.Amount)), false
			}
		}
	}

	for i := range intents.Leaves {
		leaveReq := intents.Leaves[i]
		entry := quote.LeaveQuotes[i]

		if leaveReq == nil || leaveReq.Output == nil {
			return fmt.Sprintf("leave[%d] intent "+
				"missing output", i), false
		}
		if !bytes.Equal(entry.PkScript, leaveReq.Output.PkScript) {
			return fmt.Sprintf("leave[%d] pkScript echo "+
				"mismatch", i), false
		}

		if !implicitChange && !leaveReq.IsChange {
			// Multi-output intent: only the explicit
			// IsChange=true slot may deviate from its
			// intent target.
			if entry.AmountSat != leaveReq.Output.Value {
				return fmt.Sprintf("leave[%d] "+
					"non-change amount %d != "+
					"intent target %d", i,
					entry.AmountSat,
					leaveReq.Output.Value), false
			}
		}
	}

	return "", true
}

// singleOutputFixed reports whether a one-output intent carries a fixed VTXO
// request. Leave outputs do not currently have fixed-amount metadata.
func singleOutputFixed(intents Intents) bool {
	if len(intents.VTXOs) != 1 || len(intents.Leaves) != 0 {
		return false
	}

	return intents.VTXOs[0].FixedAmount
}

// ProcessEvent for QuoteReceivedState.
func (s *QuoteReceivedState) ProcessEvent(ctx context.Context,
	event ClientEvent, env *ClientEnvironment) (*ClientStateTransition,
	error) {

	// Any transition into ClientFailedState from this pre-signing state
	// returns forfeit-reserved inputs to LiveState; see
	// releaseForfeitsOnFailure.
	transition, err := s.processEvent(ctx, event, env)

	return releaseForfeitsOnFailure(
		transition, err, fn.Some(s.RoundID), s.Intents.Forfeits,
	)
}

// processEvent runs the QuoteReceivedState event handling; ProcessEvent wraps
// it to return forfeit-reserved inputs to LiveState on failure.
func (s *QuoteReceivedState) processEvent(ctx context.Context,
	event ClientEvent, env *ClientEnvironment) (*ClientStateTransition,
	error) {

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
			ctx, env, evt.RoundID, s.Intents, evt.Quote,
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
			slog.Int64("operator_fee_sat", s.Quote.OperatorFeeSat),
		)

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
		env.Log.WarnS(ctx, "Rejecting seal-time quote",
			nil,
			slog.String("round_id", evt.RoundID.String()),
			slog.String("reason", evt.Reason),
		)

		reject := &JoinRoundRejectOutbox{
			RoundID: evt.RoundID,
			QuoteID: evt.QuoteID,
			Reason:  evt.Reason,
		}
		outbox := []ClientOutMsg{reject}
		standard, custom := forfeitRollbackOutpoints(s.Intents.Forfeits)
		if len(standard) > 0 {
			outbox = append(outbox, &ReleaseForfeitReservation{
				Outpoints: standard,
			})
		}
		if len(custom) > 0 {
			outbox = append(outbox, &DropCustomForfeitReservation{
				Outpoints: custom,
			})
		}

		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      evt.Reason,
				Recoverable: false,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: outbox,
			}),
		}, nil

	case *BoardingFailed:
		// A server-pushed round failure while we hold the quote is
		// still pre-signing: fail into ClientFailedState carrying the
		// typed code, and let ProcessEvent's releaseForfeitsOnFailure
		// wrapper return the forfeits to LiveState and retire the job
		// on a terminal-for-job code.
		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
				FailureCode: evt.FailureCode,
			},
		}, nil

	default:
		return selfLoop(s), nil
	}
}

// ProcessEvent for RoundJoinedState.
func (s *RoundJoinedState) ProcessEvent(ctx context.Context, event ClientEvent,
	env *ClientEnvironment) (*ClientStateTransition, error) {

	// Any transition into ClientFailedState from this pre-signing state
	// returns forfeit-reserved inputs to LiveState; see
	// releaseForfeitsOnFailure.
	transition, err := s.processEvent(ctx, event, env)

	return releaseForfeitsOnFailure(
		transition, err, fn.Some(s.RoundID), s.Intents.Forfeits,
	)
}

// processEvent runs the RoundJoinedState event handling; ProcessEvent wraps it
// to return forfeit-reserved inputs to LiveState on failure.
func (s *RoundJoinedState) processEvent(ctx context.Context, event ClientEvent,
	env *ClientEnvironment) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *CommitmentTxBuilt:
		// Cross-check the commitment-tx RoundID against the round
		// the FSM was admitted to. The actor's routing layer is
		// keyed by RoundID, so under normal operation the values
		// agree by construction; the FSM-level assertion is
		// defense-in-depth against future actor-routing
		// regressions and against a hostile server stamping a
		// foreign RoundID onto a commitment payload.
		if evt.RoundID != s.RoundID {
			env.Log.WarnS(ctx, "Commitment round_id mismatch",
				nil,
				slog.String(
					"event_round_id", evt.RoundID.String(),
				),
				slog.String(
					"admitted_round_id", s.RoundID.String(),
				))

			return failWithNotification(
				"commitment round_id mismatch",
				fmt.Errorf("commitment round_id=%s does not "+
					"match admitted round_id=%s",
					evt.RoundID, s.RoundID),
				false,
				fn.Some(s.RoundID),
			), nil
		}

		txid := evt.Tx.UnsignedTx.TxHash()
		env.Log.InfoS(
			ctx,
			"Received commitment transaction from server",
			slog.String("round_id", evt.RoundID.String()),
			slog.String("commitment_txid", txid.String()),
			slog.Int("vtxo_tree_count", len(evt.VTXOTreePaths)),
		)

		// Carry the operator's round flow version (validated on receipt
		// in CommitmentTxBuilt.FromProto) onto the FSM state, so it
		// threads through to the checkpointed round.Round.FlowVersion
		// and the persisted value mirrors what the operator stamped.
		return &ClientStateTransition{
			NextState: &CommitmentTxReceivedState{
				RoundID:              evt.RoundID,
				CommitmentTx:         evt.Tx,
				TxID:                 txid,
				VTXOTreePaths:        evt.VTXOTreePaths,
				TreeCosignKey:        evt.TreeCosignKey,
				ConnectorOperatorKey: evt.ConnectorOperatorKey,
				SweepKey:             evt.SweepKey,
				SweepDelay:           evt.SweepDelay,
				FlowVersion:          evt.FlowVersion,
				ForfeitKey:           evt.ForfeitKey,
				Intents:              s.Intents.Clone(),
				ClientTrees: make(
					map[SignerKey]*tree.Tree,
				),
				Quote: s.Quote,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				InternalEvent: []ClientEvent{evt},
			}),
		}, nil

	case *JoinRoundQuoteReceived:
		// Reseal-after-accept: the server resealed and is offering
		// a fresh quote for the same round before the commitment
		// tx is delivered. The accept already in flight (or in the
		// outbox queue) carries the prior pass's quote_id and the
		// server will drop it server-side; locally we walk the FSM
		// back to QuoteReceivedState so the new quote can be
		// re-evaluated end-to-end. Without this branch a fresh
		// quote arriving here would self-loop and the FSM would
		// stall waiting for a CommitmentTxBuilt that never comes.
		//
		// Replay protection: only honour quotes whose seal pass is
		// strictly higher than what we already accepted. Lower or
		// equal pass numbers are stale redeliveries.
		currentPass := uint32(0)
		if s.Quote != nil {
			currentPass = s.Quote.SealPass
		}
		if evt.Quote == nil || evt.Quote.SealPass <= currentPass {
			return selfLoop(s), nil
		}

		// Defense-in-depth: a reseal must keep the same RoundID.
		if evt.RoundID != s.RoundID {
			env.Log.WarnS(ctx, "Reseal round_id mismatch",
				nil,
				slog.String(
					"quote_round_id", evt.RoundID.String(),
				),
				slog.String(
					"admitted_round_id", s.RoundID.String(),
				))

			return failWithNotification(
				"reseal round_id mismatch",
				fmt.Errorf("reseal quote round_id=%s does "+
					"not match admitted round_id=%s",
					evt.RoundID, s.RoundID),
				false,
				fn.Some(s.RoundID),
			), nil
		}

		env.Log.InfoS(ctx, "Received post-accept reseal quote",
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
			ctx, env, evt.RoundID, s.Intents, evt.Quote,
		)

		return &ClientStateTransition{
			NextState: nextState,
			NewEvents: fn.Some(ClientEmittedEvent{
				InternalEvent: []ClientEvent{decision},
			}),
		}, nil

	case *BoardingFailed:
		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
				FailureCode: evt.FailureCode,
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
			return nil, fmt.Errorf("boarding UTXO %s not found in "+
				"commitment tx", outpoint)
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
			return fmt.Errorf("leave output not found in "+
				"commitment tx: value=%d, remaining=%d",
				key.value, count)
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
func (s *CommitmentTxReceivedState) ProcessEvent(ctx context.Context,
	event ClientEvent, env *ClientEnvironment) (*ClientStateTransition,
	error) {

	// Any transition into ClientFailedState from this pre-signing state
	// returns forfeit-reserved inputs to LiveState; see
	// releaseForfeitsOnFailure.
	transition, err := s.processEvent(ctx, event, env)

	return releaseForfeitsOnFailure(
		transition, err, fn.Some(s.RoundID), s.Intents.Forfeits,
	)
}

// processEvent runs the CommitmentTxReceivedState event handling; ProcessEvent
// wraps it to return forfeit-reserved inputs to LiveState on failure.
//
//nolint:funlen
func (s *CommitmentTxReceivedState) processEvent(ctx context.Context,
	event ClientEvent, env *ClientEnvironment) (*ClientStateTransition,
	error) {

	switch evt := event.(type) {
	case *CommitmentTxBuilt:
		env.Log.InfoS(ctx, "Validating commitment transaction",
			slog.String("round_id", s.RoundID.String()),
			slog.Int(
				"boarding_intent_count",
				len(s.Intents.Boarding),
			),
			slog.Int("vtxo_intent_count", len(s.Intents.VTXOs)),
			slog.Int("leave_intent_count", len(s.Intents.Leaves)),
		)

		// Resolve this round's operator signing keys, falling back to
		// the global operator key when talking to a server that
		// predates the per-round fields. treeCosignKey is the
		// operator's MuSig2 cosigner for this round's VTXO tree
		// (independent of the identity key); connectorOperatorKey is
		// what the connector tree was built with. Using the
		// round-delivered keys keeps a client on a previous
		// operator-key epoch in agreement with the server.
		treeCosignKey := s.TreeCosignKey
		if treeCosignKey == nil {
			treeCosignKey = env.OperatorTerms.PubKey
		}
		connectorOperatorKey := s.ConnectorOperatorKey
		if connectorOperatorKey == nil {
			connectorOperatorKey = env.OperatorTerms.PubKey
		}

		// Validate this round's sweep delay against the VTXO exit
		// delay. The sweep delay is now delivered per round (not a
		// global operator term), so the security check that the
		// operator has time to respond to unilateral exits before the
		// batch expires moves here, where the round-specific value is
		// known.
		if err := ValidateDelayParameters(
			s.SweepDelay, env.OperatorTerms.VTXOExitDelay,
		); err != nil {

			env.Log.WarnS(
				ctx,
				"Round sweep delay validation failed",
				err,
				slog.String("round_id", s.RoundID.String()),
			)

			return failBeforeForfeitSigning(
				"invalid round sweep delay", err, false,
				s.RoundID, s.Intents.Forfeits,
			), nil
		}

		// Validate boarding inputs if we have any boarding intents.
		// Refresh-only rounds have no boarding inputs to validate.
		var boardingInputIndices map[wire.OutPoint]int
		if len(s.Intents.Boarding) > 0 {
			var err error
			boardingInputIndices, err = validateBoardingInputs(
				s.CommitmentTx.UnsignedTx, s.Intents.Boarding,
			)
			if err != nil {
				env.Log.WarnS(
					ctx,
					"Commitment tx validation failed",
					err,
					slog.String(
						"round_id", s.RoundID.String(),
					),
				)

				return failBeforeForfeitSigning(
					"commitment tx validation failed", err,
					true, s.RoundID, s.Intents.Forfeits,
				), nil
			}
		} else {
			boardingInputIndices = make(map[wire.OutPoint]int)
		}

		env.Log.DebugS(
			ctx,
			"Validated boarding inputs in commitment tx",
			slog.Int(
				"boarding_input_count",
				len(boardingInputIndices),
			),
		)

		// Validate leave outputs if we have any leave requests. Each
		// leave output must be present in the commitment tx with the
		// correct value and script. When the client accepted a
		// seal-time quote, the server is the amount authority — the
		// per-leave expected value comes from Quote.LeaveAmounts
		// (positional) rather than the intent's target amount, which
		// was only a hint at seal time.
		if len(s.Intents.Leaves) > 0 {
			leaveAmounts := quoteLeaveAmounts(
				s.Quote, s.Intents.Leaves,
			)
			if err := validateLeaveOutputs(
				s.CommitmentTx.UnsignedTx, s.Intents.Leaves,
				leaveAmounts,
			); err != nil {

				env.Log.WarnS(
					ctx,
					"Leave output validation failed",
					err,
					slog.String(
						"round_id", s.RoundID.String(),
					),
				)

				return failBeforeForfeitSigning(
					"leave output validation failed", err,
					true, s.RoundID, s.Intents.Forfeits,
				), nil
			}

			env.Log.DebugS(
				ctx,
				"Validated leave outputs in commitment tx",
				slog.Int(
					"leave_output_count",
					len(s.Intents.Leaves),
				),
			)
		}

		// Before trusting any tree's contents, prove that each
		// operator-supplied VTXO tree is rooted in this round's
		// commitment tx. The per-VTXO ValidatePath check below only
		// proves internal self-consistency; without this binding a
		// self-consistent tree rooted at the wrong commitment output
		// would be co-signed, after which the client's new VTXOs are
		// unrecoverable (wavelength#680).
		if err := validateVTXOTreeBinding(
			s.CommitmentTx.UnsignedTx, s.VTXOTreePaths,
		); err != nil {

			env.Log.WarnS(
				ctx,
				"VTXO tree commitment binding failed",
				err,
				slog.String("round_id", s.RoundID.String()),
			)

			// The binding error is surfaced through the failed
			// state, not raised as a Go error. A mis-rooted tree
			// is a structural defect of the round, not a transient
			// condition, so the failure is non-recoverable.
			return failBeforeForfeitSigning(
				"VTXO tree commitment binding failed", err,
				false, s.RoundID, s.Intents.Forfeits,
			), nil
		}

		clientTrees := make(map[SignerKey]*tree.Tree)

		// Next, we'll make sure that each of the VTXO requests that we
		// originally requested are actually present in the VTXT trees
		// that the server sent us.
		for i, vtxoReq := range s.Intents.VTXOs {
			pkScript, err := vtxoReq.EffectivePkScript()
			if err != nil {
				derivedErr := fmt.Errorf("derive pkScript for "+
					"VTXO request %d: %w", i, err)

				return failBeforeForfeitSigning(
					"VTXT validation failed", derivedErr,
					true, s.RoundID, s.Intents.Forfeits,
				), nil
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
					treeCosignKey,
				)
				if validateErr == nil {
					// Found the VTXO in this tree.
					break
				}
			}
			if validateErr != nil {

				// The error is carried into the failed state;
				// the FSM does not raise it as a Go error.
				reason := fmt.Sprintf("VTXT validation failed "+
					"for VTXO request %d", i)

				return failBeforeForfeitSigning(
					reason, validateErr, false, s.RoundID,
					s.Intents.Forfeits,
				), nil
			}

			// Ensure we actually found a client tree. This handles
			// the edge case where VTXOTreePaths is empty.
			if clientTree == nil {
				reason := fmt.Sprintf("no client tree found "+
					"for VTXO request %d", i)

				return failBeforeForfeitSigning(
					reason, fmt.Errorf("VTXO tree not "+
						"found"),
					false,
					s.RoundID,
					s.Intents.Forfeits,
				), nil
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
				reason := fmt.Sprintf("anchor output "+
					"validation failed for output %d",
					outputIdx)

				return failBeforeForfeitSigning(
					reason, err, false, s.RoundID,
					s.Intents.Forfeits,
				), nil
			}
		}

		env.Log.InfoS(
			ctx,
			"Commitment transaction validated successfully",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("client_trees", len(clientTrees)),
			slog.Int("vtxo_tree_count", len(s.VTXOTreePaths)),
		)

		// Before carrying the forfeit mappings forward to signing,
		// prove that each assigned connector leaf descends from this
		// round's commitment tx. Without this an inconsistent or
		// compromised operator could hand us a connector leaf that
		// exists independently of the commitment tx; signing the
		// forfeit over it would make the old VTXO claimable even if
		// the replacement round never commits, breaking round
		// atomicity (wavelength#681).
		//
		//nolint:contextcheck // Connector-tree reconstruction is pure,
		// deterministic CPU work (the lib/tree materializer ignores its
		// context); there is no I/O to cancel, so no context to thread.
		if err := validateConnectorAncestry(
			s.CommitmentTx.UnsignedTx, connectorOperatorKey,
			evt.ForfeitMappings,
		); err != nil {
			// Error carried into failed state.
			return failBeforeForfeitSigning(
				"connector ancestry validation failed", err,
				false, s.RoundID, s.Intents.Forfeits,
			), nil
		}

		forfeitMappings, err := populateForfeitMappingAmounts(
			evt.ForfeitMappings, s.Intents.Forfeits,
		)
		if err != nil {
			return nil, fmt.Errorf("populate forfeit amounts: %w",
				err)
		}

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
				SweepDelay:           s.SweepDelay,
				FlowVersion:          s.FlowVersion,
				ForfeitKey:           s.ForfeitKey,
				Intents:              s.Intents.Clone(),
				ClientTrees:          clientTrees,
				BoardingInputIndices: boardingInputIndices,
				ForfeitMappings:      forfeitMappings,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				InternalEvent: []ClientEvent{&GenerateNonces{}},
			}),
		}, nil

	case *BoardingFailed:
		return withFailureCode(
			failBeforeForfeitSigning(
				evt.Reason, evt.Error, evt.Recoverable,
				s.RoundID, s.Intents.Forfeits,
			),
			evt.FailureCode,
		), nil

	default:
		// Self-loop on unknown events - do not halt the FSM.
		return selfLoop(s), nil
	}
}

// ProcessEvent for CommitmentTxValidatedState.
func (s *CommitmentTxValidatedState) ProcessEvent(ctx context.Context,
	event ClientEvent, env *ClientEnvironment) (*ClientStateTransition,
	error) {

	// Any transition into ClientFailedState from this pre-signing state
	// returns forfeit-reserved inputs to LiveState; see
	// releaseForfeitsOnFailure.
	transition, err := s.processEvent(ctx, event, env)

	return releaseForfeitsOnFailure(
		transition, err, fn.Some(s.RoundID), s.Intents.Forfeits,
	)
}

// processEvent runs the CommitmentTxValidatedState event handling; ProcessEvent
// wraps it to return forfeit-reserved inputs to LiveState on failure.
func (s *CommitmentTxValidatedState) processEvent(ctx context.Context,
	event ClientEvent, env *ClientEnvironment) (*ClientStateTransition,
	error) {

	switch evt := event.(type) {
	case *GenerateNonces:
		env.Log.InfoS(
			ctx,
			"Generating MuSig2 nonces for VTXO tree signing",
			slog.String("round_id", s.RoundID.String()),
			slog.Int(
				"boarding_intent_count",
				len(s.Intents.Boarding),
			),
			slog.Int("vtxo_intent_count", len(s.Intents.VTXOs)),
		)

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

			// Derive this round's forfeit penalty output script
			// from the per-round forfeit key (replacing the removed
			// global GetInfo forfeit script).
			forfeitScript, err := forfeitPenaltyScript(s.ForfeitKey)
			if err != nil {

				// Error carried into failed state.
				return failBeforeForfeitSigning(
					"invalid round forfeit key", err, false,
					s.RoundID, s.Intents.Forfeits,
				), nil
			}

			// Arm the forfeit-collection timeout FIRST, before
			// dispatching any per-VTXO forfeit requests. This
			// transition advances the in-memory FSM to
			// ForfeitSignaturesCollectingState without
			// checkpointing, so a restart cannot recover it. If a
			// later ForfeitRequestToVTXO Tell fails, processOutbox
			// aborts on that error and never reaches a trailing
			// timeout, leaving the round wedged waiting for forfeit
			// signatures forever. Emitting StartTimeoutReq first
			// guarantees the timeout is scheduled regardless of any
			// subsequent per-VTXO send failure, so the round can
			// still time out into a recoverable failed state.
			var outbox []ClientOutMsg
			outbox = append(outbox, &StartTimeoutReq{
				RoundKey: RoundKeyStr(s.RoundID.KeyString()),
				Phase:    TimeoutPhaseForfeitCollection,
				Duration: env.ForfeitCollectionTimeout,
			})

			// Build forfeit request messages for each VTXO being
			// forfeited.
			forfeitReqs := forfeitRequestMap(
				s.Intents.Forfeits,
			)
			for vtxoOutpoint, info := range s.ForfeitMappings {
				connOut := info.ConnectorOutpoint
				connScript := info.ConnectorPkScript
				connAmt := info.ConnectorAmount
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

			// Transition directly to forfeit collection.
			collectedForfeits := make(
				map[wire.OutPoint]*ForfeitSignatureResponse,
			)

			return &ClientStateTransition{
				NextState: &ForfeitSignaturesCollectingState{
					RoundID:           s.RoundID,
					CommitmentTx:      s.CommitmentTx,
					VTXOTreePaths:     s.VTXOTreePaths,
					SweepDelay:        s.SweepDelay,
					FlowVersion:       s.FlowVersion,
					ForfeitKey:        s.ForfeitKey,
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
		// Build one independent signing job for each VTXO. The executor
		// bounds concurrency across signer paths while each path keeps
		// its transaction sessions ordered locally.
		sessionJobs := make(
			[]CreateSignerSessionJob, 0, len(s.Intents.VTXOs),
		)
		seenSignerKeys := make(
			map[SignerKey]struct{}, len(s.Intents.VTXOs),
		)
		for _, vtxoReq := range s.Intents.VTXOs {
			signerKey := NewSignerKey(
				vtxoReq.SigningKey.PubKey,
			)

			// Protocol session and nonce maps are keyed solely by
			// signer key. Reject duplicates before creating signer
			// resources so map assembly cannot orphan a session.
			if _, ok := seenSignerKeys[signerKey]; ok {
				return nil, fmt.Errorf("duplicate "+
					"signer key %x", signerKey[:])
			}
			seenSignerKeys[signerKey] = struct{}{}

			// Get the client tree for this signer key.
			// The sweep tweak and batch output are
			// properties of the tree that were set when
			// the operator built it.
			clientTree := s.ClientTrees[signerKey]
			if clientTree == nil {
				return nil, fmt.Errorf("no client tree for "+
					"signer key %x", signerKey[:])
			}

			sweepTweak := clientTree.SweepTapscriptRoot
			batchOut := clientTree.BatchOutput
			root := clientTree.Root
			prevOutFetcher, err := root.PrevOutputFetcher(batchOut)
			if err != nil {
				return nil, fmt.Errorf("failed to create prev "+
					"output fetcher for signer %x: %w",
					signerKey[:], err)
			}

			sessionJobs = append(
				sessionJobs, CreateSignerSessionJob{
					SignerKey:          signerKey,
					Signer:             env.Wallet,
					SigningKey:         vtxoReq.SigningKey,
					SweepTapscriptRoot: sweepTweak,
					PrevOuts:           prevOutFetcher,
					Root:               root,
				},
			)
		}

		sessionResults, err := env.signingExecutor().CreateSessions(
			ctx, sessionJobs,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create signing "+
				"sessions: %w", err)
		}

		// Build protocol maps only after every worker completes. This
		// keeps ordinary Go maps owned by the FSM goroutine.
		musig2Sessions := make(map[SignerKey]*tree.SignerSession)
		allNonces := make(
			map[SignerKey]map[tree.TxID]tree.Musig2PubNonce,
		)
		for _, result := range sessionResults {
			musig2Sessions[result.SignerKey] = result.Session
			allNonces[result.SignerKey] = result.Nonces
		}

		env.Log.InfoS(ctx, "Generated MuSig2 nonces, sending to server",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("session_count", len(musig2Sessions)),
			slog.Int("signer_key_count", len(allNonces)),
		)

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
				SweepDelay:           s.SweepDelay,
				FlowVersion:          s.FlowVersion,
				ForfeitKey:           s.ForfeitKey,
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

	case *BoardingFailed:
		// Still pre-signing (VTXO forfeit sigs only cross to the server
		// on the ForfeitSignaturesCollecting -> InputSigSent
		// transition): fail into ClientFailedState with the typed code
		// and let ProcessEvent's releaseForfeitsOnFailure wrapper
		// release the forfeits and retire the job on a terminal-for-job
		// code.
		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
				FailureCode: evt.FailureCode,
			},
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
func (s *ForfeitSignaturesCollectingState) ProcessEvent(ctx context.Context,
	event ClientEvent, env *ClientEnvironment) (*ClientStateTransition,
	error) {

	// Failures here are still pre-signing: VTXO forfeit signatures are only
	// submitted to the server on the success transition to
	// InputSigSentState (SubmitVTXOForfeitSigsToServer), so any transition
	// into ClientFailedState may safely return forfeit-reserved inputs to
	// LiveState; see releaseForfeitsOnFailure.
	transition, err := s.processEvent(ctx, event, env)

	return releaseForfeitsOnFailure(
		transition, err, fn.Some(s.RoundID), s.Intents.Forfeits,
	)
}

// processEvent runs the ForfeitSignaturesCollectingState event handling;
// ProcessEvent wraps it to return forfeit-reserved inputs to LiveState on
// failure.
func (s *ForfeitSignaturesCollectingState) processEvent(ctx context.Context,
	event ClientEvent, env *ClientEnvironment) (*ClientStateTransition,
	error) {

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

		// Derive this round's forfeit penalty output script from the
		// per-round forfeit key (replacing the removed global GetInfo
		// forfeit script).
		forfeitScript, err := forfeitPenaltyScript(s.ForfeitKey)
		if err != nil {
			return nil, fmt.Errorf("forfeit penalty script for "+
				"VTXO %s: %w", evt.VTXOOutpoint, err)
		}

		// Validate the forfeit transaction structure using lib/tx. The
		// amount check ensures the zero-fee penalty output equals the
		// forfeited VTXO plus connector value, preventing value theft.
		params := tx.ForfeitTxParams{
			VTXOOutpoint:        evt.VTXOOutpoint,
			ConnectorOutpoint:   connectorInfo.ConnectorOutpoint,
			ServerForfeitScript: forfeitScript,
			ExpectedAmount: btcutil.Amount(
				int64(connectorInfo.VTXOAmount) +
					connectorInfo.ConnectorAmount,
			),
			ExpectedSequence: expectedForfeitSequence(req),
			ExpectedLockTime: expectedForfeitLockTime(req),
		}
		err = tx.ValidateForfeitTx(evt.ForfeitTx, params)
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
				FailureCode: evt.FailureCode,
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

		env.Log.WarnS(ctx, "Forfeit signature collection timed out",
			nil,
			slog.String("round_id", s.RoundID.String()),
			slog.Int("collected_forfeits", collectedCount),
			slog.Int("expected_forfeits", expectedCount),
		)

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
			SweepDelay:           s.SweepDelay,
			FlowVersion:          s.FlowVersion,
			ForfeitKey:           s.ForfeitKey,
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
	collectedForfeits map[wire.OutPoint]*ForfeitSignatureResponse) (
	*ClientStateTransition, error) {

	forfeitTxs := make(map[wire.OutPoint]*types.ForfeitTxSig)
	forfeitedVTXOs := make([]wire.OutPoint, 0, len(collectedForfeits))
	for outpoint, resp := range collectedForfeits {
		forfeitTxs[outpoint] = &types.ForfeitTxSig{
			UnsignedTx:          resp.ForfeitTx,
			ClientVTXOSig:       resp.Signature,
			ParticipantVTXOSigs: resp.ParticipantVTXOSigs,
			SpendPath:           resp.SpendPath,
		}
		forfeitedVTXOs = append(forfeitedVTXOs, outpoint)
	}

	env.Log.InfoS(
		ctx,
		"All forfeit signatures collected, signing boarding inputs",
		slog.String("round_id", s.RoundID.String()),
		slog.Int("forfeit_count", len(forfeitedVTXOs)),
		slog.Int("boarding_intent_count", len(s.Intents.Boarding)),
	)

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
		slog.Int("forfeit_sig_count", len(forfeitTxs)),
	)

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

// confirmationWatchScript returns the commitment-tx output script the client
// asks the chain backend to watch for round confirmation. It prefers the
// lowest VTXO batch-output index: every VTXOTreePaths key has been proven by
// validateVTXOTreeBinding to equal its tree's BatchOutpoint.Index and to be in
// range, so watching that output tracks the output that actually receives the
// client's funds rather than assuming output 0. It falls back to the first
// output when the round carries no VTXO trees (refresh-less / harness paths),
// and returns nil only when the tx has no outputs. Confirmation detection is
// by txid; the script matters for script-filtering backends (e.g. Neutrino).
func confirmationWatchScript(commitmentTx *wire.MsgTx,
	vtxoTrees map[int]*tree.Tree) []byte {

	if commitmentTx == nil || len(commitmentTx.TxOut) == 0 {
		return nil
	}

	idx := 0
	if len(vtxoTrees) > 0 {
		idx = -1
		for k := range vtxoTrees {
			if idx == -1 || k < idx {
				idx = k
			}
		}
	}

	// Guard the index even though binding already proved it in range, so a
	// future caller that reaches here on an unvalidated path degrades to
	// watching output 0 rather than panicking.
	if idx < 0 || idx >= len(commitmentTx.TxOut) {
		return commitmentTx.TxOut[0].PkScript
	}

	return commitmentTx.TxOut[idx].PkScript
}

func (s *ForfeitSignaturesCollectingState) forfeitCollectionOutbox(
	env *ClientEnvironment,
	forfeitTxs map[wire.OutPoint]*types.ForfeitTxSig,
	boardingInputSigs []*types.BoardingInputSignature,
) []ClientOutMsg {

	txid := s.CommitmentTx.UnsignedTx.TxHash()
	callerID := fmt.Sprintf("commitment-%s", txid.String())

	pkScript := confirmationWatchScript(
		s.CommitmentTx.UnsignedTx, s.VTXOTreePaths,
	)

	outboxMsgs := []ClientOutMsg{
		&CancelTimeoutReq{
			RoundKey: RoundKeyStr(s.RoundID.KeyString()),
			Phase:    TimeoutPhaseForfeitCollection,
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

	// The forfeit signatures leave the box on this transition, opening the
	// wavelength#844 hazard window: from here on, a round failure (or a
	// silent operator, the lumos#618 crash door) must resolve through a
	// status reconcile before the reservations can be released. Arm the
	// reconcile timeout so total silence still converges on a probe. A
	// boarding-only round has no reservations to reconcile, so it skips
	// the timer entirely, matching the forfeit-count gate every consumer
	// of the timeout applies.
	if len(s.Intents.Forfeits) > 0 && env.StatusReconcileTimeout > 0 {
		outboxMsgs = append(outboxMsgs, &StartTimeoutReq{
			RoundKey: RoundKeyStr(s.RoundID.KeyString()),
			Phase:    TimeoutPhaseStatusReconcile,
			Duration: env.StatusReconcileTimeout,
		})
	}

	if len(boardingInputSigs) == 0 {
		return outboxMsgs
	}

	return append(
		outboxMsgs[:2], append([]ClientOutMsg{
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
		FlowVersion:   s.FlowVersion,
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
		SweepDelay:     s.SweepDelay,
		FlowVersion:    s.FlowVersion,
		ForfeitKey:     s.ForfeitKey,
		Intents:        s.Intents.Clone(),
		ClientTrees:    s.ClientTrees,
		InputSigs:      boardingInputSigs,
		ForfeitedVTXOs: forfeitedVTXOs,
	}
}

// ProcessEvent for NoncesSentState.
func (s *NoncesSentState) ProcessEvent(ctx context.Context, event ClientEvent,
	env *ClientEnvironment) (*ClientStateTransition, error) {

	// Any transition into ClientFailedState from this pre-signing state
	// returns forfeit-reserved inputs to LiveState; see
	// releaseForfeitsOnFailure.
	transition, err := s.processEvent(ctx, event, env)

	return releaseForfeitsOnFailure(
		transition, err, fn.Some(s.RoundID), s.Intents.Forfeits,
	)
}

// processEvent runs the NoncesSentState event handling; ProcessEvent wraps it
// to return forfeit-reserved inputs to LiveState on failure.
func (s *NoncesSentState) processEvent(ctx context.Context, event ClientEvent,
	env *ClientEnvironment) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *NoncesAggregated:
		env.Log.InfoS(ctx, "Received aggregated nonces from server",
			slog.String("round_id", evt.RoundID.String()),
			slog.Int("agg_nonce_count", len(evt.AggNonces)),
		)

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
				registerErr := fmt.Errorf("failed to register "+
					"combined nonces: %w", err)
				cleanupErr := cleanupSignerSessions(
					s.Musig2Sessions,
				)

				return nil, errors.Join(registerErr, cleanupErr)
			}
		}

		env.Log.DebugS(
			ctx,
			"Registered aggregated nonces with signing sessions",
			slog.Int("session_count", len(s.Musig2Sessions)),
		)

		return &ClientStateTransition{
			NextState: &NoncesAggregatedState{
				RoundID:              s.RoundID,
				CommitmentTx:         s.CommitmentTx,
				VTXOTreePaths:        s.VTXOTreePaths,
				SweepDelay:           s.SweepDelay,
				FlowVersion:          s.FlowVersion,
				ForfeitKey:           s.ForfeitKey,
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
		cleanupErr := cleanupSignerSessions(s.Musig2Sessions)
		if cleanupErr != nil {
			env.Log.WarnS(ctx, "Unable to clean up MuSig2 sessions",
				cleanupErr,
			)
		}

		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
				FailureCode: evt.FailureCode,
			},
		}, nil

	default:
		// Self-loop on unknown events - do not halt the FSM.
		return selfLoop(s), nil
	}
}

// ProcessEvent for NoncesAggregatedState.
func (s *NoncesAggregatedState) ProcessEvent(ctx context.Context,
	event ClientEvent, env *ClientEnvironment) (*ClientStateTransition,
	error) {

	// Any transition into ClientFailedState from this pre-signing state
	// returns forfeit-reserved inputs to LiveState; see
	// releaseForfeitsOnFailure.
	transition, err := s.processEvent(ctx, event, env)

	return releaseForfeitsOnFailure(
		transition, err, fn.Some(s.RoundID), s.Intents.Forfeits,
	)
}

// processEvent runs the NoncesAggregatedState event handling; ProcessEvent
// wraps it to return forfeit-reserved inputs to LiveState on failure.
func (s *NoncesAggregatedState) processEvent(ctx context.Context,
	event ClientEvent, env *ClientEnvironment) (*ClientStateTransition,
	error) {

	switch evt := event.(type) {
	case *GeneratePartialSigs:
		env.Log.InfoS(
			ctx,
			"Generating partial signatures for VTXO tree",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("session_count", len(s.Musig2Sessions)),
		)

		// At this stage, the nonces have been aggregated for each
		// client, so now we'll generate and send our partial
		// signatures. The server expects signatures grouped by signer
		// key first, then by transaction ID.
		sessionResults := make(
			[]SignerSessionResult, 0, len(s.Musig2Sessions),
		)
		signerKeys := slices.Collect(maps.Keys(s.Musig2Sessions))
		slices.SortFunc(signerKeys, func(a, b SignerKey) int {
			return bytes.Compare(a[:], b[:])
		})
		for _, signerKey := range signerKeys {
			session := s.Musig2Sessions[signerKey]
			if session == nil {
				cleanupErr := cleanupSignerSessions(
					s.Musig2Sessions,
				)

				return nil, errors.Join(
					fmt.Errorf("signing session for "+
						"client %x not found",
						signerKey[:]),
					cleanupErr,
				)
			}

			sessionResults = append(
				sessionResults, SignerSessionResult{
					SignerKey: signerKey,
					Session:   session,
				},
			)
		}

		signatureResults, err := env.signingExecutor().Sign(
			ctx, sessionResults,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to generate partial "+
				"signatures: %w", err)
		}

		allSignatures := make(
			map[SignerKey]map[tree.TxID]*musig2.PartialSignature,
			len(signatureResults),
		)
		for _, result := range signatureResults {
			allSignatures[result.SignerKey] = result.Signatures
		}

		// Create a single message with all signatures grouped by signer
		// key.
		submitPartialSigsMsg := &SubmitPartialSigRequest{
			RoundID:    s.RoundID,
			Signatures: allSignatures,
		}

		env.Log.InfoS(ctx, "Sending partial signatures to server",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("signer_key_count", len(allSignatures)),
		)

		// Partial MuSig2 signatures have been generated using the
		// aggregated nonces. Send them to the server for signature
		// aggregation.
		return &ClientStateTransition{
			NextState: &PartialSigsSentState{
				RoundID:              s.RoundID,
				CommitmentTx:         s.CommitmentTx,
				VTXOTreePaths:        s.VTXOTreePaths,
				SweepDelay:           s.SweepDelay,
				FlowVersion:          s.FlowVersion,
				ForfeitKey:           s.ForfeitKey,
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

	case *BoardingFailed:
		// Still pre-signing (VTXO forfeit sigs only cross to the server
		// on the ForfeitSignaturesCollecting -> InputSigSent
		// transition): fail into ClientFailedState with the typed code
		// and let ProcessEvent's releaseForfeitsOnFailure wrapper
		// release the forfeits and retire the job on a terminal-for-job
		// code.
		return &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      evt.Reason,
				Error:       evt.Error,
				Recoverable: evt.Recoverable,
				FailureCode: evt.FailureCode,
			},
		}, nil

	default:
		// Self-loop on unknown events - do not halt the FSM.
		return selfLoop(s), nil
	}
}

// ProcessEvent for PartialSigsSentState.
func (s *PartialSigsSentState) ProcessEvent(ctx context.Context,
	event ClientEvent, env *ClientEnvironment) (*ClientStateTransition,
	error) {

	// Still pre-signing: this state submits MuSig2 partial sigs, not VTXO
	// forfeit sigs (those go out only when ForfeitSignaturesCollectingState
	// transitions to InputSigSentState). Any transition into
	// ClientFailedState may safely return forfeit-reserved inputs to
	// LiveState; see releaseForfeitsOnFailure.
	transition, err := s.processEvent(ctx, event, env)

	return releaseForfeitsOnFailure(
		transition, err, fn.Some(s.RoundID), s.Intents.Forfeits,
	)
}

// processEvent runs the PartialSigsSentState event handling; ProcessEvent wraps
// it to return forfeit-reserved inputs to LiveState on failure.
func (s *PartialSigsSentState) processEvent(ctx context.Context,
	event ClientEvent, env *ClientEnvironment) (*ClientStateTransition,
	error) {

	switch evt := event.(type) {
	case *OperatorSigned:
		env.Log.InfoS(
			ctx,
			"Received aggregated signatures from operator",
			slog.String("round_id", evt.RoundID.String()),
			slog.Int("agg_sig_count", len(evt.AggSigs)),
		)

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
					"failed to propagate sigs to client "+
						"tree", err, false,
					fn.Some(s.RoundID),
				), nil
			}

			if err := clientTree.VerifySigned(); err != nil {
				return failWithNotification(
					"client tree sig verification failed",
					err, false, fn.Some(s.RoundID),
				), nil
			}
		}

		env.Log.InfoS(ctx, "Validated aggregated signatures",
			slog.String("round_id", s.RoundID.String()),
			slog.Int(
				"forfeit_mapping_count", len(s.ForfeitMappings),
			),
		)

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
			slog.Int(
				"boarding_intent_count",
				len(s.Intents.Boarding),
			),
		)

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

		// Watch the validated batch output (the output that receives
		// this client's funds) for confirmation tracking, falling back
		// to output 0 for rounds with no VTXO trees.
		commitTx := s.CommitmentTx.UnsignedTx
		pkScript := confirmationWatchScript(commitTx, s.VTXOTreePaths)

		env.Log.InfoS(ctx, "Building RegisterConfirmationRequest",
			slog.String("round_id", s.RoundID.String()),
			slog.String("txid", txid.String()),
			slog.Int("num_outputs", len(commitTx.TxOut)),
			slog.Int("pkscript_len", len(pkScript)),
			slog.Int(
				"target_confs",
				int(env.OperatorTerms.MinConfirmations),
			),
		)

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
			FlowVersion:   s.FlowVersion,
		}

		env.Log.InfoS(
			ctx,
			"Signed boarding inputs, checkpointing round state",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("boarding_sig_count", len(boardingInputSigs)),
		)

		// Checkpoint round data + FSM state atomically at the "point
		// of no return". The next state is persisted so restart can
		// recover to InputSigSentState. For boarding-only rounds,
		// ForfeitedVTXOs is nil.
		nextState := &InputSigSentState{
			RoundID:       s.RoundID,
			CommitmentTx:  s.CommitmentTx,
			VTXOTreePaths: s.VTXOTreePaths,
			SweepDelay:    s.SweepDelay,
			FlowVersion:   s.FlowVersion,
			ForfeitKey:    s.ForfeitKey,
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

		env.Log.InfoS(
			ctx,
			"Round state checkpointed at point of no return",
			slog.String("round_id", s.RoundID.String()),
			slog.String("commitment_txid", txid.String()),
		)

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
				FailureCode: evt.FailureCode,
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
	ctx context.Context, env *ClientEnvironment) (*ClientStateTransition,
	error) {

	// Derive this round's forfeit penalty output script from the per-round
	// forfeit key (replacing the removed global GetInfo forfeit script).
	forfeitScript, err := forfeitPenaltyScript(s.ForfeitKey)
	if err != nil {

		// Error carried into failed state.
		return &ClientStateTransition{ //nolint:nilerr
			NextState: &ClientFailedState{
				Reason:      "invalid round forfeit key",
				Error:       err,
				Recoverable: false,
			},
		}, nil
	}

	// Arm the forfeit-collection timeout FIRST, before dispatching any
	// per-VTXO forfeit requests. This transition advances the in-memory
	// FSM to ForfeitSignaturesCollectingState without checkpointing, so a
	// restart cannot recover it. If a later ForfeitRequestToVTXO Tell
	// fails, processOutbox aborts on that error and never reaches a
	// trailing timeout, leaving the round wedged waiting for forfeit
	// signatures forever. Emitting StartTimeoutReq first guarantees the
	// timeout is scheduled regardless of any subsequent per-VTXO send
	// failure, so the round can still time out into a recoverable failed
	// state.
	var outbox []ClientOutMsg
	outbox = append(outbox, &StartTimeoutReq{
		RoundKey: RoundKeyStr(s.RoundID.KeyString()),
		Phase:    TimeoutPhaseForfeitCollection,
		Duration: env.ForfeitCollectionTimeout,
	})

	// Build forfeit request messages for each VTXO being refreshed.
	forfeitReqs := forfeitRequestMap(s.Intents.Forfeits)
	for vtxoOutpoint, info := range s.ForfeitMappings {
		req := forfeitReqs[vtxoOutpoint]
		msg := &ForfeitRequestToVTXO{
			VTXOOutpoint:          vtxoOutpoint,
			RoundID:               s.RoundID.String(),
			ConnectorOutpoint:     info.ConnectorOutpoint,
			ConnectorPkScript:     info.ConnectorPkScript,
			ConnectorAmount:       info.ConnectorAmount,
			ServerForfeitPkScript: forfeitScript,
			ForfeitSpend:          req.ForfeitSpend,
		}
		outbox = append(outbox, msg)
	}

	env.Log.InfoS(ctx, "Transitioning to forfeit collection",
		slog.String("round_id", s.RoundID.String()),
		slog.Int("forfeit_count", len(s.ForfeitMappings)),
		slog.Duration("forfeit_timeout", env.ForfeitCollectionTimeout),
	)

	// Transition to forfeit collection state. After collecting all forfeit
	// signatures, that state will sign boarding inputs and transition to
	// InputSigSent.
	collectedForfeits := make(map[wire.OutPoint]*ForfeitSignatureResponse)

	return &ClientStateTransition{
		NextState: &ForfeitSignaturesCollectingState{
			RoundID:              s.RoundID,
			CommitmentTx:         s.CommitmentTx,
			VTXOTreePaths:        s.VTXOTreePaths,
			SweepDelay:           s.SweepDelay,
			FlowVersion:          s.FlowVersion,
			ForfeitKey:           s.ForfeitKey,
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

// forfeitRollbackOutpoints splits forfeits into standard wallet reservations
// that should be released and custom refresh inputs that should be dropped when
// a round fails before signing. Custom forfeits carry explicit spend paths
// because they are not normal wallet VTXOs.
func forfeitRollbackOutpoints(requests []types.ForfeitRequest) ([]wire.OutPoint,
	[]wire.OutPoint) {

	standardSeen := make(map[wire.OutPoint]struct{}, len(requests))
	customSeen := make(map[wire.OutPoint]struct{}, len(requests))
	standard := make([]wire.OutPoint, 0, len(requests))
	custom := make([]wire.OutPoint, 0, len(requests))

	for i := range requests {
		req := requests[i]
		if req.VTXOOutpoint == nil {
			continue
		}

		op := *req.VTXOOutpoint
		if req.AuthSpend != nil || req.ForfeitSpend != nil {
			if _, ok := customSeen[op]; ok {
				continue
			}

			customSeen[op] = struct{}{}
			custom = append(custom, op)

			continue
		}

		if _, ok := standardSeen[op]; ok {
			continue
		}

		standardSeen[op] = struct{}{}
		standard = append(standard, op)
	}

	return standard, custom
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

// populateForfeitMappingAmounts copies server connector mappings and annotates
// them with the locally-known VTXO amounts needed to validate forfeit tx value.
func populateForfeitMappingAmounts(
	mappings map[wire.OutPoint]*ConnectorLeafInfo,
	requests []types.ForfeitRequest) (map[wire.OutPoint]*ConnectorLeafInfo,
	error) {

	if len(mappings) == 0 {
		return mappings, nil
	}

	requestIndex := forfeitRequestMap(requests)
	populated := make(map[wire.OutPoint]*ConnectorLeafInfo, len(mappings))
	for outpoint, info := range mappings {
		if info == nil {
			return nil, fmt.Errorf("nil connector info for %s",
				outpoint)
		}

		req, ok := requestIndex[outpoint]
		if !ok {
			return nil, fmt.Errorf("missing local forfeit "+
				"request for %s", outpoint)
		}
		if req.Amount <= 0 {
			return nil, fmt.Errorf("invalid local forfeit amount "+
				"%d for %s", req.Amount, outpoint)
		}

		infoCopy := *info
		infoCopy.VTXOAmount = req.Amount
		populated[outpoint] = &infoCopy
	}

	return populated, nil
}

const (
	// maxConnectorTreeLeaves bounds the number of connector leaves the
	// client will reconstruct from an operator-supplied NumLeaves. A
	// connector tree carries one leaf per forfeited VTXO assigned to its
	// connector output, so even very large rounds stay far below this.
	// The cap exists only to bound reconstruction memory so a malformed or
	// malicious leaf count cannot drive an out-of-memory DoS; 2^16 is far
	// above any realistic round while keeping reconstruction cheap.
	maxConnectorTreeLeaves = 1 << 16

	// maxConnectorTreeRadix bounds the operator-supplied connector tree
	// branching factor. Real connector trees use a small radix (binary by
	// default); this generous ceiling rejects nonsensical values without
	// constraining any plausible operator configuration.
	maxConnectorTreeRadix = 1 << 10
)

// validateConnectorAncestry proves that every operator-supplied connector leaf
// the client is about to forfeit against descends from an output of this
// round's commitment tx. Connector trees have identical leaves and a
// deterministic BFS layout, so the client reconstructs the exact tree from the
// scalars the operator sent (root output index, leaf count, radix) plus the
// committed connector output and the operator key — no tree transactions cross
// the wire — and asserts the assigned leaf is the one at the claimed index.
//
// Binding the connector to the commitment tx is what preserves round
// atomicity: a connector leaf is only ever spendable once the commitment tx
// confirms, so the old VTXO cannot be forfeited unless the replacement round
// also commits (wavelength#681).
func validateConnectorAncestry(commitmentTx *wire.MsgTx,
	operatorKey *btcec.PublicKey,
	mappings map[wire.OutPoint]*ConnectorLeafInfo) error {

	if len(mappings) == 0 {
		return nil
	}

	switch {
	case commitmentTx == nil:
		return fmt.Errorf("commitment tx is nil")

	case operatorKey == nil:
		return fmt.Errorf("operator key is nil")
	}

	commitmentTxID := commitmentTx.TxHash()

	for vtxoOutpoint, info := range mappings {
		if info == nil {
			return fmt.Errorf("nil connector info for %s",
				vtxoOutpoint)
		}

		if err := verifyConnectorLeaf(
			commitmentTx, commitmentTxID, operatorKey, info,
		); err != nil {
			return fmt.Errorf("connector for forfeited VTXO %s: %w",
				vtxoOutpoint, err)
		}
	}

	return nil
}

// verifyConnectorLeaf reconstructs the connector tree rooted at the commitment
// tx output named by info.RootOutputIndex and confirms that info's assigned
// connector leaf (outpoint + output) is the leaf at info.LeafIndex of that
// tree. Reconstruction uses the same deterministic builder as the operator, so
// any divergence in the leaf data, root output, leaf count, radix, or operator
// key is caught here, before the client signs the forfeit.
func verifyConnectorLeaf(commitmentTx *wire.MsgTx,
	commitmentTxID chainhash.Hash, operatorKey *btcec.PublicKey,
	info *ConnectorLeafInfo) error {

	// Compare as uint32 so a value that would overflow a signed 32-bit int
	// cannot wrap past the bounds check on 32-bit architectures.
	rootIdx := info.RootOutputIndex
	if rootIdx >= uint32(len(commitmentTx.TxOut)) {
		return fmt.Errorf("root output index %d out of range "+
			"(commitment tx has %d outputs)", rootIdx,
			len(commitmentTx.TxOut))
	}

	// NumLeaves and Radix are operator-supplied and drive tree
	// reconstruction, so bound them before allocating anything: an
	// unbounded leaf count is an out-of-memory DoS vector.
	switch {
	case info.NumLeaves == 0:
		return fmt.Errorf("num leaves must be positive")

	case info.NumLeaves > maxConnectorTreeLeaves:
		return fmt.Errorf("num leaves %d exceeds maximum %d",
			info.NumLeaves, maxConnectorTreeLeaves)

	case info.Radix < 2:
		return fmt.Errorf("radix must be at least 2, got %d",
			info.Radix)

	case info.Radix > maxConnectorTreeRadix:
		return fmt.Errorf("radix %d exceeds maximum %d", info.Radix,
			maxConnectorTreeRadix)
	}
	numLeaves := int(info.NumLeaves)
	radix := int(info.Radix)

	if info.LeafIndex < 0 || info.LeafIndex >= numLeaves {
		return fmt.Errorf("leaf index %d out of range for %d leaves",
			info.LeafIndex, numLeaves)
	}

	if info.ConnectorAmount <= 0 {
		return fmt.Errorf("connector leaf amount must be "+
			"positive, got %d", info.ConnectorAmount)
	}

	// The root output funds exactly numLeaves dust leaves; if its value
	// does not divide cleanly the operator's parameters are inconsistent
	// with the committed output and reconstruction would not match. Guard
	// the multiplication against int64 overflow first so a huge leaf
	// amount cannot wrap to a value that spuriously matches the output.
	rootOutput := commitmentTx.TxOut[rootIdx]
	if int64(numLeaves) > math.MaxInt64/info.ConnectorAmount {
		return fmt.Errorf("num leaves %d and leaf amount %d "+
			"overflow int64", numLeaves, info.ConnectorAmount)
	}
	if rootOutput.Value != int64(numLeaves)*info.ConnectorAmount {
		return fmt.Errorf("root output value %d != num leaves %d * "+
			"leaf amount %d", rootOutput.Value, numLeaves,
			info.ConnectorAmount)
	}

	rootOutpoint := wire.OutPoint{
		Hash:  commitmentTxID,
		Index: rootIdx,
	}

	// Reconstruct the connector tree exactly as the operator built it.
	connTree, err := tree.BuildConnectorTree(
		rootOutpoint, rootOutput, tree.ConnectorDescriptor{
			PkScript:  rootOutput.PkScript,
			NumLeaves: numLeaves,
			Amount:    btcutil.Amount(info.ConnectorAmount),
		}, operatorKey, radix,
	)
	if err != nil {
		return fmt.Errorf("reconstruct connector tree: %w", err)
	}

	// Verify the reconstructed structure: the root spends the commitment
	// output and every child spends its parent at the claimed index.
	if err := connTree.Verify(); err != nil {
		return fmt.Errorf("connector tree structure invalid: %w", err)
	}

	leaves := connTree.Root.GetLeafNodes()
	if len(leaves) != numLeaves {
		return fmt.Errorf("reconstructed tree has %d leaves, "+
			"expected %d", len(leaves), numLeaves)
	}

	// The assigned connector leaf must be the leaf at the claimed index.
	// Matching the outpoint binds the entire leaf transaction (the
	// outpoint hash is the leaf tx hash), proving it descends from the
	// commitment tx.
	leaf := leaves[info.LeafIndex]
	if leaf == nil {
		return fmt.Errorf("reconstructed leaf at index %d is nil",
			info.LeafIndex)
	}
	gotOutpoint, err := leaf.GetNonAnchorOutpoint()
	if err != nil {
		return fmt.Errorf("connector leaf outpoint: %w", err)
	}
	if *gotOutpoint != info.ConnectorOutpoint {
		return fmt.Errorf("assigned connector outpoint %s is not leaf "+
			"%d of the reconstructed tree (%s)",
			info.ConnectorOutpoint, info.LeafIndex, gotOutpoint)
	}

	// Cross-check the assigned connector output (the value and script the
	// forfeit penalty is built against) against the reconstructed leaf so
	// a leaf_output inconsistent with the proven outpoint cannot slip
	// through.
	gotOutput, err := connectorLeafOutput(leaf)
	if err != nil {
		return err
	}
	if gotOutput.Value != info.ConnectorAmount {
		return fmt.Errorf("connector leaf value %d != assigned "+
			"amount %d", gotOutput.Value, info.ConnectorAmount)
	}
	if !bytes.Equal(gotOutput.PkScript, info.ConnectorPkScript) {
		return fmt.Errorf("connector leaf script does not match " +
			"assigned connector script")
	}

	return nil
}

// connectorLeafOutput returns the single non-anchor output of a connector leaf
// node.
func connectorLeafOutput(leaf *tree.Node) (*wire.TxOut, error) {
	if leaf == nil {
		return nil, fmt.Errorf("connector leaf node is nil")
	}

	var found *wire.TxOut
	for _, out := range leaf.Outputs {
		if bytes.Equal(out.PkScript, arkscript.AnchorPkScript) {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("connector leaf has multiple " +
				"non-anchor outputs")
		}
		found = out
	}
	if found == nil {
		return nil, fmt.Errorf("connector leaf has no non-anchor " +
			"output")
	}

	return found, nil
}

// validateVTXOTreeBinding proves that every operator-supplied VTXO tree is
// rooted in this round's commitment tx. Internal tree self-consistency is not
// enough: a tree's BatchOutpoint and BatchOutput come from the same untrusted
// source as the rest of the tree, so a self-consistent tree rooted at the
// wrong commitment output can pass ValidatePath/ValidateAnchors and still be
// co-signed. Once the commitment tx confirms, such a tree does not spend the
// real batch output, so the client's new VTXOs (whose outpoints are derived
// from the tree root) are unrecoverable. This is the VTXO-tree counterpart to
// validateConnectorAncestry; together they restore round atomicity
// (wavelength#680, companion to #681).
//
// For each (outputIdx, tree) pair it asserts that the tree's BatchOutpoint
// names this commitment tx, that outputIdx agrees with BatchOutpoint.Index,
// that the index is in range, that the committed output byte-matches the
// tree's BatchOutput, and that the committed script equals the taproot script
// recomputed from the tree root's cosigner set and sweep tapscript root (so a
// substituted-but-self-consistent script is rejected).
func validateVTXOTreeBinding(commitmentTx *wire.MsgTx,
	vtxoTrees map[int]*tree.Tree) error {

	if len(vtxoTrees) == 0 {
		return nil
	}

	if commitmentTx == nil {
		return fmt.Errorf("commitment tx is nil")
	}

	commitmentTxID := commitmentTx.TxHash()

	for outputIdx, vtxoTree := range vtxoTrees {
		if err := verifyVTXOTreeRoot(
			commitmentTx, commitmentTxID, outputIdx, vtxoTree,
		); err != nil {
			return fmt.Errorf("vtxo tree at output %d: %w",
				outputIdx, err)
		}
	}

	return nil
}

// verifyVTXOTreeRoot checks that a single VTXO tree's root spends the
// commitment output named by outputIdx and that the committed output matches
// both the tree's claimed BatchOutput and the taproot script recomputed from
// the tree's declared cosigner set and sweep tapscript root.
func verifyVTXOTreeRoot(commitmentTx *wire.MsgTx, commitmentTxID chainhash.Hash,
	outputIdx int, vtxoTree *tree.Tree) error {

	switch {
	case vtxoTree == nil:
		return fmt.Errorf("nil tree")

	case vtxoTree.Root == nil:
		return fmt.Errorf("nil tree root")

	case vtxoTree.BatchOutput == nil:
		return fmt.Errorf("nil batch output")

	case len(vtxoTree.Root.CoSigners) == 0:
		return fmt.Errorf("tree root has no cosigners")
	}

	// The map is keyed by commitment-tx output index, so a tree filed under
	// a key that disagrees with its own BatchOutpoint.Index is internally
	// inconsistent about which output it spends. Reject before trusting
	// either value. Compare as int64 so the check is lossless for any int
	// outputIdx (no uint32 truncation) and naturally rejects negative keys.
	if int64(outputIdx) != int64(vtxoTree.BatchOutpoint.Index) {
		return fmt.Errorf("map key %d does not match batch outpoint "+
			"index %d", outputIdx, vtxoTree.BatchOutpoint.Index)
	}

	// Bind the tree to this commitment tx: the root must spend an output of
	// the tx we are about to sign into, not some other transaction.
	if vtxoTree.BatchOutpoint.Hash != commitmentTxID {
		return fmt.Errorf("batch outpoint hash %s does not match "+
			"commitment txid %s", vtxoTree.BatchOutpoint.Hash,
			commitmentTxID)
	}

	// Compare as uint32 so a value that would overflow a signed int cannot
	// wrap past the bounds check on 32-bit architectures.
	idx := vtxoTree.BatchOutpoint.Index
	if idx >= uint32(len(commitmentTx.TxOut)) {
		return fmt.Errorf("batch outpoint index %d out of range "+
			"(commitment tx has %d outputs)", idx,
			len(commitmentTx.TxOut))
	}
	committed := commitmentTx.TxOut[idx]

	// The trusted BatchOutput (used as the MuSig2 prevout when signing the
	// tree) must byte-match the real on-chain output, so the signature the
	// client contributes binds to the output that actually receives its
	// funds.
	if committed.Value != vtxoTree.BatchOutput.Value {
		return fmt.Errorf("committed output value %d != batch output "+
			"value %d", committed.Value, vtxoTree.BatchOutput.Value)
	}
	if !bytes.Equal(committed.PkScript, vtxoTree.BatchOutput.PkScript) {
		return fmt.Errorf("committed output script does not match " +
			"batch output script")
	}

	// Finally, recompute the root taproot script from the tree's declared
	// cosigner set and sweep tapscript root and require it to equal the
	// committed script. BatchOutput.PkScript is operator-supplied, so the
	// byte-match above only proves the operator put the same bytes on-chain
	// and in the tree; without this an operator could commit a taproot
	// output whose key is not the aggregate of the declared cosigners,
	// leaving the co-signed tree unable to ever spend it. We recompute from
	// the declared parameters rather than trusting Root.FinalKey, which is
	// itself operator-supplied.
	finalKey, err := tree.ComputeFinalKey(
		vtxoTree.Root.CoSigners, vtxoTree.SweepTapscriptRoot,
	)
	if err != nil {
		return fmt.Errorf("recompute tree root key: %w", err)
	}
	rootScript, err := txscript.PayToTaprootScript(finalKey)
	if err != nil {
		return fmt.Errorf("recompute tree root script: %w", err)
	}
	if !bytes.Equal(rootScript, committed.PkScript) {
		return fmt.Errorf("committed output script does not match " +
			"script recomputed from tree cosigners and sweep root")
	}

	return nil
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

// forfeitPenaltyScript derives the forfeit-tx penalty output script for a
// round from the operator's per-round forfeit key. The script is a BIP-86
// key-spend (no script tree) to the forfeit key, so the server can claim the
// penalty output with a single Schnorr signature. The forfeit key is
// delivered per round in ClientBatchInfo and replaces the removed global
// GetInfo forfeit script.
func forfeitPenaltyScript(forfeitKey *btcec.PublicKey) ([]byte, error) {
	if forfeitKey == nil {
		return nil, fmt.Errorf("round forfeit key not set")
	}

	taprootKey := txscript.ComputeTaprootKeyNoScript(forfeitKey)

	return txscript.PayToTaprootScript(taprootKey)
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
			ctx, types.VTXOSigningKeyFamily,
		)
		if err != nil {
			return nil, fmt.Errorf("derive signing key for vtxo "+
				"%d: %w", i, err)
		}

		updated[i].SigningKey = *keyDesc
	}

	return updated, nil
}

// computeClientOperatorFee derives the per-client operator fee
// contributed to this round. Under the Ark round model the
// client's fee equals the difference between its contributed input
// value (boarding inputs + forfeited VTXOs) and every output value
// this client requested from those inputs (owned VTXOs, foreign
// directed-send recipient VTXOs, and cooperative leave outputs).
// Recipient and leave values are booked separately as VTXOSentMsg
// outflows; they must not be folded into the operator fee.
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
// paid.
func computeClientOperatorFee(intents Intents, ownedVTXOs []*ClientVTXO) int64 {
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

	// Locally owned outputs are counted from the BUILT VTXOs, not the
	// intent requests. Under the seal-time fee handshake (#270) an intent's
	// Amount is the pre-fee target — the server's quote residual is what
	// actually seals into the leaf — so summing intent amounts cancels the
	// inputs exactly and computes a zero fee for every fee-charging round.
	// The built ClientVTXO carries the sealed leaf value
	// (leafNonAnchorAmount), making input − output the true operator fee.
	// Foreign outputs (directed-send recipient slots) never materialize as
	// owned VTXOs, so their intent amount remains the only local record of
	// their value and is used as-is.
	for i := range intents.VTXOs {
		if intents.VTXOs[i].HasLocalOwner() {
			continue
		}

		amt := int64(intents.VTXOs[i].Amount)
		if amt > 0 {
			outputsSat += amt
		}
	}

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

// roundLedgerOutflows returns the round outputs that reduce this
// client's VTXO balance without producing a locally owned VTXO. The
// resulting rows are separate from operator fees so directed-send
// recipient value and cooperative leave value remain visible in the
// transfers_out account.
func roundLedgerOutflows(roundID RoundID,
	intents Intents) []RoundLedgerOutflow {

	var outflows []RoundLedgerOutflow

	for i := range intents.VTXOs {
		req := &intents.VTXOs[i]
		if req.HasLocalOwner() {
			continue
		}

		amt := int64(req.Amount)
		if amt <= 0 {
			continue
		}

		outflows = append(outflows, RoundLedgerOutflow{
			AmountSat:      amt,
			IdempotencyKey: roundOutflowKey(roundID, "vtxo", i),
		})
	}

	for i := range intents.Leaves {
		amt := intents.LeaveAmount(i)
		if amt <= 0 {
			continue
		}

		outflows = append(outflows, RoundLedgerOutflow{
			AmountSat:      amt,
			IdempotencyKey: roundOutflowKey(roundID, "leave", i),
		})
	}

	return outflows
}

// roundOutflowKey returns a deterministic per-round outflow key for
// outputs that do not have a local VTXO outpoint.
func roundOutflowKey(roundID RoundID, kind string, index int) []byte {
	key := fmt.Sprintf("round-outflow:%s:%s:%d", roundID.String(), kind,
		index)

	return []byte(key)
}

// roundOperatorFeeType classifies a positive operator fee by the
// client's round composition. Boarding takes precedence because wallet
// funds entered the Ark layer in that round; all other client-paid
// operator fees are refresh-style VTXO spends.
func roundOperatorFeeType(intents Intents) string {
	if len(intents.Boarding) > 0 {
		return ledger.FeeTypeBoarding
	}

	for i := range intents.VTXOs {
		if intents.VTXOs[i].Origin == types.VTXOOriginRoundBoarding {
			return ledger.FeeTypeBoarding
		}
	}

	return ledger.FeeTypeRefresh
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
				return nil, fmt.Errorf("check owned script: %w",
					err)
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
			return nil, fmt.Errorf("missing client tree for " +
				"signing key")
		}

		leaves := clientTree.Root.GetLeafNodes()
		if len(leaves) != 1 {
			return nil, fmt.Errorf("expected exactly 1 leaf for "+
				"signing key, got %d", len(leaves))
		}
		leaf := leaves[0]

		outpoint, err := leaf.GetNonAnchorOutpoint()
		if err != nil {
			return nil, fmt.Errorf("failed to derive VTXO "+
				"outpoint: %w", err)
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

		// Stamp the round-direct ancestry fragment now. The
		// CommitmentTxID is filled in later by the confirmation
		// path once evt.TxID is known; persisting with a zero
		// txid would leave the side-table row unbound to its
		// commitment tx.
		ancestry := []types.Ancestry{{
			TreePath:  clientTree,
			TreeDepth: uint32(clientTree.Depth()),
		}}

		vtxo := &ClientVTXO{
			Outpoint:       *outpoint,
			Amount:         leafAmount,
			PolicyTemplate: policyTemplate,
			PkScript:       pkScript,
			OwnerKey:       req.OwnerKey,
			Ancestry:       ancestry,
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

// batchOutputIndex returns the commitment output used for the batch
// confirmation watch. It mirrors confirmationWatchScript so the persisted
// evidence binds the same output the chain backend observes.
func batchOutputIndex(commitmentTx *wire.MsgTx,
	vtxoTrees map[int]*tree.Tree) (uint32, error) {

	if commitmentTx == nil || len(commitmentTx.TxOut) == 0 {
		return 0, fmt.Errorf("commitment transaction has no outputs")
	}

	idx := 0
	if len(vtxoTrees) > 0 {
		idx = -1
		for outputIdx := range vtxoTrees {
			if idx == -1 || outputIdx < idx {
				idx = outputIdx
			}
		}
	}
	if idx < 0 || idx >= len(commitmentTx.TxOut) {
		return 0, fmt.Errorf("batch output index %d is out of range",
			idx)
	}

	return uint32(idx), nil
}

// creatorLineage returns every distinct commitment transaction that must
// remain canonical for a VTXO to exist.
func creatorLineage(vtxo *ClientVTXO) ([]chainhash.Hash, error) {
	lineage := make([]chainhash.Hash, 0, len(vtxo.Ancestry)+1)
	seen := make(map[chainhash.Hash]struct{}, len(vtxo.Ancestry)+1)
	add := func(txid chainhash.Hash) {
		if txid == (chainhash.Hash{}) {
			return
		}
		if _, ok := seen[txid]; ok {
			return
		}
		seen[txid] = struct{}{}
		lineage = append(lineage, txid)
	}

	add(vtxo.CommitmentTxID)
	for _, ancestor := range vtxo.Ancestry {
		add(ancestor.CommitmentTxID)
	}
	if len(lineage) == 0 {
		return nil, fmt.Errorf("VTXO %s has no creator lineage",
			vtxo.Outpoint)
	}

	return lineage, nil
}

// roundBatchRegistration builds complete authenticated batch evidence from
// the commitment PSBT retained by the round FSM. Every actual transaction
// input is paired with its WitnessUtxo, and refresh inputs additionally bind
// the exact next lifecycle revision plus their complete creator lineage.
func roundBatchRegistration(ctx context.Context, state *InputSigSentState,
	vtxos []*ClientVTXO, batchTxID chainhash.Hash, store VTXOStore,
	watchHeightHint uint32) (*batchcanon.RegisterBatchRequest, error) {

	if state.CommitmentTx == nil || state.CommitmentTx.UnsignedTx == nil {
		return nil, fmt.Errorf("commitment PSBT is missing")
	}

	commitmentTx := state.CommitmentTx.UnsignedTx
	if commitmentTx.TxHash() != batchTxID {
		return nil, fmt.Errorf("confirmed txid %s does not match "+
			"commitment transaction %s", batchTxID,
			commitmentTx.TxHash())
	}
	if len(state.CommitmentTx.Inputs) != len(commitmentTx.TxIn) {
		return nil, fmt.Errorf("commitment PSBT has %d input records "+
			"for %d transaction inputs",
			len(state.CommitmentTx.Inputs), len(commitmentTx.TxIn))
	}

	consumedInputs := make(
		[]batchcanon.ConsumedInput, 0, len(commitmentTx.TxIn),
	)
	for idx, txIn := range commitmentTx.TxIn {
		prevOut := state.CommitmentTx.Inputs[idx].WitnessUtxo
		if prevOut == nil {
			return nil, fmt.Errorf("commitment input %d (%s) has "+
				"no WitnessUtxo", idx, txIn.PreviousOutPoint)
		}
		consumedInputs = append(
			consumedInputs, batchcanon.ConsumedInput{
				Outpoint: txIn.PreviousOutPoint,
				Value:    prevOut.Value,
				PkScript: bytes.Clone(prevOut.PkScript),
			},
		)
	}

	dependentVTXOs := make([]wire.OutPoint, 0, len(vtxos))
	for _, vtxo := range vtxos {
		dependentVTXOs = append(dependentVTXOs, vtxo.Outpoint)
	}

	consumerEdges := make(
		[]batchcanon.ConsumerEdge, 0, len(state.ForfeitedVTXOs),
	)
	for _, outpoint := range state.ForfeitedVTXOs {
		consumedVTXO, err := store.GetVTXO(ctx, outpoint)
		if err != nil {
			return nil, fmt.Errorf("load forfeited VTXO %s: %w",
				outpoint, err)
		}
		lineage, err := creatorLineage(consumedVTXO)
		if err != nil {
			return nil, err
		}
		consumerEdges = append(consumerEdges, batchcanon.ConsumerEdge{
			ConsumedVTXO:     outpoint,
			ConsumerBatch:    batchTxID,
			ExpectedRevision: consumedVTXO.BusinessRevision + 1,
			CreatorLineage:   lineage,
		})
	}

	outputIdx, err := batchOutputIndex(
		commitmentTx, state.VTXOTreePaths,
	)
	if err != nil {
		return nil, err
	}
	var serializedTx bytes.Buffer
	if err := commitmentTx.Serialize(&serializedTx); err != nil {
		return nil, fmt.Errorf("serialize commitment transaction: %w",
			err)
	}

	return &batchcanon.RegisterBatchRequest{
		BatchTxID:        batchTxID,
		BatchTx:          serializedTx.Bytes(),
		BatchOutputIndex: outputIdx,
		ConfirmationPkScript: bytes.Clone(
			commitmentTx.TxOut[outputIdx].PkScript,
		),
		WatchHeightHint: watchHeightHint,
		CSVExpiryDelta:  int32(state.SweepDelay),
		ConsumedInputs:  consumedInputs,
		DependentVTXOs:  dependentVTXOs,
		ConsumedVTXOs:   consumerEdges,
	}, nil
}

// ProcessEvent for InputSigSentState.
//
//nolint:funlen
func (s *InputSigSentState) ProcessEvent(ctx context.Context, event ClientEvent,
	env *ClientEnvironment) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *BoardingFailed:
		env.Log.WarnS(
			ctx,
			"Boarding failed while awaiting confirmation",
			nil,
			slog.String("round_id", s.RoundID.String()),
			slog.String("reason", evt.Reason),
		)

		// With no forfeit reservations at stake (a boarding-only
		// round), nothing can strand and nothing was signed away, so
		// the round fails immediately as before.
		if len(s.Intents.Forfeits) == 0 ||
			env.StatusReconcileTimeout <= 0 {
			return &ClientStateTransition{
				NextState: &ClientFailedState{
					Reason:      evt.Reason,
					Error:       evt.Error,
					Recoverable: evt.Recoverable,
					FailureCode: evt.FailureCode,
				},
			}, nil
		}

		// Forfeit signatures are already out, so the notification alone
		// cannot justify releasing the reservations: the operator holds
		// fully-signed forfeit txs, and a release is double-spend-safe
		// only once the round's commitment can never confirm
		// (wavelength#844). Park the failure and probe the operator for
		// the round's authoritative status; only a dead answer fails
		// the round and releases.
		env.Log.InfoS(ctx, "Reconciling round status before releasing "+
			"forfeit reservations",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("forfeit_count", len(s.Intents.Forfeits)),
		)

		next := *s
		next.PendingFailure = evt
		next.ReconcileProbes = 1

		return &ClientStateTransition{
			NextState: &next,
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: statusReconcileProbeOutbox(
					s.RoundID, env, 0,
				),
			}),
		}, nil

	case *StatusReconcileTimedOut:
		// The reconcile window expired with no confirmation, no
		// failure resolution, and no status answer. Probe (again) and
		// re-arm. The timeout alone never fails the round: with
		// forfeit signatures out, only an authoritative dead answer
		// makes the release safe. With no forfeits at stake the
		// timeout should not even be armed; self-loop defensively.
		if len(s.Intents.Forfeits) == 0 ||
			env.StatusReconcileTimeout <= 0 {
			return selfLoop(s), nil
		}

		env.Log.InfoS(ctx, "Status reconcile window expired, probing "+
			"operator for round status",
			slog.String("round_id", s.RoundID.String()),
			slog.Uint64("probes", uint64(s.ReconcileProbes)),
		)

		next := *s
		next.ReconcileProbes++

		return &ClientStateTransition{
			NextState: &next,
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: statusReconcileProbeOutbox(
					s.RoundID, env, s.ReconcileProbes,
				),
			}),
		}, nil

	case *RoundStatusReported:
		if evt.RoundID != s.RoundID {
			return selfLoop(s), nil
		}

		if evt.Status != roundStatusDead {
			// The round is in flight, broadcast, or confirmed: the
			// commitment may still confirm, so the forfeit
			// reservations must hold. Confirmation handling stays
			// with the registered confirmation notifier; the
			// re-armed reconcile timeout keeps probing if nothing
			// resolves.
			env.Log.InfoS(ctx, "Round status probe answered; "+
				"round not dead, holding forfeit reservations",
				slog.String("round_id", s.RoundID.String()),
				slog.String("status", evt.Status.String()),
			)

			return selfLoop(s), nil
		}

		// The operator has no live FSM and no durable record of this
		// round. A finalized round is persisted atomically with its
		// VTXOs before its commitment is ever broadcast, so a dead
		// answer proves the commitment can never confirm and the
		// forfeit signatures the operator may hold are unspendable.
		// Fail the round and release the reservations.
		//
		// Trust boundary: that proof holds for an honest-but-faulty
		// operator, the failure mode this reconcile exists for. An
		// operator that lies dead while secretly holding a
		// broadcastable commitment can race the released input's next
		// spend on chain; the alternative (never releasing without an
		// on-chain proof of death, which absence cannot provide) is
		// the permanent #844 strand for every honest failure. We
		// accept the operator's self-report here and keep the
		// commitment confirmation watch registered at checkpoint, so
		// a later fraudulent broadcast still surfaces as a detected
		// conflict rather than passing silently.
		failure := s.PendingFailure
		if failure == nil {
			reason := "round dead at operator"
			if evt.Detail != "" {
				reason = evt.Detail
			}

			// The zero FailureCode is deliberate: pure silence
			// (the lumos#618 door) carries no typed cause, and a
			// non-terminal-for-job code keeps the persisted
			// pending intent in recoverable replay, so the job
			// retries on a fresh round and simply re-reserves the
			// just-released inputs.
			failure = &BoardingFailed{
				RoundID:     fn.Some(s.RoundID),
				Reason:      reason,
				Recoverable: true,
			}
		}

		env.Log.WarnS(ctx, "Round confirmed dead by operator status "+
			"probe; failing round and releasing forfeit "+
			"reservations",
			nil,
			slog.String("round_id", s.RoundID.String()),
			slog.String("reason", failure.Reason),
			slog.Int("forfeit_count", len(s.Intents.Forfeits)),
		)

		transition := &ClientStateTransition{
			NextState: &ClientFailedState{
				Reason:      failure.Reason,
				Error:       failure.Error,
				Recoverable: failure.Recoverable,
				FailureCode: failure.FailureCode,
			},
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: []ClientOutMsg{
					cancelStatusReconcileTimeout(s.RoundID),
				},
			}),
		}

		// The release is safe here for the same reason it is safe in
		// the pre-signing states: the commitment can never confirm.
		// The wrapper also retires the originating job on a
		// terminal-for-job failure code.
		return releaseForfeitsOnFailure(
			transition, nil, fn.Some(s.RoundID), s.Intents.Forfeits,
		)

	case *BoardingConfirmed:
		env.Log.InfoS(ctx, "Commitment transaction confirmed",
			slog.String("round_id", s.RoundID.String()),
			slog.String("txid", evt.TxID.String()),
			slog.Int("block_height", int(evt.BlockHeight)),
			slog.Int("confirmations", int(evt.Confirmations)),
		)

		vtxos, err := buildClientVTXOs(
			ctx, env.OwnedScriptChecker, s.Intents, s.ClientTrees,
			s.RoundID,
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

		env.Log.InfoS(
			ctx,
			"Built client VTXOs from confirmed transaction",
			slog.String("round_id", s.RoundID.String()),
			slog.Int("vtxo_count", len(vtxos)),
		)

		// Compute batch expiry as absolute block height using this
		// round's sweep delay (delivered per round, not a global term).
		sweepDelay := int32(s.SweepDelay)
		batchExpiry := evt.BlockHeight + sweepDelay

		// Fill in round metadata so VTXOs are complete from the
		// first write. This avoids a race where callers read the
		// VTXO before the VTXO manager's second upsert populates
		// these fields. The per-fragment CommitmentTxID on each
		// Ancestry entry is also stamped here so the side-table
		// row binds to the commitment tx (the round path always
		// produces a single round-direct fragment).
		for _, cv := range vtxos {
			cv.CommitmentTxID = evt.TxID
			cv.BatchExpiry = batchExpiry
			cv.CreatedHeight = evt.BlockHeight
			for i := range cv.Ancestry {
				cv.Ancestry[i].CommitmentTxID = evt.TxID
			}
		}

		// Register the complete commitment and logical consumer lineage
		// before persisting any VTXO derived from it. The admission
		// gate is fail-closed, so a registration failure leaves the
		// round at its pre-exposure checkpoint and no new liquidity
		// becomes selectable.
		if env.BatchRegistrar != nil {
			opCtx := context.WithoutCancel(ctx)
			registration, err := roundBatchRegistration(
				opCtx, s, vtxos, evt.TxID, env.VTXOStore,
				env.StartHeight,
			)
			if err != nil {
				return nil, fmt.Errorf("build batch "+
					"registration: %w", err)
			}
			if err := env.BatchRegistrar.RegisterBatch(
				opCtx, registration,
			); err != nil {
				return nil, fmt.Errorf("register batch before "+
					"VTXO exposure: %w", err)
			}
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
				return nil, fmt.Errorf("failed to save "+
					"VTXOs: %w", err)
			}

			env.Log.InfoS(
				ctx,
				"Saved owned VTXOs to store, round complete",
				slog.String("round_id", s.RoundID.String()),
				slog.Int("vtxo_count", len(vtxos)),
			)
		}

		confInfo := ConfInfo{
			Height:    evt.BlockHeight,
			BlockHash: evt.BlockHash,
		}

		operatorFee := computeClientOperatorFee(s.Intents, vtxos)
		operatorFeeType := roundOperatorFeeType(s.Intents)
		outflows := roundLedgerOutflows(s.RoundID, s.Intents)

		// Build outbox messages starting with standard notifications.
		// The confirmation resolves the round's fate, so any armed
		// status-reconcile probe is disarmed first.
		outbox := make([]ClientOutMsg, 0, 3)
		if len(s.Intents.Forfeits) > 0 &&
			env.StatusReconcileTimeout > 0 {

			outbox = append(
				outbox, cancelStatusReconcileTimeout(s.RoundID),
			)
		}
		if len(vtxos) > 0 || len(outflows) > 0 || operatorFee > 0 {
			outbox = append(outbox, &VTXOCreatedNotification{
				VTXOs:           vtxos,
				Outflows:        outflows,
				RoundID:         s.RoundID.String(),
				CommitmentTxID:  evt.TxID,
				BatchExpiry:     batchExpiry,
				CreatedHeight:   evt.BlockHeight,
				OperatorFeeSat:  operatorFee,
				OperatorFeeType: operatorFeeType,
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
func (s *ClientFailedState) ProcessEvent(ctx context.Context, event ClientEvent,
	env *ClientEnvironment) (*ClientStateTransition, error) {

	switch evt := event.(type) {
	case *RecoveryInitiated:
		env.Log.InfoS(ctx, "Initiating CSV timeout recovery",
			btclog.Fmt("outpoint", "%v", evt.Outpoint),
			slog.String("sweep_txid", evt.SweepTxID.String()),
			slog.String("reason", evt.Reason),
		)

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
			evt.logAttributes()...)

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
func (s *RecoveryInitiatedState) ProcessEvent(_ context.Context,
	event ClientEvent, env *ClientEnvironment) (*ClientStateTransition,
	error) {

	// Semi-terminal state - self-loop on all events since the recovery
	// sweep transaction has been broadcast and we're waiting for
	// confirmation.
	return &ClientStateTransition{
		NextState: s,
	}, nil
}

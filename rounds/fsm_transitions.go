package rounds

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/ledger"
	"github.com/lightninglabs/darepo/metrics"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	// ErrJoinRequestInvalid is returned when a client's join request fails
	// validation.
	ErrJoinRequestInvalid = fmt.Errorf("join request invalid")

	// ErrJoinAuthHeightUnavailable is returned when join-auth validation
	// is enabled but no usable chain height is available.
	ErrJoinAuthHeightUnavailable = fmt.Errorf("join auth validation " +
		"height unavailable")

	// ErrChangeRequiredForBoarding is the typed error reserved for the
	// "no change with boarding inputs" failure mode. It is currently
	// not returned from buildCommitmentTx — LND's coin-selection
	// invariant bounds the dropped-change overpay to its dust limit
	// (~294-546 sat), so the no-change case proceeds with a warning
	// rather than failing. The error is kept defined so a future
	// stricter fee policy can re-enable the failure path with the
	// same external semantics.
	ErrChangeRequiredForBoarding = fmt.Errorf("change output required " +
		"when funding boarding inputs")

	// ErrDuplicateBoardingOutpoint is returned when buildCommitmentTx
	// is asked to fund a round whose boarding inputs (or the funded
	// PSBT returned by LND) contain the same outpoint twice. The
	// post-FundPsbt key-spend → script-spend swap is keyed by
	// PreviousOutPoint, so duplicates would either silently overwrite
	// the same metadata twice (and leave a real duplicate with
	// dummy key-spend metadata) or create an inconsistent PSBT.
	// Production validation prevents this upstream
	// (BoardingInputLocker, per-client outpoint set), but we
	// fail-closed here as defense-in-depth.
	ErrDuplicateBoardingOutpoint = fmt.Errorf("duplicate boarding " +
		"outpoint in funded psbt")

	// ErrBoardingPInputDecorated is returned when, after FundPsbt
	// returns, a boarding input's PSBT entry has been decorated with
	// fields we did not pre-populate (NonWitnessUtxo, PartialSigs,
	// non-taproot Bip32Derivation). The post-fund metadata swap is
	// wholesale (Inputs[idx] = pin), which is only safe under the
	// assumption that LND leaves external inputs alone. If that
	// assumption ever changes (e.g. LND starts decorating non-wallet
	// inputs), the wholesale swap would silently drop the new
	// fields. We fail-closed instead.
	ErrBoardingPInputDecorated = fmt.Errorf("boarding pinput decorated " +
		"by lnd: swap pattern unsafe")
)

// schnorrSigStackElemSize is the wire-witness contribution of a single
// schnorr signature when serialized as a stack element: one length byte
// plus the 64 byte signature itself. Two of these populate the boarding
// collab leaf's witness (cosigner + owner sigs).
const schnorrSigStackElemSize = 1 + schnorr.SignatureSize

// collabLeafWitnessSize is the leaf-witness size that
// input.TxWeightEstimator.AddTapscriptInput expects: the sum of all
// stack elements consumed by the revealed script, length-prefix
// included, but excluding the witness-element-count byte (which the
// estimator adds itself). For the collab leaf, that is exactly the two
// schnorr signatures.
const collabLeafWitnessSize lntypes.WeightUnit = 2 * schnorrSigStackElemSize

// unexpectedEvent returns a StateTransition that remains in the current state
// and logs a warning. This is used instead of returning an error to avoid
// crashing the FSM on unexpected events.
func unexpectedEvent(state State, stateName string, event Event,
	env *Environment) *StateTransition {

	env.Log.WarnS(context.Background(), "Ignoring unexpected event",
		nil,
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
		LogInputCount(len(inputs)),
	)

	for _, input := range inputs {
		err := env.BoardingInputLocker.Lock(
			ctx, input.Outpoint, env.RoundID,
		)
		if err != nil {
			env.Log.WarnS(ctx, "Failed to lock boarding input",
				err,
				LogOutpoint(input.Outpoint),
			)

			// If we fail to lock the boarding input, return an
			// error to the client but remain in the current state.
			return fmt.Errorf("failed to lock boarding input "+
				"%v: %v", input.Outpoint, err)
		}
	}

	env.Log.DebugS(ctx, "Boarding inputs locked successfully",
		LogInputCount(len(inputs)),
	)

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
		return errors.New("vtxo locker not configured")
	}

	owner := vtxo.RoundLockOwner(env.RoundID.String())
	err := env.VTXOLocker.LockMany(ctx, outpoints, owner)
	if err != nil {
		return fmt.Errorf("failed to lock forfeit VTXOs: %w", err)
	}

	return nil
}

// unlockBoardingInputs unlocks all boarding inputs for the given client
// registrations. This is called when a round fails to release all locked
// inputs. Errors are logged but don't stop the unlocking process, ensuring
// we attempt to unlock all inputs even if some fail.
func unlockBoardingInputs(ctx context.Context, env *Environment,
	clientRegs map[clientconn.ClientID]*ClientRegistration) {

	for _, reg := range clientRegs {
		for _, bi := range reg.BoardingInputs {
			err := env.BoardingInputLocker.Unlock(
				ctx, bi.Outpoint, env.RoundID,
			)
			if err != nil {
				env.Log.ErrorS(ctx, "Failed to unlock boarding "+
					"input", err,
					"outpoint", bi.Outpoint.String())
			}
		}
	}
}

// unlockForfeitVTXOsList unlocks a flat list of forfeit inputs. Used
// during seal-time quote fan-out to release locks on clients whose
// intent was rejected by the builder, and during QuoteSentState
// drop-client paths when per-client reject caps are hit. Errors are
// logged but do not stop the unlocking process.
func unlockForfeitVTXOsList(ctx context.Context, env *Environment,
	inputs []*ForfeitInput) {

	if len(inputs) == 0 {
		return
	}

	outpoints := make([]wire.OutPoint, 0, len(inputs))
	for _, fi := range inputs {
		if fi == nil || fi.Outpoint == nil {
			continue
		}
		outpoints = append(outpoints, *fi.Outpoint)
	}
	if len(outpoints) == 0 {
		return
	}

	if env.VTXOLocker == nil {
		err := env.VTXOStore.UnlockVTXO(
			ctx, env.RoundID, outpoints...,
		)
		if err != nil {
			env.Log.ErrorS(ctx, "Failed to unlock forfeit VTXOs",
				err,
				slog.Int("count", len(outpoints)),
			)
		}

		return
	}

	owner := vtxo.RoundLockOwner(env.RoundID.String())
	err := env.VTXOLocker.UnlockMany(ctx, outpoints, owner)
	if err != nil {
		env.Log.ErrorS(ctx, "Failed to unlock forfeit VTXOs",
			err,
			slog.Int("count", len(outpoints)),
		)
	}
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
		for _, fi := range reg.ForfeitInputs {
			outpoints = append(outpoints, *fi.Outpoint)
		}

		if env.VTXOLocker == nil {
			env.Log.ErrorS(ctx, "Failed to unlock forfeit VTXOs",
				errors.New("vtxo locker not configured"),
				"count", len(outpoints))

			continue
		}

		owner := vtxo.RoundLockOwner(env.RoundID.String())
		err := env.VTXOLocker.UnlockMany(ctx, outpoints, owner)
		if err != nil {
			env.Log.ErrorS(ctx, "Failed to unlock forfeit "+
				"VTXOs", err,
				"count", len(outpoints))
		}
	}
}

// releasePriorOnlyLocks releases the boarding-input and forfeit-VTXO
// locks held by a prior registration that are NOT carried over into
// the replacement registration. Outpoints present in both prior and
// new are retained (still locked under env.RoundID), since the new
// registration legitimately owns them. Called only on the success
// path of the re-registration replacement.
func releasePriorOnlyLocks(ctx context.Context, env *Environment,
	prior *ClientRegistration, result *JoinRequestResult) {

	newBoarding := fn.NewSet[wire.OutPoint]()
	for _, in := range result.BoardingInputs {
		if in != nil && in.Outpoint != nil {
			newBoarding.Add(*in.Outpoint)
		}
	}

	priorOnlyBoarding := make(
		[]*BoardingInput, 0, len(prior.BoardingInputs),
	)
	for _, in := range prior.BoardingInputs {
		if in == nil || in.Outpoint == nil {
			continue
		}
		if newBoarding.Contains(*in.Outpoint) {
			continue
		}
		priorOnlyBoarding = append(priorOnlyBoarding, in)
	}
	unlockBoardingInputsList(ctx, env, priorOnlyBoarding)

	newForfeit := fn.NewSet[wire.OutPoint]()
	for _, in := range result.ForfeitInputs {
		if in != nil && in.Outpoint != nil {
			newForfeit.Add(*in.Outpoint)
		}
	}

	priorOnlyForfeit := make([]*ForfeitInput, 0, len(prior.ForfeitInputs))
	for _, in := range prior.ForfeitInputs {
		if in == nil || in.Outpoint == nil {
			continue
		}
		if newForfeit.Contains(*in.Outpoint) {
			continue
		}
		priorOnlyForfeit = append(priorOnlyForfeit, in)
	}
	unlockForfeitVTXOsList(ctx, env, priorOnlyForfeit)
}

// rollbackNewBoardingInputs unwinds boarding-input locks that were
// freshly acquired in this handler when a subsequent step (e.g.
// forfeit-VTXO locking) fails. For a fresh admission this means every
// outpoint in newBoarding. For a re-registration the overlapping
// outpoints were already owned by this round from the prior
// registration, so we must not release those: only outpoints absent
// from prior.BoardingInputs are unwound.
func rollbackNewBoardingInputs(ctx context.Context, env *Environment,
	isReregistration bool, prior *ClientRegistration,
	newBoarding []*BoardingInput) {

	if !isReregistration || prior == nil {
		unlockBoardingInputsList(ctx, env, newBoarding)

		return
	}

	priorSet := fn.NewSet[wire.OutPoint]()
	for _, in := range prior.BoardingInputs {
		if in != nil && in.Outpoint != nil {
			priorSet.Add(*in.Outpoint)
		}
	}

	toRelease := make([]*BoardingInput, 0, len(newBoarding))
	for _, in := range newBoarding {
		if in == nil || in.Outpoint == nil {
			continue
		}
		if priorSet.Contains(*in.Outpoint) {
			continue
		}
		toRelease = append(toRelease, in)
	}
	unlockBoardingInputsList(ctx, env, toRelease)
}

// releaseWalletInputs releases UTXO leases acquired by a prior FundPsbt call.
// Errors are logged but do not halt execution, since failing to release a
// lease only means the UTXOs remain locked until the lease expires naturally.
func releaseWalletInputs(ctx context.Context, env *Environment, lockID [32]byte,
	lockedOutpoints []wire.OutPoint) {

	if len(lockedOutpoints) == 0 {
		return
	}

	err := env.WalletController.ReleaseInputs(
		ctx, lockID, lockedOutpoints,
	)
	if err != nil {
		env.Log.WarnS(ctx, "Failed to release wallet inputs",
			err,
			slog.Int("count", len(lockedOutpoints)),
		)
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
		IntentVTXOReqs:  result.IntentVTXOReqs,
		IntentLeaveReqs: result.IntentLeaveReqs,
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
//
// existingRegCount is the number of clients already admitted to this
// round (excluding the joining client). Threaded through to
// ValidateJoinRequestAtHeight for telemetry; under the seal-time
// fee handshake fee math is deferred to the seal-time builder.
func validateJoinRequestForAdmission(ctx context.Context, env *Environment,
	req *types.JoinRoundRequest, currentBlockHeight uint32,
	existingRegCount int) (*JoinRequestResult, error) {

	validationHeight := currentBlockHeight
	if validationHeight == 0 {
		validationHeight = env.StartHeight
	}

	if !env.DisableJoinRequestAuth && validationHeight == 0 {
		return nil, ErrJoinAuthHeightUnavailable
	}

	return ValidateJoinRequestAtHeight(
		ctx, env, req, validationHeight, existingRegCount,
	)
}

// ProcessEvent handles the events from the CreatedState state.
//
// Event handling:
//
//   - ClientJoinIntentEvent: Validates the join request. If validation fails,
//     remains in CreatedState and sends ClientErrorResp. On success,
//     transitions to IntentCollectingState with the first client registered,
//     sends ClientSuccessResp, requests boarding input locks, and starts
//     the registration timeout. If the seal predicate fires after adding
//     the client, emits SealEvent to seal the round early.
//
//   - TickEvent: Records that the current empty round was ticked, then remains
//     in CreatedState waiting for the first client.
func (s *CreatedState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Processing event",
		LogState("Created"),
		LogEvent(event),
	)

	switch evt := event.(type) {
	case *ClientJoinIntentEvent:
		env.Log.DebugS(ctx, "First client joining round",
			LogClientID(evt.ClientID),
			LogVTXOCount(len(evt.Request.VTXOReqs)),
			LogBoardingCount(len(evt.Request.BoardingReqs)),
			LogLeaveCount(len(evt.Request.LeaveReqs)),
		)

		// Validate the join request. If this fails, this is not an FSM
		// error, but we should respond to the client accordingly.
		// existingRegCount=0 because CreatedState handles the very
		// first client; the joining client is the only registration
		// the validation path will count toward batch sizing.
		result, err := validateJoinRequestForAdmission(
			ctx, env, evt.Request, evt.CurrentBlockHeight, 0,
		)
		if err != nil {
			env.Log.WarnS(ctx, "Join request validation failed",
				err,
				LogClientID(evt.ClientID),
			)

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

		env.Log.InfoS(
			ctx,
			"First client registered, starting registration phase",
			LogClientID(evt.ClientID),
		)

		successResp := &ClientSuccessResp{
			Client:  evt.ClientID,
			RoundID: env.RoundID,
			AcceptedBoardingOutpoints: extractBoardingOutpoints(
				result.BoardingInputs,
			),
			AcceptedVTXOOutpoints: extractVTXOOutpoints(
				result.ForfeitInputs,
			),
		}

		outbox := []OutboxEvent{
			successResp,
			newStartTimeoutReq(
				env, TimeoutPhaseRegistration,
			),
		}

		// Evaluate the seal predicate. If it fires on the very
		// first client (e.g. MaxClients(1)), seal immediately
		// instead of waiting for the registration timeout.
		if env.ShouldSeal != nil &&
			env.ShouldSeal(clientRegs) {

			env.Log.InfoS(
				ctx,
				"Seal predicate triggered on first client",
				LogClientID(evt.ClientID),
			)

			// Cancel the registration timeout we just
			// started — the predicate already sealed the
			// round. SealEvent emits RoundSealedReq.
			outbox = append(outbox,
				&CancelTimeoutReq{
					RoundID: env.RoundID,
					Phase:   TimeoutPhaseRegistration,
				},
			)

			return &StateTransition{
				NextState: newIntentCollectingState(clientRegs),
				NewEvents: fn.Some(EmittedEvent{
					Outbox: outbox,
					InternalEvent: []Event{
						&SealEvent{},
					},
				}),
			}, nil
		}

		return &StateTransition{
			NextState: newIntentCollectingState(clientRegs),
			NewEvents: fn.Some(EmittedEvent{
				Outbox: outbox,
			}),
		}, nil

	case *TickEvent:
		env.Log.DebugS(
			ctx, "Tick fired on empty created round, skipping",
		)

		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&RoundTickFiredReq{
						RoundID: env.RoundID,
						Result:  TickResultSkippedEmpty,
					},
				},
			}),
		}, nil

	default:
		return unexpectedEvent(s, "created", event, env), nil
	}
}

// ProcessEvent handles the events from the IntentCollectingState state.
//
// Event handling:
//
//   - ClientJoinIntentEvent: Validates the join request. On validation
//     failure, sends ClientErrorResp without state change. On success,
//     admits the client and sends ClientSuccessResp. If the client was
//     ALREADY registered in this round, the prior boarding-input and
//     forfeit-VTXO locks are released and the registration is overwritten;
//     this is the recovery path for the Board replay-on-restart flow
//     (darepo-client#416, ARK-02 §Restart Safety). The replay can only
//     originate from the same authenticated ClientID (env.Sender is
//     server-stamped) and the new submission carries its own BIP-322
//     ownership proof for the newly-declared outpoints. If the seal
//     predicate fires after admission, emits SealEvent to seal the round
//     early.
//
//   - RegistrationTimeoutEvent: Registration phase timed out. Emits
//     RoundSealedReq to notify actor, then internal SealEvent to seal.
//
//   - SealEvent: Transitions to BatchBuildingState with all accumulated
//     registrations, emits BuildBatchTxEvent to start batch construction.
//
//nolint:funlen
func (s *IntentCollectingState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Processing event",
		LogState("Registration"),
		LogEvent(event),
		LogClientCount(len(s.ClientRegistrations)),
	)

	switch evt := event.(type) {
	case *ClientJoinIntentEvent:
		env.Log.DebugS(ctx, "Client requesting to join",
			LogClientID(evt.ClientID),
			LogVTXOCount(len(evt.Request.VTXOReqs)),
			LogBoardingCount(len(evt.Request.BoardingReqs)),
			LogLeaveCount(len(evt.Request.LeaveReqs)),
		)

		// If this client is already registered in this round, treat
		// the re-join as a REPLACEMENT rather than a duplicate. This
		// is the recovery path for darepo-client#416: a daemon restart
		// between Board admission and round seal loses the in-memory
		// FSM, so the wallet's startup replay re-issues
		// TriggerBoardMsg, JoinRoundRequest. The server still holds
		// the pre-restart registration; without replacement the
		// re-join would bounce as "already registered" and the round
		// would never seal with the recovered client. Replacement is
		// safe because the client's identity is authenticated by the
		// JoinRoundRequest's signature, so only the legitimate owner
		// of the prior registration can overwrite it.
		//
		// Ordering matters: we validate the new request and acquire
		// the new locks BEFORE releasing any prior locks. The
		// boarding locker and the VTXO locker are owner-idempotent
		// on same-owner re-Lock, and ValidateBoardingRequest accepts
		// IsLocked=true when the owner is env.RoundID, so the
		// same-outpoint replay path validates and re-locks without
		// ever passing through an unlocked window. Only on success do
		// we release the prior-only outpoints (the set difference
		// between the prior registration and the new one). On any
		// failure path the prior locks are still held and
		// ClientRegistrations is unchanged, so state and locker stay
		// coherent.
		isReregistration := s.isClientRegistered(evt.ClientID)

		// Validate the join request structurally (inputs, auth,
		// policy shape). The seal-time fee builder computes
		// per-client fees at the actual round occupancy once
		// the round seals; no submit-time fee math runs here.
		//
		// On a re-registration we pass (existing count - 1) so the
		// existingRegCount telemetry surfaced via admin RPC reflects
		// true occupancy: the replacement reuses the same round slot,
		// not a fresh one. Seal-time fee math is independent of this
		// value under #270; the seal-time quote builder sizes against
		// the survivor set, not the admission-time count.
		regCount := len(s.ClientRegistrations)
		if isReregistration {
			regCount--
		}
		result, err := validateJoinRequestForAdmission(
			ctx, env, evt.Request, evt.CurrentBlockHeight, regCount,
		)
		if err != nil {
			env.Log.WarnS(ctx, "Join request validation failed",
				err,
				LogClientID(evt.ClientID),
			)

			errMsg := fmt.Sprintf("%v: %v", ErrJoinRequestInvalid,
				err)

			return clientErrorTransition(
				s, evt.ClientID, errMsg,
			), nil
		}

		// Attempt to lock all boarding inputs for this client. For a
		// re-registration the overlapping outpoints are owned by this
		// round already; Lock is a no-op for same-owner re-Lock so
		// the call is idempotent.
		err = lockBoardingInputs(ctx, env, result.BoardingInputs)
		if err != nil {
			return clientErrorTransition(
				s, evt.ClientID, err.Error(),
			), nil
		}

		// Attempt to lock all forfeit VTXOs for this client. LockMany
		// is owner-idempotent so the re-registration overlap case is
		// safe here too.
		err = lockForfeitVTXOs(ctx, env, result.ForfeitInputs)
		if err != nil {
			// Unlock the boarding inputs we just acquired. For a
			// re-registration this only releases outpoints that
			// were NOT held by the prior registration, since
			// same-owner unlock is the rollback and prior-owned
			// overlaps were never freshly acquired.
			rollbackNewBoardingInputs(
				ctx, env, isReregistration,
				s.ClientRegistrations[evt.ClientID],
				result.BoardingInputs,
			)

			return clientErrorTransition(
				s, evt.ClientID, err.Error(),
			), nil
		}

		// Replacement is now committed. Release the prior-only
		// outpoints (those held by the prior registration but absent
		// from the new one). Overlapping outpoints stay locked under
		// the same owner.
		if isReregistration {
			prior := s.ClientRegistrations[evt.ClientID]
			releasePriorOnlyLocks(ctx, env, prior, result)
		}

		newState := s.withNewClient(evt.ClientID, result)

		newClientCount := len(newState.ClientRegistrations)
		env.Log.InfoS(ctx, "Client registered successfully",
			LogClientID(evt.ClientID),
			LogClientCount(newClientCount),
		)

		successResp := &ClientSuccessResp{
			Client:  evt.ClientID,
			RoundID: env.RoundID,
			AcceptedBoardingOutpoints: extractBoardingOutpoints(
				result.BoardingInputs,
			),
			AcceptedVTXOOutpoints: extractVTXOOutpoints(
				result.ForfeitInputs,
			),
			IsReregistration: isReregistration,
		}

		outbox := []OutboxEvent{successResp}

		// Evaluate the seal predicate. If it fires, seal the
		// round immediately instead of waiting for the
		// registration timeout.
		if env.ShouldSeal != nil &&
			env.ShouldSeal(newState.ClientRegistrations) {

			env.Log.InfoS(
				ctx,
				"Seal predicate triggered, sealing round",
				LogClientCount(newClientCount),
			)

			// Cancel the registration timeout — the
			// predicate sealed the round early. SealEvent
			// emits RoundSealedReq.
			outbox = append(outbox,
				&CancelTimeoutReq{
					RoundID: env.RoundID,
					Phase:   TimeoutPhaseRegistration,
				},
			)

			return &StateTransition{
				NextState: newState,
				NewEvents: fn.Some(EmittedEvent{
					Outbox: outbox,
					InternalEvent: []Event{
						&SealEvent{},
					},
				}),
			}, nil
		}

		return &StateTransition{
			NextState: newState,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: outbox,
			}),
		}, nil

	case *RegistrationTimeoutEvent:
		env.Log.InfoS(ctx, "Registration timeout, sealing round",
			LogClientCount(len(s.ClientRegistrations)),
		)

		// Registration timeout expired. Emit internal SealEvent to
		// seal the round. SealEvent emits RoundSealedReq to notify
		// the actor to create a new round.
		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				InternalEvent: []Event{
					&SealEvent{},
				},
			}),
		}, nil

	case *TickEvent:
		// Periodic round tick. Unlike RegistrationTimeoutEvent
		// (which is scheduled on first join and unconditionally
		// seals), the tick is scheduled at round creation and only
		// seals if at least one client has joined and the
		// configured SealPredicate accepts the current
		// registrations. Both branches still emit a
		// RoundTickFiredReq so the per-result counter stays a
		// faithful rate of every fire.
		regs := s.ClientRegistrations

		switch {
		case len(regs) == 0:
			env.Log.DebugS(
				ctx, "Tick fired on empty round, skipping",
			)

			return &StateTransition{
				NextState: s,
				NewEvents: fn.Some(EmittedEvent{
					Outbox: []OutboxEvent{
						&RoundTickFiredReq{
							RoundID: env.RoundID,
							Result:  TickResultSkippedEmpty, //nolint:ll
						},
					},
				}),
			}, nil

		case env.ShouldSeal != nil && !env.ShouldSeal(regs):
			env.Log.DebugS(ctx, "Tick rejected by seal "+
				"predicate, skipping",
				LogClientCount(len(regs)))

			return &StateTransition{
				NextState: s,
				NewEvents: fn.Some(EmittedEvent{
					Outbox: []OutboxEvent{
						&RoundTickFiredReq{
							RoundID: env.RoundID,
							Result:  TickResultSkippedPredicate, //nolint:ll
						},
					},
				}),
			}, nil
		}

		env.Log.InfoS(ctx, "Tick sealing round",
			LogClientCount(len(regs)),
		)

		// Cancel both the registration timeout (if any) and the
		// recurring tick before sealing. The timeout package's
		// Cancel is a no-op for unscheduled IDs so the
		// registration cancel is safe even when no client has
		// joined (impossible here, len(regs) > 0) — included for
		// symmetry with the predicate-on-first-client path above.
		// The actor also cancels the tick on RoundSealedReq, so
		// this duplicate cancel is harmless and keeps the FSM
		// self-consistent.
		regPhase := TimeoutPhaseRegistration

		return &StateTransition{
			NextState: s,
			NewEvents: fn.Some(EmittedEvent{
				Outbox: []OutboxEvent{
					&CancelTimeoutReq{
						RoundID: env.RoundID,
						Phase:   regPhase,
					},
					&CancelTimeoutReq{
						RoundID: env.RoundID,
						Phase:   TimeoutPhaseTick,
					},
					&RoundTickFiredReq{
						RoundID: env.RoundID,
						Result:  TickResultSealed,
					},
				},
				InternalEvent: []Event{
					&SealEvent{},
				},
			}),
		}, nil

	case *SealEvent:
		env.Log.InfoS(ctx, "Registration sealed, computing quotes",
			LogClientCount(len(s.ClientRegistrations)),
		)

		// Test escape hatch: pre-#270 tests drive straight from
		// SealEvent → BatchBuildingState without a quote
		// handshake. Skip the fan-out in that case.
		if env.SkipQuoteHandshake {
			regs := s.ClientRegistrations
			sealed := &RoundSealedReq{SealedRoundID: env.RoundID}

			return &StateTransition{
				NextState: &BatchBuildingState{
					ClientRegistrations: regs,
				},
				NewEvents: fn.Some(EmittedEvent{
					InternalEvent: []Event{
						&BuildBatchTxEvent{},
					},
					Outbox: []OutboxEvent{sealed},
				}),
			}, nil
		}

		// Registration closes. Instead of transitioning directly to
		// BatchBuildingState, run the seal-time fee builder to
		// compute one JoinRoundQuote per client, drop clients with
		// non-OK reject reasons (releasing their locks), fan out
		// quote envelopes, schedule per-client acceptance timeouts,
		// and transition to QuoteSentState. The FSM does not start
		// building the PSBT until every client accepts, rejects, or
		// times out — the VTXO tree depends on the accepted set.
		return sealRoundWithQuotes(
			ctx, env, s.ClientRegistrations, 0, nil,
		)

	default:
		return unexpectedEvent(s, "registration", event, env), nil
	}
}

// sealRoundWithQuotes runs the seal-time fee builder, fans out
// per-client JoinRoundQuote envelopes, schedules per-client quote
// timeouts, and transitions to QuoteSentState. Shared between the
// initial SealEvent path (sealPass=0) and the reseal path
// (sealPass>0) emitted by QuoteSentState when any client rejects or
// times out.
//
// Clients whose quote resolves to a non-OK reject reason (e.g.
// insufficient residual) have their boarding / forfeit locks
// released, receive a ClientRoundFailedResp with the reason, and do
// not appear in the next state's ClientRegistrations map.
//
// When the surviving set is empty the FSM falls back to
// IntentCollectingState with no registrations, preserving the
// pre-#270 "no clients joined" shape. Callers threading through a
// reseal populate priorRejectCounts so per-client reject caps track
// across passes.
func sealRoundWithQuotes(ctx context.Context, env *Environment,
	regs map[clientconn.ClientID]*ClientRegistration, sealPass uint32,
	priorRejectCounts map[clientconn.ClientID]uint32) (*StateTransition,
	error) {

	// Shared outbox across this transition: per-client
	// JoinRoundQuote envelopes, per-client drop notifications, the
	// quote-phase timeout, and (only when at least one client
	// survives quoting on pass 0) a RoundSealedReq so the actor
	// spawns a fresh round for incoming registrations. The
	// RoundSealedReq is deferred until after pruning — emitting it
	// before the survivor count is known would orphan an empty
	// round if every client failed admission.
	var outbox []OutboxEvent

	// If the fee calculator is missing (should have been enforced
	// at Actor.Start) we cannot build quotes at all; fail the round
	// rather than silently drop clients.
	if env.FeeCalculator == nil {
		env.Log.ErrorS(ctx,
			"Cannot seal: FeeCalculator is nil", nil)

		return &StateTransition{
			NextState: &FailedState{
				Reason: "fee calculator not configured",
			},
		}, nil
	}

	// Best-effort live chain inputs. env.StartHeight is the height
	// at which the round was created; rounds are short-lived so the
	// delta to the true chain tip at seal time is at most a handful
	// of blocks, well within the FeeCalculator's δ_min floor. Fee
	// rate and utilization come from their respective hot-path
	// sources.
	currentHeight := env.StartHeight
	var (
		feeRate     chainfee.SatPerKWeight
		utilization float64
	)
	if env.FeeEstimator != nil {
		r, err := env.FeeEstimator.EstimateFeePerKW(env.ConfTarget)
		if err == nil {
			feeRate = r
		} else {
			env.Log.WarnS(ctx, "Seal-time fee estimation failed; "+
				"falling back to zero fee rate", err,
				LogRoundID(env.RoundID),
				slog.Int64("conf_target",
					int64(env.ConfTarget)),
			)
		}
	}
	if env.TreasuryTracker != nil {
		utilization = env.TreasuryTracker.Utilization()
	}

	env.Log.InfoS(ctx, "Computing seal-time quotes",
		LogRoundID(env.RoundID),
		LogClientCount(len(regs)),
		slog.Int64("seal_pass", int64(sealPass)),
		slog.Int64("current_height", int64(currentHeight)),
		slog.Int64("conf_target", int64(env.ConfTarget)),
		slog.Int64("fee_rate_sat_kw", int64(feeRate)),
		slog.Int64("fee_rate_sat_vbyte",
			int64(feeRate.FeePerVByte())),
		slog.Float64("utilization", utilization),
		slog.Int64(
			"dust_limit_sat", int64(env.Terms.ConnectorDustAmount),
		),
	)

	// Use ConnectorDustAmount as the residual floor — it is the
	// operator's canonical sub-dust threshold for this round.
	quotes, err := computeSealTimeQuotes(
		env.RoundID, regs, sealPass, currentHeight, feeRate,
		utilization, env.Terms.ConnectorDustAmount, env.FeeCalculator,
	)
	if err != nil {
		env.Log.ErrorS(ctx, "Quote builder failure", err)

		return &StateTransition{
			NextState: &FailedState{
				Reason: fmt.Sprintf("quote builder: %v", err),
			},
		}, nil
	}

	// Partition quotes into "admitted" (RejectReason == OK) and
	// "dropped" (non-OK). Dropped clients have their boarding /
	// forfeit locks released and receive a ClientRoundFailedResp so
	// their UX does not hang waiting for a quote that already came
	// back empty.
	quoteExpiresAt := time.Now().Add(env.quoteTTL()).Unix()
	survivors := make(map[clientconn.ClientID]*ClientRegistration)
	admittedQuotes := make(map[clientconn.ClientID]*Quote)
	droppedClients := make(map[clientconn.ClientID]struct{})

	for cid, reg := range regs {
		q, ok := quotes[cid]
		if !ok || q == nil {
			droppedClients[cid] = struct{}{}
			outbox = append(outbox, &ClientRoundFailedResp{
				Client:  cid,
				RoundID: env.RoundID,
				Reason:  "seal-time quote computation missing",
			})
			unlockBoardingInputsList(ctx, env, reg.BoardingInputs)
			unlockForfeitVTXOsList(ctx, env, reg.ForfeitInputs)

			continue
		}

		if !q.isOK() {
			logDroppedSealQuote(ctx, env, cid, q)

			droppedClients[cid] = struct{}{}
			outbox = append(outbox, &ClientRoundFailedResp{
				Client:  cid,
				RoundID: env.RoundID,
				Reason: fmt.Sprintf(
					"seal-time quote rejected: %s",
					q.RejectReason,
				),
			})
			unlockBoardingInputsList(ctx, env, reg.BoardingInputs)
			unlockForfeitVTXOsList(ctx, env, reg.ForfeitInputs)

			continue
		}

		logAdmittedSealQuote(ctx, env, cid, q)

		// Stamp the quote's binding amounts onto the registration
		// so the downstream commitment-tx builder uses the
		// server-computed residuals (and echoed non-change targets)
		// instead of the client's intent-time values, which were
		// zero for change outputs. Without this patch, the VTXO
		// tree leaves would carry stale amounts and client-side
		// validation at CommitmentTxReceivedState would fail.
		applyQuoteAmountsToRegistration(reg, q)

		survivors[cid] = reg
		admittedQuotes[cid] = q

		outbox = append(outbox, &JoinRoundQuoteOutbox{
			Client:         cid,
			RoundID:        env.RoundID,
			Quote:          q,
			QuoteExpiresAt: quoteExpiresAt,
		})
	}

	// No survivors → fail the round outright. Without this branch
	// the FSM would park in IntentCollectingState (empty) but the
	// round is sealed and unable to accept new intents, while no
	// RoundSealedReq has spawned a replacement round either —
	// repeated empty seals would accumulate dead rounds in actor
	// memory. RoundFailedReq triggers the actor's cleanup + fresh
	// round spawn path so incoming registrations have somewhere to
	// land.
	if len(survivors) == 0 {
		env.Log.InfoS(ctx, "No clients survived seal-time quoting")

		outbox = append(outbox, &RoundFailedReq{
			FailedRoundID: env.RoundID,
			Reason:        "all clients dropped at seal time",
		})

		return &StateTransition{
			NextState: &FailedState{
				Reason: "all clients dropped at seal time",
			},
			NewEvents: fn.Some(EmittedEvent{
				Outbox: outbox,
			}),
		}, nil
	}

	// Survivors exist — emit RoundSealedReq (only on pass 0) so
	// the actor spawns a fresh round for incoming registrations
	// while this round runs the quote handshake. Deferring the
	// emission until after we know the round has at least one
	// admitted client is what closes the orphan-round leak the
	// pre-fix flow exposed.
	if sealPass == 0 {
		outbox = append(outbox, &RoundSealedReq{
			SealedRoundID: env.RoundID,
		})
	}

	// Schedule a single phase-level timeout; the actor fans out
	// per-client QuoteTimeoutEvents with their bound QuoteID via
	// the QuoteSentState timeout handler path once the timer fires.
	outbox = append(outbox, &StartTimeoutReq{
		RoundID:  env.RoundID,
		Phase:    TimeoutPhaseQuote,
		Duration: env.quoteTTL(),
	})

	status := make(
		map[clientconn.ClientID]QuoteStatus, len(survivors),
	)
	for cid := range survivors {
		status[cid] = QuotePending
	}

	rejectCounts := make(
		map[clientconn.ClientID]uint32, len(survivors),
	)
	for cid, n := range priorRejectCounts {
		if _, still := survivors[cid]; still {
			rejectCounts[cid] = n
		}
	}

	return &StateTransition{
		NextState: &QuoteSentState{
			ClientRegistrations: survivors,
			Quotes:              admittedQuotes,
			Status:              status,
			SealPass:            sealPass,
			QuoteExpires: time.Unix(
				quoteExpiresAt, 0,
			),
			RejectCounts:   rejectCounts,
			DroppedClients: droppedClients,
		},
		NewEvents: fn.Some(EmittedEvent{
			Outbox: outbox,
		}),
	}, nil
}

// logDroppedSealQuote logs quote rejection details while only attaching
// fee fields to reject reasons that actually computed a fee breakdown.
func logDroppedSealQuote(ctx context.Context, env *Environment,
	cid clientconn.ClientID, q *Quote) {

	dropAttrs := []any{
		LogClientID(cid),
		slog.String("reason", q.RejectReason.String()),
	}
	if q.RejectReason == QuoteReasonInsufficientResidual {
		dropAttrs = append(
			dropAttrs, slog.Bool("fee_breakdown_available", true),
		)
		dropAttrs = append(dropAttrs, quoteFeeBreakdownLogAttrs(q)...)
	} else {
		dropAttrs = append(
			dropAttrs, slog.Bool("fee_breakdown_available", false),
		)
	}

	env.Log.InfoS(ctx, "Dropping client at seal time", dropAttrs...)
}

// logAdmittedSealQuote logs per-client happy-path quote details at debug
// level so production info logs stay focused on notable events.
func logAdmittedSealQuote(ctx context.Context, env *Environment,
	cid clientconn.ClientID, q *Quote) {

	env.Log.DebugS(
		ctx, "Client seal-time quote admitted",
		append(
			[]any{LogClientID(cid)},
			quoteFeeBreakdownLogAttrs(q)...,
		)...,
	)
}

// quoteFeeBreakdownLogAttrs converts a quote's fee breakdown into
// consistently named structured log fields.
func quoteFeeBreakdownLogAttrs(q *Quote) []any {
	return []any{
		slog.Int64("operator_fee_sat", int64(q.OperatorFee)),
		slog.Int64("chain_fee_sat", q.Breakdown.ChainFeeSat),
		slog.Int64("liquidity_fee_sat", q.Breakdown.LiquidityFeeSat),
		slog.Int64("congestion_fee_sat", q.Breakdown.CongestionFeeSat),
		slog.Int64("fee_rate_sat_kw", q.Breakdown.FeeRateSatKw),
		slog.Int64(
			"fee_rate_sat_vbyte",
			int64(
				chainfee.SatPerKWeight(
					q.Breakdown.FeeRateSatKw,
				).FeePerVByte(),
			),
		),
		slog.Int64("quote_batch_size", int64(q.Breakdown.BatchSize)),
	}
}

// ProcessEvent handles the events from the QuoteSentState state.
// QuoteSentState is entered after SealEvent fires and
// sealRoundWithQuotes fans out a JoinRoundQuote per admitted client.
// It waits for every client to accept (ClientQuoteAcceptEvent),
// reject (ClientQuoteRejectEvent), or time out (QuoteTimeoutEvent),
// then picks one of three exits:
//
//   - All accepted (no rejects / no timeouts): transition to
//     BatchBuildingState with the accepted registration set and
//     fire BuildBatchTxEvent.
//   - Any reject / timeout but SealPass+1 < MaxSealPasses: fire a
//     fresh SealEvent over the survivors (accepted minus dropped)
//     via sealRoundWithQuotes — the reseal happens entirely inside
//     the existing round, no new RoundSealedReq.
//   - Any reject / timeout and SealPass+1 >= MaxSealPasses: finalize
//     with the current pass's accepted set (drop unresolved).
//   - Zero accepted: fail the round rather than parking an empty
//     IntentCollectingState.
//
// Every accept / reject / timeout carries a QuoteID that must match
// the active quote the server issued to that client on the current
// pass; mismatches (stale or forged quote_ids after a reseal) are
// dropped silently to keep the handler idempotent.
func (s *QuoteSentState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Processing event",
		LogState("QuoteSent"),
		LogEvent(event),
		slog.Int("pass", int(s.SealPass)),
		LogClientCount(len(s.ClientRegistrations)),
	)

	switch evt := event.(type) {
	case *ClientQuoteAcceptEvent:
		return s.handleClientResolved(
			ctx, env, evt.ClientID, evt.QuoteID, QuoteAccepted, "",
		)

	case *ClientQuoteRejectEvent:
		return s.handleClientResolved(
			ctx, env, evt.ClientID, evt.QuoteID, QuoteRejected,
			evt.Reason,
		)

	case *QuoteTimeoutEvent:
		return s.handleClientResolved(
			ctx, env, evt.ClientID, evt.QuoteID, QuoteTimedOut, "",
		)

	case *AllQuotesResolvedEvent:
		return s.resolvePass(ctx, env)

	default:
		return unexpectedEvent(s, "quote-sent", event, env), nil
	}
}

// handleClientResolved flips the client's status to newStatus when
// the event's QuoteID matches the active quote and the client is
// still QuotePending. Stale quote_ids, unknown clients, and
// already-resolved clients are silent no-ops (keeps the handler
// idempotent against timer / network replays). When the update
// leaves every pending client resolved, the handler emits
// AllQuotesResolvedEvent internally so the pass resolution runs as
// a dedicated event rather than side-effecting through whichever
// real client event resolved the last pending status.
func (s *QuoteSentState) handleClientResolved(ctx context.Context,
	env *Environment, clientID clientconn.ClientID, quoteID [32]byte,
	newStatus QuoteStatus, rejectReason string) (*StateTransition, error) {

	activeQuote, ok := s.Quotes[clientID]
	if !ok || activeQuote == nil {
		env.Log.DebugS(ctx, "Dropping quote event from unknown client",
			LogClientID(clientID),
		)

		return &StateTransition{NextState: s}, nil
	}

	if activeQuote.QuoteID != quoteID {
		env.Log.DebugS(ctx, "Dropping stale quote_id",
			LogClientID(clientID),
		)

		return &StateTransition{NextState: s}, nil
	}

	current, ok := s.Status[clientID]
	if !ok || current != QuotePending {

		// Already terminal for this pass.
		return &StateTransition{NextState: s}, nil
	}

	newState := s.cloneWithUpdatedClient(
		clientID, newStatus, rejectReason,
	)

	// If this transition resolved the last pending client, fire
	// the internal resolution event.
	var internal []Event
	if newState.allResolved() {
		internal = append(internal, &AllQuotesResolvedEvent{})
	}

	if len(internal) == 0 {
		return &StateTransition{NextState: newState}, nil
	}

	return &StateTransition{
		NextState: newState,
		NewEvents: fn.Some(EmittedEvent{
			InternalEvent: internal,
		}),
	}, nil
}

// cloneWithUpdatedClient returns a shallow copy of the state with
// Status[clientID]=newStatus applied (and RejectCounts / reject-cap
// drop handled for the QuoteRejected case). Reject-cap evictions
// additionally move the client into DroppedClients so it does not
// carry into a reseal and its locks are released by the caller.
func (s *QuoteSentState) cloneWithUpdatedClient(
	clientID clientconn.ClientID, newStatus QuoteStatus,
	_ string,
) *QuoteSentState {

	next := &QuoteSentState{
		ClientRegistrations: s.ClientRegistrations,
		Quotes:              s.Quotes,
		SealPass:            s.SealPass,
		QuoteExpires:        s.QuoteExpires,
	}

	// Deep-copy the maps we mutate so the old state snapshot is
	// still consistent for any holder (e.g. the test harness).
	next.Status = make(map[clientconn.ClientID]QuoteStatus, len(s.Status))
	for k, v := range s.Status {
		next.Status[k] = v
	}
	next.Status[clientID] = newStatus

	next.RejectCounts = make(
		map[clientconn.ClientID]uint32, len(s.RejectCounts),
	)
	for k, v := range s.RejectCounts {
		next.RejectCounts[k] = v
	}
	if newStatus == QuoteRejected {
		next.RejectCounts[clientID]++
	}

	next.DroppedClients = make(
		map[clientconn.ClientID]struct{}, len(s.DroppedClients)+1,
	)
	for k, v := range s.DroppedClients {
		next.DroppedClients[k] = v
	}

	return next
}

// resolvePass runs the post-wait transition decision: every client
// in Status is terminal, so pick between advance (all accepted),
// reseal (any reject/timeout + cap not hit), finalize-at-cap, and
// empty-rollback (zero accepted). Any drop-eligible clients have
// their locks released here before the state transition.
func (s *QuoteSentState) resolvePass(ctx context.Context, env *Environment) (
	*StateTransition, error) {

	if !s.allResolved() {

		// Defensive: resolvePass is only called from the
		// AllQuotesResolvedEvent path, but if a timer fired this
		// event spuriously we fall through as a no-op.
		return &StateTransition{NextState: s}, nil
	}

	accepted := s.acceptedClients()
	hasUnresolved := s.hasAnyUnresolvedReject()

	// Drop clients that hit the reject cap (count > cap).
	// Rejecting and timing-out clients release their locks
	// regardless (they are not participating in this round
	// further; at minimum, not this pass). On reseal, dropped
	// clients stay dropped.
	dropOutbox, dropSet := releaseResolvedNonAcceptors(
		ctx, env, s,
	)

	// No survivors → every client participating in this round has
	// rejected, timed out, or hit the reject cap. Emit a
	// RoundFailedReq alongside the drop-outbox so the actor layer
	// untracks client-round bindings and rolls the sealed round
	// over to a fresh FSM; otherwise the untracked round stays
	// bound to the now-failed clients and GetClientRounds keeps
	// returning it, which trips systest assertions that require the
	// failed round to disappear before a rejoin.
	if len(accepted) == 0 {
		env.Log.InfoS(ctx, "Quote pass closed with no survivors",
			slog.Int("pass", int(s.SealPass)),
		)

		dropOutbox = append(dropOutbox, &RoundFailedReq{
			FailedRoundID: env.RoundID,
			Reason:        "quote pass closed with no survivors",
		})

		return &StateTransition{
			NextState: &FailedState{
				Reason: "quote pass closed with no survivors",
			},
			NewEvents: fn.Some(EmittedEvent{Outbox: dropOutbox}),
		}, nil
	}

	// Happy path: all clients accepted. Advance to
	// BatchBuildingState — the PSBT is built from the accepted set.
	if !hasUnresolved {
		env.Log.InfoS(ctx, "All quotes accepted, building batch",
			slog.Int("pass", int(s.SealPass)),
			LogClientCount(len(accepted)),
		)

		acceptedRegs := extractSurvivingRegs(
			s.ClientRegistrations, accepted,
		)

		return &StateTransition{
			NextState: &BatchBuildingState{
				ClientRegistrations: acceptedRegs,
			},
			NewEvents: fn.Some(EmittedEvent{
				InternalEvent: []Event{&BuildBatchTxEvent{}},
				Outbox:        dropOutbox,
			}),
		}, nil
	}

	// At least one reject / timeout but there are still
	// survivors. Reseal unless the cap would be exceeded.
	nextPass := s.SealPass + 1
	passCap := env.maxSealPasses()
	if nextPass >= passCap {
		env.Log.InfoS(
			ctx,
			"Reseal cap hit, finalizing with accepted set",
			slog.Int("pass", int(s.SealPass)),
			slog.Uint64("cap", uint64(passCap)),
		)

		acceptedRegs := extractSurvivingRegs(
			s.ClientRegistrations, accepted,
		)

		return &StateTransition{
			NextState: &BatchBuildingState{
				ClientRegistrations: acceptedRegs,
			},
			NewEvents: fn.Some(EmittedEvent{
				InternalEvent: []Event{&BuildBatchTxEvent{}},
				Outbox:        dropOutbox,
			}),
		}, nil
	}

	env.Log.InfoS(ctx, "Resealing over survivors",
		slog.Int("pass", int(s.SealPass)),
		LogClientCount(len(accepted)),
	)

	survivors := extractSurvivingRegs(
		s.ClientRegistrations, accepted,
	)

	// Re-merge prior reject counts for survivors so cap tracking
	// persists across passes.
	priorRejects := make(
		map[clientconn.ClientID]uint32, len(s.RejectCounts),
	)
	for cid, n := range s.RejectCounts {
		if _, still := survivors[cid]; still {
			priorRejects[cid] = n
		}
	}

	sealTransition, err := sealRoundWithQuotes(
		ctx, env, survivors, nextPass, priorRejects,
	)
	if err != nil {
		return nil, err
	}

	// Fold any drop-client outbox entries from this pass ahead of
	// the fresh quote fan-out so the client sees its reject ack
	// before a new quote arrives.
	if len(dropOutbox) > 0 && sealTransition.NewEvents.IsSome() {
		ee := sealTransition.NewEvents.UnwrapOr(EmittedEvent{})
		ee.Outbox = append(dropOutbox, ee.Outbox...)
		sealTransition.NewEvents = fn.Some(ee)
	}

	// Silence the drop-set noise.
	_ = dropSet

	return sealTransition, nil
}

// releaseResolvedNonAcceptors walks the status map and releases
// boarding + forfeit locks for clients that rejected, timed out, or
// hit the reject cap. Returns the outbox of ClientRoundFailedResp
// messages and the set of client IDs that have been permanently
// dropped from the round (for ClientRegistrations pruning).
//
// Under the #270 quote handshake, non-accepting clients fall into
// three buckets:
//
//   - QuoteTimedOut: the round has no surviving accepted clients, so
//     this pass terminates. The client must be told explicitly — it
//     is sitting in RoundJoined (or QuoteReceived) waiting on
//     CommitmentTxBuilt that will never arrive — so we fan out a
//     ClientRoundFailedResp with a recoverable-reason string.
//     Timeouts do NOT evict the client permanently: if this is a
//     reseal-candidate pass, the client can still re-engage.
//
//   - QuoteRejected under the cap: the client chose to reject and
//     may retry next pass. We release locks for this pass only; no
//     fail-resp is emitted because the client already knows (it
//     sent the reject).
//
//   - QuoteRejected over the cap: the client is permanently dropped
//     for this round. We both release locks and emit a fail-resp.
func releaseResolvedNonAcceptors(ctx context.Context, env *Environment,
	s *QuoteSentState,
) ([]OutboxEvent, map[clientconn.ClientID]struct{}) {

	var outbox []OutboxEvent
	dropped := make(map[clientconn.ClientID]struct{})

	for cid, status := range s.Status {
		if status == QuoteAccepted || status == QuotePending {
			continue
		}

		reg, ok := s.ClientRegistrations[cid]
		if !ok {
			continue
		}

		// Timeouts release locks but do not evict the client
		// across passes — they may re-engage in a reseal. We still
		// surface a ClientRoundFailedResp so the client FSM
		// doesn't sit in RoundJoined waiting for a commitment tx
		// that will never come.
		switch status {
		case QuoteTimedOut:
			outbox = append(outbox,
				&ClientRoundFailedResp{
					Client:  cid,
					RoundID: env.RoundID,
					Reason:  "seal-time quote timeout",
				},
			)

		case QuoteRejected:
			if s.RejectCounts[cid] >= env.maxClientRejects() {
				dropped[cid] = struct{}{}
				outbox = append(outbox,
					&ClientRoundFailedResp{
						Client:  cid,
						RoundID: env.RoundID,
						Reason: "quote reject " +
							"cap exceeded",
					},
				)
			}
		}

		unlockBoardingInputsList(
			ctx, env, reg.BoardingInputs,
		)
		unlockForfeitVTXOsList(
			ctx, env, reg.ForfeitInputs,
		)
	}

	return outbox, dropped
}

// applyQuoteAmountsToRegistration stamps the server-computed quote
// amounts onto the registration's VTXODescriptors and LeaveOutputs
// so the commitment-tx builder ultimately produces a tree whose
// leaf values match the per-client quote the server fanned out.
//
// The registration's IntentVTXOReqs is positionally aligned with
// the Quote.VTXOAmounts slice by construction (both built in the
// same iteration order inside the builder). Same story for
// IntentLeaveReqs and Quote.LeaveAmounts. LeaveOutputs indexes
// match IntentLeaveReqs 1:1.
//
// This is the single point where the quote becomes authoritative:
// after this call, any code path that reads
// reg.VTXODescriptors[key].Amount or reg.LeaveOutputs[i].Value
// sees the quote's residual or echoed target, not the stale
// intent-time value.
func applyQuoteAmountsToRegistration(reg *ClientRegistration, q *Quote) {
	if reg == nil || q == nil {
		return
	}

	for i, vr := range reg.IntentVTXOReqs {
		if i >= len(q.VTXOAmounts) {
			break
		}

		desc := reg.VTXODescriptors[signingKeyVertex(vr)]
		if desc == nil {
			continue
		}

		desc.Amount = q.VTXOAmounts[i]
	}

	for i := range reg.IntentLeaveReqs {
		if i >= len(q.LeaveAmounts) || i >= len(reg.LeaveOutputs) {
			break
		}

		out := reg.LeaveOutputs[i]
		if out == nil {
			continue
		}

		out.Value = int64(q.LeaveAmounts[i])
	}
}

// extractSurvivingRegs builds a new ClientRegistrations map
// containing only the entries whose ClientID is in the given slice.
// Used to project the pre-reseal / pre-build registration set down
// to the accepted-clients subset.
func extractSurvivingRegs(
	regs map[clientconn.ClientID]*ClientRegistration,
	survivors []clientconn.ClientID,
) map[clientconn.ClientID]*ClientRegistration {

	out := make(
		map[clientconn.ClientID]*ClientRegistration, len(survivors),
	)
	for _, cid := range survivors {
		if reg, ok := regs[cid]; ok {
			out[cid] = reg
		}
	}

	return out
}

// ProcessEvent handles the events from the BatchBuildingState state.
func (s *BatchBuildingState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Processing event",
		LogState("BatchBuilding"),
		LogEvent(event),
	)

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
			LogVTXOCount(len(allVTXODescriptors)),
		)

		// Build the commitment transaction PSBT with fee
		// estimation and wallet funding.
		lockID := roundLockID(env.RoundID)
		fundingOpts := &FundingOpts{
			LockID:       lockID,
			LockDuration: env.Terms.FundPsbtLockDuration,
		}
		psbtPacket, changeOutputIdx, vtxoTrees, connectorTrees,
			connectorAssignments, lockedOutpoints,
			err := buildCommitmentTx(
			ctx, env.Terms, env.FeeEstimator,
			env.ConfTarget, env.WalletController,
			env.MinConfs, env.WalletAccount,
			allBoardingInputs, allForfeitInputs,
			allLeaveOutputs, allVTXODescriptors,
			fundingOpts,
		)
		if err != nil {
			env.Log.WarnS(
				ctx, "Commitment tx build failed", err,
			)

			reason := fmt.Sprintf("build commitment tx: %v", err)

			return buildFailureTransition(
				ctx, env, s.ClientRegistrations, reason,
				[32]byte{}, nil,
			), nil
		}

		// FundPsbt may legitimately produce no change when boarding
		// inputs cover Σoutputs + fees by less than LND's change-dust
		// threshold; the dropped amount is bounded by LND's
		// coin-selection invariant (`changeAmt < dust_limit`), so the
		// overpay above target fee is at most a few hundred sats. We
		// surface a warning so operators can observe the case rather
		// than failing the round.
		if changeOutputIdx < 0 && len(allBoardingInputs) > 0 {
			metrics.RoundChangeRequiredForBoardingTotal.Inc()
			env.Log.WarnS(ctx,
				"FundPsbt produced no change for boarding "+
					"round; witness-delta uncompensated, "+
					"residual goes to miners as bounded "+
					"overpay",
				nil,
				LogRoundID(env.RoundID),
				LogBoardingCount(len(allBoardingInputs)),
				LogLeaveCount(len(allLeaveOutputs)),
			)
		}

		connectorDescriptors, err := buildConnectorDescriptors(
			connectorAssignments, env.ForfeitScript,
			int(env.Terms.ConnectorTreeRadix),
		)
		if err != nil {
			// Release wallet UTXO leases since batch
			// building partially succeeded.
			releaseWalletInputs(
				ctx, env, lockID, lockedOutpoints,
			)

			reason := fmt.Sprintf("build connector descriptors: %v",
				err)

			return buildFailureTransition(
				ctx, env, s.ClientRegistrations, reason,
				[32]byte{}, nil,
			), nil
		}

		env.Log.InfoS(ctx,
			"Commitment transaction built successfully",
			slog.Int("tree_count", len(vtxoTrees)),
			slog.Int("input_count",
				len(psbtPacket.Inputs)),
			slog.Int("output_count",
				len(psbtPacket.Outputs)))

		// Transition to BatchBuiltState with the funded PSBT.
		return &StateTransition{
			NextState: &BatchBuiltState{
				ClientRegistrations:  s.ClientRegistrations,
				PSBT:                 psbtPacket,
				VTXOTrees:            vtxoTrees,
				ConnectorTrees:       connectorTrees,
				ConnectorAssignments: connectorAssignments,
				ConnectorDescriptors: connectorDescriptors,
				ChangeOutputIdx:      changeOutputIdx,
				LockedOutpoints:      lockedOutpoints,
			},
			NewEvents: fn.Some(EmittedEvent{
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

	env.Log.DebugS(ctx, "Processing event",
		LogState("BatchBuilt"),
		LogEvent(event),
	)

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
		slog.Int("tree_count", len(s.VTXOTrees)),
	)

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
				env.Log.WarnS(
					ctx,
					"Failed to extract VTXO paths",
					err,
					LogClientID(clientID),
				)

				return buildFailureTransition(
					ctx, env, s.ClientRegistrations,
					fmt.Sprintf("extract VTXO paths for "+
						"client %s: %v", clientID, err),
					roundLockID(env.RoundID),
					s.LockedOutpoints,
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
						fmt.Sprintf(
							"missing connector "+
								"assignment "+
								"for client %s",
							clientID),
						roundLockID(env.RoundID),
						s.LockedOutpoints,
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
			slog.Int("tree_count", len(s.VTXOTrees)),
		)

		return s.transitionToVTXONonces(ctx, env, outboxMsgs)
	}

	env.Log.InfoS(
		ctx, "No VTXOs, transitioning to input signature collection",
	)

	// No VTXOs - go directly to boarding signatures.
	return s.transitionToInputSigs(ctx, env, outboxMsgs)
}

// transitionToVTXONonces creates TreeSignCoordinators for each VTXO tree and
// transitions to AwaitingVTXONoncesState.
func (s *BatchBuiltState) transitionToVTXONonces(ctx context.Context,
	env *Environment, outboxMsgs []OutboxEvent) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Creating tree sign coordinators",
		slog.Int("tree_count", len(s.VTXOTrees)),
	)

	// Create TreeSignCoordinators for each VTXO tree.
	treeCoordinators := make(map[int]*batch.TreeSignCoordinator)
	for idx, vtxoTree := range s.VTXOTrees {
		coordinator, err := batch.NewTreeSignCoordinator(
			env.WalletController, &env.Terms.OperatorKey, vtxoTree,
		)
		if err != nil {
			env.Log.WarnS(ctx, "Failed to create tree coordinator",
				err,
				LogOutputIndex(idx),
			)

			return buildFailureTransition(
				ctx, env, s.ClientRegistrations,
				fmt.Sprintf("create tree coordinator for "+
					"output %d: %v", idx, err),
				roundLockID(env.RoundID),
				s.LockedOutpoints,
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
			ChangeOutputIdx: s.ChangeOutputIdx,
			LockedOutpoints: s.LockedOutpoints,
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
		LogClientCount(len(s.ClientRegistrations)),
	)

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
			ChangeOutputIdx:     s.ChangeOutputIdx,
			LockedOutpoints:     s.LockedOutpoints,
		},
		NewEvents: fn.Some(EmittedEvent{
			Outbox: outboxMsgs,
		}),
	}, nil
}

// buildFailureTransition creates a state transition to FailedState with all
// the necessary outbox events to notify clients and inform the actor of
// the failure. Boarding inputs, forfeit VTXOs, and wallet UTXO leases
// are unlocked inline before transitioning.
func buildFailureTransition(ctx context.Context, env *Environment,
	clientRegs map[clientconn.ClientID]*ClientRegistration, reason string,
	lockID [32]byte, lockedOutpoints []wire.OutPoint) *StateTransition {

	env.Log.WarnS(context.Background(), "Round entering failed state",
		nil,
		LogReason(reason),
		LogClientCount(len(clientRegs)),
	)

	var outboxMsgs []OutboxEvent

	// Notify each client that the round has failed.
	for clientID := range clientRegs {
		outboxMsgs = append(outboxMsgs, &ClientRoundFailedResp{
			Client:  clientID,
			RoundID: env.RoundID,
			Reason:  reason,
		})
	}

	// Unlock all boarding inputs and forfeit VTXOs inline.
	unlockBoardingInputs(ctx, env, clientRegs)
	unlockForfeitVTXOs(ctx, env, clientRegs)

	// Release wallet UTXO leases if any were acquired during FundPsbt.
	releaseWalletInputs(ctx, env, lockID, lockedOutpoints)

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
func (s *AwaitingInputSigsState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Processing event",
		LogState("AwaitingInputSigs"),
		LogEvent(event),
		LogSubmitted(len(s.ClientsSubmitted)),
		LogExpected(len(s.ClientRegistrations)),
	)

	switch evt := event.(type) {
	case *ClientInputSignaturesEvent:
		return s.handleInputSignatures(ctx, evt, env)

	case *InputSignaturesTimeoutEvent:
		env.Log.WarnS(ctx, "Input signature collection timeout",
			nil,
			LogSubmitted(len(s.ClientsSubmitted)),
			LogExpected(len(s.ClientRegistrations)),
		)

		// Timeout expired - fail the round.
		reason := "input signature collection timeout"

		return buildFailureTransition(
			ctx, env, s.ClientRegistrations, reason,
			roundLockID(env.RoundID), s.LockedOutpoints,
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
//
//nolint:funlen
func (s *AwaitingInputSigsState) handleInputSignatures(ctx context.Context,
	evt *ClientInputSignaturesEvent, env *Environment) (*StateTransition,
	error) {

	clientID := evt.ClientID

	env.Log.DebugS(ctx, "Received boarding signatures",
		LogClientID(clientID),
		LogSigCount(len(evt.Signatures)),
	)

	// Check if client is registered in this round.
	reg, exists := s.ClientRegistrations[clientID]
	if !exists {
		return clientErrorTransition(s, clientID, "not registered"), nil
	}

	// Check if client already completed their submission.
	if s.hasClientSubmitted(clientID) {
		return clientErrorTransition(
			s, clientID, "already submitted",
		), nil
	}

	errMsg := s.emptyInputArtifactsError(reg, evt)
	if errMsg != "" {
		return clientErrorTransition(s, clientID, errMsg), nil
	}

	errMsg = s.validateDeliveredInputArtifactCounts(
		ctx, clientID, reg, evt, env,
	)
	if errMsg != "" {
		return clientErrorTransition(s, clientID, errMsg), nil
	}

	// Build a map from outpoints to boarding inputs for quick lookup.
	outpointToInput := make(map[wire.OutPoint]*BoardingInput)
	for _, bi := range reg.BoardingInputs {
		outpointToInput[*bi.Outpoint] = bi
	}

	// Build a prevout fetcher from the PSBT's WitnessUtxo fields.
	tx := s.PSBT.UnsignedTx
	prevOutFetcher := buildInputSigPrevOutFetcher(s.PSBT)

	// Validate each signature cryptographically.
	for _, sig := range evt.Signatures {
		// Look up the boarding input for this signature.
		boardingInput, found := outpointToInput[sig.Outpoint]
		if !found {
			errMsg := fmt.Sprintf("unknown outpoint: %v",
				sig.Outpoint)

			return clientErrorTransition(s, clientID, errMsg), nil
		}

		// Verify the input index is valid.
		if sig.InputIndex < 0 || sig.InputIndex >= len(s.PSBT.Inputs) {
			errMsg := fmt.Sprintf("invalid input index: %d",
				sig.InputIndex)

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

	// Validate forfeit transactions when this delivery includes them.
	if len(evt.ForfeitTxs) > 0 {
		err := validateForfeitTxs(
			ctx, env.Log, evt.ForfeitTxs, reg,
			s.ConnectorAssignments, env.ForfeitScript,
			env.Terms.OperatorKey.PubKey,
		)
		if err != nil {
			return clientErrorTransition(
				s, clientID, err.Error(),
			), nil
		}
	}

	env.Log.DebugS(ctx, "Input artifacts validated successfully",
		LogClientID(clientID),
		LogSigCount(len(evt.Signatures)),
	)

	// Copy the completed-submissions tracker.
	newClientsSubmitted := make(map[clientconn.ClientID]struct{})
	for id := range s.ClientsSubmitted {
		newClientsSubmitted[id] = struct{}{}
	}

	// Copy collected signatures and add any new client's signatures.
	newCollectedSigs := make(InputSigsMap)
	for id, sigs := range s.CollectedSignatures {
		newCollectedSigs[id] = sigs
	}
	if len(evt.Signatures) > 0 {
		if _, exists := newCollectedSigs[clientID]; exists {
			return clientErrorTransition(
				s, clientID,
				"boarding signatures already submitted",
			), nil
		}

		newCollectedSigs[clientID] = evt.Signatures
	}

	// Copy collected forfeit txs and add any new client's submissions.
	newCollectedForfeitTxs := make(ForfeitTxsMap)
	for id, txs := range s.CollectedForfeitTxs {
		newCollectedForfeitTxs[id] = txs
	}
	if len(evt.ForfeitTxs) > 0 {
		if _, exists := newCollectedForfeitTxs[clientID]; exists {
			return clientErrorTransition(
				s, clientID, "forfeit txs already submitted",
			), nil
		}

		newCollectedForfeitTxs[clientID] = evt.ForfeitTxs
	}

	if len(reg.BoardingInputs) > 0 {
		if _, ok := newCollectedSigs[clientID]; !ok {
			env.Log.DebugS(ctx, "Waiting for boarding signatures",
				LogClientID(clientID),
				slog.Int("expected", len(reg.BoardingInputs)),
			)
		}
	}

	if len(reg.ForfeitInputs) > 0 {
		if _, ok := newCollectedForfeitTxs[clientID]; !ok {
			env.Log.DebugS(ctx, "Waiting for forfeit txs",
				LogClientID(clientID),
				slog.Int("expected", len(reg.ForfeitInputs)),
			)
		}
	}

	// Create new state with updated tracking. Every field carried by
	// AwaitingInputSigsState must be copied verbatim here: the struct
	// has no builder, so omitting a field silently zero-initialises
	// it. In particular, ChangeOutputIdx defaults to 0 (not -1) if
	// dropped, which would later have the ledger attribute output
	// index 0 -- a VTXO tree root -- as the wallet change output.
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
		ChangeOutputIdx:      s.ChangeOutputIdx,
		LockedOutpoints:      s.LockedOutpoints,
	}

	if newState.hasCompleteInputSubmission(clientID) {
		newState.ClientsSubmitted[clientID] = struct{}{}

		env.Log.InfoS(ctx, "Client input artifacts accepted",
			LogClientID(clientID),
			LogSubmitted(len(newState.ClientsSubmitted)),
			LogExpected(len(s.ClientRegistrations)),
		)
	} else {
		env.Log.InfoS(ctx, "Stored partial client input artifacts",
			LogClientID(clientID),
			slog.Bool(
				"has_boarding_sigs",
				len(newCollectedSigs[clientID]) > 0,
			),
			slog.Bool(
				"has_forfeit_txs",
				len(newCollectedForfeitTxs[clientID]) > 0,
			))
	}

	// Check if all clients have submitted.
	if newState.allClientsSubmitted() {
		env.Log.InfoS(
			ctx,
			"All signatures collected, transitioning to server "+
				"signing",
			LogClientCount(len(s.ClientRegistrations)),
		)

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
				ConnectorTrees:       s.ConnectorTrees,
				ChangeOutputIdx:      s.ChangeOutputIdx,
				LockedOutpoints:      s.LockedOutpoints,
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

func (s *AwaitingInputSigsState) emptyInputArtifactsError(
	reg *ClientRegistration, evt *ClientInputSignaturesEvent,
) string {

	if len(evt.Signatures) > 0 || len(evt.ForfeitTxs) > 0 {
		return ""
	}

	switch {
	case len(reg.BoardingInputs) > 0 && len(reg.ForfeitInputs) == 0:
		return fmt.Sprintf("expected %d signatures, got 0",
			len(reg.BoardingInputs))

	case len(reg.ForfeitInputs) > 0 && len(reg.BoardingInputs) == 0:
		return fmt.Sprintf("expected %d forfeit txs, got 0",
			len(reg.ForfeitInputs))

	default:
		return "no input artifacts submitted"
	}
}

func (s *AwaitingInputSigsState) validateDeliveredInputArtifactCounts(
	ctx context.Context, clientID clientconn.ClientID,
	reg *ClientRegistration, evt *ClientInputSignaturesEvent,
	env *Environment,
) string {

	if len(evt.Signatures) > 0 &&
		len(evt.Signatures) != len(reg.BoardingInputs) {

		env.Log.WarnS(ctx, "Signature count mismatch",
			nil,
			LogClientID(clientID),
			slog.Int("expected", len(reg.BoardingInputs)),
			slog.Int("got", len(evt.Signatures)),
		)

		return fmt.Sprintf("expected %d signatures, got %d",
			len(reg.BoardingInputs), len(evt.Signatures))
	}

	if len(evt.ForfeitTxs) > 0 &&
		len(evt.ForfeitTxs) != len(reg.ForfeitInputs) {
		return fmt.Sprintf("expected %d forfeit txs, got %d",
			len(reg.ForfeitInputs), len(evt.ForfeitTxs))
	}

	return ""
}

func buildInputSigPrevOutFetcher(
	packet *psbt.Packet,
) *txscript.MultiPrevOutFetcher {

	tx := packet.UnsignedTx
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(nil)
	for i, pIn := range packet.Inputs {
		if pIn.WitnessUtxo == nil {
			continue
		}

		prevOutFetcher.AddPrevOut(
			tx.TxIn[i].PreviousOutPoint, pIn.WitnessUtxo,
		)
	}

	return prevOutFetcher
}

// roundLockID derives a deterministic 32-byte UTXO lease identifier from
// the round ID. Using a deterministic ID allows the caller to release
// leases explicitly when a round fails.
func roundLockID(roundID RoundID) [32]byte {
	return sha256.Sum256(roundID[:])
}

// buildCommitmentTx constructs the commitment transaction PSBT with boarding
// inputs, forfeit inputs, required outputs (leaves), VTXO tree outputs, and
// connector outputs for forfeits. It funds the transaction using the wallet
// and builds both VTXO and connector trees if needed.
//
// LND's PsbtCoinSelect path cannot estimate witness weight for taproot
// script-path external inputs (lnd/lnwallet/btcwallet/psbt.go:
// EstimateInputWeight returns ErrScriptSpendFeeEstimationUnsupported), but
// it does accept P2TR key-spend external inputs. We exploit this with a
// dummy-then-swap approach so LND correctly accounts for the boarding
// contribution to the funding amount:
//
//  1. Build the outputs-only transaction (leaves, VTXO tree outputs,
//     connector outputs).
//  2. Pre-add each boarding input to the PSBT with key-spend appearance
//     (real outpoint, real WitnessUtxo, real internal key + merkle root,
//     but TaprootBip32Derivation with empty LeafHashes and no
//     TaprootLeafScript). LND's validateSigningMethod routes this to
//     TaprootKeySpendSignMethod and EstimateInputWeight charges
//     TaprootKeyPathWitnessSize per input.
//  3. Call FundPsbt. LND sees inputSum = Σboarding and only adds wallet
//     inputs to cover (outputs - Σboarding + fees) — not the full output
//     value. This is the operator-side liquidity fix from issue #309.
//  4. Locate boarding inputs by PreviousOutPoint (LND may reorder) and
//     swap their PSBT metadata to the real script-spend layout
//     (TaprootLeafScript + non-empty LeafHashes).
//  5. Reduce the change output by the witness-weight delta fee:
//     feeRate.FeeForWeight((scriptW − keySpendW) × N). LND under-charged
//     fees because it estimated the boarding inputs as ~66 wu key-spends,
//     but the real script-path witness is ~235 wu. Subtracting the delta
//     from change keeps the implicit miner fee at the script-spend level
//     once the real witnesses are attached at finalization time.
//
// If FundPsbt produces no change output (changeIdx == -1) but boarding
// inputs are present, we cannot compensate for the witness-weight delta
// and would silently overpay miners. In that case we fail with
// ErrChangeRequiredForBoarding so the operator can top up their wallet.
//
//nolint:funlen
func buildCommitmentTx(ctx context.Context, terms *batch.Terms,
	feeEstimator chainfee.Estimator, confTarget uint32,
	walletCtrl WalletController, minConfs int32, walletAccount string,
	boardingInputs []*BoardingInput, forfeitInputs []*ForfeitInput,
	requiredOutputs []*wire.TxOut, vtxoDescriptors []tree.VTXODescriptor,
	opts *FundingOpts) (*psbt.Packet, int32, map[int]*tree.Tree,
	map[int]*tree.Tree, map[wire.OutPoint]*ConnectorLeafAssignment,
	[]wire.OutPoint, error) {

	feeRate, err := feeEstimator.EstimateFeePerKW(confTarget)
	if err != nil {
		return nil, -1, nil, nil, nil, nil,
			fmt.Errorf("estimate fee: %w", err)
	}

	// Build outputs-only transaction first; we'll attach boarding inputs
	// below before calling FundPsbt.
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
			return nil, -1, nil, nil, nil, nil,
				fmt.Errorf("build batch outputs: %w", err)
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
			return nil, -1, nil, nil, nil, nil, fmt.Errorf("max " +
				"connectors per tree must be > 0")
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
				return nil, -1, nil, nil, nil, nil, fmt.Errorf(
					"build connector "+
						"output: %w", err)
			}

			connectorOutputs = append(
				connectorOutputs, connectorOutput,
			)
			tx.AddTxOut(connectorOutput)
		}
	}

	packet, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, -1, nil, nil, nil, nil,
			fmt.Errorf("create psbt: %w", err)
	}

	// Pre-add boarding inputs to the PSBT with key-spend appearance so
	// LND's PsbtCoinSelect path counts them in inputSum and only requires
	// the wallet to fund (outputs - Σboarding + fees). The metadata is
	// swapped to the real script-spend layout after FundPsbt returns.
	for _, bi := range boardingInputs {
		pin, err := boardingPInputKeySpend(bi)
		if err != nil {
			return nil, -1, nil, nil, nil, nil, fmt.Errorf("build "+
				"boarding key-spend pinput: %w", err)
		}

		packet.UnsignedTx.TxIn = append(packet.UnsignedTx.TxIn,
			&wire.TxIn{
				PreviousOutPoint: *bi.Outpoint,
				Sequence:         wire.MaxTxInSequenceNum,
			},
		)
		packet.Inputs = append(packet.Inputs, pin)
	}

	// Now we'll call FundPsbt to add wallet inputs and change.
	//
	// Note: FundPsbt reorders inputs and outputs, so any indices
	// recorded before this call will be invalid.
	changeIdx, lockedOutpoints, err := walletCtrl.FundPsbt(
		ctx, packet, minConfs, feeRate, walletAccount, opts,
	)
	if err != nil {
		return nil, -1, nil, nil, nil, nil,
			fmt.Errorf("fund psbt: %w", err)
	}

	// releaseOnErr is a helper that releases the locked outpoints
	// acquired by FundPsbt above. Any error after FundPsbt succeeds
	// must call this before returning so that the UTXOs don't stay
	// locked until lease expiry.
	releaseOnErr := func(cause error) (*psbt.Packet, int32,
		map[int]*tree.Tree, map[int]*tree.Tree,
		map[wire.OutPoint]*ConnectorLeafAssignment, []wire.OutPoint,
		error) {

		if opts != nil && len(lockedOutpoints) > 0 {
			// Best-effort release; nothing useful we can do
			// with a release error here.
			_ = walletCtrl.ReleaseInputs(
				ctx, opts.LockID, lockedOutpoints,
			)
		}

		return nil, -1, nil, nil, nil, nil, cause
	}

	// Swap each boarding input's PSBT metadata from the dummy key-spend
	// appearance we used to coax LND's coin selector into the real
	// script-spend layout. We look up by PreviousOutPoint because LND is
	// allowed to reorder inputs during coin selection — index would be
	// unsafe.
	//
	// We pre-build a map[outpoint]index over packet.UnsignedTx.TxIn so
	// the swap is O(M) instead of O(N·M), and so duplicates surface as
	// a typed error rather than silently overwriting the same input.
	indexByOutpoint, err := buildInputIndexMap(packet)
	if err != nil {
		return releaseOnErr(err)
	}
	swapped := make(map[wire.OutPoint]struct{}, len(boardingInputs))
	for _, bi := range boardingInputs {
		if _, dup := swapped[*bi.Outpoint]; dup {
			return releaseOnErr(
				fmt.Errorf("%w: %v in boarding inputs",
					ErrDuplicateBoardingOutpoint,
					bi.Outpoint),
			)
		}

		idx, ok := indexByOutpoint[*bi.Outpoint]
		if !ok {
			return releaseOnErr(
				fmt.Errorf("boarding outpoint %v missing "+
					"from funded psbt", bi.Outpoint),
			)
		}

		// Defensive check: the swap is wholesale (Inputs[idx] = pin),
		// which is only safe if LND did not decorate our pre-added
		// boarding PInput. Today LND's DecorateInputs is a no-op for
		// non-wallet inputs (boarding outpoints fail ErrNotMine, so
		// failOnUnknown=false skips them), but a future LND change
		// that decorates external inputs would be silently dropped
		// here. Fail-closed if any assumption-violating field is set.
		if err := assertBoardingPInputUntouched(
			&packet.Inputs[idx],
		); err != nil {
			return releaseOnErr(
				fmt.Errorf("boarding input %v decorated "+
					"by lnd: %w", bi.Outpoint, err),
			)
		}

		pin, err := boardingPInputScriptSpend(bi)
		if err != nil {
			return releaseOnErr(
				fmt.Errorf("build boarding script-spend "+
					"pinput: %w", err),
			)
		}

		packet.Inputs[idx] = pin
		swapped[*bi.Outpoint] = struct{}{}
	}

	// Compensate the change output for LND's underestimated boarding
	// witness weight. LND charged fees as if each boarding input were
	// a P2TR key-spend (TaprootKeyPathWitnessSize), but each boarding
	// input is actually spent via its collab tapscript leaf. We drive
	// the script-spend weight through input.TxWeightEstimator's
	// AddTapscriptInput, which reads the real leaf script length and
	// the real merkle inclusion proof depth from a partial-reveal
	// Tapscript; this is the same machinery LND uses internally for
	// known tapscripts and matches what wire.TxWitness produces at
	// signing. We then subtract a single FeeForWeight call (so integer
	// truncation lands once, not twice). Doing this BEFORE tree
	// construction makes the tree's batchOutpoint hash bind to the
	// final commitment txid that will be broadcast.
	//
	// When FundPsbt produced no change (changeIdx < 0) the residual
	// from boarding inputs minus outputs and LND-estimated fees was
	// below LND's change-dust threshold, so the residual implicitly
	// went to miners. We can't subtract the witness-delta there, so
	// the on-chain effective fee rate ends up slightly below the
	// requested rate (the real witness is bigger than LND charged
	// for). The mismatch is bounded by `change_dust_limit + delta_fee`
	// — a few hundred sats — and only affects confirmation latency,
	// not validity, so we log a warning and proceed instead of
	// failing the round.
	if len(boardingInputs) > 0 && changeIdx >= 0 {
		var keySpendEst input.TxWeightEstimator
		for range boardingInputs {
			keySpendEst.AddTaprootKeySpendInput(
				txscript.SigHashDefault,
			)
		}

		var scriptSpendEst input.TxWeightEstimator
		for _, bi := range boardingInputs {
			scriptTapscript, err := boardingScriptSpendTapscript(
				bi,
			)
			if err != nil {
				return releaseOnErr(
					fmt.Errorf("build boarding "+
						"script-spend tapscript: %w",
						err),
				)
			}

			scriptSpendEst.AddTapscriptInput(
				collabLeafWitnessSize, scriptTapscript,
			)
		}

		extraFee := feeRate.FeeForWeight(
			scriptSpendEst.Weight() - keySpendEst.Weight(),
		)

		// Clamp the subtraction so the change output never goes
		// negative or to zero. LND's change can land at dust when
		// the residual after fees is barely above its dust limit,
		// in which case a full witness-delta deduction would
		// invalidate the tx ("bad txns vout negative" at broadcast).
		// In that case we cap the subtraction at
		// `change.Value − dust_floor`; the unrecovered delta lands
		// implicitly in the miner fee, the same bounded
		// underpayment the no-change branch tolerates.
		const p2wkhDustFloor = 294
		changeOut := packet.UnsignedTx.TxOut[changeIdx]
		maxSub := changeOut.Value - p2wkhDustFloor
		if maxSub < 0 {
			maxSub = 0
		}
		sub := int64(extraFee)
		if sub > maxSub {
			sub = maxSub
		}
		changeOut.Value -= sub
	}
	// When changeIdx < 0 with boarding inputs, FundPsbt determined the
	// would-be change was below LND's dust threshold and let it go to
	// miners. The dropped amount is bounded by LND's coin-selection
	// invariant (`changeAmt < dust_limit ≈ 294-546 sat`), so the
	// overpay above target fee is at most a few hundred sats — not a
	// function of feeRate or round size. We can't bill the
	// witness-weight delta against a non-existent change output, so
	// the on-chain effective fee rate ends up slightly under target;
	// only confirmation latency suffers, not validity. The caller
	// (FSM transition) logs a warning so operators can observe.

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
			return releaseOnErr(
				fmt.Errorf("find batch outputs: %w", err),
			)
		}

		// Build VTXO trees using the post-FundPsbt batch output
		// indices.
		vtxoTrees, err = vtxoTreeCtx.BuildVTXOTreesForCommitmentTx(
			packet.UnsignedTx, batchOutputIndices,
		)
		if err != nil {
			return releaseOnErr(
				fmt.Errorf("build VTXO trees: %w", err),
			)
		}
	}

	// Step 7: Build connector trees and assignments if forfeits
	// exist.
	var (
		connectorTrees       map[int]*tree.Tree
		connectorAssignments map[wire.OutPoint]*ConnectorLeafAssignment
	)
	if numForfeits > 0 {
		connectorOutputIndices, err := findOutputIndices(
			connectorOutputs, packet.UnsignedTx,
		)
		if err != nil {
			return releaseOnErr(
				fmt.Errorf("find connector outputs: %w", err),
			)
		}

		connectorTrees, connectorAssignments, err =
			buildConnectorTreesAndAssignments(
				terms, packet.UnsignedTx, forfeitInputs,
				connectorOutputIndices,
			)
		if err != nil {
			return releaseOnErr(
				fmt.Errorf("build connector trees: %w", err),
			)
		}
	}

	return packet, changeIdx, vtxoTrees, connectorTrees,
		connectorAssignments, lockedOutpoints, nil
}

// boardingPInputKeySpend returns a psbt.PInput for a boarding input that
// looks like a P2TR key-spend to LND's PsbtCoinSelect path. We use this
// to coax LND into estimating the input's witness weight (it errors on
// taproot script-path external inputs) and counting its value toward the
// inputSum that reduces fundingAmount. The metadata is replaced with the
// real script-spend layout (boardingPInputScriptSpend) after FundPsbt
// returns. The key-spend appearance is achieved by:
//   - Populating WitnessUtxo with the real value and pkScript.
//   - Populating TaprootInternalKey and TaprootMerkleRoot from the real
//     control block (validateSigningMethod accepts either BIP0086 or
//     tap-tweaked key-spend, both producing TaprootKeyPathWitnessSize).
//   - Providing a TaprootBip32Derivation for the operator key with empty
//     LeafHashes (the discriminator that selects key-spend over
//     script-spend in lnd/lnwallet/btcwallet/psbt.go:validateSigningMethod).
//   - NOT setting TaprootLeafScript (its presence would push detection to
//     script-spend and trigger ErrScriptSpendFeeEstimationUnsupported).
func boardingPInputKeySpend(bi *BoardingInput) (psbt.PInput, error) {
	if len(bi.Tapscript.Leaves) == 0 {
		return psbt.PInput{}, fmt.Errorf("boarding tapscript missing " +
			"collab leaf")
	}

	collabLeaf := bi.Tapscript.Leaves[0]
	keyFamily := uint32(bi.OperatorKeyDesc.Family)
	bip32Path := []uint32{keyFamily, bi.OperatorKeyDesc.Index}

	return psbt.PInput{
		WitnessUtxo: &wire.TxOut{
			Value:    int64(bi.Value),
			PkScript: bi.PkScript,
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
				LeafHashes:           nil,
				MasterKeyFingerprint: 0,
				Bip32Path:            bip32Path,
			},
		},
	}, nil
}

// boardingPInputScriptSpend returns the real script-spend psbt.PInput for
// a boarding input. This is the metadata the rest of the FSM (signing,
// finalization) consumes; we install it after FundPsbt has committed to
// a funding amount on the basis of the dummy key-spend appearance.
func boardingPInputScriptSpend(bi *BoardingInput) (psbt.PInput, error) {
	if len(bi.Tapscript.Leaves) == 0 {
		return psbt.PInput{}, fmt.Errorf("boarding tapscript missing " +
			"collab leaf")
	}

	collabLeaf := bi.Tapscript.Leaves[0]
	ctrlBlockBytes, err := bi.Tapscript.ControlBlock.ToBytes()
	if err != nil {
		return psbt.PInput{}, fmt.Errorf("serialize control block: %w",
			err)
	}

	leafHash := txscript.NewTapLeaf(
		collabLeaf.LeafVersion, collabLeaf.Script,
	).TapHash()
	leafHashBytes := leafHash[:]

	keyFamily := uint32(bi.OperatorKeyDesc.Family)
	bip32Path := []uint32{keyFamily, bi.OperatorKeyDesc.Index}

	return psbt.PInput{
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
	}, nil
}

// assertBoardingPInputUntouched verifies that the post-FundPsbt PInput
// for a boarding input contains only the fields we pre-populated in
// boardingPInputKeySpend. If LND has added anything else
// (NonWitnessUtxo, PartialSigs, or a non-taproot Bip32Derivation), it
// indicates LND treated the input as wallet-owned — which would
// invalidate the wholesale swap. We surface that as
// ErrBoardingPInputDecorated so the round fails fast rather than
// silently dropping the decoration on the swap. The TaprootBip32
// derivation entry we set ourselves is allowed (and required); we
// cap it at one entry for the same reason.
func assertBoardingPInputUntouched(pin *psbt.PInput) error {
	if pin.NonWitnessUtxo != nil {
		return fmt.Errorf("%w: NonWitnessUtxo populated",
			ErrBoardingPInputDecorated)
	}
	if len(pin.PartialSigs) != 0 {
		return fmt.Errorf("%w: %d PartialSigs populated",
			ErrBoardingPInputDecorated, len(pin.PartialSigs))
	}
	if len(pin.Bip32Derivation) != 0 {
		return fmt.Errorf("%w: %d non-taproot Bip32Derivations "+
			"populated", ErrBoardingPInputDecorated,
			len(pin.Bip32Derivation))
	}
	if len(pin.TaprootBip32Derivation) > 1 {
		return fmt.Errorf("%w: %d TaprootBip32Derivations (expected 1)",
			ErrBoardingPInputDecorated,
			len(pin.TaprootBip32Derivation))
	}

	return nil
}

// boardingScriptSpendTapscript returns a *waddrmgr.Tapscript in
// partial-reveal form that exposes the collab leaf script and the
// merkle inclusion proof needed to reach the boarding output's root.
// We feed this to input.TxWeightEstimator.AddTapscriptInput so the
// per-input witness weight is computed from the actual leaf script
// bytes and the real control-block depth, not from a magic constant.
//
// The collab leaf is at index 0 by construction in arkscript.VTXOTapScript
// (see client/lib/arkscript/spend_helpers.go). The control block we
// derive here matches what arkscript.SpendInfo.CollabWitness installs
// at signing time, so the estimator-derived weight equals the real
// on-chain witness weight once finalize attaches the signatures.
func boardingScriptSpendTapscript(bi *BoardingInput) (*waddrmgr.Tapscript,
	error) {

	if len(bi.Tapscript.Leaves) == 0 {
		return nil, fmt.Errorf("boarding tapscript missing collab leaf")
	}

	const collabLeafIdx = 0
	tapTree := txscript.AssembleTaprootScriptTree(
		bi.Tapscript.Leaves...,
	)
	leafProof := tapTree.LeafMerkleProofs[collabLeafIdx]
	controlBlock := leafProof.ToControlBlock(&arkscript.ARKNUMSKey)

	return &waddrmgr.Tapscript{
		Type:           waddrmgr.TapscriptTypePartialReveal,
		ControlBlock:   &controlBlock,
		RevealedScript: bi.Tapscript.Leaves[collabLeafIdx].Script,
	}, nil
}

// buildInputIndexMap returns a map from each input outpoint in the
// packet to its index in packet.UnsignedTx.TxIn. If the packet contains
// the same outpoint at two different positions (which would indicate a
// malformed funded PSBT — wallet UTXOs and external boarding inputs
// must be disjoint), the function returns ErrDuplicateBoardingOutpoint
// so callers fail-closed rather than silently aliasing inputs.
func buildInputIndexMap(packet *psbt.Packet) (map[wire.OutPoint]int, error) {
	indexByOutpoint := make(
		map[wire.OutPoint]int, len(packet.UnsignedTx.TxIn),
	)
	for i, txIn := range packet.UnsignedTx.TxIn {
		if _, dup := indexByOutpoint[txIn.PreviousOutPoint]; dup {
			return nil, fmt.Errorf("%w: %v at indices %d and %d",
				ErrDuplicateBoardingOutpoint,
				txIn.PreviousOutPoint,
				indexByOutpoint[txIn.PreviousOutPoint], i)
		}
		indexByOutpoint[txIn.PreviousOutPoint] = i
	}

	return indexByOutpoint, nil
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
// connector assignments. The radix is stamped on each descriptor so the
// fraud responder can rebuild the connector path with the same branching
// factor used at finalization, regardless of any later runtime config
// rotation.
func buildConnectorDescriptors(
	connectorAssignments map[wire.OutPoint]*ConnectorLeafAssignment,
	forfeitScript []byte, radix int) ([]*ConnectorTreeDescriptor, error) {

	if len(connectorAssignments) == 0 {
		return nil, nil
	}

	if radix < 2 {
		return nil, fmt.Errorf("connector tree radix %d must be "+
			"at least 2", radix)
	}

	counts := make(map[int]int)
	for _, assignment := range connectorAssignments {
		if assignment == nil {
			return nil, fmt.Errorf("connector assignment cannot " +
				"be nil")
		}

		if assignment.ConnectorOutputIndex < 0 {
			return nil, fmt.Errorf("connector output index must " +
				"be non-negative")
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
			Radix:         radix,
		})
	}

	return descriptors, nil
}

// buildConnectorTreesAndAssignments builds connector trees and assigns each
// forfeit input to a connector leaf.
func buildConnectorTreesAndAssignments(terms *batch.Terms, tx *wire.MsgTx,
	forfeitInputs []*ForfeitInput, connectorOutputIndices []int) (
	map[int]*tree.Tree, map[wire.OutPoint]*ConnectorLeafAssignment, error) {

	numForfeits := len(forfeitInputs)
	if numForfeits == 0 {
		return nil, nil, nil
	}

	sortedForfeits := make([]*ForfeitInput, 0, numForfeits)
	for _, input := range forfeitInputs {
		if input == nil || input.Outpoint == nil {
			return nil, nil, fmt.Errorf("forfeit input outpoint " +
				"is nil")
		}

		sortedForfeits = append(sortedForfeits, input)
	}
	sort.Slice(sortedForfeits, func(i, j int) bool {
		return sortedForfeits[i].Outpoint.String() <
			sortedForfeits[j].Outpoint.String()
	})

	maxPerTree := int(terms.MaxConnectorsPerTree)
	if maxPerTree <= 0 {
		return nil, nil, fmt.Errorf("max connectors per tree must be " +
			"> 0")
	}

	if terms.ConnectorDustAmount <= 0 {
		return nil, nil, fmt.Errorf("connector dust amount must be > 0")
	}

	if terms.ConnectorAddress == nil {
		return nil, nil, fmt.Errorf("connector address cannot be nil")
	}

	if terms.OperatorKey.PubKey == nil {
		return nil, nil, fmt.Errorf("operator key cannot be nil")
	}

	radix := int(terms.ConnectorTreeRadix)
	if radix < 2 {
		return nil, nil, fmt.Errorf("connector tree radix %d must be "+
			"at least 2", radix)
	}

	expectedOutputs := (numForfeits + maxPerTree - 1) / maxPerTree
	if len(connectorOutputIndices) != expectedOutputs {
		return nil, nil, fmt.Errorf("connector output count mismatch: "+
			"%d != %d", len(connectorOutputIndices),
			expectedOutputs)
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
			return nil, nil, fmt.Errorf("build connector tree "+
				"%d: %w", i, err)
		}

		leaves := connectorTree.Root.GetLeafNodes()
		if len(leaves) != numInOutput {
			return nil, nil, fmt.Errorf("connector tree %d leaf "+
				"count mismatch: %d != %d", i, len(leaves),
				numInOutput)
		}

		connectorTrees[outputIdx] = connectorTree

		for leafIdx := 0; leafIdx < numInOutput; leafIdx++ {
			forfeitInput := sortedForfeits[offset+leafIdx]
			leaf := leaves[leafIdx]
			leafOutpoint, err := leaf.GetNonAnchorOutpoint()
			if err != nil {
				return nil, nil, fmt.Errorf("connector leaf "+
					"outpoint: %w", err)
			}

			leafOutput, err := leafNonAnchorOutput(leaf)
			if err != nil {
				return nil, nil, fmt.Errorf("connector leaf "+
					"output: %w", err)
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

	anchorScript := arkscript.AnchorOutput().PkScript
	for _, output := range leaf.Outputs {
		if !bytes.Equal(output.PkScript, anchorScript) {
			return output, nil
		}
	}

	return nil, fmt.Errorf("no non-anchor output found")
}

// ProcessEvent handles events in the AwaitingVTXONoncesState. This state waits
// for clients with VTXOs to submit their MuSig2 nonces.
func (s *AwaitingVTXONoncesState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Processing event",
		LogState("AwaitingVTXONonces"),
		LogEvent(event),
		LogSubmitted(len(s.ClientsWithNonces)),
		slog.Int("tree_count", len(s.VTXOTrees)),
	)

	switch evt := event.(type) {
	case *ClientVTXONoncesEvent:
		return s.handleClientNonces(ctx, env, evt)

	case *VTXONoncesTimeoutEvent:
		env.Log.WarnS(ctx, "VTXO nonce collection timeout",
			nil,
			LogSubmitted(len(s.ClientsWithNonces)),
		)

		// The timeout was reached before all nonces were collected.
		return buildFailureTransition(
			ctx, env, s.ClientRegistrations, "VTXO nonce "+
				"collection timeout", roundLockID(env.RoundID),
			s.LockedOutpoints,
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
		LogKeyCount(len(evt.Nonces)),
	)

	// Check if client is registered in this round.
	reg, exists := s.ClientRegistrations[clientID]
	if !exists {
		env.Log.WarnS(ctx, "Client not registered",
			nil,
			LogClientID(clientID),
		)

		return clientErrorTransition(
			s, clientID, "not registered in this round",
		), nil
	}

	// Only accept nonces from clients with VTXOs.
	if len(reg.VTXODescriptors) == 0 {
		env.Log.WarnS(ctx, "Client has no VTXOs",
			nil,
			LogClientID(clientID),
		)

		return clientErrorTransition(
			s, clientID, "client has no VTXOs",
		), nil
	}

	if s.hasClientSubmittedNonces(clientID) {
		env.Log.WarnS(ctx, "Client already submitted nonces",
			nil,
			LogClientID(clientID),
		)

		return clientErrorTransition(
			s, clientID, "already submitted nonces",
		), nil
	}

	if len(evt.Nonces) == 0 {
		env.Log.WarnS(ctx, "No nonces provided",
			nil,
			LogClientID(clientID),
		)

		return clientErrorTransition(
			s, clientID, "no nonces provided",
		), nil
	}

	// Verify client submitted nonces for all their signing keys.
	for keyHex := range reg.VTXODescriptors {
		if _, ok := evt.Nonces[keyHex]; !ok {
			errMsg := fmt.Sprintf("missing nonces for "+
				"signing key %x", keyHex[:])

			return clientErrorTransition(s, clientID, errMsg), nil
		}
	}

	totalAccepted := 0

	for signingKeyHex, nonces := range evt.Nonces {
		if len(nonces) == 0 {
			errMsg := fmt.Sprintf("no nonces for signing key %x",
				signingKeyHex[:])

			return clientErrorTransition(s, clientID, errMsg), nil
		}

		desc := reg.VTXODescriptors[signingKeyHex]
		if desc == nil || desc.CoSignerKey == nil || nonces == nil {
			errMsg := fmt.Sprintf("unknown signing key %x",
				signingKeyHex[:])

			return clientErrorTransition(s, clientID, errMsg), nil
		}

		for idx, coordinator := range s.TreeSignCoordinators {
			accepted, err := coordinator.AddNonces(
				desc.CoSignerKey, nonces,
			)
			if err != nil {
				errMsg := fmt.Sprintf("add nonces for tree "+
					"%d: %v", idx, err)

				return clientErrorTransition(
					s, clientID, errMsg,
				), nil
			}

			totalAccepted += accepted
		}
	}

	if totalAccepted == 0 {
		env.Log.WarnS(ctx, "No valid nonces provided",
			nil,
			LogClientID(clientID),
		)

		return clientErrorTransition(
			s, clientID, "no valid nonces provided",
		), nil
	}

	env.Log.DebugS(ctx, "Nonces validated successfully",
		LogClientID(clientID),
		slog.Int("accepted_count", totalAccepted),
	)

	// Track that this client has submitted nonces.
	newClientsWithNonces := make(map[clientconn.ClientID]struct{})
	for cid := range s.ClientsWithNonces {
		newClientsWithNonces[cid] = struct{}{}
	}
	newClientsWithNonces[clientID] = struct{}{}

	env.Log.InfoS(ctx, "Client nonces accepted",
		LogClientID(clientID),
		LogSubmitted(len(newClientsWithNonces)),
	)

	// Create new state with updated tracking. ChangeOutputIdx and
	// LockedOutpoints must be carried forward verbatim: the former
	// defaults to 0 (not -1) if dropped and mis-attributes a VTXO
	// tree root as the wallet change output downstream; the latter
	// is needed to unlock wallet inputs if the round later fails.
	newState := &AwaitingVTXONoncesState{
		ClientRegistrations:  s.ClientRegistrations,
		PSBT:                 s.PSBT,
		VTXOTrees:            s.VTXOTrees,
		ConnectorTrees:       s.ConnectorTrees,
		ConnectorAssignments: s.ConnectorAssignments,
		TreeSignCoordinators: s.TreeSignCoordinators,
		ClientsWithNonces:    newClientsWithNonces,
		ChangeOutputIdx:      s.ChangeOutputIdx,
		LockedOutpoints:      s.LockedOutpoints,
	}

	// Check if all clients have submitted nonces.
	if newState.allClientsSubmittedNonces() {
		env.Log.InfoS(
			ctx, "All nonces collected, transitioning to VTXO "+
				"signatures",
		)

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
		slog.Int("tree_count", len(s.TreeSignCoordinators)),
	)

	// Generate operator's partial signatures for all trees. This must be
	// done after all client nonces are collected.
	for idx, coordinator := range s.TreeSignCoordinators {
		err := coordinator.Sign()
		if err != nil {
			env.Log.WarnS(ctx, "Operator signing failed",
				err,
				LogOutputIndex(idx),
			)

			return buildFailureTransition(
				ctx, env, s.ClientRegistrations,
				fmt.Sprintf("operator sign for tree %d: %v",
					idx, err),
				roundLockID(env.RoundID),
				s.LockedOutpoints,
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
				GetAggNoncesForSigners(
					clientKeys,
				)
			if err != nil {
				return buildFailureTransition(
					ctx, env, s.ClientRegistrations,
					fmt.Sprintf("get agg nonces for "+
						"%s: %v", clientID, err),
					roundLockID(env.RoundID),
					s.LockedOutpoints,
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
			ChangeOutputIdx: s.ChangeOutputIdx,
			LockedOutpoints: s.LockedOutpoints,
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
		LogSubmitted(len(s.ClientsWithSignatures)),
	)

	switch evt := event.(type) {
	case *ClientVTXOPartialSigsEvent:
		return s.handleClientPartialSigs(ctx, env, evt)

	case *VTXOSignaturesTimeoutEvent:
		env.Log.WarnS(ctx, "VTXO signature collection timeout",
			nil,
			LogSubmitted(len(s.ClientsWithSignatures)),
		)

		// Timeout expired before all partial sigs were collected.
		return buildFailureTransition(
			ctx, env, s.ClientRegistrations, "VTXO signature "+
				"collection timeout", roundLockID(env.RoundID),
			s.LockedOutpoints,
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
		LogKeyCount(len(evt.Signatures)),
	)

	// Check if client is registered in this round.
	reg, exists := s.ClientRegistrations[clientID]
	if !exists {
		env.Log.WarnS(ctx, "Client not registered",
			nil,
			LogClientID(clientID),
		)

		return clientErrorTransition(
			s, clientID, "not registered in this round",
		), nil
	}

	// Only accept signatures from clients with VTXOs.
	if len(reg.VTXODescriptors) == 0 {
		env.Log.WarnS(ctx, "Client has no VTXOs",
			nil,
			LogClientID(clientID),
		)

		return clientErrorTransition(
			s, clientID, "client has no VTXOs",
		), nil
	}

	if len(evt.Signatures) == 0 {
		env.Log.WarnS(ctx, "No signatures provided",
			nil,
			LogClientID(clientID),
		)

		return clientErrorTransition(
			s, clientID, "no signatures provided",
		), nil
	}

	if s.hasClientSubmittedSignatures(clientID) {
		env.Log.WarnS(ctx, "Client already submitted signatures",
			nil,
			LogClientID(clientID),
		)

		return clientErrorTransition(
			s, clientID, "already submitted signatures",
		), nil
	}

	// Verify client submitted signatures for all their signing keys.
	for keyHex := range reg.VTXODescriptors {
		if _, ok := evt.Signatures[keyHex]; !ok {
			errMsg := fmt.Sprintf("missing signatures for "+
				"signing key %x", keyHex[:])

			return clientErrorTransition(s, clientID, errMsg), nil
		}
	}

	totalAccepted := 0

	for signingKeyHex, sigs := range evt.Signatures {
		if len(sigs) == 0 {
			errMsg := fmt.Sprintf("no signatures for "+
				"signing key %x", signingKeyHex[:])

			return clientErrorTransition(s, clientID, errMsg), nil
		}

		desc := reg.VTXODescriptors[signingKeyHex]
		if desc == nil || desc.CoSignerKey == nil || sigs == nil {
			errMsg := fmt.Sprintf("unknown signing key %x",
				signingKeyHex[:])

			return clientErrorTransition(s, clientID, errMsg), nil
		}

		for idx, coordinator := range s.TreeSignCoordinators {
			accepted, err := coordinator.AddPartialSignatures(
				desc.CoSignerKey, sigs,
			)
			if err != nil {
				errMsg := fmt.Sprintf("add sigs for tree "+
					"%d: %v", idx, err)

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

	// Create new state with updated tracking. ChangeOutputIdx must be
	// carried forward -- dropping it silently zero-inits to 0, and
	// the downstream ledger attribution would then treat output 0
	// (a VTXO tree root) as the wallet change output.
	newState := &AwaitingVTXOSignaturesState{
		ClientRegistrations:   s.ClientRegistrations,
		PSBT:                  s.PSBT,
		VTXOTrees:             s.VTXOTrees,
		ConnectorTrees:        s.ConnectorTrees,
		ConnectorAssignments:  s.ConnectorAssignments,
		TreeSignCoordinators:  s.TreeSignCoordinators,
		ClientsWithSignatures: newClientsWithSignatures,
		ChangeOutputIdx:       s.ChangeOutputIdx,
		LockedOutpoints:       s.LockedOutpoints,
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
func (s *AwaitingVTXOSignaturesState) transitionToInputSigs(ctx context.Context,
	env *Environment) (*StateTransition, error) {

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
				errMsg := fmt.Sprintf("get final sigs for "+
					"client %s: %v", clientID, err)

				return buildFailureTransition(
					ctx, env, s.ClientRegistrations, errMsg,
					roundLockID(env.RoundID),
					s.LockedOutpoints,
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

	// Apply aggregated signatures to VTXOTrees so they are
	// persisted with signatures when the round is stored. This
	// enables OOR receivers to obtain signed tree paths from the
	// indexer for unilateral exit.
	for idx, coordinator := range s.TreeSignCoordinators {
		allSigs, err := coordinator.AllFinalSigs()
		if err != nil {
			return nil, fmt.Errorf("failed to collect final sigs "+
				"for tree persistence: %w", err)
		}

		// VTXOTrees is a map keyed by commitment-tx output index (not
		// a dense slice), so check map membership rather than a
		// slice-style bounds check. The previous len() comparison
		// silently skipped trees whenever the output index landed at
		// or beyond the map size, e.g. once buildCommitmentTx
		// produces a change output that pushes the tree root past
		// index 0.
		vtxoTree, ok := s.VTXOTrees[idx]
		if !ok {
			continue
		}

		if err := vtxoTree.SubmitTreeSigs(allSigs); err != nil {
			return nil, fmt.Errorf("submit tree sigs to "+
				"VTXOTree: %w", err)
		}
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
		int(env.Terms.ConnectorTreeRadix),
	)
	if err != nil {
		return buildFailureTransition(
			ctx, env, s.ClientRegistrations,
			fmt.Sprintf("build connector descriptors: %v", err),
			roundLockID(env.RoundID), s.LockedOutpoints,
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
			ChangeOutputIdx:     s.ChangeOutputIdx,
			LockedOutpoints:     s.LockedOutpoints,
		},
		NewEvents: fn.Some(EmittedEvent{
			Outbox: outboxMsgs,
		}),
	}, nil
}

// ProcessEvent handles events in the ServerSigningState. This state signs the
// server's wallet inputs on the commitment transaction.
func (s *ServerSigningState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Processing event",
		LogState("ServerSigning"),
		LogEvent(event),
	)

	switch event.(type) {
	case *ServerSignInputsEvent:
		return s.handleServerSigning(ctx, env)

	default:
		return unexpectedEvent(s, "server-signing", event, env), nil
	}
}

// handleServerSigning performs server-side signing of all inputs in the
// PSBT. For boarding inputs, it adds the operator's signature to complete
// the collaborative spend path. For wallet inputs, it calls FinalizePsbt.
// After signing, it persists the round and VTXOs, then transitions to
// FinalizedState with a BroadcastRoundReq.
func (s *ServerSigningState) handleServerSigning(ctx context.Context,
	env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Server signing inputs",
		slog.Int("input_count", len(s.PSBT.Inputs)),
		LogClientCount(len(s.CollectedSignatures)),
	)

	lockID := roundLockID(env.RoundID)

	// Sign all boarding inputs with the collected client signatures
	// and the operator's signatures.
	err := signBoardingInputs(
		s.PSBT, s.CollectedSignatures, s.ClientRegistrations,
		env.WalletController,
	)
	if err != nil {
		env.Log.WarnS(ctx, "Failed to sign boarding inputs", err)

		return buildFailureTransition(
			ctx, env, s.ClientRegistrations, fmt.Sprintf("failed "+
				"to sign boarding inputs: %v", err),
			lockID,
			s.LockedOutpoints,
		), nil
	}

	forfeitInfos := make(map[wire.OutPoint]*ForfeitInfo)

	// Complete forfeit transactions with the server's signatures.
	for clientID, reg := range s.ClientRegistrations {
		if len(reg.ForfeitInputs) == 0 {
			continue
		}

		if len(s.ConnectorAssignments) == 0 {
			return buildFailureTransition(
				ctx, env, s.ClientRegistrations, fmt.Sprintf(
					"connector assignments missing for "+
						"client %s", clientID),
				lockID,
				s.LockedOutpoints,
			), nil
		}

		forfeitTxs, ok := s.CollectedForfeitTxs[clientID]
		if !ok {
			return buildFailureTransition(
				ctx, env, s.ClientRegistrations, fmt.Sprintf(
					"missing forfeit txs for "+
						"client %s", clientID),
				lockID,
				s.LockedOutpoints,
			), nil
		}

		spent, err := completeForfeitTxs(
			forfeitTxs, reg, s.ConnectorAssignments,
			env.WalletController, env.Terms.OperatorKey,
			env.Terms.VTXOExitDelay, env.RoundID,
		)
		if err != nil {
			return buildFailureTransition(
				ctx, env, s.ClientRegistrations, fmt.Sprintf(
					"complete forfeit txs for client "+
						"%s: %v", clientID, err),
				lockID,
				s.LockedOutpoints,
			), nil
		}

		for _, spentVTXO := range spent {
			if spentVTXO.ForfeitInfo == nil {
				return buildFailureTransition(
					ctx, env, s.ClientRegistrations,
					fmt.Sprintf("missing forfeit info "+
						"for client %s", clientID),
					lockID,
					s.LockedOutpoints,
				), nil
			}

			forfeitInfos[spentVTXO.VTXOOutpoint] =
				spentVTXO.ForfeitInfo
		}
	}

	env.Log.DebugS(
		ctx, "Boarding inputs and forfeit txs signed, finalizing PSBT",
	)

	// Finalize the PSBT which signs all wallet-controlled inputs.
	// When boarding inputs alone cover the round (FundPsbt added zero
	// wallet inputs), every PSBT input already has a FinalScriptWitness
	// set by signBoardingInputs above and the packet is complete. LND's
	// FinalizePsbt rejects complete packets at the lnrpc front door
	// ("PSBT is already fully signed") before the wallet ever sees them,
	// so we extract the final tx ourselves and skip the round-trip.
	var finalTx *wire.MsgTx
	if s.PSBT.IsComplete() {
		finalTx, err = psbt.Extract(s.PSBT)
	} else {
		finalTx, err = env.WalletController.FinalizePsbt(ctx, s.PSBT)
	}
	if err != nil {
		env.Log.WarnS(ctx, "Failed to finalize PSBT", err)

		return buildFailureTransition(
			ctx, env, s.ClientRegistrations, fmt.Sprintf("failed "+
				"to finalize PSBT: %v", err),
			lockID,
			s.LockedOutpoints,
		), nil
	}

	env.Log.DebugS(ctx, "PSBT finalized",
		LogTxID(finalTx.TxHash().String()),
	)

	// Collect the operator-controlled output indices so the
	// ledger notification path can attribute the change output
	// and every connector output to the round. The set is also
	// persisted on the Round so a rounds-actor restart can
	// reload the attribution data on the reconstructed
	// FinalizedState; otherwise the classifier would mis-book
	// the change output as external_deposit on top of the
	// round's RecordCapitalCommitted ledger leg.
	connectorIndices := make(
		[]int32, 0, len(s.ConnectorTrees),
	)
	for idx := range s.ConnectorTrees {
		connectorIndices = append(connectorIndices, int32(idx))
	}

	// Persist the round to storage.
	round := &Round{
		RoundID:                env.RoundID,
		FinalTx:                finalTx,
		VTXOTrees:              s.VTXOTrees,
		ConnectorDescriptors:   s.ConnectorDescriptors,
		ForfeitInfos:           forfeitInfos,
		ClientRegistrations:    s.ClientRegistrations,
		SweepKey:               env.Terms.SweepKey.PubKey,
		CSVDelay:               env.Terms.SweepDelay,
		ChangeOutputIdx:        s.ChangeOutputIdx,
		ConnectorOutputIndices: connectorIndices,
	}

	err = env.RoundStore.PersistRound(ctx, round)
	if err != nil {
		return buildFailureTransition(
			ctx, env, s.ClientRegistrations, fmt.Sprintf("failed "+
				"to persist round: %v", err),
			lockID,
			s.LockedOutpoints,
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
				fmt.Sprintf("collect VTXOs: %v", err), lockID,
				s.LockedOutpoints,
			), nil
		}

		err = env.VTXOStore.PersistVTXOs(ctx, vtxos)
		if err != nil {
			return buildFailureTransition(
				ctx, env, s.ClientRegistrations, fmt.Sprintf(
					"persist "+
						"VTXOs: %v", err),
				lockID,
				s.LockedOutpoints,
			), nil
		}
	}

	env.Log.InfoS(ctx, "Persisted round",
		"round_id", env.RoundID)

	// Compute the absolute mining fee from the PSBT before we
	// drop it on the transition into FinalizedState: the packet
	// carries PInput.WitnessUtxo for both boarding inputs (we
	// attach them) and wallet funding inputs (FundPsbt
	// populates them). Reading the fee here (rather than on the
	// notify side from the bare wire.MsgTx) is the only way to
	// get input amounts without re-walking the wallet / UTXO
	// store, so the ledger handler can book a real mining_fees
	// leg instead of a zero-valued no-op.
	miningFeeSat := computeMiningFeeSatFromPSBT(s.PSBT)

	return &StateTransition{
		NextState: &FinalizedState{
			ClientRegistrations:    s.ClientRegistrations,
			FinalTx:                finalTx,
			VTXOTrees:              s.VTXOTrees,
			ForfeitInfos:           forfeitInfos,
			ChangeOutputIdx:        s.ChangeOutputIdx,
			ConnectorOutputIndices: connectorIndices,
			MiningFeeSat:           miningFeeSat,
		},
		NewEvents: fn.Some(EmittedEvent{
			Outbox: []OutboxEvent{
				&BroadcastRoundReq{
					RoundID:     env.RoundID,
					SignedTx:    finalTx,
					StartHeight: env.StartHeight,
				},
			},
		}),
	}, nil
}

// signBoardingInputs signs all boarding inputs with both the client's
// signature (from CollectedSignatures) and the operator's signature.
// This is a free function so it can be called from both FSM transitions
// and tests.
func signBoardingInputs(psbtPacket *psbt.Packet, collectedSigs InputSigsMap,
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
				psbtPacket, reg, clientSig, tx, sigHashes,
				prevOutFetcher, walletCtrl,
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
func signSingleBoardingInput(psbtPacket *psbt.Packet, reg *ClientRegistration,
	clientSig *types.BoardingInputSignature, tx *wire.MsgTx,
	sigHashes *txscript.TxSigHashes,
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

	// Derive the spend info for the collaborative path from the
	// tapscript tree. Leaf 0 is the collab multisig path.
	const collabLeafIdx = 0
	tapTree := txscript.AssembleTaprootScriptTree(
		boardingInput.Tapscript.Leaves...,
	)
	leafProof := tapTree.LeafMerkleProofs[collabLeafIdx]
	controlBlock := leafProof.ToControlBlock(&arkscript.ARKNUMSKey)
	ctrlBytes, err := controlBlock.ToBytes()
	if err != nil {
		return fmt.Errorf("failed to serialize control block: %w", err)
	}

	witnessScript := boardingInput.Tapscript.Leaves[collabLeafIdx].Script
	spendInfo := &arkscript.SpendInfo{
		WitnessScript: witnessScript,
		ControlBlock:  ctrlBytes,
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
	operatorSig, err := arkscript.SignVTXOCollabInput(
		walletCtrl, tx, inputIdx, spendInfo,
		boardingInput.OperatorKeyDesc, prevOut, sigHashes,
		prevOutFetcher,
	)
	if err != nil {
		return fmt.Errorf("operator signing failed: %w", err)
	}

	// Build the witness stack with both signatures.
	witness, err := spendInfo.CollabWitness(
		clientSig.ClientSignature, operatorSig,
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
					return fmt.Errorf("no descriptor "+
						"for leaf %x", pkScript)
				}

				// Compute the outpoint for this VTXO leaf.
				outpoint, err := node.GetNonAnchorOutpoint()
				if err != nil {
					return fmt.Errorf("get VTXO "+
						"outpoint: %w", err)
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

// ProcessEvent handles events in the FinalizedState. This state holds the
// fully signed transaction ready for broadcast.
//
// On TransactionConfirmedEvent the FSM persists confirmation data inline
// (marks VTXOs live, records forfeits, marks the round confirmed) and
// transitions to ConfirmedState.
//
// TODO(elle): handle re-broadcast logic.
func (s *FinalizedState) ProcessEvent(ctx context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	env.Log.DebugS(ctx, "Processing event",
		LogState("Finalized"),
		LogEvent(event),
	)

	switch e := event.(type) {
	case *TransactionConfirmedEvent:
		env.Log.InfoS(ctx, "Transaction confirmed",
			slog.Int("block_height", int(e.BlockHeight)),
			slog.Int("vtxo_trees", len(s.VTXOTrees)),
		)

		// Mark VTXOs live upon confirmation.
		if len(s.VTXOTrees) > 0 {
			err := env.VTXOStore.MarkVTXOsLive(
				ctx, env.RoundID,
			)
			if err != nil {
				env.Log.WarnS(ctx, "Failed to mark VTXOs live",
					err,
				)

				return confirmFailureTransition(
					env, s.ClientRegistrations,
					fmt.Sprintf("mark VTXOs live: %v", err),
				), nil
			}
		}

		// Mark forfeited VTXOs after confirmation.
		for outpoint, info := range s.ForfeitInfos {
			err := env.VTXOStore.MarkVTXOForfeit(
				ctx, outpoint, info,
			)
			if err != nil {
				return confirmFailureTransition(
					env, s.ClientRegistrations,
					fmt.Sprintf("mark VTXO forfeit: %v",
						err),
				), nil
			}
		}

		// Persist the round as confirmed for bookkeeping.
		err := env.RoundStore.MarkRoundConfirmed(
			ctx, env.RoundID, e.BlockHeight, e.BlockHash,
		)
		if err != nil {
			env.Log.WarnS(ctx, "Failed to mark round confirmed",
				err,
			)

			return confirmFailureTransition(
				env, s.ClientRegistrations, fmt.Sprintf(
					"mark round confirmed: %v", err),
			), nil
		}

		env.Log.InfoS(ctx, "Round confirmed and complete",
			slog.Int("block_height", int(e.BlockHeight)),
		)

		// Notify the ledger actor of the confirmed round
		// for double-entry accounting. This is
		// fire-and-forget: errors are logged but never
		// block round progress.
		notifyLedgerRoundConfirmed(
			ctx, env, s, e.BlockHeight,
		)

		// Notify the ledger of any forfeited VTXOs in
		// this round for refresh fee tracking.
		notifyLedgerVTXOsForfeited(ctx, env, s)

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

// confirmFailureTransition builds a failure transition for confirmation
// errors. Since the transaction IS confirmed on-chain, unlocking inputs
// is nonsensical — only notify clients and the actor of the persistence
// failure.
func confirmFailureTransition(env *Environment,
	clientRegs map[clientconn.ClientID]*ClientRegistration,
	reason string) *StateTransition {

	var outboxMsgs []OutboxEvent
	for clientID := range clientRegs {
		outboxMsgs = append(
			outboxMsgs, &ClientRoundFailedResp{
				Client:  clientID,
				RoundID: env.RoundID,
				Reason:  reason,
			},
		)
	}
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

// ProcessEvent handles the events from the FailedState state.
// FailedState is a terminal state, so it ignores all events.
func (s *FailedState) ProcessEvent(_ context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	return unexpectedEvent(s, "failed", event, env), nil
}

// ProcessEvent handles events in the ConfirmedState. This is a terminal state,
// so all events are ignored.
func (s *ConfirmedState) ProcessEvent(_ context.Context, event Event,
	env *Environment) (*StateTransition, error) {

	return unexpectedEvent(s, "confirmed", event, env), nil
}

// notifyLedgerRoundConfirmed sends a RoundConfirmedMsg to the
// ledger actor with capital deployment, fee, and VTXO count
// data extracted from the finalized round state. This is
// fire-and-forget: errors are logged but never block round
// progress.
func notifyLedgerRoundConfirmed(ctx context.Context, env *Environment,
	s *FinalizedState, blockHeight int32) {

	if env.LedgerRef == nil {
		return
	}

	// Sum total VTXO amount and count across all client
	// registrations, and split the VTXO output total by origin
	// so the ledger handler can book the matching liability legs
	// (RecordBoardingDeposit for boarding-new, RecordRefreshNewVTXO
	// for refresh-new). The partition rule mirrors
	// clientOperatorFeeSplit below: any forfeit input on a client
	// classifies that client's entire VTXO output as refresh-new;
	// otherwise it is boarding-new. Without these legs,
	// deployed_capital grows every round while user_vtxo_claims
	// stays at zero, silently breaking the double-entry balance.
	var (
		totalVTXOAmountSat int64
		vtxoCount          int32
		boardingNewSat     int64
		refreshNewSat      int64
	)
	for _, reg := range s.ClientRegistrations {
		var clientVTXO int64
		for _, desc := range reg.VTXODescriptors {
			clientVTXO += int64(desc.Amount)
			vtxoCount++
		}
		totalVTXOAmountSat += clientVTXO

		if len(reg.ForfeitInputs) > 0 {
			refreshNewSat += clientVTXO
		} else {
			boardingNewSat += clientVTXO
		}
	}

	// Split the per-client operator fee into the boarding
	// bucket. A client with any forfeit input is treated as a
	// refresh (RefreshFeeSat on VTXOsForfeitedMsg below);
	// clients with only boarding inputs book their whole fee as
	// BoardingFeeSat here. Mining fee comes from the PSBT.
	boardingFeeSat, _ := clientOperatorFeeSplit(s.ClientRegistrations)

	// Collect the commitment transaction's inputs and the
	// operator-bound change outputs so the ledger classifier
	// can short-circuit external_* booking for the treasury
	// wallet movements this round caused. Boarding inputs
	// are included unconditionally; the classifier ignores
	// attribution rows that never match a real wallet diff
	// observation, so per-client gating is not required
	// here.
	fundingOutpoints, changeOutpoints := roundAttributedOutpoints(
		s,
	)

	tellErr := env.LedgerRef.Tell(
		ctx, &ledger.RoundConfirmedMsg{
			RoundID:            env.RoundID,
			TotalVTXOAmountSat: totalVTXOAmountSat,
			VTXOCount:          vtxoCount,
			BoardingFeeSat:     boardingFeeSat,
			MiningFeeSat:       s.MiningFeeSat,
			BlockHeight:        uint32(blockHeight),
			FundingOutpoints:   fundingOutpoints,
			ChangeOutpoints:    changeOutpoints,
			BoardingNewSat:     boardingNewSat,
			RefreshNewSat:      refreshNewSat,
		},
	)
	if tellErr != nil {
		env.Log.WarnS(
			ctx,
			"Failed to notify ledger of round "+
				"confirmation",
			tellErr,
		)
	}
}

// roundAttributedOutpoints returns the outpoint slices the
// ledger classifier needs to short-circuit external_* booking
// for the round commitment transaction.
//
// Funding outpoints are every input the commitment tx
// consumed (operator funding plus boarding inputs from
// clients); orphaned attribution rows for boarding inputs
// never match a wallet diff observation and are harmless
// noise on the audit log, so per-input gating is not
// required.
//
// Change outpoints carry the explicit wallet change index
// (recorded by FundPsbt at build time) plus every connector
// output. Connector outputs are dust-valued operator-
// controlled outputs that land in the treasury wallet on
// round confirmation and get spent by forfeit transactions
// later; attributing them here keeps the classifier from
// double-booking external_deposit on top of the round's
// RecordCapitalCommitted. A ChangeOutputIdx of -1 means
// FundPsbt did not add a change output (round value exactly
// matched funding), in which case only connector outputs
// need attribution.
func roundAttributedOutpoints(s *FinalizedState) ([]wire.OutPoint,
	[]wire.OutPoint) {

	if s == nil || s.FinalTx == nil {
		return nil, nil
	}

	funding := make([]wire.OutPoint, 0, len(s.FinalTx.TxIn))
	for _, in := range s.FinalTx.TxIn {
		funding = append(funding, in.PreviousOutPoint)
	}

	txid := s.FinalTx.TxHash()
	var change []wire.OutPoint

	// Capture the wallet change first (if any), then every
	// connector output. Ordering is irrelevant -- the
	// classifier keys on (outpoint, event).
	if s.ChangeOutputIdx >= 0 {
		change = append(change, wire.OutPoint{
			Hash:  txid,
			Index: uint32(s.ChangeOutputIdx),
		})
	}

	for _, idx := range s.ConnectorOutputIndices {
		change = append(change, wire.OutPoint{
			Hash:  txid,
			Index: uint32(idx),
		})
	}

	return funding, change
}

// notifyLedgerVTXOsForfeited sends a VTXOsForfeitedMsg to the
// ledger actor when VTXOs are forfeited during a round. This
// enables refresh fee tracking and treasury capital reduction.
//
// TotalAmountSat is the gross forfeited VTXO value (sum over
// every ClientRegistration.ForfeitInputs.VTXO.Descriptor.Amount)
// -- this is the retirement leg the ledger handler books to
// move the user claim back to deployed_capital. RefreshFeeSat
// is the operator fee share collected from every client that
// had any forfeit input, computed as the pooled
// inputs-minus-outputs delta for those clients.
func notifyLedgerVTXOsForfeited(ctx context.Context, env *Environment,
	s *FinalizedState) {

	if env.LedgerRef == nil {
		return
	}

	if len(s.ForfeitInfos) == 0 {
		return
	}

	forfeitedTotalSat := totalForfeitedVTXOAmount(
		s.ClientRegistrations,
	)
	_, refreshFeeSat := clientOperatorFeeSplit(s.ClientRegistrations)

	// If the round produced ForfeitInfos but the registrations
	// carry no resolvable amounts (e.g. state reloaded from a
	// partial checkpoint), there is nothing the handler can
	// book. Suppress the Tell -- sending zero amounts would
	// silently no-op both the retirement leg and the fee leg in
	// ledger/handlers.go, which is worse than not sending.
	if forfeitedTotalSat == 0 && refreshFeeSat == 0 {
		return
	}

	tellErr := env.LedgerRef.Tell(
		ctx, &ledger.VTXOsForfeitedMsg{
			RoundID:        env.RoundID,
			TotalAmountSat: forfeitedTotalSat,
			Count:          int32(len(s.ForfeitInfos)),
			RefreshFeeSat:  refreshFeeSat,
		},
	)
	if tellErr != nil {
		env.Log.WarnS(
			ctx,
			"Failed to notify ledger of VTXO "+
				"forfeiture",
			tellErr,
		)
	}
}

// computeMiningFeeSatFromPSBT returns the absolute on-chain
// fee paid for the commitment transaction: sum of
// PInput.WitnessUtxo.Value minus sum of UnsignedTx.TxOut.Value.
// FundPsbt attaches WitnessUtxo on the wallet inputs it adds,
// and buildCommitmentTx attaches WitnessUtxo on the boarding
// inputs, so every input has a resolvable value at the
// ServerSigning -> Finalized transition. Returns zero when the
// packet or any input witness utxo is missing -- the ledger
// handler skips the mining_fees leg cleanly on zero.
func computeMiningFeeSatFromPSBT(packet *psbt.Packet) int64 {
	if packet == nil || packet.UnsignedTx == nil {
		return 0
	}

	var inputTotal int64
	for _, in := range packet.Inputs {
		if in.WitnessUtxo == nil {
			return 0
		}
		inputTotal += in.WitnessUtxo.Value
	}

	var outputTotal int64
	for _, out := range packet.UnsignedTx.TxOut {
		outputTotal += out.Value
	}

	fee := inputTotal - outputTotal
	if fee <= 0 {
		return 0
	}

	return fee
}

// clientOperatorFeeSplit walks every ClientRegistration and
// computes the per-client operator fee as
// Σ(boarding input values) + Σ(forfeit VTXO amounts) -
// Σ(owned VTXO output amounts) - Σ(cooperative leave output
// values), clamped to zero. The fee is attributed by
// operation kind: clients with any forfeit input are
// classified as refresh and their whole fee flows to the
// refresh revenue bucket; clients with only boarding inputs
// flow to the boarding bucket. This matches the client-side
// origin-routing in client/round/operator_fee.go: the client
// emits FeePaidMsg{FeeType=FeeTypeRefresh} on refresh rounds
// and defers boarding-fee emission, and the server books
// boarding fee from the operator side.
func clientOperatorFeeSplit(regs map[clientconn.ClientID]*ClientRegistration) (
	int64, int64) {

	var boardingFeeSat, refreshFeeSat int64
	for _, reg := range regs {
		if reg == nil {
			continue
		}

		var boardingIn, forfeitIn, out int64
		for _, bi := range reg.BoardingInputs {
			if bi == nil {
				continue
			}
			boardingIn += int64(bi.Value)
		}
		for _, fi := range reg.ForfeitInputs {
			if fi == nil || fi.VTXO == nil ||
				fi.VTXO.Descriptor == nil {

				continue
			}
			forfeitIn += int64(fi.VTXO.Descriptor.Amount)
		}
		for _, desc := range reg.VTXODescriptors {
			if desc == nil {
				continue
			}
			out += int64(desc.Amount)
		}
		for _, leave := range reg.LeaveOutputs {
			if leave == nil {
				continue
			}
			out += leave.Value
		}

		fee := boardingIn + forfeitIn - out
		if fee <= 0 {
			continue
		}

		// Attribute whole fee by input-kind presence: any
		// forfeit input means the client is refreshing, so
		// the fee flows to refresh revenue. Otherwise it is
		// a pure boarding client.
		if forfeitIn > 0 {
			refreshFeeSat += fee
		} else {
			boardingFeeSat += fee
		}
	}

	return boardingFeeSat, refreshFeeSat
}

// totalForfeitedVTXOAmount sums every forfeited VTXO's amount
// across all client registrations. Used as the gross amount on
// VTXOsForfeitedMsg so the ledger handler can retire the
// user_vtxo_claims liability back to deployed_capital.
func totalForfeitedVTXOAmount(
	regs map[clientconn.ClientID]*ClientRegistration) int64 {

	var total int64
	for _, reg := range regs {
		if reg == nil {
			continue
		}
		for _, fi := range reg.ForfeitInputs {
			if fi == nil || fi.VTXO == nil ||
				fi.VTXO.Descriptor == nil {

				continue
			}
			total += int64(fi.VTXO.Descriptor.Amount)
		}
	}

	return total
}

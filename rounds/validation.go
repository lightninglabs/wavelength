package rounds

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/lib/bip322"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/tx"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	// ErrCheckLockFailed is returned when checking if a boarding input is
	// locked fails.
	ErrCheckLockFailed = errors.New("failed to check if boarding input " +
		"is locked")

	// ErrBoardingInputLocked is returned when a boarding input is already
	// locked by another round.
	ErrBoardingInputLocked = errors.New("boarding input is already locked")

	// ErrOperatorKeyMismatch is returned when the operator key in a
	// boarding request does not match this operator's key.
	ErrOperatorKeyMismatch = errors.New("operator key does not match")

	// ErrExitDelayTooLow is returned when the exit delay in a boarding
	// request is less than the operator's minimum.
	ErrExitDelayTooLow = errors.New("exit delay is less than minimum")

	// ErrDelayPathTooClose is returned when a boarding input has too many
	// confirmations and is close to hitting the delay path.
	ErrDelayPathTooClose = errors.New("boarding input delay path is too " +
		"close")

	// ErrScriptConstruction is returned when constructing the expected
	// tapscript or P2TR script fails.
	ErrScriptConstruction = errors.New("failed to construct script")

	// ErrFetchUTXO is returned when fetching a UTXO from the chain source
	// fails.
	ErrFetchUTXO = errors.New("failed to fetch UTXO")

	// ErrInsufficientConfirmations is returned when a boarding input has
	// fewer confirmations than the operator's minimum.
	ErrInsufficientConfirmations = errors.New("insufficient confirmations")

	// ErrPkScriptMismatch is returned when a boarding input's pkScript
	// does not match the expected tapscript.
	ErrPkScriptMismatch = errors.New("pkScript does not match expected " +
		"tapscript")

	// ErrVTXOAmountTooLow is returned when a VTXO amount is below the
	// operator's minimum.
	ErrVTXOAmountTooLow = errors.New("VTXO amount is below minimum")

	// ErrVTXOAmountTooHigh is returned when a VTXO amount exceeds the
	// operator's maximum.
	ErrVTXOAmountTooHigh = errors.New("VTXO amount exceeds maximum")

	// ErrVTXOExpiryTooLow is returned when a VTXO expiry is less than
	// the operator's minimum VTXOExitDelay.
	ErrVTXOExpiryTooLow = errors.New("VTXO expiry is below minimum")

	// ErrSigningKeyNotUnique is returned when a signing key has already
	// been used in the batch.
	ErrSigningKeyNotUnique = errors.New("signing key is not unique")

	// ErrVTXODescriptorConstruction is returned when creating a VTXO
	// descriptor fails.
	ErrVTXODescriptorConstruction = errors.New(
		"failed to create VTXO descriptor",
	)

	// ErrVTXOPkScriptMismatch is returned when a VTXO's pkScript does not
	// match the expected descriptor.
	ErrVTXOPkScriptMismatch = errors.New("VTXO pkScript does not match " +
		"expected descriptor")

	// ErrLeaveOutputNil is returned when a leave request has a nil output.
	ErrLeaveOutputNil = errors.New("leave request has nil output")

	// ErrLeaveOutputValueInvalid is returned when a leave request output
	// value is not positive.
	ErrLeaveOutputValueInvalid = errors.New(
		"leave request output value must be positive",
	)

	// ErrLeaveOutputEmptyPkScript is returned when a leave request output
	// has an empty pkScript.
	ErrLeaveOutputEmptyPkScript = errors.New(
		"leave request output has empty pkScript",
	)

	// ErrLeaveAmountTooLow is returned when a leave request output value
	// is below the operator's minimum.
	ErrLeaveAmountTooLow = errors.New("leave amount is below minimum")

	// ErrDuplicateBoardingRequest is returned when a join request contains
	// duplicate boarding request outpoints.
	ErrDuplicateBoardingRequest = errors.New(
		"duplicate boarding request",
	)

	// ErrOutputExceedsInput is returned when the total output value (leave
	// + VTXO) exceeds the total boarding input value.
	ErrOutputExceedsInput = errors.New(
		"output total exceeds boarding input total",
	)

	// ErrOperatorFeeTooLow is returned when the implicit operator fee
	// (total input - total output) is below the operator's minimum.
	// This prevents clients from using the operator as a free UTXO
	// consolidation service.
	ErrOperatorFeeTooLow = errors.New(
		"operator fee is below minimum",
	)

	// ErrForfeitVTXONotFound is returned when a forfeit request references
	// a VTXO that doesn't exist in the store.
	ErrForfeitVTXONotFound = errors.New("forfeit VTXO not found")

	// ErrForfeitVTXONotLive is returned when a forfeit request references
	// a VTXO that is not in "live" status.
	ErrForfeitVTXONotLive = errors.New("forfeit VTXO is not live")

	// ErrForfeitLookupFailed is returned when looking up a VTXO in the
	// store fails.
	ErrForfeitLookupFailed = errors.New("failed to lookup forfeit VTXO")

	// ErrForfeitOutpointNil is returned when a forfeit request has a nil
	// outpoint.
	ErrForfeitOutpointNil = errors.New("forfeit request has nil outpoint")

	// ErrDuplicateForfeitRequest is returned when a join request contains
	// duplicate forfeit request outpoints.
	ErrDuplicateForfeitRequest = errors.New("duplicate forfeit request")

	// ErrJoinRequestAuthMissing is returned when a join request does not
	// include an authorization payload.
	ErrJoinRequestAuthMissing = errors.New("join request auth is missing")

	// ErrJoinRequestIdentifierMissing is returned when a join request does
	// not include the identifier key used for the join-auth challenge
	// script.
	ErrJoinRequestIdentifierMissing = errors.New(
		"join request identifier is missing",
	)

	// ErrJoinRequestAuthMessageMismatch is returned when the request's auth
	// message does not match the canonical request encoding.
	ErrJoinRequestAuthMessageMismatch = errors.New(
		"join request auth message does not match request",
	)

	// ErrJoinRequestAuthInputCountMismatch is returned when the signed
	// proof-of-funds inputs do not match the number of expected ownership
	// proofs.
	ErrJoinRequestAuthInputCountMismatch = errors.New(
		"join request auth input count mismatch",
	)

	// ErrJoinRequestAuthInputOrderMismatch is returned when the signed
	// proof-of-funds input outpoints do not match the request.
	ErrJoinRequestAuthInputOrderMismatch = errors.New(
		"join request auth input order mismatch",
	)

	// ErrTxProofRequired is returned when no ChainSource is available
	// and the boarding request does not include a TxProof.
	ErrTxProofRequired = errors.New(
		"TxProof is required when server has no chain source",
	)

	// ErrTxProofInvalid is returned when a TxProof fails validation.
	ErrTxProofInvalid = errors.New("TxProof validation failed")

	// ErrTxProofOutpointMismatch is returned when the TxProof's claimed
	// outpoint does not match the boarding request outpoint.
	ErrTxProofOutpointMismatch = errors.New(
		"TxProof claimed outpoint does not match boarding outpoint",
	)
)

const (
	// joinAuthMaxSignatureSize caps the serialized BIP-322 full-format
	// signature payload accepted in a join request. This bounds tx
	// deserialization work before signature validation.
	joinAuthMaxSignatureSize = 256 * 1024

	// joinAuthMaxProofInputs bounds decoded proof-of-funds inputs to cap
	// script-engine work during join-auth verification.
	joinAuthMaxProofInputs = 128
)

// JoinRequestResult holds the validated results from a join request.
type JoinRequestResult struct {
	// BoardingInputs contains the validated boarding inputs.
	BoardingInputs []*BoardingInput

	// RequiredOutputs contains the outputs from leave requests that must
	// be included in the round transaction.
	RequiredOutputs []*wire.TxOut

	// ForfeitInputs contains the validated forfeit inputs from VTXOs.
	ForfeitInputs []*ForfeitInput

	// VTXODescriptors maps signing key hex strings to their VTXO
	// descriptors. The map key is the serialized public key.
	VTXODescriptors map[SigningKeyHex]*tree.VTXODescriptor

	// SigningKeys contains the unique signing keys from VTXO requests.
	// These are used for MuSig2 signing sessions when building the VTXO
	// tree. The map key is the serialized public key to ensure uniqueness.
	SigningKeys map[SigningKeyHex]*btcec.PublicKey
}

// ValidateJoinRequest validates a client's join request for the round using
// the environment's start height for join-auth freshness checks.
func ValidateJoinRequest(ctx context.Context, env *Environment,
	req *types.JoinRoundRequest) (*JoinRequestResult, error) {

	return validateJoinRequest(ctx, env, req, env.StartHeight)
}

// ValidateJoinRequestAtHeight validates a client's join request for the round
// using the specified chain height for join-auth freshness checks.
func ValidateJoinRequestAtHeight(ctx context.Context, env *Environment,
	req *types.JoinRoundRequest, currentBlockHeight uint32) (
	*JoinRequestResult, error) {

	return validateJoinRequest(ctx, env, req, currentBlockHeight)
}

// validateJoinRequest validates all join-request contents, balance
// constraints, and optional BIP-322 ownership proofs.
func validateJoinRequest(ctx context.Context, env *Environment,
	req *types.JoinRoundRequest, currentBlockHeight uint32) (
	*JoinRequestResult, error) {

	var (
		boardingInputs  []*BoardingInput
		forfeitInputs   []*ForfeitInput
		requiredOutputs []*wire.TxOut
		vtxoDescriptors = make(map[SigningKeyHex]*tree.VTXODescriptor)
		signingKeys     = make(map[SigningKeyHex]*btcec.PublicKey)
		totalInputValue btcutil.Amount
		totalLeaveValue btcutil.Amount
		totalVTXOValue  btcutil.Amount
	)

	// Validate each boarding request individually and also make sure that
	// there are no duplicate inputs across all boarding requests.
	boardReqs := fn.NewSet[wire.OutPoint]()
	for _, boardReq := range req.BoardingReqs {
		if boardReqs.Contains(*boardReq.Outpoint) {
			return nil, fmt.Errorf("%w: %v",
				ErrDuplicateBoardingRequest, boardReq.Outpoint)
		}

		boardingInput, err := ValidateBoardingRequest(
			ctx, env, boardReq, currentBlockHeight,
		)
		if err != nil {
			return nil, fmt.Errorf("invalid boarding request "+
				"for outpoint %v: %w", boardReq.Outpoint, err)
		}

		boardReqs.Add(*boardReq.Outpoint)
		boardingInputs = append(boardingInputs, boardingInput)
		totalInputValue += boardingInput.Value
	}

	// Validate each forfeit request individually and also make sure that
	// there are no duplicate forfeit requests.
	forfeitReqs := fn.NewSet[wire.OutPoint]()
	for _, forfeitReq := range req.ForfeitReqs {
		if forfeitReq.VTXOOutpoint == nil {
			return nil, ErrForfeitOutpointNil
		}

		if forfeitReqs.Contains(*forfeitReq.VTXOOutpoint) {
			return nil, fmt.Errorf("%w: %v",
				ErrDuplicateForfeitRequest,
				forfeitReq.VTXOOutpoint)
		}

		forfeitInput, err := ValidateForfeitRequest(
			ctx, env, forfeitReq,
		)
		if err != nil {
			return nil, fmt.Errorf("invalid forfeit request "+
				"for outpoint %v: %w",
				forfeitReq.VTXOOutpoint, err)
		}

		forfeitReqs.Add(*forfeitReq.VTXOOutpoint)
		forfeitInputs = append(forfeitInputs, forfeitInput)
		totalInputValue += forfeitInput.VTXO.Descriptor.Amount
	}

	// Validate each leave request.
	for i, leaveReq := range req.LeaveReqs {
		err := ValidateLeaveRequest(env.Terms, leaveReq)
		if err != nil {
			return nil, fmt.Errorf("invalid leave request "+
				"at index %d: %w", i, err)
		}

		requiredOutputs = append(requiredOutputs, leaveReq.Output)
		totalLeaveValue += btcutil.Amount(leaveReq.Output.Value)
	}

	// Validate each VTXO request.
	for _, vtxoReq := range req.VTXOReqs {
		descriptor, err := ValidateVTXORequest(
			env.Terms, vtxoReq, signingKeys,
		)
		if err != nil {
			return nil, err
		}

		// Track this signing key as used and map it to the descriptor.
		signingKeyVertex := route.NewVertex(vtxoReq.SigningKey.PubKey)
		signingKeys[signingKeyVertex] = vtxoReq.SigningKey.PubKey
		vtxoDescriptors[signingKeyVertex] = descriptor

		totalVTXOValue += vtxoReq.Amount
	}

	// Ensure the client isn't asking for more output value than they are
	// providing as input. This is a basic balance check - the client's
	// leave request total + VTXO total cannot exceed their boarding +
	// forfeit input total.
	totalOutputValue := totalLeaveValue + totalVTXOValue
	if totalOutputValue > totalInputValue {
		return nil, fmt.Errorf("%w: got %d sats, max %d sats",
			ErrOutputExceedsInput, totalOutputValue,
			totalInputValue)
	}

	// Enforce the minimum operator fee when boarding inputs are
	// present. The fee is the implicit difference between total input
	// and total output value. Without this check a client could
	// submit equal boarding inputs and outputs, effectively using the
	// operator as a free UTXO consolidator. Refresh and leave
	// requests (forfeit-only) are exempt because the operator already
	// collected a fee when the VTXO was originally created.
	operatorFee := totalInputValue - totalOutputValue
	if len(boardingInputs) > 0 &&
		operatorFee < env.Terms.MinOperatorFee {

		return nil, fmt.Errorf("%w: got %d sats, min %d sats",
			ErrOperatorFeeTooLow, operatorFee,
			env.Terms.MinOperatorFee)
	}

	// Validate the join authorization proof once all request components
	// have passed semantic validation. This allows tests targeting earlier
	// validation failures to remain focused on those specific errors.
	if !env.DisableJoinRequestAuth {
		err := validateJoinRequestAuth(
			req, boardingInputs, forfeitInputs,
			currentBlockHeight,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"join request auth invalid: %w", err,
			)
		}
	}

	return &JoinRequestResult{
		BoardingInputs:  boardingInputs,
		ForfeitInputs:   forfeitInputs,
		RequiredOutputs: requiredOutputs,
		VTXODescriptors: vtxoDescriptors,
		SigningKeys:     signingKeys,
	}, nil
}

// validateJoinRequestAuth validates the BIP-322 authorization payload attached
// to a join request.
func validateJoinRequestAuth(req *types.JoinRoundRequest,
	boardingInputs []*BoardingInput, forfeitInputs []*ForfeitInput,
	currentBlockHeight uint32) error {

	if req == nil {
		return fmt.Errorf("join request must be provided")
	}

	if req.Identifier == nil {
		return ErrJoinRequestIdentifierMissing
	}

	if req.Auth == nil {
		return ErrJoinRequestAuthMissing
	}

	if len(req.Auth.Message) == 0 {
		return fmt.Errorf("join request auth message must be provided")
	}

	if len(req.Auth.Signature) == 0 {
		return fmt.Errorf(
			"join request auth signature must be provided",
		)
	}

	if len(req.Auth.Signature) > joinAuthMaxSignatureSize {
		return fmt.Errorf(
			"join request auth signature size %d exceeds max %d",
			len(req.Auth.Signature), joinAuthMaxSignatureSize,
		)
	}

	expectedMessage, err := types.JoinRoundAuthMessage(req)
	if err != nil {
		return fmt.Errorf("canonical join auth message: %w", err)
	}

	if !bytes.Equal(expectedMessage, req.Auth.Message) {
		return ErrJoinRequestAuthMessageMismatch
	}

	messageChallenge, err := bip322.JoinRoundMessageChallenge(
		req.Identifier,
	)
	if err != nil {
		return fmt.Errorf("join request message challenge: %w", err)
	}

	proofPrevOutputs, err := mapJoinAuthPrevOutputs(
		boardingInputs, forfeitInputs,
	)
	if err != nil {
		return err
	}

	var currentHeightPtr *uint32
	if currentBlockHeight != 0 {
		currentHeight := currentBlockHeight
		currentHeightPtr = &currentHeight
	}

	authCtx, err := bip322.NewIntentAuthContext(
		expectedMessage, req.Auth.ValidFrom, req.Auth.ValidUntil,
		messageChallenge, req.Auth.Signature, proofPrevOutputs,
		currentHeightPtr,
		bip322.WithMaxProofInputs(joinAuthMaxProofInputs),
	)
	if err != nil {
		return fmt.Errorf("join request auth context: %w", err)
	}

	expectedOutpoints := expectedJoinAuthOutpoints(req)
	if len(expectedOutpoints) == 0 {
		return fmt.Errorf("join request must include at least one " +
			"boarding or forfeit input")
	}

	if authCtx.Sig.ToSign == nil {
		return fmt.Errorf("join auth signature transaction is missing")
	}

	if len(authCtx.Sig.ToSign.TxIn) != len(expectedOutpoints)+1 {
		return fmt.Errorf("%w: expected %d signed inputs, got %d",
			ErrJoinRequestAuthInputCountMismatch,
			len(expectedOutpoints), len(authCtx.Sig.ToSign.TxIn)-1)
	}

	for i := 0; i < len(expectedOutpoints); i++ {
		expectedOutpoint := expectedOutpoints[i]
		actualOutpoint := authCtx.Sig.ToSign.TxIn[i+1].PreviousOutPoint
		if actualOutpoint != expectedOutpoint {
			return fmt.Errorf("%w at input %d: expected %s, got %s",
				ErrJoinRequestAuthInputOrderMismatch, i+1,
				expectedOutpoint, actualOutpoint)
		}
	}

	result := authCtx.Validate()
	if result.State != bip322.VerificationStateValid {
		return fmt.Errorf("join request auth verification %s: %s",
			result.State, result.Reason)
	}

	return nil
}

// expectedJoinAuthOutpoints returns the ownership-proof outpoints expected in
// the join auth signature. Order is boarding requests first, then forfeit
// requests, both in request order.
func expectedJoinAuthOutpoints(
	req *types.JoinRoundRequest) []wire.OutPoint {

	outpoints := make(
		[]wire.OutPoint, 0, len(req.BoardingReqs)+len(req.ForfeitReqs),
	)

	for i := 0; i < len(req.BoardingReqs); i++ {
		boardingReq := req.BoardingReqs[i]
		outpoints = append(outpoints, *boardingReq.Outpoint)
	}

	for i := 0; i < len(req.ForfeitReqs); i++ {
		forfeitReq := req.ForfeitReqs[i]
		outpoints = append(outpoints, *forfeitReq.VTXOOutpoint)
	}

	return outpoints
}

// mapJoinAuthPrevOutputs builds prevout metadata for all join-auth
// proof-of-funds inputs using validated boarding and forfeit inputs.
func mapJoinAuthPrevOutputs(boardingInputs []*BoardingInput,
	forfeitInputs []*ForfeitInput) (map[wire.OutPoint]*wire.TxOut, error) {

	prevOutputs := make(
		map[wire.OutPoint]*wire.TxOut,
		len(boardingInputs)+len(forfeitInputs),
	)

	for i := 0; i < len(boardingInputs); i++ {
		boardingInput := boardingInputs[i]
		if boardingInput.Outpoint == nil {
			return nil, fmt.Errorf(
				"boarding input %d has nil outpoint",
				i,
			)
		}

		prevOutputs[*boardingInput.Outpoint] = &wire.TxOut{
			Value:    int64(boardingInput.Value),
			PkScript: bytes.Clone(boardingInput.PkScript),
		}
	}

	for i := 0; i < len(forfeitInputs); i++ {
		forfeitInput := forfeitInputs[i]
		if forfeitInput.Outpoint == nil {
			return nil, fmt.Errorf(
				"forfeit input %d has nil outpoint",
				i,
			)
		}

		if forfeitInput.VTXO == nil ||
			forfeitInput.VTXO.Descriptor == nil {

			return nil, fmt.Errorf(
				"forfeit input %d descriptor missing",
				i,
			)
		}

		prevOutputs[*forfeitInput.Outpoint] = &wire.TxOut{
			Value: int64(forfeitInput.VTXO.Descriptor.Amount),
			PkScript: bytes.Clone(
				forfeitInput.VTXO.Descriptor.PkScript,
			),
		}
	}

	return prevOutputs, nil
}

// ValidateLeaveRequest validates a single leave request. It verifies:
//   - The output is not nil.
//   - The output value is positive.
//   - The output value meets the minimum amount requirement.
//   - The pkScript is not empty.
func ValidateLeaveRequest(terms *batch.Terms,
	req *types.LeaveRequest) error {

	if req.Output == nil {
		return ErrLeaveOutputNil
	}

	if req.Output.Value <= 0 {
		return fmt.Errorf("%w: got %d", ErrLeaveOutputValueInvalid,
			req.Output.Value)
	}

	// Check that the output value meets the minimum requirement.
	if btcutil.Amount(req.Output.Value) < terms.MinLeaveAmount {
		return fmt.Errorf("%w: got %v, want %v", ErrLeaveAmountTooLow,
			btcutil.Amount(req.Output.Value),
			terms.MinLeaveAmount)
	}

	if len(req.Output.PkScript) == 0 {
		return ErrLeaveOutputEmptyPkScript
	}

	return nil
}

// ValidateBoardingRequest validates a boarding request from a client. It
// verifies:
//   - The input is not already locked by another round.
//   - The operator key matches this operator.
//   - The exit delay meets the operator's minimum.
//   - The UTXO exists and has sufficient confirmations.
//   - The UTXO's confirmations are not too close to the exit delay (ensures
//     the delay path isn't about to be hit).
//   - The UTXO's script matches the expected tapscript.
//   - Ownership proof is enforced at join-request scope in
//     validateJoinRequestAuth.
//
// On success, returns a BoardingInput containing all data needed for
// transaction construction.
func ValidateBoardingRequest(ctx context.Context, env *Environment,
	req *types.BoardingRequest,
	currentHeight uint32) (*BoardingInput, error) {

	// Make sure this boarding request input isn't already locked.
	locked, _, err := env.BoardingInputLocker.IsLocked(ctx, req.Outpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCheckLockFailed, err)
	}
	if locked {
		return nil, fmt.Errorf("%w: %v", ErrBoardingInputLocked,
			req.Outpoint)
	}

	terms := env.Terms

	if req.OperatorKey == nil {
		return nil, fmt.Errorf("%w: operator key is nil",
			ErrOperatorKeyMismatch)
	}

	// Check that the boarding request's operator key matches this
	// operator's key.
	if !req.OperatorKey.IsEqual(terms.OperatorKey.PubKey) {
		return nil, fmt.Errorf("%w: got %x, want %x",
			ErrOperatorKeyMismatch,
			req.OperatorKey.SerializeCompressed(),
			terms.OperatorKey.PubKey.SerializeCompressed())
	}

	// Verify that the exit delay meets the operator's minimum.
	if req.ExitDelay < terms.BoardingExitDelay {
		return nil, fmt.Errorf("%w: got %d, want %d",
			ErrExitDelayTooLow, req.ExitDelay,
			terms.BoardingExitDelay)
	}

	// Validate the script on-chain matches what we expect given the
	// client's parameters.
	expectedTapscript, err := scripts.VTXOTapScript(
		req.ClientKey, req.OperatorKey, req.ExitDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("%w (tapscript): %w",
			ErrScriptConstruction, err)
	}

	expectedPkScript, err := buildP2TRScript(expectedTapscript)
	if err != nil {
		return nil, fmt.Errorf("%w (P2TR): %w",
			ErrScriptConstruction, err)
	}

	// When a ChainSource is available, fetch the UTXO from the
	// chain and validate confirmations, delay path safety, and
	// script match. When no ChainSource is configured, the client
	// must provide a TxProof (SPV merkle inclusion proof) that
	// proves the UTXO exists in a confirmed block.
	var utxoValue btcutil.Amount
	if env.ChainSource != nil {
		utxo, err := env.ChainSource.GetUTXO(*req.Outpoint)
		if err != nil {
			return nil, fmt.Errorf("%w: %w",
				ErrFetchUTXO, err)
		}

		// Verify the UTXO meets the minimum confirmation
		// requirement.
		if utxo.Confirmations < int64(
			terms.MinBoardingConfirmations,
		) {

			return nil, fmt.Errorf("%w: got %d, want %d",
				ErrInsufficientConfirmations,
				utxo.Confirmations,
				terms.MinBoardingConfirmations,
			)
		}

		// Ensure the delay path isn't already hit or close
		// to being hit.
		safetyMargin := terms.BoardingExitDelaySafetyMargin
		maxSafe := req.ExitDelay - safetyMargin
		if utxo.Confirmations >= int64(maxSafe) {
			return nil, fmt.Errorf(
				"%w: got %d confirmations, max "+
					"safe %d (exit delay %d "+
					"- safety margin %d)",
				ErrDelayPathTooClose,
				utxo.Confirmations, maxSafe,
				req.ExitDelay, safetyMargin,
			)
		}

		// Check that the pkScript matches the expected
		// script.
		if !bytes.Equal(
			utxo.Output.PkScript, expectedPkScript,
		) {

			return nil, ErrPkScriptMismatch
		}

		utxoValue = btcutil.Amount(utxo.Output.Value)
	} else {
		// No direct chain source: validate via the
		// client-provided TxProof.
		val, err := validateBoardingTxProof(
			env, req, expectedPkScript, currentHeight,
		)
		if err != nil {
			return nil, err
		}

		utxoValue = val
	}

	// Build the BoardingInput.
	return &BoardingInput{
		Outpoint:        req.Outpoint,
		Tapscript:       expectedTapscript,
		Value:           utxoValue,
		PkScript:        expectedPkScript,
		ClientKey:       req.ClientKey,
		OperatorKeyDesc: &terms.OperatorKey,
	}, nil
}

// validateBoardingTxProof verifies a client-provided TxProof for a
// boarding request. This is used when the server has no direct chain
// source and must rely on SPV proofs from clients. The proof
// demonstrates that the claimed UTXO exists in a confirmed block by
// providing the transaction, merkle inclusion proof, and block header.
// The currentHeight parameter is used to compute confirmation depth
// and enforce the same safety checks as the ChainSource path.
func validateBoardingTxProof(env *Environment,
	req *types.BoardingRequest,
	expectedPkScript []byte,
	currentHeight uint32) (btcutil.Amount, error) {

	// A HeaderVerifier is required to anchor the proof to the
	// real chain. Without one the proof is meaningless since a
	// client could fabricate an arbitrary block header.
	if env.HeaderVerifier == nil {
		return 0, fmt.Errorf("%w: no header verifier "+
			"configured", ErrTxProofInvalid,
		)
	}

	// The TxProof must be present when there is no ChainSource.
	txProofOpt := req.TxProof
	if txProofOpt.IsNone() {
		return 0, ErrTxProofRequired
	}

	txProof := txProofOpt.UnsafeFromSome()

	// Verify the claimed outpoint in the proof matches the
	// boarding request outpoint.
	if txProof.ClaimedOutPoint != *req.Outpoint {
		return 0, fmt.Errorf("%w: proof claims %v, "+
			"request has %v", ErrTxProofOutpointMismatch,
			txProof.ClaimedOutPoint, *req.Outpoint,
		)
	}

	// Verify the transaction hash matches the outpoint hash.
	txHash := txProof.MsgTx.TxHash()
	if txHash != req.Outpoint.Hash {
		return 0, fmt.Errorf("%w: tx hash %s does not "+
			"match outpoint hash %s",
			ErrTxProofInvalid, txHash,
			req.Outpoint.Hash,
		)
	}

	// Verify the output index is valid.
	if req.Outpoint.Index >= uint32(len(txProof.MsgTx.TxOut)) {
		return 0, fmt.Errorf("%w: output index %d out "+
			"of range (tx has %d outputs)",
			ErrTxProofInvalid, req.Outpoint.Index,
			len(txProof.MsgTx.TxOut),
		)
	}

	// Verify the output's pkScript matches what we expect.
	provenOutput := txProof.MsgTx.TxOut[req.Outpoint.Index]
	if !bytes.Equal(provenOutput.PkScript, expectedPkScript) {
		return 0, ErrPkScriptMismatch
	}

	// Verify the merkle inclusion proof: the transaction is
	// included in the block whose header is provided.
	merkleRoot := txProof.BlockHeader.MerkleRoot
	if !txProof.MerkleProof.Verify(
		&txProof.MsgTx, merkleRoot,
	) {

		return 0, fmt.Errorf("%w: merkle inclusion "+
			"proof failed", ErrTxProofInvalid,
		)
	}

	// Verify the block header exists on the best chain at the
	// claimed height using the server's header verifier.
	err := env.HeaderVerifier(
		txProof.BlockHeader, txProof.BlockHeight,
	)
	if err != nil {
		return 0, fmt.Errorf("%w: header "+
			"verification failed: %w",
			ErrTxProofInvalid, err,
		)
	}

	// Enforce confirmation depth: the UTXO must have at least
	// MinBoardingConfirmations blocks on top of it.
	terms := env.Terms
	confirmations := currentHeight - txProof.BlockHeight
	if confirmations < terms.MinBoardingConfirmations {
		return 0, fmt.Errorf("%w: got %d, want %d",
			ErrInsufficientConfirmations,
			confirmations,
			terms.MinBoardingConfirmations,
		)
	}

	// Ensure the delay path isn't already hit or close to being
	// hit, matching the ChainSource validation path.
	safetyMargin := terms.BoardingExitDelaySafetyMargin
	maxSafe := req.ExitDelay - safetyMargin
	if confirmations >= maxSafe {
		return 0, fmt.Errorf(
			"%w: got %d confirmations, max "+
				"safe %d (exit delay %d "+
				"- safety margin %d)",
			ErrDelayPathTooClose,
			confirmations, maxSafe,
			req.ExitDelay, safetyMargin,
		)
	}

	return btcutil.Amount(provenOutput.Value), nil
}

// buildP2TRScript builds a P2TR pkScript from the given tapscript.
func buildP2TRScript(tapscript *waddrmgr.Tapscript) ([]byte, error) {
	outputKey, err := tapscript.TaprootKey()
	if err != nil {
		return nil, fmt.Errorf("failed to get taproot key: %w", err)
	}

	pkScript, err := input.PayToTaprootScript(outputKey)
	if err != nil {
		return nil, fmt.Errorf("failed to build taproot script: %w",
			err)
	}

	return pkScript, nil
}

// ValidateVTXORequest validates a single VTXO request from a client. It
// verifies:
//   - The amount is within the min/max bounds.
//   - The expiry meets the minimum VTXOExitDelay.
//   - The operator key matches this operator.
//   - The signing key is unique (not already used in the batch).
//   - The pkScript matches the expected VTXO descriptor.
//
// On success, returns the validated VTXO descriptor.
func ValidateVTXORequest(terms *batch.Terms, req *types.VTXORequest,
	usedSigningKeys map[SigningKeyHex]*btcec.PublicKey) (
	*tree.VTXODescriptor, error) {

	// Validate amount is within bounds.
	if req.Amount < terms.MinVTXOAmount {
		return nil, fmt.Errorf("%w: got %v, want %v",
			ErrVTXOAmountTooLow, req.Amount, terms.MinVTXOAmount)
	}

	if req.Amount > terms.MaxVTXOAmount {
		return nil, fmt.Errorf("%w: got %v, want %v",
			ErrVTXOAmountTooHigh, req.Amount, terms.MaxVTXOAmount)
	}

	// Validate expiry meets minimum requirement.
	if req.Expiry < terms.VTXOExitDelay {
		return nil, fmt.Errorf("%w: got %d, want %d",
			ErrVTXOExpiryTooLow, req.Expiry, terms.VTXOExitDelay)
	}

	// Verify operator key matches this operator's key.
	if !req.OperatorKey.IsEqual(terms.OperatorKey.PubKey) {
		return nil, fmt.Errorf("%w: got %x, want %x",
			ErrOperatorKeyMismatch,
			req.OperatorKey.SerializeCompressed(),
			terms.OperatorKey.PubKey.SerializeCompressed())
	}

	// Verify signing key is unique for this batch.
	signingKeyVertex := route.NewVertex(req.SigningKey.PubKey)
	if _, exists := usedSigningKeys[signingKeyVertex]; exists {
		return nil, fmt.Errorf("%w: %x", ErrSigningKeyNotUnique,
			req.SigningKey.PubKey.SerializeCompressed())
	}

	// Compute the expected VTXO descriptor.
	expectedDescriptor, err := tree.NewVTXODescriptor(
		req.Amount, req.ClientKey, req.OperatorKey, req.Expiry,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrVTXODescriptorConstruction,
			err)
	}

	// Verify the pkScript matches the expected descriptor.
	if !bytes.Equal(req.PkScript, expectedDescriptor.PkScript) {
		return nil, ErrVTXOPkScriptMismatch
	}

	return expectedDescriptor, nil
}

// ValidateBoardingSignature verifies a client's schnorr signature for a
// boarding input. The signature is validated against the tapscript
// collaborative spend path sighash and the client's public key.
//
// The boarding tapscript has a collaborative multisig leaf where:
//   - The owner (client) signs with OP_CHECKSIGVERIFY
//   - The cosigner (operator) signs with OP_CHECKSIG
//
// This function verifies the client's signature for the owner position.
func ValidateBoardingSignature(boardingInput *BoardingInput,
	sig *types.BoardingInputSignature, tx *wire.MsgTx,
	prevOutFetcher txscript.PrevOutputFetcher) error {

	// Get the collaborative leaf from the tapscript (index 0).
	if len(boardingInput.Tapscript.Leaves) == 0 {
		return fmt.Errorf("boarding input has no tapscript leaves")
	}

	collabLeaf := boardingInput.Tapscript.Leaves[0]

	// Create the tap leaf for the collaborative spend path.
	tapLeaf := txscript.NewBaseTapLeaf(collabLeaf.Script)

	// Create signature hashes for the transaction.
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)

	// Compute the tapscript signature hash for the collaborative spend
	// path. This is different from keypath signing - we use
	// CalcTapscriptSignaturehash which takes the TapLeaf directly.
	sigHash, err := txscript.CalcTapscriptSignaturehash(
		sigHashes, txscript.SigHashDefault, tx,
		sig.InputIndex, prevOutFetcher, tapLeaf,
	)
	if err != nil {
		return fmt.Errorf("failed to compute sighash: %w", err)
	}

	// Verify the schnorr signature against the client's public key.
	if !sig.ClientSignature.Verify(sigHash, boardingInput.ClientKey) {
		return fmt.Errorf("invalid signature for input %d",
			sig.InputIndex)
	}

	return nil
}

// validateForfeitTxs validates that forfeit transactions are correctly
// constructed and have valid client signatures for the VTXO input.
func validateForfeitTxs(
	forfeitTxSigs []*types.ForfeitTxSig,
	reg *ClientRegistration,
	connectorAssignments map[wire.OutPoint]*ConnectorLeafAssignment,
	forfeitScript []byte, operatorKey *btcec.PublicKey) error {

	// Build a map of expected forfeit outpoints from the registration.
	expectedForfeits := make(map[wire.OutPoint]*ForfeitInput)
	for _, forfeitInput := range reg.ForfeitInputs {
		if forfeitInput.Outpoint == nil {
			return fmt.Errorf("forfeit outpoint cannot be nil")
		}

		expectedForfeits[*forfeitInput.Outpoint] = forfeitInput
	}

	seenForfeits := make(map[wire.OutPoint]struct{})

	// Client must provide exactly one forfeit tx per forfeit input.
	if len(forfeitTxSigs) != len(expectedForfeits) {
		return fmt.Errorf("expected %d forfeit txs, got %d",
			len(expectedForfeits), len(forfeitTxSigs))
	}

	// Validate each forfeit transaction.
	for _, forfeitTxSig := range forfeitTxSigs {
		if forfeitTxSig.UnsignedTx == nil {
			return fmt.Errorf("forfeit tx cannot be nil")
		}

		if forfeitTxSig.ClientVTXOSig == nil {
			return fmt.Errorf("client VTXO signature cannot be nil")
		}

		ftx := forfeitTxSig.UnsignedTx

		// Forfeit tx must have exactly 2 inputs: VTXO and connector.
		if len(ftx.TxIn) != 2 {
			return fmt.Errorf("forfeit tx must have exactly 2 "+
				"inputs, got %d", len(ftx.TxIn))
		}

		// Forfeit tx must have exactly 2 outputs: penalty and anchor.
		if len(ftx.TxOut) != 2 {
			return fmt.Errorf("forfeit tx must have exactly 2 "+
				"outputs, got %d", len(ftx.TxOut))
		}

		// Verify VTXO input is at index 0.
		vtxoInput := ftx.TxIn[tx.ForfeitVTXOInputIndex]
		vtxoOutpoint := vtxoInput.PreviousOutPoint

		// Verify this VTXO was in the client's forfeit inputs.
		forfeitInput, exists := expectedForfeits[vtxoOutpoint]
		if !exists {
			return fmt.Errorf("forfeit tx references unexpected "+
				"VTXO %v", vtxoOutpoint)
		}

		if _, seen := seenForfeits[vtxoOutpoint]; seen {
			return fmt.Errorf("duplicate forfeit tx for VTXO %v",
				vtxoOutpoint)
		}

		seenForfeits[vtxoOutpoint] = struct{}{}

		if forfeitInput.VTXO == nil {
			return fmt.Errorf("forfeit tx missing VTXO data")
		}

		// Look up the connector assignment for this forfeit.
		assignment, exists := connectorAssignments[vtxoOutpoint]
		if !exists {
			return fmt.Errorf("no connector assignment for VTXO %v",
				vtxoOutpoint)
		}

		if assignment.LeafOutput == nil {
			return fmt.Errorf("connector leaf output missing for "+
				"VTXO %v", vtxoOutpoint)
		}

		// Verify the forfeit tx spends the correct connector leaf.
		connectorInput := ftx.TxIn[tx.ForfeitConnectorInputIndex]
		if connectorInput.PreviousOutPoint != assignment.LeafOutpoint {
			return fmt.Errorf("connector input references wrong "+
				"leaf: expected %v, got %v",
				assignment.LeafOutpoint,
				connectorInput.PreviousOutPoint)
		}

		// Verify the penalty output sends to the server's forfeit
		// script and has the correct amount.
		penaltyOutput := ftx.TxOut[0]
		if !bytes.Equal(penaltyOutput.PkScript, forfeitScript) {
			return fmt.Errorf("penalty output script does not " +
				"match server's forfeit script")
		}

		expectedAmount := forfeitInput.VTXO.Descriptor.Amount
		if penaltyOutput.Value != int64(expectedAmount) {
			return fmt.Errorf("penalty output amount mismatch: "+
				"expected %d, got %d",
				expectedAmount, penaltyOutput.Value)
		}

		// Verify anchor output is at index 1.
		anchorOutput := ftx.TxOut[1]
		expectedAnchorScript := scripts.AnchorOutput().PkScript
		if !bytes.Equal(anchorOutput.PkScript, expectedAnchorScript) {
			return fmt.Errorf("anchor output script mismatch")
		}

		// Validate the client's VTXO signature cryptographically.
		if err := validateForfeitVTXOSignature(
			ftx, forfeitTxSig.ClientVTXOSig,
			forfeitInput.VTXO, vtxoOutpoint,
			assignment.LeafOutput, operatorKey,
		); err != nil {
			return fmt.Errorf("invalid VTXO signature for %v: %w",
				vtxoOutpoint, err)
		}
	}

	if len(seenForfeits) != len(expectedForfeits) {
		return fmt.Errorf("forfeit txs missing expected VTXOs")
	}

	return nil
}

// validateForfeitVTXOSignature validates the client's schnorr signature for
// the VTXO input in a forfeit transaction.
func validateForfeitVTXOSignature(
	ftx *wire.MsgTx, clientSig *schnorr.Signature, vtxo *VTXO,
	vtxoOutpoint wire.OutPoint, connectorLeafOutput *wire.TxOut,
	operatorKey *btcec.PublicKey) error {

	if vtxo == nil || vtxo.Descriptor == nil {
		return fmt.Errorf("VTXO descriptor must be provided")
	}

	if operatorKey == nil {
		return fmt.Errorf("operator key must be provided")
	}

	// Create the VTXO output.
	vtxoOutput := &wire.TxOut{
		Value:    int64(vtxo.Descriptor.Amount),
		PkScript: vtxo.Descriptor.PkScript,
	}

	// Get the connector input outpoint.
	connectorOutpoint :=
		ftx.TxIn[tx.ForfeitConnectorInputIndex].PreviousOutPoint

	// Reconstruct the collaborative spend leaf for the VTXO.
	collabLeaf, err := scripts.MultiSigCollabTapLeaf(
		vtxo.Descriptor.CoSignerKey, operatorKey,
	)
	if err != nil {
		return fmt.Errorf("failed to reconstruct VTXO collab leaf: %w",
			err)
	}

	// Create VTXO spend context.
	vtxoCtx := &tx.VTXOSpendContext{
		Outpoint:  vtxoOutpoint,
		Output:    vtxoOutput,
		TapScript: nil,
	}

	// Create connector spend context.
	connectorCtx := &tx.ConnectorSpendContext{
		Outpoint: connectorOutpoint,
		Output:   connectorLeafOutput,
	}

	// Build prev output fetcher using the tx package helper.
	prevOutFetcher, err := tx.NewForfeitPrevOutFetcher(
		vtxoCtx, connectorCtx,
	)
	if err != nil {
		return fmt.Errorf("failed to create prev out fetcher: %w",
			err)
	}

	// Create signature hashes.
	sigHashes := txscript.NewTxSigHashes(ftx, prevOutFetcher)

	tapLeaf := txscript.NewBaseTapLeaf(collabLeaf.Script)

	// Calculate the tapscript signature hash for the collaborative path.
	sigHash, err := txscript.CalcTapscriptSignaturehash(
		sigHashes, txscript.SigHashDefault, ftx,
		tx.ForfeitVTXOInputIndex, prevOutFetcher, tapLeaf,
	)
	if err != nil {
		return fmt.Errorf("failed to calculate tapscript "+
			"signature hash: %w", err)
	}

	// Verify the schnorr signature against the client's public key.
	if !clientSig.Verify(sigHash, vtxo.Descriptor.CoSignerKey) {
		return fmt.Errorf("invalid client VTXO signature")
	}

	return nil
}

// ValidateForfeitRequest validates a forfeit request from a client. It
// verifies:
//   - The VTXO exists in the store.
//   - The VTXO is in "live" status (confirmed on-chain).
//
// On success, returns a ForfeitInput containing the VTXO data.
func ValidateForfeitRequest(ctx context.Context, env *Environment,
	req *types.ForfeitRequest) (*ForfeitInput, error) {

	// Look up the VTXO in the store.
	vtxo, err := env.VTXOStore.GetVTXO(ctx, *req.VTXOOutpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrForfeitLookupFailed, err)
	}

	// Verify the VTXO exists.
	if vtxo == nil {
		return nil, fmt.Errorf("%w: %v", ErrForfeitVTXONotFound,
			req.VTXOOutpoint)
	}

	// Verify the VTXO is live (confirmed on-chain and not spent or
	// expired).
	if vtxo.Status != VTXOStatusLive {
		return nil, fmt.Errorf("%w: status is %s",
			ErrForfeitVTXONotLive, vtxo.Status)
	}

	return &ForfeitInput{
		Outpoint: req.VTXOOutpoint,
		VTXO:     vtxo,
	}, nil
}

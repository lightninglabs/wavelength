package rounds

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tree"
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
)

// JoinRequestResult holds the validated results from a join request.
type JoinRequestResult struct {
	// BoardingInputs contains the validated boarding inputs.
	BoardingInputs []*BoardingInput

	// RequiredOutputs contains the outputs from leave requests that must
	// be included in the round transaction.
	RequiredOutputs []*wire.TxOut

	// VTXODescriptors maps signing key hex strings to their VTXO
	// descriptors. The map key is the serialized public key.
	VTXODescriptors map[SigningKeyHex]*tree.VTXODescriptor

	// SigningKeys contains the unique signing keys from VTXO requests.
	// These are used for MuSig2 signing sessions when building the VTXO
	// tree. The map key is the serialized public key to ensure uniqueness.
	SigningKeys map[SigningKeyHex]*btcec.PublicKey
}

// ValidateJoinRequest validates a client's join request for the round. It
// verifies:
//   - Each boarding request is valid (no duplicates, passes individual
//     validation).
//   - Each leave request is valid (non-nil output, positive value, non-empty
//     pkScript).
//   - Each VTXO request is valid (passes individual validation).
//   - The total output value (leave + VTXO) does not exceed the total input
//     value.
//
// TODO(elle): Add forfeit request validation.
//
// On success, returns JoinRequestResult containing all validated data.
func ValidateJoinRequest(ctx context.Context, env *Environment,
	req *types.JoinRoundRequest) (*JoinRequestResult, error) {

	var (
		boardingInputs  []*BoardingInput
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
			ctx, env, boardReq,
		)
		if err != nil {
			return nil, fmt.Errorf("invalid boarding request "+
				"for outpoint %v: %w", boardReq.Outpoint, err)
		}

		boardReqs.Add(*boardReq.Outpoint)
		boardingInputs = append(boardingInputs, boardingInput)
		totalInputValue += boardingInput.Value
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
	// leave request total + VTXO total cannot exceed their boarding input
	// total.
	totalOutputValue := totalLeaveValue + totalVTXOValue
	if totalOutputValue > totalInputValue {
		return nil, fmt.Errorf("%w: got %d sats, max %d sats",
			ErrOutputExceedsInput, totalOutputValue,
			totalInputValue)
	}

	return &JoinRequestResult{
		BoardingInputs:  boardingInputs,
		RequiredOutputs: requiredOutputs,
		VTXODescriptors: vtxoDescriptors,
		SigningKeys:     signingKeys,
	}, nil
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
//
// TODO(elle): Add proof of ownership check.
//
// On success, returns a BoardingInput containing all data needed for
// transaction construction.
func ValidateBoardingRequest(ctx context.Context, env *Environment,
	req *types.BoardingRequest) (*BoardingInput, error) {

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

	// Fetch the UTXO from the chain.
	utxo, err := env.ChainSource.GetUTXO(*req.Outpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFetchUTXO, err)
	}

	// Verify the UTXO meets the minimum confirmation requirement.
	if utxo.Confirmations < int64(terms.MinBoardingConfirmations) {
		return nil, fmt.Errorf("%w: got %d, want %d",
			ErrInsufficientConfirmations, utxo.Confirmations,
			terms.MinBoardingConfirmations)
	}

	// Ensure the delay path isn't already hit or close to being hit. If
	// the UTXO has too many confirmations (approaching the exit delay),
	// the client could claim the funds via the delay path before the
	// operator can use them in a round.
	safetyMargin := terms.BoardingExitDelaySafetyMargin
	maxSafeConfirmations := req.ExitDelay - safetyMargin
	if utxo.Confirmations >= int64(maxSafeConfirmations) {
		return nil, fmt.Errorf("%w: got %d confirmations, max "+
			"safe %d (exit delay %d - safety margin %d)",
			ErrDelayPathTooClose, utxo.Confirmations,
			maxSafeConfirmations, req.ExitDelay, safetyMargin)
	}

	// Validate that the UTXO's script matches expectations.
	expectedPkScript, err := buildP2TRScript(expectedTapscript)
	if err != nil {
		return nil, fmt.Errorf("%w (P2TR): %w", ErrScriptConstruction,
			err)
	}

	// Check that the pkScript matches the expected script.
	if !bytes.Equal(utxo.Output.PkScript, expectedPkScript) {
		return nil, ErrPkScriptMismatch
	}

	// Build the BoardingInput.
	return &BoardingInput{
		Outpoint:        req.Outpoint,
		Tapscript:       expectedTapscript,
		Value:           btcutil.Amount(utxo.Output.Value),
		PkScript:        expectedPkScript,
		ClientKey:       req.ClientKey,
		OperatorKeyDesc: &terms.OperatorKey,
	}, nil
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

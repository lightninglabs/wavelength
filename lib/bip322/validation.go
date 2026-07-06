package bip322

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

var (
	// errMissingProofPrevOut marks missing UTXO metadata for proof-of-funds
	// additional inputs.
	errMissingProofPrevOut = errors.New("missing proof prevout")
)

// VerificationState is the verification result category defined by BIP-322.
type VerificationState string

const (
	// VerificationStateValid indicates the signature validated
	// successfully.
	VerificationStateValid VerificationState = "valid"

	// VerificationStateInconclusive indicates script evaluation could
	// not be completed with the available information or policy.
	VerificationStateInconclusive VerificationState = "inconclusive"

	// VerificationStateInvalid indicates verification failed.
	VerificationStateInvalid VerificationState = "invalid"
)

const (
	// defaultMaxProofInputs bounds proof-of-funds input count accepted
	// during validation to cap worst-case script-engine work per auth
	// package.
	defaultMaxProofInputs = 128
)

// VerificationResult contains the result of validating a BIP-322 auth package.
type VerificationResult struct {
	// State is the verification state.
	State VerificationState

	// ValidAtTime is the to_sign nLockTime value when State is valid.
	ValidAtTime uint32

	// ValidAtAge is the to_sign first input nSequence value when State is
	// valid.
	ValidAtAge uint32

	// Reason contains a short reason for invalid/inconclusive outcomes.
	Reason string
}

// AuthPkg holds all data required to validate a full-format BIP-322
// signature.
type AuthPkg struct {
	// Message is the raw message byte payload to verify.
	Message []byte

	// MessageChallenge is the scriptPubKey being proven.
	MessageChallenge []byte

	// Sig is the full-format BIP-322 signature payload.
	Sig *Sig

	// ProofPrevOutputs provides UTXO details for additional to_sign inputs
	// used in proof-of-funds signatures.
	ProofPrevOutputs map[wire.OutPoint]*wire.TxOut
}

// ValidateAuthOption configures optional application-level checks in
// ValidateAuthPkg.
type ValidateAuthOption func(*validateAuthOptions) error

// validateAuthOptions carries optional policy checks layered above core
// BIP-322 validation.
type validateAuthOptions struct {
	// maxProofInputs bounds additional proof-of-funds inputs (vin[1..N])
	// accepted during validation.
	maxProofInputs int
}

// defaultValidateAuthOptions returns default validation policy options.
func defaultValidateAuthOptions() validateAuthOptions {
	return validateAuthOptions{
		maxProofInputs: defaultMaxProofInputs,
	}
}

// WithMaxProofInputs sets the maximum number of proof-of-funds inputs
// accepted by ValidateAuthPkg. This limit applies to additional inputs
// only; input 0 (the to_spend reference) is not counted.
func WithMaxProofInputs(maxProofInputs int) ValidateAuthOption {
	return func(opts *validateAuthOptions) error {
		if maxProofInputs < 0 {
			return fmt.Errorf("max proof inputs must be " +
				"non-negative")
		}

		opts.maxProofInputs = maxProofInputs

		return nil
	}
}

// applyValidateAuthOptions applies validation options and returns final
// settings.
func applyValidateAuthOptions(opts []ValidateAuthOption) (validateAuthOptions,
	error) {

	validationOpts := defaultValidateAuthOptions()

	for i := 0; i < len(opts); i++ {
		opt := opts[i]
		if opt == nil {
			return validateAuthOptions{}, fmt.Errorf("validate "+
				"auth option %d must be provided", i)
		}

		err := opt(&validationOpts)
		if err != nil {
			return validateAuthOptions{}, err
		}
	}

	return validationOpts, nil
}

// ValidateAuthPkg validates the provided authentication package according
// to BIP-322 and then applies optional application-level policy checks.
func ValidateAuthPkg(pkg *AuthPkg,
	opts ...ValidateAuthOption) VerificationResult {

	// Step 0: Parse application-level validation options.
	validationOpts, err := applyValidateAuthOptions(opts)
	if err != nil {
		return invalidResult(err.Error())
	}

	// Step 1: Ensure the package is complete enough to construct
	// the BIP-322 virtual transactions deterministically.
	if pkg == nil {
		return invalidResult("auth package must be provided")
	}

	if len(pkg.MessageChallenge) == 0 {
		return invalidResult(
			"message challenge script must be provided",
		)
	}

	if pkg.Sig == nil {
		return invalidResult("signature must be provided")
	}

	if pkg.Sig.ToSign == nil {
		return invalidResult(
			"full signature transaction must be provided",
		)
	}

	// Step 2: Rebuild to_spend from the message and challenge script so we
	// validate against the exact commitment model defined by the spec.
	messageHash := MessageHash(pkg.Message)
	toSpend, err := BuildToSpend(messageHash, pkg.MessageChallenge)
	if err != nil {
		return invalidResult(fmt.Sprintf("build to_spend: %v", err))
	}

	// Step 3: Work on a copy of to_sign so validation never mutates caller
	// state.
	toSign := pkg.Sig.ToSign.Copy()

	// Step 4: Apply BIP-322 full-format structural checks before
	// running the script engine.
	err = validateFullToSignShape(
		toSign, toSpend, validationOpts.maxProofInputs,
	)
	if err != nil {
		return invalidResult(err.Error())
	}

	// Step 5: Apply upgradeable rule checks that map to
	// inconclusive instead of invalid.
	err = validateToSignVersion(toSign.Version)
	if err != nil {
		return inconclusiveResult(err.Error())
	}

	// Step 6: Build prevout data for all to_sign inputs. For proof-of-funds
	// inputs, missing UTXO data is inconclusive by definition.
	prevFetcher, err := buildPrevOutFetcher(
		toSign, toSpend, pkg.ProofPrevOutputs,
	)
	if err != nil {
		if errors.Is(err, errMissingProofPrevOut) {
			return inconclusiveResult(err.Error())
		}

		return invalidResult(err.Error())
	}

	// Step 7: Execute script validation for every input under
	// standard policy flags. Upgradeable script failures are
	// mapped to inconclusive.
	hashCache := txscript.NewTxSigHashes(toSign, prevFetcher)
	for inputIndex := 0; inputIndex < len(toSign.TxIn); inputIndex++ {
		err = validateInputScript(
			toSign, inputIndex, hashCache, prevFetcher,
		)
		if err != nil {
			if isUpgradeableScriptError(err) {
				return inconclusiveResult(
					fmt.Sprintf("input %d uses "+
						"upgradeable script "+
						"feature: %v", inputIndex, err),
				)
			}

			return invalidResult(
				fmt.Sprintf("input %d script validation "+
					"failed: %v", inputIndex, err),
			)
		}
	}

	// Step 8: Surface the lock metadata exactly as BIP-322 specifies,
	// then return to the caller.
	return validResult(toSign.LockTime, toSign.TxIn[0].Sequence)
}

// validateFullToSignShape enforces the BIP-322 full-format structural rules.
func validateFullToSignShape(toSign *wire.MsgTx, toSpend *wire.MsgTx,
	maxProofInputs int) error {

	if len(toSign.TxIn) == 0 {
		return fmt.Errorf("full signature tx must include at least " +
			"one input")
	}

	proofInputCount := len(toSign.TxIn) - 1
	if proofInputCount > maxProofInputs {
		return fmt.Errorf("full signature tx proof input count %d "+
			"exceeds max %d", proofInputCount, maxProofInputs)
	}

	firstInput := toSign.TxIn[0].PreviousOutPoint
	expectedFirstInput := wire.OutPoint{
		Hash:  toSpend.TxHash(),
		Index: 0,
	}
	if firstInput != expectedFirstInput {
		return fmt.Errorf("full signature tx input 0 must spend " +
			"to_spend output 0")
	}

	if len(toSign.TxOut) != 1 {
		return fmt.Errorf("full signature tx must include exactly " +
			"one output")
	}

	output := toSign.TxOut[0]
	if output.Value != 0 {
		return fmt.Errorf("full signature tx output value must be 0")
	}

	if !bytes.Equal(output.PkScript, []byte{txscript.OP_RETURN}) {
		return fmt.Errorf("full signature tx output script must be " +
			"OP_RETURN")
	}

	return nil
}

// buildPrevOutFetcher assembles the prev-output lookup required for script
// verification of to_sign inputs.
func buildPrevOutFetcher(toSign *wire.MsgTx, toSpend *wire.MsgTx,
	proofPrevOuts map[wire.OutPoint]*wire.TxOut) (
	txscript.PrevOutputFetcher, error) {

	if len(toSign.TxIn) == 0 {
		return nil, fmt.Errorf("to_sign has no inputs")
	}

	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(toSign.TxIn))
	firstPrevOut := toSign.TxIn[0].PreviousOutPoint

	// Input 0 always spends to_spend output 0 in the full format.
	prevOuts[firstPrevOut] = toSpend.TxOut[0]

	for inputIndex := 1; inputIndex < len(toSign.TxIn); inputIndex++ {
		outPoint := toSign.TxIn[inputIndex].PreviousOutPoint

		proofPrevOut, ok := proofPrevOuts[outPoint]
		if !ok || proofPrevOut == nil {
			return nil, fmt.Errorf("%w for input %d (%s)",
				errMissingProofPrevOut, inputIndex, outPoint)
		}

		prevOuts[outPoint] = proofPrevOut
	}

	return txscript.NewMultiPrevOutFetcher(prevOuts), nil
}

// validateInputScript executes script validation for one input.
func validateInputScript(toSign *wire.MsgTx, inputIndex int,
	hashCache *txscript.TxSigHashes,
	prevFetcher txscript.PrevOutputFetcher) error {

	prevOut := prevFetcher.FetchPrevOutput(
		toSign.TxIn[inputIndex].PreviousOutPoint,
	)
	if prevOut == nil {
		return fmt.Errorf("missing prevout for input %d", inputIndex)
	}

	engine, err := txscript.NewEngine(
		prevOut.PkScript, toSign, inputIndex,
		txscript.StandardVerifyFlags, nil, hashCache, prevOut.Value,
		prevFetcher,
	)
	if err != nil {
		return err
	}

	return engine.Execute()
}

// isUpgradeableScriptError identifies script failures that BIP-322 maps to the
// inconclusive state.
func isUpgradeableScriptError(err error) bool {
	upgradeableCodes := [...]txscript.ErrorCode{
		txscript.ErrDiscourageUpgradableNOPs,
		txscript.ErrDiscourageUpgradableWitnessProgram,
		txscript.ErrDiscourageUpgradeableTaprootVersion,
		txscript.ErrDiscourageUpgradeablePubKeyType,
		txscript.ErrDiscourageOpSuccess,
	}

	for i := 0; i < len(upgradeableCodes); i++ {
		if txscript.IsErrorCode(err, upgradeableCodes[i]) {
			return true
		}
	}

	return false
}

// validResult creates a successful verification result.
func validResult(validAtTime uint32, validAtAge uint32) VerificationResult {
	return VerificationResult{
		State:       VerificationStateValid,
		ValidAtTime: validAtTime,
		ValidAtAge:  validAtAge,
	}
}

// invalidResult creates an invalid verification result.
func invalidResult(reason string) VerificationResult {
	return VerificationResult{
		State:  VerificationStateInvalid,
		Reason: reason,
	}
}

// inconclusiveResult creates an inconclusive verification result.
func inconclusiveResult(reason string) VerificationResult {
	return VerificationResult{
		State:  VerificationStateInconclusive,
		Reason: reason,
	}
}

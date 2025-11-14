package lib

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

type getUTXOFn func(op *wire.OutPoint) (*wire.TxOut, bool, int64, error)

// BoardingValidator encapsulates the validation rules for boarding requests.
type BoardingValidator struct {
	operatorKeyDesc  *keychain.KeyDescriptor
	minExitDelay     uint32
	minConfirmations int64
	minAmount        btcutil.Amount
	getUTXO          getUTXOFn
}

// NewBoardingValidator creates a new validator with the given parameters.
func NewBoardingValidator(operatorKeyDesc *keychain.KeyDescriptor,
	minExitDelay uint32, minBoardingAmount btcutil.Amount,
	minConfirmations int64, getUTXO getUTXOFn) *BoardingValidator {

	return &BoardingValidator{
		operatorKeyDesc:  operatorKeyDesc,
		minExitDelay:     minExitDelay,
		minConfirmations: minConfirmations,
		minAmount:        minBoardingAmount,
		getUTXO:          getUTXO,
	}
}

// ValidateRequest performs all validations on a boarding request and returns
// a BoardingInput if valid.
func (v *BoardingValidator) ValidateRequest(req *BoardingRequest) (
	*BoardingInput, error) {

	// First perform pure validations.
	if err := v.validateOperatorTerms(req); err != nil {
		return nil, err
	}

	// Then perform chain-based validations.
	output, err := v.validateUTXO(req.Outpoint)
	if err != nil {
		return nil, err
	}

	// Build and validate the boarding tapscript
	tapscript, err := BoardingTapScript(
		req.ClientKey, v.operatorKeyDesc.PubKey, req.ExitDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build boarding "+
			"tapscript: %w", err)
	}

	// Validate that the UTXO's script matches expectations.
	expectedPkScript, err := buildP2TRScript(tapscript)
	if err != nil {
		return nil, err
	}

	// Check that the pkScript matches the expected script.
	if !bytes.Equal(output.PkScript, expectedPkScript) {
		return nil, fmt.Errorf("boarding input pkScript does not " +
			"match expected tapscript")
	}

	// All validations passed, construct the BoardingInput
	return &BoardingInput{
		Outpoint:        req.Outpoint,
		Tapscript:       tapscript,
		Value:           btcutil.Amount(output.Value),
		PkScript:        expectedPkScript,
		ClientKey:       req.ClientKey,
		OperatorKeyDesc: v.operatorKeyDesc,
	}, nil
}

// validateOperatorTerms validates that the boarding request operator key
// and exit delay.
func (v *BoardingValidator) validateOperatorTerms(req *BoardingRequest) error {
	if !req.OperatorKey.IsEqual(v.operatorKeyDesc.PubKey) {
		return fmt.Errorf("invalid operator key for boarding request")
	}

	if req.ExitDelay < v.minExitDelay {
		return fmt.Errorf("boarding request exit delay %d is less "+
			"than required minimum %d", req.ExitDelay,
			v.minExitDelay)
	}
	return nil
}

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

// validateUTXO performs chain-based validation of the boarding UTXO.
func (v *BoardingValidator) validateUTXO(outpoint *wire.OutPoint) (*wire.TxOut,
	error) {

	// Fetch the UTXO from the chain
	output, spent, confs, err := v.getUTXO(outpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch boarding input"+
			" transaction: %w", err)
	}

	// Check that it has not been spent yet
	if spent {
		return nil, fmt.Errorf("boarding input %s already spent",
			outpoint.String())
	}

	if btcutil.Amount(output.Value) < v.minAmount {
		return nil, fmt.Errorf("boarding input %s has value %d, "+
			"below minimum %d", outpoint.String(), output.Value,
			v.minAmount)
	}

	// Check that it has enough confirmations
	if confs < v.minConfirmations {
		return nil, fmt.Errorf("boarding input %s has only %d "+
			"confirmations, requires at least %d",
			outpoint.String(), confs, v.minConfirmations)
	}

	return output, nil
}

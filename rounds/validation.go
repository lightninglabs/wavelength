package rounds

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightningnetwork/lnd/input"
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
)

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

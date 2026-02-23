package bip322

import "fmt"

// BlockWindow defines application-level block-height validity bounds for a
// BIP-322 signature.
//
// This wrapper layer maps the bounds onto BIP-322's surfaced lock metadata:
//   - ValidFromBlock -> to_sign nLockTime
//   - ValidUntilBlock -> to_sign vin[0].nSequence
//
// BIP-322 surfaces these values in the verification result as "valid at time"
// and "valid at age". The core spec does not mandate one global application
// meaning, so this type defines a local policy on top of the base spec.
type BlockWindow struct {
	// ValidFromBlock is the first block height at which the signature is
	// accepted by the application policy.
	ValidFromBlock uint32

	// ValidUntilBlock is the last block height at which the signature is
	// accepted by the application policy.
	//
	// A value of 0 means there is no upper bound.
	ValidUntilBlock uint32
}

// WithBlockWindow applies a BlockWindow to to_sign lock metadata and
// automatically sets the transaction version to 2 so that nLockTime and
// nSequence carry their intended meaning.
func WithBlockWindow(window BlockWindow) ToSignOption {
	return func(opts *toSignBuildOptions) error {
		err := validateBlockWindow(window)
		if err != nil {
			return err
		}

		opts.version = 2
		opts.lockTime = window.ValidFromBlock
		opts.sequence = window.ValidUntilBlock

		return nil
	}
}

// WithCurrentBlockHeight enables BlockWindow policy checks in
// ValidateAuthPkg.
func WithCurrentBlockHeight(currentBlockHeight uint32) ValidateAuthOption {
	return func(opts *validateAuthOptions) error {
		opts.currentBlockHeight = &currentBlockHeight
		return nil
	}
}

// applyBlockWindowValidationPolicy enforces block-height window checks
// when a current chain height is provided.
//
// This is an application-level policy layer and not a core BIP-322
// validity requirement.
func applyBlockWindowValidationPolicy(result VerificationResult,
	opts validateAuthOptions) VerificationResult {

	if result.State != VerificationStateValid {
		return result
	}

	if opts.currentBlockHeight == nil {
		return result
	}

	window := BlockWindow{
		ValidFromBlock:  result.ValidAtTime,
		ValidUntilBlock: result.ValidAtAge,
	}

	err := validateBlockWindow(window)
	if err != nil {
		result.State = VerificationStateInvalid
		result.Reason = err.Error()
		return result
	}

	currentBlockHeight := *opts.currentBlockHeight
	if currentBlockHeight < window.ValidFromBlock {
		result.State = VerificationStateInvalid
		result.Reason = fmt.Sprintf(
			"signature not yet valid: current block %d is "+
				"below valid-from block %d",
			currentBlockHeight,
			window.ValidFromBlock,
		)

		return result
	}

	if window.ValidUntilBlock != 0 &&
		currentBlockHeight > window.ValidUntilBlock {

		result.State = VerificationStateInvalid
		result.Reason = fmt.Sprintf(
			"signature expired: current block %d is above "+
				"valid-until block %d",
			currentBlockHeight,
			window.ValidUntilBlock,
		)

		return result
	}

	return result
}

// validateBlockWindow checks whether a window is internally consistent.
func validateBlockWindow(window BlockWindow) error {
	if window.ValidUntilBlock != 0 &&
		window.ValidUntilBlock < window.ValidFromBlock {

		return fmt.Errorf(
			"valid-until block %d must be greater than or "+
				"equal to valid-from block %d",
			window.ValidUntilBlock,
			window.ValidFromBlock,
		)
	}

	return nil
}

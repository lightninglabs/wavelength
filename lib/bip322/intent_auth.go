package bip322

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
)

// IntentAuthContext holds all data required to validate an intent-bound
// BIP-322 authorization proof.
type IntentAuthContext struct {
	// Intent is the application-layer intent committed into the message
	// digest.
	Intent *Intent

	// MessageChallenge is the scriptPubKey proven by to_sign input 0.
	MessageChallenge []byte

	// Sig is the decoded full-format BIP-322 signature payload.
	Sig *Sig

	// ProofPrevOutputs provides UTXO metadata for proof-of-funds inputs.
	ProofPrevOutputs map[wire.OutPoint]*wire.TxOut

	// CurrentBlockHeight optionally enforces intent validity against a
	// specific chain height.
	CurrentBlockHeight *uint32

	// ValidateAuthOptions are forwarded to ValidateAuthPkg.
	ValidateAuthOptions []ValidateAuthOption
}

// NewIntentAuthContext builds an intent-auth validation context from raw join
// auth fields and decodes the serialized signature.
func NewIntentAuthContext(payload []byte, validFrom uint32, validUntil uint32,
	messageChallenge []byte, rawSignature []byte,
	proofPrevOutputs map[wire.OutPoint]*wire.TxOut,
	currentBlockHeight *uint32,
	validateAuthOptions ...ValidateAuthOption) (*IntentAuthContext, error) {

	intent, err := NewIntent(payload, validFrom, validUntil)
	if err != nil {
		return nil, fmt.Errorf("intent: %w", err)
	}

	if len(messageChallenge) == 0 {
		return nil, fmt.Errorf("message challenge script must be " +
			"provided")
	}

	if len(rawSignature) == 0 {
		return nil, fmt.Errorf("signature must be provided")
	}

	sig, err := DecodeSig(rawSignature)
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}

	ctx := &IntentAuthContext{
		Intent:           intent,
		MessageChallenge: bytes.Clone(messageChallenge),
		Sig:              sig,
		ProofPrevOutputs: cloneProofPrevOutputsMap(proofPrevOutputs),
		ValidateAuthOptions: append(
			[]ValidateAuthOption(nil), validateAuthOptions...,
		),
	}

	if currentBlockHeight != nil {
		height := *currentBlockHeight
		ctx.CurrentBlockHeight = &height
	}

	return ctx, nil
}

// Validate checks intent validity policy and then runs core BIP-322
// verification for this context.
func (c *IntentAuthContext) Validate() VerificationResult {
	if c == nil {
		return invalidResult("intent auth context must be provided")
	}

	if c.Intent == nil {
		return invalidResult("intent must be provided")
	}

	var err error
	if c.CurrentBlockHeight != nil {
		err = c.Intent.ValidateAtHeight(*c.CurrentBlockHeight)
	} else {
		err = c.Intent.Validate()
	}
	if err != nil {
		return invalidResult(fmt.Sprintf("intent invalid: %v", err))
	}

	intentMessage, err := c.Intent.SigningMessage()
	if err != nil {
		return invalidResult(fmt.Sprintf("intent message: %v", err))
	}

	return ValidateAuthPkg(
		&AuthPkg{
			Message:          intentMessage,
			MessageChallenge: c.MessageChallenge,
			Sig:              c.Sig,
			ProofPrevOutputs: c.ProofPrevOutputs,
		},
		c.ValidateAuthOptions...,
	)
}

// cloneProofPrevOutputsMap deep-copies the proof-prevout map so callers can
// safely mutate their inputs after context construction.
func cloneProofPrevOutputsMap(
	src map[wire.OutPoint]*wire.TxOut) map[wire.OutPoint]*wire.TxOut {

	if len(src) == 0 {
		return nil
	}

	dst := make(map[wire.OutPoint]*wire.TxOut, len(src))
	for outpoint, txOut := range src {
		dst[outpoint] = cloneTxOut(txOut)
	}

	return dst
}

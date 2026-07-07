package bip322

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
)

// JoinRoundMessageChallenge derives the challenge script used for round
// join authorization signatures from the request identifier key.
//
// The challenge is a key-path Taproot output (BIP-86 style, no script
// tree) so the signer can satisfy input 0 with a single Schnorr
// signature.
func JoinRoundMessageChallenge(identifier *btcec.PublicKey) ([]byte, error) {
	if identifier == nil {
		return nil, fmt.Errorf("join request identifier must be " +
			"provided")
	}

	taprootKey := txscript.ComputeTaprootKeyNoScript(identifier)

	challengeScript, err := txscript.PayToTaprootScript(taprootKey)
	if err != nil {
		return nil, fmt.Errorf("join auth challenge script: %w", err)
	}

	return challengeScript, nil
}

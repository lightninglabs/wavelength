package bip322

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/stretchr/testify/require"
)

// TestJoinRoundMessageChallengeMatchesTaprootKeyPath asserts the
// join-round challenge script is the expected BIP-86 key-path taproot
// script.
func TestJoinRoundMessageChallengeMatchesTaprootKeyPath(t *testing.T) {
	t.Parallel()

	privateKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	taprootKey := txscript.ComputeTaprootKeyNoScript(
		privateKey.PubKey(),
	)
	expectedScript, err := txscript.PayToTaprootScript(taprootKey)
	require.NoError(t, err)

	challengeScript, err := JoinRoundMessageChallenge(
		privateKey.PubKey(),
	)
	require.NoError(t, err)
	require.Equal(t, expectedScript, challengeScript)
}

// TestJoinRoundMessageChallengeRejectsMissingIdentifier asserts
// identifier keys are mandatory when deriving join-round challenge
// scripts.
func TestJoinRoundMessageChallengeRejectsMissingIdentifier(t *testing.T) {
	t.Parallel()

	_, err := JoinRoundMessageChallenge(nil)
	require.Error(t, err)
}

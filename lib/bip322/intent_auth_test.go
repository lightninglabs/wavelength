package bip322

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/stretchr/testify/require"
)

// TestNewIntentCopiesPayload asserts NewIntent clones payload bytes.
func TestNewIntentCopiesPayload(t *testing.T) {
	t.Parallel()

	payload := []byte("payload")
	intent, err := NewIntent(payload, 1, 2)
	require.NoError(t, err)

	payload[0] ^= 0xff
	require.NotEqual(t, payload, intent.Payload)
}

// TestIntentAuthContextValidateValid asserts context validation succeeds for a
// correctly signed intent payload.
func TestIntentAuthContextValidateValid(t *testing.T) {
	t.Parallel()

	payload := []byte("join-auth")
	challengeScript, rawSig := buildRawIntentSignature(
		t, payload, 100, 200,
	)
	height := uint32(150)

	ctx, err := NewIntentAuthContext(
		payload, 100, 200, challengeScript, rawSig, nil, &height,
	)
	require.NoError(t, err)

	result := ctx.Validate()
	require.Equal(t, VerificationStateValid, result.State)
}

// TestIntentAuthContextValidateNotYetValid asserts context validation rejects
// intent metadata that is not yet active at the given height.
func TestIntentAuthContextValidateNotYetValid(t *testing.T) {
	t.Parallel()

	payload := []byte("join-auth")
	challengeScript, rawSig := buildRawIntentSignature(
		t, payload, 100, 200,
	)
	height := uint32(99)

	ctx, err := NewIntentAuthContext(
		payload, 100, 200, challengeScript, rawSig, nil, &height,
	)
	require.NoError(t, err)

	result := ctx.Validate()
	require.Equal(t, VerificationStateInvalid, result.State)
	require.Contains(t, result.Reason, "not yet valid")
}

// TestIntentAuthContextValidateDetectsTamperedMetadata asserts validation
// fails when validity metadata is changed without re-signing.
func TestIntentAuthContextValidateDetectsTamperedMetadata(t *testing.T) {
	t.Parallel()

	payload := []byte("join-auth")
	challengeScript, rawSig := buildRawIntentSignature(
		t, payload, 100, 200,
	)
	height := uint32(150)

	ctx, err := NewIntentAuthContext(
		payload, 100, 201, challengeScript, rawSig, nil, &height,
	)
	require.NoError(t, err)

	result := ctx.Validate()
	require.Equal(t, VerificationStateInvalid, result.State)
}

// TestNewIntentAuthContextRejectsBadSignature asserts constructor rejects
// malformed raw signature payloads.
func TestNewIntentAuthContextRejectsBadSignature(t *testing.T) {
	t.Parallel()

	_, err := NewIntentAuthContext(
		[]byte("payload"), 1, 2, []byte{0x51}, []byte{0x01, 0x02}, nil,
		nil,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "decode signature")
}

// buildRawIntentSignature signs the intent message and returns challenge script
// and encoded full-format signature bytes.
func buildRawIntentSignature(t *testing.T, payload []byte, validFrom uint32,
	validUntil uint32) ([]byte, []byte) {

	t.Helper()

	privateKey, challengeScript := testTaprootChallengeScript(t)

	intent, err := NewIntent(payload, validFrom, validUntil)
	require.NoError(t, err)

	intentMessage, err := intent.SigningMessage()
	require.NoError(t, err)

	sig, err := BuildAndSignFullTx(
		intentMessage, challengeScript, &taprootTxSigner{
			privateKey: privateKey,
		},
		WithToSignVersion(2),
	)
	require.NoError(t, err)

	rawSig, err := sig.Encode()
	require.NoError(t, err)

	return challengeScript, rawSig
}

// testTaprootChallengeScript returns a fresh private key and matching key-path
// taproot challenge script.
func testTaprootChallengeScript(t *testing.T) (*btcec.PrivateKey, []byte) {
	t.Helper()

	privateKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	taprootKey := txscript.ComputeTaprootKeyNoScript(privateKey.PubKey())
	challengeScript, err := txscript.PayToTaprootScript(taprootKey)
	require.NoError(t, err)

	return privateKey, challengeScript
}

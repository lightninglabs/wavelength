package bip322

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// TestBuildAndSignFullTxWithBlockWindowValid asserts the block-window
// option can build, sign, and validate a proof-of-funds signature.
func TestBuildAndSignFullTxWithBlockWindowValid(t *testing.T) {
	t.Parallel()

	privateKey, challengeScript := testTaprootChallenge(t)

	additionalOutPoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0x0a, 0x0b, 0x0c, 0x0d,
		},
		Index: 7,
	}
	additionalPrevOut := wire.TxOut{
		Value:    321_000,
		PkScript: cloneBytes(challengeScript),
	}

	window := BlockWindow{
		ValidFromBlock:  100,
		ValidUntilBlock: 200,
	}
	message := []byte("windowed proof-of-funds signature")

	sig, err := BuildAndSignFullTx(
		message, challengeScript,
		&taprootTxSigner{privateKey: privateKey},
		WithBlockWindow(window),
		WithToSignAdditionalInputs(
			AdditionalInput{
				PreviousOutPoint: additionalOutPoint,
				Sequence:         21,
				WitnessUtxo:      &additionalPrevOut,
			},
		),
	)
	require.NoError(t, err)
	require.NotNil(t, sig)
	require.Equal(t, int32(2), sig.ToSign.Version)
	require.Equal(t, uint32(100), sig.ToSign.LockTime)
	require.Equal(t, uint32(200), sig.ToSign.TxIn[0].Sequence)

	result := ValidateAuthPkg(
		&AuthPkg{
			Message:          message,
			MessageChallenge: challengeScript,
			Sig:              sig,
			ProofPrevOutputs: map[wire.OutPoint]*wire.TxOut{
				additionalOutPoint: {
					Value: additionalPrevOut.Value,
					PkScript: cloneBytes(
						additionalPrevOut.PkScript,
					),
				},
			},
		},
		WithCurrentBlockHeight(150),
	)
	require.Equal(t, VerificationStateValid, result.State)
	require.Equal(t, uint32(100), result.ValidAtTime)
	require.Equal(t, uint32(200), result.ValidAtAge)
}

// TestBuildAndSignFullTxBlockWindowSetsVersion asserts WithBlockWindow
// automatically sets to_sign version to 2.
func TestBuildAndSignFullTxBlockWindowSetsVersion(t *testing.T) {
	t.Parallel()

	privateKey, challengeScript := testTaprootChallenge(t)

	sig, err := BuildAndSignFullTx(
		[]byte("auto version"), challengeScript,
		&taprootTxSigner{privateKey: privateKey},
		WithBlockWindow(BlockWindow{
			ValidFromBlock:  10,
			ValidUntilBlock: 20,
		}),
	)
	require.NoError(t, err)
	require.Equal(t, int32(2), sig.ToSign.Version)
}

// TestBuildAndSignFullTxRejectsInvalidBlockWindow asserts invalid block
// windows are rejected before signing.
func TestBuildAndSignFullTxRejectsInvalidBlockWindow(t *testing.T) {
	t.Parallel()

	privateKey, challengeScript := testTaprootChallenge(t)

	_, err := BuildAndSignFullTx(
		[]byte("invalid window"), challengeScript,
		&taprootTxSigner{privateKey: privateKey},
		WithBlockWindow(BlockWindow{
			ValidFromBlock:  50,
			ValidUntilBlock: 49,
		}),
	)
	require.Error(t, err)
}

// TestValidateAuthPkgWithCurrentHeightTooEarly asserts signatures are
// rejected before the configured valid-from height.
func TestValidateAuthPkgWithCurrentHeightTooEarly(t *testing.T) {
	t.Parallel()

	pkg := buildWindowedAuthPkg(
		t, []byte("too early"),
		BlockWindow{
			ValidFromBlock:  300,
			ValidUntilBlock: 400,
		},
	)

	result := ValidateAuthPkg(pkg, WithCurrentBlockHeight(299))
	require.Equal(t, VerificationStateInvalid, result.State)
	require.Contains(t, result.Reason, "not yet valid")
}

// TestValidateAuthPkgWithCurrentHeightExpired asserts signatures are
// rejected after the configured valid-until height.
func TestValidateAuthPkgWithCurrentHeightExpired(t *testing.T) {
	t.Parallel()

	pkg := buildWindowedAuthPkg(
		t, []byte("expired"),
		BlockWindow{
			ValidFromBlock:  10,
			ValidUntilBlock: 20,
		},
	)

	result := ValidateAuthPkg(pkg, WithCurrentBlockHeight(21))
	require.Equal(t, VerificationStateInvalid, result.State)
	require.Contains(t, result.Reason, "expired")
}

// TestValidateAuthPkgWithCurrentHeightNoUpperBound asserts a zero
// valid-until block means the signature does not expire.
func TestValidateAuthPkgWithCurrentHeightNoUpperBound(t *testing.T) {
	t.Parallel()

	pkg := buildWindowedAuthPkg(
		t, []byte("open ended"),
		BlockWindow{
			ValidFromBlock:  123,
			ValidUntilBlock: 0,
		},
	)

	result := ValidateAuthPkg(pkg, WithCurrentBlockHeight(100_000))
	require.Equal(t, VerificationStateValid, result.State)
}

// TestValidateAuthPkgRejectsInvalidEmbeddedBlockWindow asserts the
// validation layer rejects signatures whose lock metadata encodes an
// invalid block window.
func TestValidateAuthPkgRejectsInvalidEmbeddedBlockWindow(t *testing.T) {
	t.Parallel()

	privateKey, challengeScript := testTaprootChallenge(t)
	message := []byte("invalid embedded window")

	sig, err := BuildAndSignFullTx(
		message, challengeScript,
		&taprootTxSigner{privateKey: privateKey},
		WithToSignVersion(2),
		WithToSignLockTime(500),
		WithToSignSequence(400),
	)
	require.NoError(t, err)

	result := ValidateAuthPkg(
		&AuthPkg{
			Message:          message,
			MessageChallenge: challengeScript,
			Sig:              sig,
		},
		WithCurrentBlockHeight(450),
	)
	require.Equal(t, VerificationStateInvalid, result.State)
	require.Contains(t, result.Reason, "valid-until block")
}

// TestValidateAuthPkgRejectsNilValidationOption asserts nil validation
// options are rejected as invalid input.
func TestValidateAuthPkgRejectsNilValidationOption(t *testing.T) {
	t.Parallel()

	result := ValidateAuthPkg(
		&AuthPkg{
			Message:          []byte("x"),
			MessageChallenge: []byte{txscript.OP_TRUE},
			Sig: &Sig{
				ToSign: wire.NewMsgTx(0),
			},
		},
		nil,
	)
	require.Equal(t, VerificationStateInvalid, result.State)
}

// buildWindowedAuthPkg creates and signs a package with the requested
// block validity window for tests that only need one signing input.
func buildWindowedAuthPkg(t *testing.T, message []byte,
	window BlockWindow) *AuthPkg {

	t.Helper()

	privateKey, challengeScript := testTaprootChallenge(t)

	sig, err := BuildAndSignFullTx(
		message, challengeScript,
		&taprootTxSigner{privateKey: privateKey},
		WithBlockWindow(window),
	)
	require.NoError(t, err)

	return &AuthPkg{
		Message:          message,
		MessageChallenge: challengeScript,
		Sig:              sig,
	}
}

// testTaprootChallenge returns a fresh private key and matching key-path
// taproot script that can be used for end-to-end BIP-322 signing tests.
func testTaprootChallenge(t *testing.T) (*btcec.PrivateKey, []byte) {
	t.Helper()

	privateKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	taprootKey := txscript.ComputeTaprootKeyNoScript(
		privateKey.PubKey(),
	)
	challengeScript, err := txscript.PayToTaprootScript(taprootKey)
	require.NoError(t, err)

	return privateKey, challengeScript
}

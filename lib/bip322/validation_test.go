package bip322

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// TestValidateAuthPkgVectorSignature asserts the BIP-322 witness vector
// validates when carried in full-format signature container.
func TestValidateAuthPkgVectorSignature(t *testing.T) {
	t.Parallel()

	challengeScript, sig := buildHelloWorldVectorFullSig(t)

	result := ValidateAuthPkg(&AuthPkg{
		Message:          []byte("Hello World"),
		MessageChallenge: challengeScript,
		Sig:              sig,
	})

	require.Equal(t, VerificationStateValid, result.State)
	require.Equal(t, uint32(0), result.ValidAtTime)
	require.Equal(t, uint32(0), result.ValidAtAge)
}

// TestValidateAuthPkgVectorRejectsMutation asserts signature corruption is
// classified as invalid.
func TestValidateAuthPkgVectorRejectsMutation(t *testing.T) {
	t.Parallel()

	challengeScript, sig := buildHelloWorldVectorFullSig(t)

	mutated := &Sig{
		ToSign: sig.ToSign.Copy(),
	}
	require.NotEmpty(t, mutated.ToSign.TxIn[0].Witness)
	require.NotEmpty(t, mutated.ToSign.TxIn[0].Witness[0])
	mutated.ToSign.TxIn[0].Witness[0][10] ^= 0x01

	result := ValidateAuthPkg(&AuthPkg{
		Message:          []byte("Hello World"),
		MessageChallenge: challengeScript,
		Sig:              mutated,
	})

	require.Equal(t, VerificationStateInvalid, result.State)
}

// TestValidateAuthPkgRejectsMissingChallenge asserts empty challenge scripts
// are rejected as invalid input.
func TestValidateAuthPkgRejectsMissingChallenge(t *testing.T) {
	t.Parallel()

	result := ValidateAuthPkg(&AuthPkg{
		Message: []byte("abc"),
		Sig: &Sig{
			ToSign: wire.NewMsgTx(0),
		},
	})

	require.Equal(t, VerificationStateInvalid, result.State)
}

// TestValidateAuthPkgInconclusiveVersion asserts unsupported to_sign versions
// are inconclusive per BIP-322 upgradeable rules.
func TestValidateAuthPkgInconclusiveVersion(t *testing.T) {
	t.Parallel()

	message := []byte("version check")
	messageHash := MessageHash(message)

	toSpend, err := BuildToSpend(messageHash, []byte{txscript.OP_TRUE})
	require.NoError(t, err)

	toSign, err := BuildToSignTx(toSpend)
	require.NoError(t, err)
	toSign.Version = 1

	result := ValidateAuthPkg(&AuthPkg{
		Message:          message,
		MessageChallenge: []byte{txscript.OP_TRUE},
		Sig: &Sig{
			ToSign: toSign,
		},
	})

	require.Equal(t, VerificationStateInconclusive, result.State)
}

// TestValidateAuthPkgProofOfFundsNeedsPrevOuts asserts missing UTXO data for
// proof inputs results in inconclusive.
func TestValidateAuthPkgProofOfFundsNeedsPrevOuts(t *testing.T) {
	t.Parallel()

	message := []byte("proof of funds missing prevouts")
	messageHash := MessageHash(message)

	challengeScript := []byte{txscript.OP_TRUE}
	toSpend, err := BuildToSpend(messageHash, challengeScript)
	require.NoError(t, err)

	additionalOutPoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0xaa, 0xbb, 0xcc, 0xdd,
		},
		Index: 1,
	}

	toSign, err := BuildToSignTx(
		toSpend,
		WithToSignAdditionalInputs(
			AdditionalInput{
				PreviousOutPoint: additionalOutPoint,
			},
		),
	)
	require.NoError(t, err)

	result := ValidateAuthPkg(&AuthPkg{
		Message:          message,
		MessageChallenge: challengeScript,
		Sig: &Sig{
			ToSign: toSign,
		},
	})

	require.Equal(t, VerificationStateInconclusive, result.State)
}

// TestValidateAuthPkgProofOfFundsValid asserts proof-of-funds signatures can
// validate when prevout metadata is provided.
func TestValidateAuthPkgProofOfFundsValid(t *testing.T) {
	t.Parallel()

	message := []byte("proof of funds valid")
	messageHash := MessageHash(message)

	challengeScript := []byte{txscript.OP_TRUE}
	toSpend, err := BuildToSpend(messageHash, challengeScript)
	require.NoError(t, err)

	additionalOutPoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0x10, 0x20, 0x30, 0x40,
		},
		Index: 9,
	}

	toSign, err := BuildToSignTx(
		toSpend,
		WithToSignLockTime(321),
		WithToSignSequence(654),
		WithToSignAdditionalInputs(
			AdditionalInput{
				PreviousOutPoint: additionalOutPoint,
				Sequence:         33,
			},
		),
	)
	require.NoError(t, err)

	result := ValidateAuthPkg(&AuthPkg{
		Message:          message,
		MessageChallenge: challengeScript,
		Sig: &Sig{
			ToSign: toSign,
		},
		ProofPrevOutputs: map[wire.OutPoint]*wire.TxOut{
			additionalOutPoint: {
				Value:    1000,
				PkScript: []byte{txscript.OP_TRUE},
			},
		},
	})

	require.Equal(t, VerificationStateValid, result.State)
	require.Equal(t, uint32(321), result.ValidAtTime)
	require.Equal(t, uint32(654), result.ValidAtAge)
}

// buildHelloWorldVectorFullSig builds a full-format signature that uses the
// published BIP-322 Hello World witness vector.
func buildHelloWorldVectorFullSig(t *testing.T) ([]byte, *Sig) {
	t.Helper()

	challengeScript, err := hex.DecodeString(
		"00142b05d564e6a7a33c087f16e0f730d1440123799d",
	)
	require.NoError(t, err)

	witnessSigHex := "304402206517c8637a7bfc3a154edcba6196d64bbd5b73" +
		"955cb7da7d" +
		"1626bcdde466c364022022bf10d19fc0bb69b4596e306b362acaa" +
		"835293cf6" +
		"93bb176f7324b531f5afec01"

	witnessSig, err := hex.DecodeString(
		witnessSigHex,
	)
	require.NoError(t, err)

	witnessPub, err := hex.DecodeString(
		"02c7f12003196442943d8588e01aee840423cc54fc1521526a3b85c2" +
			"b0cbd58872",
	)
	require.NoError(t, err)

	messageHash := MessageHash([]byte("Hello World"))
	toSpend, err := BuildToSpend(messageHash, challengeScript)
	require.NoError(t, err)

	toSign, err := BuildToSignTx(
		toSpend,
		WithToSignWitness(
			wire.TxWitness{
				witnessSig,
				witnessPub,
			},
		),
	)
	require.NoError(t, err)

	return challengeScript, &Sig{
		ToSign: toSign,
	}
}

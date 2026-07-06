package bip322

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestBuildToSignShapeAndMetadata asserts PSBT construction keeps the
// expected BIP-322 shape and input metadata.
func TestBuildToSignShapeAndMetadata(t *testing.T) {
	t.Parallel()

	messageHash := MessageHash([]byte("psbt build"))

	challengeScript := []byte{txscript.OP_TRUE}
	toSpend, err := BuildToSpend(messageHash, challengeScript)
	require.NoError(t, err)

	additionalOutPoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0x10, 0x11, 0x12, 0x13,
		},
		Index: 3,
	}
	additionalPrevOut := wire.TxOut{
		Value: 777,
		PkScript: []byte{
			txscript.OP_TRUE,
		},
	}

	packet, err := BuildToSign(
		toSpend,
		WithToSignAdditionalInputs(
			AdditionalInput{
				PreviousOutPoint: additionalOutPoint,
				Sequence:         42,
				WitnessUtxo:      &additionalPrevOut,
			},
		),
	)
	require.NoError(t, err)
	require.NotNil(t, packet)
	require.NotNil(t, packet.UnsignedTx)
	require.Len(t, packet.UnsignedTx.TxIn, 2)
	require.Len(t, packet.UnsignedTx.TxOut, 1)
	require.Equal(
		t, toSpend.TxHash(),
		packet.UnsignedTx.TxIn[0].PreviousOutPoint.Hash,
	)
	require.Equal(
		t, uint32(0), packet.UnsignedTx.TxIn[0].PreviousOutPoint.Index,
	)
	require.Equal(
		t, additionalOutPoint,
		packet.UnsignedTx.TxIn[1].PreviousOutPoint,
	)
	require.Equal(
		t, []byte{txscript.OP_RETURN},
		packet.UnsignedTx.TxOut[0].PkScript,
	)

	require.Len(t, packet.Inputs, 2)
	require.NotNil(t, packet.Inputs[0].WitnessUtxo)
	require.Equal(
		t, challengeScript, packet.Inputs[0].WitnessUtxo.PkScript,
	)
	require.Equal(
		t, txscript.SigHashAll, packet.Inputs[0].SighashType,
	)
	require.NotNil(t, packet.Inputs[1].WitnessUtxo)
	require.Equal(
		t, additionalPrevOut.PkScript,
		packet.Inputs[1].WitnessUtxo.PkScript,
	)
	require.Equal(
		t, additionalPrevOut.Value, packet.Inputs[1].WitnessUtxo.Value,
	)
	require.Equal(
		t, txscript.SigHashAll, packet.Inputs[1].SighashType,
	)
}

// TestBuildToSignUsesTaprootAwareSighashType asserts PSBT sighash metadata is
// selected from each input's script type.
func TestBuildToSignUsesTaprootAwareSighashType(t *testing.T) {
	t.Parallel()

	messageHash := MessageHash(
		[]byte("psbt taproot-aware sighash metadata"),
	)

	privateKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	taprootKey := txscript.ComputeTaprootKeyNoScript(privateKey.PubKey())
	taprootScript, err := txscript.PayToTaprootScript(taprootKey)
	require.NoError(t, err)

	toSpend, err := BuildToSpend(messageHash, taprootScript)
	require.NoError(t, err)

	nonTaprootScript, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_0).
		AddData(make([]byte, 20)).
		Script()
	require.NoError(t, err)

	packet, err := BuildToSign(
		toSpend,
		WithToSignAdditionalInputs(
			AdditionalInput{
				PreviousOutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{0x11},
					Index: 5,
				},
				Sequence: 10,
				WitnessUtxo: &wire.TxOut{
					Value:    1_000,
					PkScript: nonTaprootScript,
				},
			}, AdditionalInput{
				PreviousOutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{0x22},
					Index: 6,
				},
				Sequence: 11,
				WitnessUtxo: &wire.TxOut{
					Value:    2_000,
					PkScript: taprootScript,
				},
			},
		),
	)
	require.NoError(t, err)
	require.Len(t, packet.Inputs, 3)
	require.Equal(
		t, txscript.SigHashDefault, packet.Inputs[0].SighashType,
	)
	require.Equal(
		t, txscript.SigHashAll, packet.Inputs[1].SighashType,
	)
	require.Equal(
		t, txscript.SigHashDefault, packet.Inputs[2].SighashType,
	)
}

// TestBuildToSignRejectsInvalidAdditionalInput asserts invalid additional
// input metadata is rejected before PSBT creation.
func TestBuildToSignRejectsInvalidAdditionalInput(t *testing.T) {
	t.Parallel()

	messageHash := MessageHash([]byte("psbt invalid additional"))

	toSpend, err := BuildToSpend(messageHash, []byte{txscript.OP_TRUE})
	require.NoError(t, err)

	_, err = BuildToSign(
		toSpend,
		WithToSignAdditionalInputs(
			AdditionalInput{
				PreviousOutPoint: wire.OutPoint{
					Hash: chainhash.Hash{
						0x55, 0x66, 0x77, 0x88,
					},
					Index: 9,
				},
			},
		),
	)
	require.Error(t, err)
}

// TestFinalizeToSignPSBTRejectsIncomplete asserts extracting a signature from
// an unsigned packet fails.
func TestFinalizeToSignPSBTRejectsIncomplete(t *testing.T) {
	t.Parallel()

	messageHash := MessageHash([]byte("psbt incomplete"))

	toSpend, err := BuildToSpend(messageHash, []byte{txscript.OP_TRUE})
	require.NoError(t, err)

	packet, err := BuildToSign(toSpend)
	require.NoError(t, err)

	_, err = FinalizeToSignPSBT(packet)
	require.Error(t, err)
}

// TestBuildAndSignFullTxTaprootProofOfFunds asserts the transaction-signer
// helper can build and validate proof-of-funds signatures.
func TestBuildAndSignFullTxTaprootProofOfFunds(t *testing.T) {
	t.Parallel()

	privateKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	taprootKey := txscript.ComputeTaprootKeyNoScript(privateKey.PubKey())
	challengeScript, err := txscript.PayToTaprootScript(taprootKey)
	require.NoError(t, err)

	additionalOutPoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0x08, 0x07, 0x06, 0x05,
		},
		Index: 2,
	}
	additionalPrevOut := wire.TxOut{
		Value:    21_000,
		PkScript: challengeScript,
	}

	message := []byte("tx signer bip322 signing")
	sig, err := BuildAndSignFullTx(
		message, challengeScript, &taprootTxSigner{
			privateKey: privateKey,
		},
		WithToSignVersion(2),
		WithToSignLockTime(100),
		WithToSignSequence(200),
		WithToSignAdditionalInputs(
			AdditionalInput{
				PreviousOutPoint: additionalOutPoint,
				Sequence:         44,
				WitnessUtxo:      &additionalPrevOut,
			},
		),
	)
	require.NoError(t, err)
	require.NotNil(t, sig)
	require.NotNil(t, sig.ToSign)
	require.Len(t, sig.ToSign.TxIn, 2)

	result := ValidateAuthPkg(&AuthPkg{
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
	})
	require.Equal(t, VerificationStateValid, result.State)
	require.Equal(t, uint32(100), result.ValidAtTime)
	require.Equal(t, uint32(200), result.ValidAtAge)
}

// TestBuildAndSignFullTxRejectsMissingSigner asserts the tx-signer helper
// requires a signer implementation.
func TestBuildAndSignFullTxRejectsMissingSigner(t *testing.T) {
	t.Parallel()

	_, err := BuildAndSignFullTx(
		[]byte("message"), []byte{txscript.OP_TRUE}, nil,
	)
	require.Error(t, err)
}

// TestBuildAndSignFullTxRejectsMissingWitnessUtxo asserts the tx-signer helper
// rejects additional inputs without witness utxos.
func TestBuildAndSignFullTxRejectsMissingWitnessUtxo(t *testing.T) {
	t.Parallel()

	privateKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	_, err = BuildAndSignFullTx(
		[]byte("missing witness utxo"), []byte{txscript.OP_TRUE},
		&taprootTxSigner{
			privateKey: privateKey,
		},
		WithToSignAdditionalInputs(
			AdditionalInput{
				PreviousOutPoint: wire.OutPoint{
					Hash: chainhash.Hash{0x01},
				},
			},
		),
	)
	require.Error(t, err)
}

// taprootTxSigner signs all to_sign inputs using key-path taproot signatures.
type taprootTxSigner struct {
	privateKey *btcec.PrivateKey
}

// SignBIP322 implements TxSigner for key-path taproot signatures.
func (s *taprootTxSigner) SignBIP322(_ *wire.MsgTx, toSign *wire.MsgTx,
	prevFetcher txscript.PrevOutputFetcher,
	hashCache *txscript.TxSigHashes) error {

	if s.privateKey == nil {
		return fmt.Errorf("private key must be provided")
	}

	for inputIndex := 0; inputIndex < len(toSign.TxIn); inputIndex++ {
		prevOut := prevFetcher.FetchPrevOutput(
			toSign.TxIn[inputIndex].PreviousOutPoint,
		)
		if prevOut == nil {
			return fmt.Errorf("missing prevout for input %d",
				inputIndex)
		}

		signature, err := txscript.RawTxInTaprootSignature(
			toSign, hashCache, inputIndex, prevOut.Value,
			prevOut.PkScript, []byte{}, txscript.SigHashAll,
			s.privateKey,
		)
		if err != nil {
			return fmt.Errorf("sign input %d: %w", inputIndex, err)
		}

		toSign.TxIn[inputIndex].Witness = wire.TxWitness{
			signature,
		}
	}

	return nil
}

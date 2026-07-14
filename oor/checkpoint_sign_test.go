package oor

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/stretchr/testify/require"
)

// TestAttachExternalTaprootScriptSignaturesRejectsUnexpectedPubKey verifies an
// external signature must come from a key required by the custom spend path.
func TestAttachExternalTaprootScriptSignaturesRejectsUnexpectedPubKey(
	t *testing.T) {

	t.Parallel()

	_, requiredKey := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x01}, 32))
	_, unexpectedKey := btcec.PrivKeyFromBytes(
		bytes.Repeat(
			[]byte{0x02}, 32,
		),
	)
	witnessScript := []byte{0x51}

	input := &TransferInput{
		CustomSpend: &arkscript.SpendPath{
			SpendInfo: &arkscript.SpendInfo{
				WitnessScript: witnessScript,
			},
		},
		CustomSpendKeys: []*btcec.PublicKey{
			requiredKey,
		},
		ExternalSignatures: []ExternalTaprootScriptSignature{
			{
				PubKey:        unexpectedKey,
				WitnessScript: witnessScript,
				Signature: []byte{
					0x01,
				},
			},
		},
	}

	err := attachExternalTaprootScriptSignatures(input, &psbt.PInput{})
	require.ErrorContains(t, err, "pubkey is not required")
}

// TestAssembleCustomFinalWitnessOrdersSignatures locks down the final witness
// layout for custom-spend checkpoints that use more than two signing keys. The
// tapscript multisig helper consumes signatures in reverse key order, so a
// forward-ordered witness would fail script evaluation even if every signature
// is individually present and valid. The test builds a three-key custom spend,
// attaches distinct PSBT signature records for each key, assembles the final
// witness, and verifies the stack is [key3 sig, key2 sig, key1 sig,
// conditions..., witness script, control block].
func TestAssembleCustomFinalWitnessOrdersSignatures(t *testing.T) {
	t.Parallel()

	_, key1 := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x01}, 32))
	_, key2 := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x02}, 32))
	_, key3 := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x03}, 32))

	witnessScript := []byte{0x51}
	controlBlock := []byte{0xc0}
	conditions := [][]byte{
		{
			0xaa,
		},
		{
			0xbb,
		},
	}
	input := &TransferInput{
		CustomSpend: &arkscript.SpendPath{
			SpendInfo: &arkscript.SpendInfo{
				WitnessScript: witnessScript,
				ControlBlock:  controlBlock,
			},
			Conditions: conditions,
		},
		CustomSpendKeys: []*btcec.PublicKey{
			key1,
			key2,
			key3,
		},
	}

	signatures := [][]byte{
		{
			0x11,
		},
		{
			0x22,
		},
		{
			0x33,
		},
	}
	pIn := &psbt.PInput{}
	for i, key := range input.CustomSpendKeys {
		err := psbtutil.AddTaprootScriptSpendSig(
			pIn, key, witnessScript, signatures[i],
			txscript.SigHashDefault,
		)
		require.NoError(t, err)
	}

	err := assembleCustomFinalWitness(input, pIn)
	require.NoError(t, err)

	witness := decodeFinalScriptWitness(t, pIn.FinalScriptWitness)
	require.Equal(t, wire.TxWitness{
		signatures[2],
		signatures[1],
		signatures[0],
		conditions[0],
		conditions[1],
		witnessScript,
		controlBlock,
	}, witness)
}

func decodeFinalScriptWitness(t *testing.T, raw []byte) wire.TxWitness {
	t.Helper()

	reader := bytes.NewReader(raw)
	count, err := wire.ReadVarInt(reader, 0)
	require.NoError(t, err)

	witness := make(wire.TxWitness, count)
	for i := uint64(0); i < count; i++ {
		item, err := wire.ReadVarBytes(
			reader, 0, txscript.MaxScriptSize, "witness",
		)
		require.NoError(t, err)

		witness[i] = item
	}
	require.Zero(t, reader.Len())

	return witness
}

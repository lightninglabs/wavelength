package oor

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// newSignedPackage builds a checkpoint + Ark PSBT pair for signed-validation
// tests. It spends a single VTXO with the given pkScript and owner leaf script
// under a fresh operator policy, mirroring the production builders. The owner
// leaf is attached to the Ark input only when attachLeaf is set, allowing
// callers to exercise the missing-leaf rejection path.
func newSignedPackage(t *testing.T, prevByte byte, vtxoScript []byte,
	ownerLeafScript []byte, attachLeaf bool) (*psbt.Packet, *psbt.Packet) {

	t.Helper()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	checkpointRes, err := BuildCheckpointPSBT(policy, CheckpointInput{
		SpentVTXO: SpentVTXORef{
			Outpoint: wire.OutPoint{
				Hash:  [32]byte{prevByte},
				Index: 0,
			},
			Output: &wire.TxOut{
				Value:    5000,
				PkScript: vtxoScript,
			},
		},
		OwnerLeafScript: ownerLeafScript,
	})
	require.NoError(t, err)

	arkPSBT, err := BuildArkPSBT([]CheckpointOutput{{
		Txid:           checkpointRes.PSBT.UnsignedTx.TxHash(),
		Output:         checkpointRes.PSBT.UnsignedTx.TxOut[0],
		TapTreeEncoded: checkpointRes.TapTreeEncoded,
	}}, []RecipientOutput{{
		PkScript: randomP2TRScript(t),
		Value:    btcutil.Amount(5000),
	}})
	require.NoError(t, err)

	if attachLeaf {
		leaf, err := BuildTaprootTapLeafScript(
			checkpointRes.TapTreeEncoded, ownerLeafScript,
		)
		require.NoError(t, err)
		arkPSBT.Inputs[0].TaprootLeafScript =
			[]*psbt.TaprootTapLeafScript{leaf}
	}

	return checkpointRes.PSBT, arkPSBT
}

// TestValidateSubmitPackageSignedHappyPath asserts a signed submit package
// with valid tapscript data passes full validation.
func TestValidateSubmitPackageSignedHappyPath(t *testing.T) {
	t.Parallel()

	checkpoint, arkPSBT := newSignedPackage(
		t, 1, randomP2TRScript(t), []byte{txscript.OP_TRUE}, true,
	)

	_, err := ValidateSubmitPackageSigned(
		arkPSBT, []*psbt.Packet{checkpoint},
	)
	require.NoError(t, err)
}

// TestValidateSubmitPackageSignedRejectsBadControlBlock asserts a tampered
// control block fails full validation.
func TestValidateSubmitPackageSignedRejectsBadControlBlock(t *testing.T) {
	t.Parallel()

	checkpoint, arkPSBT := newSignedPackage(
		t, 2, randomP2TRScript(t), []byte{txscript.OP_TRUE}, true,
	)
	arkPSBT.Inputs[0].TaprootLeafScript[0].ControlBlock[0] ^= 0x01

	_, err := ValidateSubmitPackageSigned(
		arkPSBT, []*psbt.Packet{checkpoint},
	)
	require.Error(t, err)
}

// TestValidateFinalizePackageSignedWithFinalWitness asserts finalize signed
// validation accepts checkpoint inputs when FinalScriptWitness is present and
// spendable.
func TestValidateFinalizePackageSignedWithFinalWitness(t *testing.T) {
	t.Parallel()

	checkpoint, arkPSBT := newSignedPackage(
		t, 3, p2WSHTrueScript(), []byte{txscript.OP_TRUE}, false,
	)

	finalWitness, err := encodeFinalWitness(
		wire.TxWitness{[]byte{txscript.OP_TRUE}},
	)
	require.NoError(t, err)
	checkpoint.Inputs[0].FinalScriptWitness = finalWitness
	checkpoint.Inputs[0].Unknowns = nil

	err = ValidateFinalizePackageSigned(
		arkPSBT, []*psbt.Packet{checkpoint},
	)
	require.NoError(t, err)
}

// TestValidateSubmitSignedRejectsMissingArkInputLeafScript asserts submit
// signed validation still requires explicit Ark-input tapleaf metadata for a
// script spend.
func TestValidateSubmitSignedRejectsMissingArkInputLeafScript(t *testing.T) {
	t.Parallel()

	checkpoint, arkPSBT := newSignedPackage(
		t, 5, randomP2TRScript(t), []byte{txscript.OP_TRUE}, false,
	)

	_, err := ValidateSubmitPackageSigned(
		arkPSBT, []*psbt.Packet{checkpoint},
	)
	require.ErrorContains(
		t, err, "missing taproot signature or leaf script",
	)
}

// TestBuildTaprootWitnessIncludesConditionWitness asserts Ark condition
// witness metadata is inserted between script-spend signatures and the leaf
// script when reconstructing a tapscript witness.
func TestBuildTaprootWitnessIncludesConditionWitness(t *testing.T) {
	t.Parallel()

	receiverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	leafScript, err := (&arkscript.Multisig{
		Keys: []*btcec.PublicKey{
			receiverKey.PubKey(),
			serverKey.PubKey(),
		},
	}).Script()
	require.NoError(t, err)

	controlBlock := bytes.Repeat([]byte{0x01}, 33)
	leafHash := txscript.NewBaseTapLeaf(leafScript).TapHash()
	conditionA := bytes.Repeat([]byte{0x02}, 32)
	conditionB := bytes.Repeat([]byte{0x03}, 16)
	receiverSig := bytes.Repeat([]byte{0x04}, 64)
	serverSig := bytes.Repeat([]byte{0x06}, 64)

	pIn := psbt.PInput{
		TaprootScriptSpendSig: []*psbt.TaprootScriptSpendSig{
			{
				XOnlyPubKey: schnorr.SerializePubKey(
					receiverKey.PubKey(),
				),
				LeafHash:  leafHash[:],
				Signature: receiverSig,
			},
			{
				XOnlyPubKey: schnorr.SerializePubKey(
					serverKey.PubKey(),
				),
				LeafHash:  leafHash[:],
				Signature: serverSig,
			},
		},
		TaprootLeafScript: []*psbt.TaprootTapLeafScript{{
			ControlBlock: controlBlock,
			Script:       leafScript,
			LeafVersion:  txscript.BaseLeafVersion,
		}},
	}

	pkt, err := psbt.NewFromUnsignedTx(wire.NewMsgTx(2))
	require.NoError(t, err)
	pkt.Inputs = []psbt.PInput{pIn}

	err = arkscript.PutConditionWitnessPSBTInput(
		pkt, 0, [][]byte{conditionA, conditionB},
	)
	require.NoError(t, err)

	witness, err := buildTaprootWitness(pkt.Inputs[0])
	require.NoError(t, err)
	require.Len(t, witness, 6)
	require.Equal(t, serverSig, witness[0])
	require.Equal(t, receiverSig, witness[1])
	require.Equal(t, conditionA, witness[2])
	require.Equal(t, conditionB, witness[3])
	require.Equal(t, leafScript, witness[4])
	require.Equal(t, controlBlock, witness[5])
}

// TestOrderTaprootScriptSpendSignaturesSupportsMissingOptionalSignatures
// verifies optional checksig positions are preserved as empty witness items
// and that preceding non-data opcodes do not hide the last pushed pubkey.
func TestOrderTaprootScriptSpendSignaturesSupportsMissingOptionalSignatures(
	t *testing.T) {

	t.Parallel()

	keyA, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	keyB, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	keyC, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	leafScript, err := txscript.NewScriptBuilder().
		AddData(schnorr.SerializePubKey(keyA.PubKey())).
		AddOp(txscript.OP_DUP).
		AddOp(txscript.OP_CHECKSIGVERIFY).
		AddData(schnorr.SerializePubKey(keyB.PubKey())).
		AddOp(txscript.OP_CHECKSIG).
		AddData(schnorr.SerializePubKey(keyC.PubKey())).
		AddOp(txscript.OP_CHECKSIG).
		AddOp(txscript.OP_BOOLOR).
		Script()
	require.NoError(t, err)

	sigA := bytes.Repeat([]byte{0x0a}, 64)
	sigC := bytes.Repeat([]byte{0x0c}, 64)

	ordered, err := orderTaprootScriptSpendSignatures(
		[]*psbt.TaprootScriptSpendSig{
			{
				XOnlyPubKey: schnorr.SerializePubKey(
					keyA.PubKey(),
				),
				Signature: sigA,
			},
			{
				XOnlyPubKey: schnorr.SerializePubKey(
					keyC.PubKey(),
				),
				Signature: sigC,
			},
		},
		leafScript,
	)
	require.NoError(t, err)
	require.Len(t, ordered, 3)
	require.Equal(t, sigC, ordered[0])
	require.Empty(t, ordered[1])
	require.Equal(t, sigA, ordered[2])
}

func p2WSHTrueScript() []byte {
	scriptHash := sha256.Sum256([]byte{txscript.OP_TRUE})

	return append([]byte{txscript.OP_0, 0x20}, scriptHash[:]...)
}

func encodeFinalWitness(wit wire.TxWitness) ([]byte, error) {
	var b bytes.Buffer

	err := wire.WriteVarInt(&b, 0, uint64(len(wit)))
	if err != nil {
		return nil, err
	}

	for i := range wit {
		err = wire.WriteVarBytes(&b, 0, wit[i])
		if err != nil {
			return nil, err
		}
	}

	return b.Bytes(), nil
}

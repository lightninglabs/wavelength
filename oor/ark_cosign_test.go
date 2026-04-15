package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oorlib "github.com/lightninglabs/darepo-client/lib/tx/oor"
	clientoor "github.com/lightninglabs/darepo-client/oor"
	clientvtxo "github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestTapLeafScriptPushesPubKey verifies that collaborative-leaf detection
// only matches pubkeys that are actually pushed by the script.
func TestTapLeafScriptPushesPubKey(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKeyBytes := schnorr.SerializePubKey(operatorKey.PubKey())
	pushedScript, err := txscript.NewScriptBuilder().
		AddData(pubKeyBytes).
		AddOp(txscript.OP_CHECKSIG).
		Script()
	require.NoError(t, err)

	require.True(t, tapLeafScriptPushesPubKey(
		pushedScript, operatorKey.PubKey(),
	))

	rawByteScript := append([]byte{txscript.OP_TRUE}, pubKeyBytes...)
	require.False(t, tapLeafScriptPushesPubKey(
		rawByteScript, operatorKey.PubKey(),
	))
}

func TestCoSignArkPSBTCompletesCollabWitness(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	ownerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	exitDelay := uint32(10)
	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    exitDelay,
	}

	vtxoTapScript, err := arkscript.VTXOTapScript(
		ownerKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	vtxoTapKey, err := arkscript.VTXOTapKey(
		ownerKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	vtxoPkScript, err := txscript.PayToTaprootScript(vtxoTapKey)
	require.NoError(t, err)

	collabLeaf, err := arkscript.MultiSigCollabTapLeaf(
		ownerKey.PubKey(), operatorKey.PubKey(),
	)
	require.NoError(t, err)

	outpoint := wire.OutPoint{
		Hash:  [32]byte{1},
		Index: 9,
	}

	transferInput := clientoor.TransferInput{
		VTXO: &clientvtxo.Descriptor{
			Outpoint: outpoint,
			Amount:   btcutil.Amount(testVTXOValue),
			PkScript: vtxoPkScript,
			ClientKey: keychain.KeyDescriptor{
				PubKey: ownerKey.PubKey(),
			},
			OperatorKey:    operatorKey.PubKey(),
			TapScript:      vtxoTapScript,
			RelativeExpiry: exitDelay,
			Status:         clientvtxo.VTXOStatusLive,
		},
		OwnerLeafScript: collabLeaf.Script,
	}

	checkpointRes, err := oorlib.BuildCheckpointPSBT(
		policy, oorlib.CheckpointInput{
			SpentVTXO: oorlib.SpentVTXORef{
				Outpoint: outpoint,
				Output: &wire.TxOut{
					Value:    testVTXOValue,
					PkScript: vtxoPkScript,
				},
			},
			OwnerLeafScript: collabLeaf.Script,
		},
	)
	require.NoError(t, err)

	arkPSBT, err := oorlib.BuildArkPSBT([]oorlib.CheckpointOutput{{
		Txid:           checkpointRes.PSBT.UnsignedTx.TxHash(),
		Output:         checkpointRes.PSBT.UnsignedTx.TxOut[0],
		TapTreeEncoded: checkpointRes.TapTreeEncoded,
	}}, []oorlib.RecipientOutput{{
		PkScript: randomP2TRScript(t),
		Value:    btcutil.Amount(testVTXOValue),
	}})
	require.NoError(t, err)

	leaf, err := oorlib.BuildTaprootTapLeafScript(
		checkpointRes.TapTreeEncoded, collabLeaf.Script,
	)
	require.NoError(t, err)
	arkPSBT.Inputs[0].TaprootLeafScript =
		[]*psbt.TaprootTapLeafScript{leaf}

	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{ownerKey}, nil,
	)
	err = clientoor.SignArkPSBT(
		clientSigner, arkPSBT, []*psbt.Packet{checkpointRes.PSBT},
		[]clientoor.TransferInput{transferInput},
	)
	require.NoError(t, err)
	require.Len(t, arkPSBT.Inputs[0].TaprootScriptSpendSig, 1)

	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)
	signed, err := CoSignArkPSBT(operatorSigner, keychain.KeyDescriptor{
		PubKey: operatorKey.PubKey(),
	}, arkPSBT)
	require.NoError(t, err)
	require.True(t, signed)

	require.Len(t, arkPSBT.Inputs[0].TaprootScriptSpendSig, 2)

	_, err = oorlib.ValidateSubmitPackageSigned(
		arkPSBT, []*psbt.Packet{checkpointRes.PSBT},
	)
	require.NoError(t, err)
}

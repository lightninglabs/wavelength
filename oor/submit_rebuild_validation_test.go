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
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/stretchr/testify/require"
)

// TestValidateSubmitRebuildAndPolicyHappyPath exercises a fully valid rebuild
// path that matches the reconstructed checkpoint and Ark transactions.
func TestValidateSubmitRebuildAndPolicyHappyPath(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	ownerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	exitDelay := uint32(10)
	vtxoTapKey, err := arkscript.VTXOTapKey(
		ownerKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	vtxoPkScript, err := txscript.PayToTaprootScript(vtxoTapKey)
	require.NoError(t, err)

	outpoint := wire.OutPoint{
		Hash:  [32]byte{0x01},
		Index: 0,
	}

	store := vtxo.NewInMemoryStore()
	err = store.Create(ctx, &vtxo.Record{
		Outpoint: outpoint,
		Value:    int64(10000),
		PkScript: vtxoPkScript,
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    exitDelay,
	}

	ownerLeaf, ownerLeafPolicy := rebuildTestOwnerLeaf(
		t, ownerKey.PubKey(), operatorKey.PubKey(),
	)
	checkpointRes, err := oortx.BuildCheckpointPSBT(
		arkscript.CheckpointPolicy{
			OperatorKey: policy.OperatorKey,
			CSVDelay:    policy.CSVDelay,
		}, oortx.CheckpointInput{
			SpentVTXO: oortx.SpentVTXORef{
				Outpoint: outpoint,
				Output: &wire.TxOut{
					Value:    10000,
					PkScript: vtxoPkScript,
				},
			},
			OwnerLeafScript: ownerLeaf,
			OwnerLeafPolicy: ownerLeafPolicy,
		},
	)
	require.NoError(t, err)

	arkPSBT, err := oortx.BuildArkPSBT([]oortx.CheckpointOutput{{
		Txid:            checkpointRes.PSBT.UnsignedTx.TxHash(),
		Output:          checkpointRes.PSBT.UnsignedTx.TxOut[0],
		TapTreeEncoded:  checkpointRes.TapTreeEncoded,
		OwnerLeafPolicy: checkpointRes.OwnerLeafPolicy,
	}}, []oortx.RecipientOutput{{
		PkScript: vtxoPkScript,
		Value:    btcutil.Amount(10000),
	}})
	require.NoError(t, err)

	leaf, err := oortx.BuildTaprootTapLeafScript(
		checkpointRes.TapTreeEncoded, ownerLeaf,
	)
	require.NoError(t, err)
	arkPSBT.Inputs[0].TaprootLeafScript =
		[]*psbt.TaprootTapLeafScript{leaf}

	// Attach a dummy non-operator signature marker.
	leafHash := txscript.NewBaseTapLeaf(ownerLeaf).TapHash()
	arkPSBT.Inputs[0].TaprootScriptSpendSig =
		[]*psbt.TaprootScriptSpendSig{{
			XOnlyPubKey: schnorr.SerializePubKey(ownerKey.PubKey()),
			LeafHash:    leafHash[:],
			Signature:   []byte{0x01},
			SigHash:     txscript.SigHashDefault,
		}}

	err = validateSubmitRebuildAndPolicy(
		ctx, arkPSBT, []*psbt.Packet{checkpointRes.PSBT},
		[]VTXOSigningDescriptor{{
			Outpoint: outpoint,
			VTXOPolicyTemplate: rebuildStandardPolicyTemplate(
				t, ownerKey.PubKey(), operatorKey.PubKey(),
				exitDelay,
			),
			SpendPath: rebuildStandardCollabSpendPath(
				t, ownerKey.PubKey(), operatorKey.PubKey(),
				exitDelay,
			),
			OwnerLeafPolicy: ownerLeafPolicy,
		}},
		policy, store,
		SubmitOutputPolicy{},
	)
	require.NoError(t, err)
}

// TestValidateSubmitRebuildAndPolicyRejectsArkMismatch asserts the validator
// rejects Ark packages whose rebuilt txid does not match the submitted Ark.
func TestValidateSubmitRebuildAndPolicyRejectsArkMismatch(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	ownerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	exitDelay := uint32(10)
	vtxoTapKey, err := arkscript.VTXOTapKey(
		ownerKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	vtxoPkScript, err := txscript.PayToTaprootScript(vtxoTapKey)
	require.NoError(t, err)

	outpoint := wire.OutPoint{
		Hash:  [32]byte{0x02},
		Index: 0,
	}

	store := vtxo.NewInMemoryStore()
	err = store.Create(ctx, &vtxo.Record{
		Outpoint: outpoint,
		Value:    int64(10000),
		PkScript: vtxoPkScript,
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    exitDelay,
	}

	ownerLeaf, ownerLeafPolicy := rebuildTestOwnerLeaf(
		t, ownerKey.PubKey(), operatorKey.PubKey(),
	)
	checkpointRes, err := oortx.BuildCheckpointPSBT(
		policy, oortx.CheckpointInput{
			SpentVTXO: oortx.SpentVTXORef{
				Outpoint: outpoint,
				Output: &wire.TxOut{
					Value:    10000,
					PkScript: vtxoPkScript,
				},
			},
			OwnerLeafScript: ownerLeaf,
			OwnerLeafPolicy: ownerLeafPolicy,
		},
	)
	require.NoError(t, err)

	arkPSBT, err := oortx.BuildArkPSBT([]oortx.CheckpointOutput{{
		Txid:            checkpointRes.PSBT.UnsignedTx.TxHash(),
		Output:          checkpointRes.PSBT.UnsignedTx.TxOut[0],
		TapTreeEncoded:  checkpointRes.TapTreeEncoded,
		OwnerLeafPolicy: checkpointRes.OwnerLeafPolicy,
	}}, []oortx.RecipientOutput{{
		PkScript: vtxoPkScript,
		Value:    btcutil.Amount(10000),
	}})
	require.NoError(t, err)

	arkPSBT.UnsignedTx.TxOut[0].Value += 1

	leaf, err := oortx.BuildTaprootTapLeafScript(
		checkpointRes.TapTreeEncoded, ownerLeaf,
	)
	require.NoError(t, err)
	arkPSBT.Inputs[0].TaprootLeafScript =
		[]*psbt.TaprootTapLeafScript{leaf}

	leafHash := txscript.NewBaseTapLeaf(ownerLeaf).TapHash()
	arkPSBT.Inputs[0].TaprootScriptSpendSig =
		[]*psbt.TaprootScriptSpendSig{{
			XOnlyPubKey: schnorr.SerializePubKey(ownerKey.PubKey()),
			LeafHash:    leafHash[:],
			Signature:   []byte{0x01},
			SigHash:     txscript.SigHashDefault,
		}}

	err = validateSubmitRebuildAndPolicy(
		ctx, arkPSBT, []*psbt.Packet{checkpointRes.PSBT},
		[]VTXOSigningDescriptor{{
			Outpoint: outpoint,
			VTXOPolicyTemplate: rebuildStandardPolicyTemplate(
				t, ownerKey.PubKey(), operatorKey.PubKey(),
				exitDelay,
			),
			SpendPath: rebuildStandardCollabSpendPath(
				t, ownerKey.PubKey(), operatorKey.PubKey(),
				exitDelay,
			),
			OwnerLeafPolicy: ownerLeafPolicy,
		}},
		policy, store,
		SubmitOutputPolicy{},
	)
	require.Error(t, err)
}

func rebuildTestOwnerLeaf(t *testing.T, ownerKey,
	operatorKey *btcec.PublicKey) ([]byte, []byte) {

	t.Helper()

	leaf := arkscript.LeafTemplate{
		Node: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{ownerKey, operatorKey},
		},
	}

	script, err := leaf.Script()
	require.NoError(t, err)

	encoded, err := leaf.Encode()
	require.NoError(t, err)

	return script, encoded
}

func rebuildStandardPolicyTemplate(t *testing.T, ownerKey,
	operatorKey *btcec.PublicKey, exitDelay uint32) []byte {

	t.Helper()

	policy, err := arkscript.EncodeStandardVTXOTemplate(
		ownerKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	return policy
}

func rebuildStandardCollabSpendPath(t *testing.T, ownerKey,
	operatorKey *btcec.PublicKey, exitDelay uint32) []byte {

	t.Helper()

	policy, err := arkscript.NewVTXOPolicy(
		ownerKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	info, err := policy.CollabSpendInfo()
	require.NoError(t, err)

	path := &arkscript.SpendPath{
		SpendInfo: info,
	}
	raw, err := path.Encode()
	require.NoError(t, err)

	return raw
}

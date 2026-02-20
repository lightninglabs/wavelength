package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
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
	vtxoTapKey, err := scripts.VTXOTapKey(
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

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    exitDelay,
	}

	ownerLeaf := []byte{txscript.OP_TRUE}
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
		},
	)
	require.NoError(t, err)

	arkPSBT, err := oortx.BuildArkPSBT([]oortx.CheckpointOutput{{
		Txid:           checkpointRes.PSBT.UnsignedTx.TxHash(),
		Output:         checkpointRes.PSBT.UnsignedTx.TxOut[0],
		TapTreeEncoded: checkpointRes.TapTreeEncoded,
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
			Outpoint:  outpoint,
			OwnerKey:  ownerKey.PubKey(),
			ExitDelay: exitDelay,
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
	vtxoTapKey, err := scripts.VTXOTapKey(
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

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    exitDelay,
	}

	ownerLeaf := []byte{txscript.OP_TRUE}
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
		},
	)
	require.NoError(t, err)

	arkPSBT, err := oortx.BuildArkPSBT([]oortx.CheckpointOutput{{
		Txid:           checkpointRes.PSBT.UnsignedTx.TxHash(),
		Output:         checkpointRes.PSBT.UnsignedTx.TxOut[0],
		TapTreeEncoded: checkpointRes.TapTreeEncoded,
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
			Outpoint:  outpoint,
			OwnerKey:  ownerKey.PubKey(),
			ExitDelay: exitDelay,
		}},
		policy, store,
		SubmitOutputPolicy{},
	)
	require.Error(t, err)
}

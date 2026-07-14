package oor

import (
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// randomP2TRScript returns a P2TR pkScript with a random key.
func randomP2TRScript(t *testing.T) []byte {
	t.Helper()

	var key [32]byte
	_, err := rand.Read(key[:])
	require.NoError(t, err)

	return append([]byte{txscript.OP_1, 0x20}, key[:]...)
}

// TestBuildCheckpointAndArkPSBT asserts the builders produce a submit package
// that passes the shared submit validator.
func TestBuildCheckpointAndArkPSBT(t *testing.T) {
	t.Parallel()

	// This is an integration-style unit test over the tx builder layer:
	// - BuildCheckpointPSBT produces a checkpoint spend for a single VTXO.
	// - BuildArkPSBT consumes the checkpoint output and adds recipients +
	//   anchor output.
	// - ValidateSubmitPackage then enforces the shared structural rules.
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	vtxoWitness := &wire.TxOut{
		Value:    5000,
		PkScript: randomP2TRScript(t),
	}

	ownerLeafScript := []byte{
		txscript.OP_1,
		txscript.OP_1,
		txscript.OP_ADD,
		txscript.OP_2,
		txscript.OP_EQUAL,
	}

	cpResult, err := BuildCheckpointPSBT(policy, CheckpointInput{
		SpentVTXO: SpentVTXORef{
			Outpoint: wire.OutPoint{
				Hash:  chainhash.Hash{1},
				Index: 0,
			},
			Output: vtxoWitness,
		},
		OwnerLeafScript: ownerLeafScript,
	})
	require.NoError(t, err)
	require.NotNil(t, cpResult)

	checkpointTx := cpResult.PSBT.UnsignedTx
	require.NotNil(t, checkpointTx)
	require.Len(t, checkpointTx.TxOut, 2)
	require.Equal(
		t, arkscript.AnchorOutput().Value, checkpointTx.TxOut[1].Value,
	)
	require.Equal(
		t, arkscript.AnchorOutput().PkScript,
		checkpointTx.TxOut[1].PkScript,
	)

	arkPsbt, err := BuildArkPSBT([]CheckpointOutput{
		{
			Txid:           checkpointTx.TxHash(),
			Output:         checkpointTx.TxOut[0],
			TapTreeEncoded: cpResult.TapTreeEncoded,
		},
	}, []RecipientOutput{
		{
			PkScript: randomP2TRScript(t),
			Value:    5000,
		},
	})
	require.NoError(t, err)
	require.NotNil(t, arkPsbt)

	_, err = ValidateSubmitPackage(arkPsbt, []*psbt.Packet{cpResult.PSBT})
	require.NoError(t, err)
}

// TestBuildCheckpointPSBTPreservesOwnerLeafPolicy verifies that building from
// a semantic owner-leaf policy compiles the owner leaf and stores the
// authoritative tap tree on the checkpoint output itself.
func TestBuildCheckpointPSBTPreservesOwnerLeafPolicy(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	ownerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	ownerLeafPolicy, err := (arkscript.LeafTemplate{
		Node: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{
				ownerKey.PubKey(),
				operatorKey.PubKey(),
			},
		},
	}).Encode()
	require.NoError(t, err)

	cpResult, err := BuildCheckpointPSBT(policy, CheckpointInput{
		SpentVTXO: SpentVTXORef{
			Outpoint: wire.OutPoint{
				Hash:  chainhash.Hash{2},
				Index: 0,
			},
			Output: &wire.TxOut{
				Value:    5000,
				PkScript: randomP2TRScript(t),
			},
		},
		OwnerLeafPolicy: ownerLeafPolicy,
	})
	require.NoError(t, err)

	require.Equal(t, ownerLeafPolicy, cpResult.OwnerLeafPolicy)
	require.Equal(
		t, cpResult.TapTreeEncoded,
		cpResult.PSBT.Outputs[0].TaprootTapTree,
	)
}

func TestRecipientOutPointUsesCanonicalRecipientOrder(t *testing.T) {
	t.Parallel()

	txid := chainhash.Hash{1, 2, 3}
	target := RecipientOutput{
		PkScript: hexBytes(t, "5120bbbb"),
		Value:    btcutil.Amount(500),
	}
	recipients := []RecipientOutput{
		{
			PkScript: hexBytes(t, "5120aaaa"),
			Value:    btcutil.Amount(2_000),
		},
		target,
		{
			PkScript: hexBytes(t, "5120cccc"),
			Value:    btcutil.Amount(1_000),
		},
	}

	outpoint, err := RecipientOutPoint(txid, recipients, target)
	require.NoError(t, err)
	require.Equal(t, txid, outpoint.Hash)
	require.EqualValues(t, 0, outpoint.Index)
}

func TestRecipientOutputIndexRejectsAmbiguousTarget(t *testing.T) {
	t.Parallel()

	target := RecipientOutput{
		PkScript: hexBytes(t, "5120bbbb"),
		Value:    btcutil.Amount(500),
	}

	_, err := RecipientOutputIndex(
		[]RecipientOutput{target, target}, target,
	)
	require.ErrorContains(t, err, "ambiguous")
}

func TestRecipientOutputValidatesTaprootAssetCommitment(t *testing.T) {
	t.Parallel()

	ownerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	policy, err := arkscript.NewVTXOPolicy(
		ownerKey.PubKey(), operatorKey.PubKey(), 10,
	)
	require.NoError(t, err)
	encodedPolicy, err := policy.Template.Encode()
	require.NoError(t, err)
	assetRoot := chainhash.Hash{1, 2, 3, 4}
	composed, err := arkscript.ComposeWithSiblingRoot(
		policy.CompiledPolicy, assetRoot,
	)
	require.NoError(t, err)
	pkScript, err := txscript.PayToTaprootScript(composed.OutputKey())
	require.NoError(t, err)

	output := RecipientOutput{
		PkScript:           pkScript,
		Value:              5000,
		VTXOPolicyTemplate: encodedPolicy,
		TaprootAssetRoot:   &assetRoot,
	}
	require.NoError(t, output.ValidateTaprootAssetCommitment())

	withoutPolicy := output
	withoutPolicy.VTXOPolicyTemplate = nil
	require.ErrorContains(
		t, withoutPolicy.ValidateTaprootAssetCommitment(),
		"recipient policy is required",
	)

	wrongRoot := assetRoot
	wrongRoot[0] ^= 1
	output.TaprootAssetRoot = &wrongRoot
	require.ErrorContains(
		t, output.ValidateTaprootAssetCommitment(),
		"asset root and pkscript mismatch",
	)
}

func hexBytes(t *testing.T, value string) []byte {
	t.Helper()

	decoded, err := hex.DecodeString(value)
	require.NoError(t, err)

	return decoded
}

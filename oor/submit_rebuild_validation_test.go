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

// rebuildMakeTestArkPSBT is a minimal Ark PSBT constructor for
// validateArkOutputs tests. It builds an ark tx with a configurable
// number of outputs of varying kinds.
func rebuildMakeTestArkPSBT(t *testing.T,
	pkScripts [][]byte) *psbt.Packet {

	t.Helper()

	tx := wire.NewMsgTx(2)

	var hash [32]byte
	hash[0] = 0xAB
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: hash, Index: 0},
	})

	for i, pkScript := range pkScripts {
		tx.AddTxOut(&wire.TxOut{
			Value:    int64(1000 + i),
			PkScript: pkScript,
		})
	}

	pkt, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	return pkt
}

// TestValidateArkOutputsRequiresAnchor asserts an Ark package with no
// anchor output is rejected (anchor is a canonical v0 invariant).
func TestValidateArkOutputsRequiresAnchor(t *testing.T) {
	t.Parallel()

	payScript := []byte{0x51, 0x20, 0x01}
	arkPSBT := rebuildMakeTestArkPSBT(t, [][]byte{payScript})

	err := validateArkOutputs(
		[]oortx.RecipientOutput{{PkScript: payScript, Value: 1000}},
		SubmitOutputPolicy{}, arkPSBT,
	)
	require.ErrorContains(t, err, "missing anchor")
}

// TestValidateArkOutputsRejectsMultipleAnchors asserts exactly one
// anchor output is allowed.
func TestValidateArkOutputsRejectsMultipleAnchors(t *testing.T) {
	t.Parallel()

	anchor := arkscript.AnchorOutput().PkScript
	payScript := []byte{0x51, 0x20, 0x01}
	arkPSBT := rebuildMakeTestArkPSBT(
		t, [][]byte{payScript, anchor, anchor},
	)

	err := validateArkOutputs(
		[]oortx.RecipientOutput{{PkScript: payScript, Value: 1000}},
		SubmitOutputPolicy{}, arkPSBT,
	)
	require.ErrorContains(t, err, "multiple anchor")
}

// TestValidateArkOutputsRejectsMultipleOpReturn asserts at most one
// OP_RETURN output is permitted.
func TestValidateArkOutputsRejectsMultipleOpReturn(t *testing.T) {
	t.Parallel()

	anchor := arkscript.AnchorOutput().PkScript
	payScript := []byte{0x51, 0x20, 0x01}
	op1 := []byte{0x6a, 0x02, 0x01, 0x02}
	op2 := []byte{0x6a, 0x02, 0x03, 0x04}

	arkPSBT := rebuildMakeTestArkPSBT(
		t, [][]byte{payScript, anchor, op1, op2},
	)

	err := validateArkOutputs(
		[]oortx.RecipientOutput{{PkScript: payScript, Value: 1000}},
		SubmitOutputPolicy{}, arkPSBT,
	)
	require.ErrorContains(t, err, "multiple op_return")
}

// TestValidateArkOutputsEnforcesDust asserts outputs below the dust
// threshold are rejected. This uses an Ark PSBT that includes an
// anchor so the missing-anchor check doesn't short-circuit first.
func TestValidateArkOutputsEnforcesDust(t *testing.T) {
	t.Parallel()

	anchor := arkscript.AnchorOutput().PkScript
	payScript := []byte{0x51, 0x20, 0x01}
	arkPSBT := rebuildMakeTestArkPSBT(t, [][]byte{payScript, anchor})

	err := validateArkOutputs(
		[]oortx.RecipientOutput{{PkScript: payScript, Value: 100}},
		SubmitOutputPolicy{DustAmount: 546}, arkPSBT,
	)
	require.ErrorContains(t, err, "below dust")
}

// TestValidateArkOutputsEnforcesMinMax asserts min/max amount
// constraints are applied when set. Each constraint is exercised
// separately via a t.Run subtest.
func TestValidateArkOutputsEnforcesMinMax(t *testing.T) {
	t.Parallel()

	anchor := arkscript.AnchorOutput().PkScript
	payScript := []byte{0x51, 0x20, 0x01}
	arkPSBT := rebuildMakeTestArkPSBT(t, [][]byte{payScript, anchor})

	t.Run("below min rejected", func(t *testing.T) {
		t.Parallel()

		err := validateArkOutputs(
			[]oortx.RecipientOutput{{
				PkScript: payScript, Value: 100,
			}},
			SubmitOutputPolicy{MinVTXOAmount: 1000}, arkPSBT,
		)
		require.ErrorContains(t, err, "below min")
	})

	t.Run("above max rejected", func(t *testing.T) {
		t.Parallel()

		err := validateArkOutputs(
			[]oortx.RecipientOutput{{
				PkScript: payScript, Value: 5000,
			}},
			SubmitOutputPolicy{MaxVTXOAmount: 1000}, arkPSBT,
		)
		require.ErrorContains(t, err, "above max")
	})
}

// TestValidateRebuildRecordRejectsNotFound asserts an unknown VTXO
// outpoint is rejected cleanly.
func TestValidateRebuildRecordRejectsNotFound(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := vtxo.NewInMemoryStore()

	desc := VTXOSigningDescriptor{
		Outpoint: wire.OutPoint{Hash: [32]byte{0x01}, Index: 0},
	}

	_, err := validateRebuildRecord(ctx, store, desc)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

// TestValidateRebuildRecordRejectsNonSpendable asserts that a VTXO
// in a non-spendable state (e.g. Spent) is rejected.
func TestValidateRebuildRecordRejectsNonSpendable(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := vtxo.NewInMemoryStore()

	outpoint := wire.OutPoint{Hash: [32]byte{0x02}, Index: 0}
	err := store.Create(ctx, &vtxo.Record{
		Outpoint: outpoint,
		Value:    10000,
		PkScript: []byte{0x51, 0x20, 0x01},
		Status:   vtxo.StatusSpent,
	})
	require.NoError(t, err)

	desc := VTXOSigningDescriptor{Outpoint: outpoint}
	_, err = validateRebuildRecord(ctx, store, desc)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not spendable")
}

// TestValidateRebuildRecordRejectsPkScriptMismatch asserts that a
// descriptor whose pkScript derived from the policy template does not
// match the stored VTXO pkScript is rejected.
func TestValidateRebuildRecordRejectsPkScriptMismatch(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	ownerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	exitDelay := uint32(144)

	store := vtxo.NewInMemoryStore()
	outpoint := wire.OutPoint{Hash: [32]byte{0x03}, Index: 0}
	err = store.Create(ctx, &vtxo.Record{
		Outpoint: outpoint,
		Value:    10000,
		// Wrong pkScript, won't match what the policy derives to.
		PkScript: []byte{0x51, 0x20, 0xFF},
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	desc := VTXOSigningDescriptor{
		Outpoint: outpoint,
		VTXOPolicyTemplate: rebuildStandardPolicyTemplate(
			t, ownerKey.PubKey(), operatorKey.PubKey(), exitDelay,
		),
		SpendPath: rebuildStandardCollabSpendPath(
			t, ownerKey.PubKey(), operatorKey.PubKey(), exitDelay,
		),
	}

	_, err = validateRebuildRecord(ctx, store, desc)
	require.Error(t, err)
	require.Contains(t, err.Error(), "pkscript mismatch")
}

// TestValidateRebuildRecordRejectsPolicyTemplateMismatch asserts
// that a descriptor whose policy template disagrees with the stored
// record's persisted template is rejected.
func TestValidateRebuildRecordRejectsPolicyTemplateMismatch(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	ownerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	exitDelay := uint32(144)

	vtxoTapKey, err := arkscript.VTXOTapKey(
		ownerKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)
	pkScript, err := txscript.PayToTaprootScript(vtxoTapKey)
	require.NoError(t, err)

	// Store the VTXO with a specific policy template.
	storedTemplate := rebuildStandardPolicyTemplate(
		t, ownerKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)

	store := vtxo.NewInMemoryStore()
	outpoint := wire.OutPoint{Hash: [32]byte{0x04}, Index: 0}
	err = store.Create(ctx, &vtxo.Record{
		Outpoint:       outpoint,
		Value:          10000,
		PkScript:       pkScript,
		Status:         vtxo.StatusLive,
		PolicyTemplate: storedTemplate,
	})
	require.NoError(t, err)

	// Submit a descriptor claiming a different exit delay (so the
	// template bytes differ).
	desc := VTXOSigningDescriptor{
		Outpoint: outpoint,
		VTXOPolicyTemplate: rebuildStandardPolicyTemplate(
			t, ownerKey.PubKey(), operatorKey.PubKey(),
			exitDelay+1,
		),
		SpendPath: rebuildStandardCollabSpendPath(
			t, ownerKey.PubKey(), operatorKey.PubKey(),
			exitDelay+1,
		),
	}

	_, err = validateRebuildRecord(ctx, store, desc)
	require.Error(t, err)
	// Either the pkScript mismatch or the template mismatch fires
	// first depending on hashing order; both are correct rejection
	// paths, so accept either.
	msg := err.Error()
	require.True(
		t,
		containsAny(msg, []string{
			"policy template mismatch",
			"pkscript mismatch",
		}),
		"unexpected error %q", msg,
	)
}

// containsAny returns true when any of the needles appears in s.
func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if len(n) == 0 {
			continue
		}
		for i := 0; i+len(n) <= len(s); i++ {
			if s[i:i+len(n)] == n {
				return true
			}
		}
	}

	return false
}

// TestFindOwnerLeafScriptRequiresOperatorKeyInLeaf asserts that a
// submitted OwnerLeafPolicy whose AST does not reference the
// operator key is rejected before any checkpoint rebuild.
func TestFindOwnerLeafScriptRequiresOperatorKeyInLeaf(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	ownerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// OwnerLeafPolicy that contains ONLY the owner key — no
	// operator reference.
	badLeaf := arkscript.LeafTemplate{
		Node: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{ownerKey.PubKey()},
		},
	}
	badBytes, err := badLeaf.Encode()
	require.NoError(t, err)

	ark := rebuildMakeTestArkPSBT(t, [][]byte{{0x51, 0x20, 0x01}})
	checkpoint := makeTestPSBT(t, 7)
	// Populate the tap tree so the earlier "tap tree not found"
	// guard doesn't short-circuit before we hit the operator-key
	// check.
	checkpoint.Outputs[0].TaprootTapTree = []byte{0x01}

	desc := VTXOSigningDescriptor{
		Outpoint:        wire.OutPoint{Hash: [32]byte{0x05}, Index: 0},
		OwnerLeafPolicy: badBytes,
	}

	_, err = findOwnerLeafScript(ark, 0, checkpoint, desc,
		arkscript.CheckpointPolicy{
			OperatorKey: operatorKey.PubKey(), CSVDelay: 144,
		})
	require.Error(t, err)
	require.Contains(
		t, err.Error(),
		"does not contain operator key",
	)
}

// TestFindOwnerLeafScriptRequiresOwnerLeafPolicy asserts that a
// descriptor missing OwnerLeafPolicy bytes is rejected explicitly
// rather than nil-dereferencing downstream.
func TestFindOwnerLeafScriptRequiresOwnerLeafPolicy(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	ark := rebuildMakeTestArkPSBT(t, [][]byte{{0x51, 0x20, 0x01}})

	// The checkpoint must have a non-empty TaprootTapTree so the
	// earlier guard does not short-circuit; otherwise we'd hit
	// "checkpoint output tap tree not found" first.
	checkpoint := makeTestPSBT(t, 8)
	checkpoint.Outputs[0].TaprootTapTree = []byte{0x01}

	desc := VTXOSigningDescriptor{
		Outpoint: wire.OutPoint{Hash: [32]byte{0x06}, Index: 0},
		// OwnerLeafPolicy deliberately empty.
	}

	_, err = findOwnerLeafScript(ark, 0, checkpoint, desc,
		arkscript.CheckpointPolicy{
			OperatorKey: operatorKey.PubKey(), CSVDelay: 144,
		})
	require.ErrorContains(t, err, "owner leaf policy not found")
}

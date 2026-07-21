package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestTransferInputSnapshotRoundTrip verifies a durable TransferInputSnapshot
// contains enough information to rebuild the VTXO signing descriptor for a
// custom spend after process restart. The round trip covers the custom
// spend-path script, control block, condition witness, required sequence and
// locktime, and externally supplied tapscript signatures. Those fields are the
// resume-critical material for cooperative refunds, where the client may
// already hold a server signature and must reconstruct the same custom OOR
// input before final witness assembly.
func TestTransferInputSnapshotRoundTrip(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	exitDelay := uint32(10)

	tapScript, err := arkscript.VTXOTapScript(
		clientKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	policy, err := arkscript.NewVTXOPolicy(
		clientKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)
	assetRoot := chainhash.Hash{1, 2, 3, 4}
	composed, err := arkscript.ComposeWithSiblingRoot(
		policy.CompiledPolicy, assetRoot,
	)
	require.NoError(t, err)
	collabSpend, err := policy.CollabSpendInfo()
	require.NoError(t, err)
	collabIndex := policy.ScriptIndex(collabSpend.WitnessScript)
	require.GreaterOrEqual(t, collabIndex, 0)
	composedCollabSpend, err := composed.SpendInfo(collabIndex)
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(composed.OutputKey())
	require.NoError(t, err)

	ownerLeaf, err := arkscript.MultiSigCollabTapLeaf(
		clientKey.PubKey(), operatorKey.PubKey(),
	)
	require.NoError(t, err)

	ownerLeafPolicy, err := arkscript.LeafTemplate{
		Node: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{
				clientKey.PubKey(),
				operatorKey.PubKey(),
			},
		},
	}.Encode()
	require.NoError(t, err)

	vtxoPolicyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		clientKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	in := &TransferInput{
		VTXO: &vtxo.Descriptor{
			Outpoint: wire.OutPoint{
				Hash: [32]byte{
					1,
				},
				Index: 2,
			},
			Amount:   btcutil.Amount(5000),
			PkScript: pkScript,
			ClientKey: keychain.KeyDescriptor{
				KeyLocator: keychain.KeyLocator{
					Family: 1,
					Index:  2,
				},
				PubKey: clientKey.PubKey(),
			},
			OperatorKey:        operatorKey.PubKey(),
			TapScript:          tapScript,
			RelativeExpiry:     exitDelay,
			Status:             vtxo.VTXOStatusLive,
			TaprootAssetRoot:   &assetRoot,
			TaprootAssetRef:    "asset-id:010203",
			TaprootAssetAmount: 21,
		},
		VTXOPolicyTemplate: vtxoPolicyTemplate,
		TaprootAssetRoot:   &assetRoot,
		OwnerLeafScript:    ownerLeaf.Script,
		OwnerLeafPolicy:    ownerLeafPolicy,
	}
	in.CustomSpend = &arkscript.SpendPath{
		SpendInfo: &arkscript.SpendInfo{
			WitnessScript: composedCollabSpend.WitnessScript,
			ControlBlock:  composedCollabSpend.ControlBlock,
		},
		RequiredSequence: wire.MaxTxInSequenceNum - 1,
		RequiredLockTime: 113,
		Conditions: [][]byte{
			{
				0xaa,
				0xbb,
			},
		},
	}
	in.ExternalSignatures = []ExternalTaprootScriptSignature{
		{
			PubKey:        operatorKey.PubKey(),
			WitnessScript: ownerLeaf.Script,
			Signature: []byte{
				0x30,
				0x31,
			},
			SigHash: txscript.SigHashDefault,
		},
	}

	snap, err := in.ToSnapshot()
	require.NoError(t, err)
	require.NotNil(t, snap)
	require.Equal(t, in.VTXO.Outpoint, snap.Outpoint)
	require.Equal(t, int64(in.VTXO.Amount), snap.AmountSat)
	require.Equal(
		t, int32(in.VTXO.ClientKey.KeyLocator.Family),
		snap.ClientKeyFamily,
	)
	require.Equal(
		t, in.VTXO.ClientKey.KeyLocator.Index, snap.ClientKeyIndex,
	)
	require.Equal(
		t, in.VTXO.ClientKey.PubKey.SerializeCompressed(),
		snap.ClientPubKey,
	)
	require.Equal(
		t, in.VTXO.OperatorKey.SerializeCompressed(),
		snap.OperatorPubKey,
	)
	require.Equal(t, in.VTXO.RelativeExpiry, snap.ExitDelay)
	require.Equal(t, in.OwnerLeafScript, snap.OwnerLeafScript)
	require.Equal(t, in.OwnerLeafPolicy, snap.OwnerLeafPolicy)
	require.Equal(t, in.VTXOPolicyTemplate, snap.VTXOPolicyTemplate)
	require.Equal(t, in.TaprootAssetRoot, snap.TaprootAssetRoot)
	require.Equal(t, in.VTXO.TaprootAssetRef, snap.TaprootAssetRef)
	require.Equal(t, in.VTXO.TaprootAssetAmount,
		snap.TaprootAssetAmount)
	require.Equal(t, in.CustomSpend.RequiredSequence,
		snap.RequiredSequence)
	require.Equal(t, in.CustomSpend.RequiredLockTime,
		snap.RequiredLockTime)
	requireExternalSignatureEqual(
		t, in.ExternalSignatures[0], snap.ExternalSignatures[0],
	)
	rawSnapshot, err := encodeTransferInputSnapshot(snap)
	require.NoError(t, err)
	snap, err = decodeTransferInputSnapshot(rawSnapshot)
	require.NoError(t, err)
	require.Equal(t, in.TaprootAssetRoot, snap.TaprootAssetRoot)
	require.Equal(t, in.VTXO.TaprootAssetRef, snap.TaprootAssetRef)
	require.Equal(t, in.VTXO.TaprootAssetAmount,
		snap.TaprootAssetAmount)

	rebuilt, err := TransferInputFromSnapshot(snap)
	require.NoError(t, err)
	require.NotNil(t, rebuilt.VTXO)
	require.Equal(t, in.VTXO.Outpoint, rebuilt.VTXO.Outpoint)
	require.Equal(t, in.VTXO.Amount, rebuilt.VTXO.Amount)
	require.Equal(t, in.VTXO.PkScript, rebuilt.VTXO.PkScript)
	require.Equal(
		t, in.VTXO.ClientKey.KeyLocator,
		rebuilt.VTXO.ClientKey.KeyLocator,
	)
	require.Equal(
		t, in.VTXO.ClientKey.PubKey.SerializeCompressed(),
		rebuilt.VTXO.ClientKey.PubKey.SerializeCompressed(),
	)
	require.Equal(
		t, in.VTXO.OperatorKey.SerializeCompressed(),
		rebuilt.VTXO.OperatorKey.SerializeCompressed(),
	)
	require.Equal(t, in.VTXO.RelativeExpiry, rebuilt.VTXO.RelativeExpiry)
	require.NotNil(t, rebuilt.VTXO.TapScript)
	require.Equal(t, in.OwnerLeafScript, rebuilt.OwnerLeafScript)
	require.Equal(t, in.OwnerLeafPolicy, rebuilt.OwnerLeafPolicy)
	require.Equal(t, in.VTXOPolicyTemplate, rebuilt.VTXOPolicyTemplate)
	require.Equal(t, in.TaprootAssetRoot, rebuilt.TaprootAssetRoot)
	require.Equal(t, in.VTXO.TaprootAssetRef,
		rebuilt.VTXO.TaprootAssetRef)
	require.Equal(
		t, in.VTXO.TaprootAssetAmount, rebuilt.VTXO.TaprootAssetAmount,
	)
	spendPath, err := rebuilt.EffectiveSpendPath()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(spendPath.ControlBlock), 32)
	require.Equal(
		t, assetRoot[:],
		spendPath.ControlBlock[len(spendPath.ControlBlock)-32:],
	)
	require.NotNil(t, rebuilt.CustomSpend)
	require.Equal(
		t, in.CustomSpend.RequiredSequence,
		rebuilt.CustomSpend.RequiredSequence,
	)
	require.Equal(
		t, in.CustomSpend.RequiredLockTime,
		rebuilt.CustomSpend.RequiredLockTime,
	)
	require.Equal(
		t, in.CustomSpend.Conditions, rebuilt.CustomSpend.Conditions,
	)
	requireExternalSignatureEqual(
		t, in.ExternalSignatures[0], rebuilt.ExternalSignatures[0],
	)

	disagreedRoot := assetRoot
	disagreedRoot[0] ^= 1
	rebuilt.TaprootAssetRoot = &disagreedRoot
	err = rebuilt.Validate()
	require.ErrorContains(t, err, "asset roots disagree")
	rebuilt.TaprootAssetRoot = &assetRoot

	rebuilt.VTXO.TaprootAssetAmount = 0
	err = rebuilt.Validate()
	require.ErrorContains(t, err, "ref and amount must both be provided")
	rebuilt.VTXO.TaprootAssetAmount = in.VTXO.TaprootAssetAmount

	wrongRoot := assetRoot
	wrongRoot[0] ^= 1
	rebuilt.TaprootAssetRoot = &wrongRoot
	rebuilt.VTXO.TaprootAssetRoot = &wrongRoot
	err = rebuilt.Validate()
	require.ErrorContains(t, err, "asset root and vtxo pkscript mismatch")
}

// TestCloneExternalSignaturesDeepCopiesMutableBytes verifies persisted custom
// signature snapshots do not alias the mutable witness-script or signature
// slices held by a live TransferInput. Restart recovery depends on these
// snapshots being immutable once captured; otherwise a later signing attempt
// could mutate the in-memory transfer input and silently corrupt the durable
// external signature material used to resume a cooperative refund.
func TestCloneExternalSignaturesDeepCopiesMutableBytes(t *testing.T) {
	t.Parallel()

	_, key := btcec.PrivKeyFromBytes([]byte{
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
	})
	original := []ExternalTaprootScriptSignature{
		{
			PubKey: key,
			WitnessScript: []byte{
				0x51,
				0x20,
			},
			Signature: []byte{
				0xaa,
				0xbb,
			},
			SigHash: txscript.SigHashDefault,
		},
	}

	clone := cloneExternalSignatures(original)
	requireExternalSignatureEqual(t, original[0], clone[0])

	clone[0].WitnessScript[0] = 0x00
	clone[0].Signature[0] = 0x00

	require.Equal(t, []byte{0x51, 0x20}, original[0].WitnessScript)
	require.Equal(t, []byte{0xaa, 0xbb}, original[0].Signature)
	require.Same(t, original[0].PubKey, clone[0].PubKey)
}

// TestTransferInputValidateRejectsNil asserts nil receivers are rejected.
func TestTransferInputValidateRejectsNil(t *testing.T) {
	t.Parallel()

	var in *TransferInput
	err := in.Validate()
	require.Error(t, err)
}

// TestTransferInputFromSnapshotRejectsMissingFields asserts malformed snapshots
// are rejected early.
func TestTransferInputFromSnapshotRejectsMissingFields(t *testing.T) {
	t.Parallel()

	_, err := TransferInputFromSnapshot(nil)
	require.Error(t, err)

	_, err = TransferInputFromSnapshot(&TransferInputSnapshot{})
	require.Error(t, err)

	_, err = TransferInputFromSnapshot(&TransferInputSnapshot{
		AmountSat: 1,
	})
	require.Error(t, err)

	_, err = TransferInputFromSnapshot(&TransferInputSnapshot{
		AmountSat:       1,
		ClientPubKey:    []byte{0x02},
		OperatorPubKey:  []byte{0x02},
		ExitDelay:       1,
		OwnerLeafScript: []byte{0x51},
	})
	require.Error(t, err)
}

func requireExternalSignatureEqual(t *testing.T,
	want ExternalTaprootScriptSignature,
	got ExternalTaprootScriptSignature) {

	t.Helper()

	require.Equal(
		t, want.PubKey.SerializeCompressed(),
		got.PubKey.SerializeCompressed(),
	)
	require.Equal(t, want.WitnessScript, got.WitnessScript)
	require.Equal(t, want.Signature, got.Signature)
	require.Equal(t, want.SigHash, got.SigHash)
}

// TestTransferInputToSnapshotRejectsMissingVTXO asserts we require a full VTXO
// descriptor before snapshotting.
func TestTransferInputToSnapshotRejectsMissingVTXO(t *testing.T) {
	t.Parallel()

	in := &TransferInput{}
	_, err := in.ToSnapshot()
	require.Error(t, err)
}
